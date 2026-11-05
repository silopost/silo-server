package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

// User is the signed-in user.
type User struct {
	ID          string `json:"id" example:"u_1"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	IsAdmin     bool   `json:"isAdmin"`
}

// Settings are the per-user preferences (stored in users.settings).
type Settings struct {
	Density                  string  `json:"density" enum:"default,comfortable,compact"`
	UndoSendSeconds          int     `json:"undoSendSeconds" doc:"Delay before a sent message leaves, in seconds."`
	Signature                string  `json:"signature"`
	Timezone                 string  `json:"timezone" doc:"IANA time zone name." example:"Europe/Warsaw"`
	KeyboardShortcutsEnabled bool    `json:"keyboardShortcutsEnabled"`
	ConversationView         bool    `json:"conversationView"`
	ActiveAccountID          *string `json:"activeAccountId" doc:"Account shown by default; the first account when unset."`
}

// SessionBody is the state of the signed-in user.
type SessionBody struct {
	User      User      `json:"user"`
	CSRFToken string    `json:"csrfToken" doc:"Send as X-Silo-CSRF on every mutating request."`
	Settings  Settings  `json:"settings"`
	Accounts  []Account `json:"accounts"`
}

// SessionInfo describes one signed-in browser or device.
type SessionInfo struct {
	ID         string    `json:"id" example:"s_3k"`
	CreatedAt  time.Time `json:"createdAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	UserAgent  string    `json:"userAgent"`
	IP         *string   `json:"ip"`
	Current    bool      `json:"current" doc:"Whether this is the session making the request."`
}

type loginInput struct {
	Body struct {
		Email    string `json:"email" minLength:"1" maxLength:"254" doc:"Mailbox address (or local part, per SILO_DEFAULT_LOGIN_FORMAT)."`
		Password string `json:"password" minLength:"1" maxLength:"1024"`
	}
	remote, userAgent string
}

// Resolve captures request details that are not part of the schema.
func (i *loginInput) Resolve(ctx huma.Context) []error {
	i.remote, i.userAgent = ctx.RemoteAddr(), ctx.Header("User-Agent")
	return nil
}

type sessionOutput struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
	Body      SessionBody
}

type sessionGetOutput struct{ Body SessionBody }

type sessionsOutput struct {
	Body struct {
		Sessions []SessionInfo `json:"sessions"`
	}
}

type sessionIDInput struct {
	ID string `path:"id" example:"s_3k"`
}

type logoutOutput struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
}

func (d *deps) sessionCookie(token string, expires time.Time) http.Cookie {
	return http.Cookie{
		Name: SessionCookie, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: d.cookieSecure, SameSite: http.SameSiteLaxMode,
	}
}

func (d *deps) clearedCookie() http.Cookie {
	return http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: d.cookieSecure, SameSite: http.SameSiteLaxMode,
	}
}

func (d *deps) registerSession(a huma.API) {
	huma.Register(a, huma.Operation{
		OperationID: "login", Method: http.MethodPost, Path: "/session", Tags: []string{"session"},
		Summary: "Sign in",
		Description: "In imap mode the credentials are verified with LOGIN on the default mail server. " +
			"A new user and first account are created on first success. When the mail server is " +
			"unreachable, a known user with the right password is let in and the account reports state `backoff`. " +
			"Every failure answers 401 invalid_credentials after a constant minimum time.",
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusBadGateway},
		Middlewares: huma.Middlewares{d.checkOrigin},
	}, func(ctx context.Context, in *loginInput) (*sessionOutput, error) {
		res, err := d.auth.Login(ctx, in.Body.Email, in.Body.Password, auth.Client{IP: in.remote, UserAgent: in.userAgent})
		var rl *auth.RateLimitError
		switch {
		case errors.As(err, &rl):
			return nil, errRateLimited(rl.RetryAfter)
		case errors.Is(err, auth.ErrInvalidCredentials):
			return nil, errInvalidCredentials()
		case errors.Is(err, auth.ErrUpstream):
			return nil, problem(http.StatusBadGateway, CodeUpstreamIMAP, "The mail server could not be reached.")
		case err != nil:
			d.log.Error("login", "error", err)
			return nil, errInternal(err)
		}
		body, err := d.sessionBody(ctx, res.User, res.Session)
		if err != nil {
			return nil, err
		}
		return &sessionOutput{SetCookie: d.sessionCookie(res.Token, auth.AbsoluteExpiry(res.Session)), Body: body}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "getSession", Method: http.MethodGet, Path: "/session", Tags: []string{"session"},
		Summary: "Get the current session",
		Errors:  []int{http.StatusUnauthorized},
	}), func(ctx context.Context, _ *struct{}) (*sessionGetOutput, error) {
		sess := sessionFrom(ctx)
		user, err := d.auth.User(ctx, sess.UserID)
		if err != nil {
			return nil, errInternal(err)
		}
		body, err := d.sessionBody(ctx, user, sess)
		if err != nil {
			return nil, err
		}
		return &sessionGetOutput{Body: body}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "logout", Method: http.MethodDelete, Path: "/session", Tags: []string{"session"},
		Summary: "Sign out", DefaultStatus: http.StatusNoContent,
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden},
	}), func(ctx context.Context, _ *struct{}) (*logoutOutput, error) {
		sess := sessionFrom(ctx)
		if err := d.auth.Revoke(ctx, sess.UserID, sess.ID); err != nil && !errors.Is(err, auth.ErrNotFound) {
			return nil, errInternal(err)
		}
		return &logoutOutput{SetCookie: d.clearedCookie()}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "listSessions", Method: http.MethodGet, Path: "/sessions", Tags: []string{"session"},
		Summary: "List the sessions of the user",
		Errors:  []int{http.StatusUnauthorized},
	}), func(ctx context.Context, _ *struct{}) (*sessionsOutput, error) {
		cur := sessionFrom(ctx)
		list, err := d.auth.Sessions(ctx, cur.UserID)
		if err != nil {
			return nil, errInternal(err)
		}
		out := &sessionsOutput{}
		out.Body.Sessions = make([]SessionInfo, 0, len(list))
		for _, s := range list {
			info := SessionInfo{
				ID: formatID("s_", s.ID), CreatedAt: s.CreatedAt.Time, LastSeenAt: s.LastSeenAt.Time,
				ExpiresAt: s.ExpiresAt.Time, UserAgent: s.UserAgent, Current: s.ID == cur.ID,
			}
			if s.Ip != nil {
				ip := s.Ip.String()
				info.IP = &ip
			}
			out.Body.Sessions = append(out.Body.Sessions, info)
		}
		return out, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "revokeSession", Method: http.MethodDelete, Path: "/sessions/{id}", Tags: []string{"session"},
		Summary: "Revoke a session", DefaultStatus: http.StatusNoContent,
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound},
	}), func(ctx context.Context, in *sessionIDInput) (*logoutOutput, error) {
		cur := sessionFrom(ctx)
		id, ok := parseID("s_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		if err := d.auth.Revoke(ctx, cur.UserID, id); err != nil {
			if errors.Is(err, auth.ErrNotFound) {
				return nil, errNotFound()
			}
			return nil, errInternal(err)
		}
		out := &logoutOutput{}
		if id == cur.ID {
			out.SetCookie = d.clearedCookie()
		}
		return out, nil
	})
}

func (d *deps) sessionBody(ctx context.Context, user sqlcgen.User, sess sqlcgen.Session) (SessionBody, error) {
	accs, err := d.accts.List(ctx, user.ID)
	if err != nil {
		return SessionBody{}, errInternal(err)
	}
	return SessionBody{
		User:      User{ID: formatID("u_", user.ID), Email: user.Email, DisplayName: user.DisplayName, IsAdmin: user.IsAdmin},
		CSRFToken: sess.CsrfToken,
		Settings:  settingsFrom(user.Settings, accs),
		Accounts:  accountList(accs),
	}, nil
}

// settingsFrom overlays users.settings (snake_case JSON) on the defaults.
func settingsFrom(raw []byte, accs []sqlcgen.Account) Settings {
	s := Settings{
		Density: "default", UndoSendSeconds: 5, Timezone: "UTC",
		KeyboardShortcutsEnabled: true, ConversationView: true,
	}
	var stored struct {
		Density                  *string `json:"density"`
		UndoSendSeconds          *int    `json:"undo_send_seconds"`
		Signature                *string `json:"signature"`
		Timezone                 *string `json:"timezone"`
		KeyboardShortcutsEnabled *bool   `json:"keyboard_shortcuts_enabled"`
		ConversationView         *bool   `json:"conversation_view"`
		ActiveAccountID          *int64  `json:"active_account_id"`
	}
	_ = json.Unmarshal(raw, &stored) // malformed settings fall back to defaults
	if stored.Density != nil {
		s.Density = *stored.Density
	}
	if stored.UndoSendSeconds != nil {
		s.UndoSendSeconds = *stored.UndoSendSeconds
	}
	if stored.Signature != nil {
		s.Signature = *stored.Signature
	}
	if stored.Timezone != nil && *stored.Timezone != "" {
		s.Timezone = *stored.Timezone
	}
	if stored.KeyboardShortcutsEnabled != nil {
		s.KeyboardShortcutsEnabled = *stored.KeyboardShortcutsEnabled
	}
	if stored.ConversationView != nil {
		s.ConversationView = *stored.ConversationView
	}
	active := int64(0)
	if stored.ActiveAccountID != nil {
		for _, a := range accs {
			if a.ID == *stored.ActiveAccountID {
				active = a.ID
			}
		}
	}
	if active == 0 && len(accs) > 0 {
		active = accs[0].ID
	}
	if active != 0 {
		id := formatID("acc_", active)
		s.ActiveAccountID = &id
	}
	return s
}
