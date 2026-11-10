package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
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
	TOTPEnabled bool   `json:"totpEnabled" doc:"Whether sign-in asks for a TOTP code (mode local)."`
}

// Settings are the per-user preferences (stored in users.settings).
type Settings struct {
	Density                  string     `json:"density" enum:"default,comfortable,compact"`
	UndoSendSeconds          int        `json:"undoSendSeconds" doc:"Delay before a sent message leaves, in seconds."`
	Signature                string     `json:"signature"`
	Timezone                 string     `json:"timezone" doc:"IANA time zone name." example:"Europe/Warsaw"`
	KeyboardShortcutsEnabled bool       `json:"keyboardShortcutsEnabled"`
	ConversationView         bool       `json:"conversationView"`
	ActiveAccountID          *string    `json:"activeAccountId" doc:"Account shown by default; the first account when unset."`
	DesktopNotifications     bool       `json:"desktopNotifications" doc:"Whether the user wants desktop/system notifications of new mail; each client asks the system for its own permission."`
	Language                 string     `json:"language" enum:"en,pl" doc:"UI language shared by every client of the user (BCP 47 code of a supported language); en until the user chooses one."`
	TrustedImageSenders      []string   `json:"trustedImageSenders" nullable:"false" doc:"Sender addresses (lowercase) whose messages show external images without asking; always an array."`
	UpdatedAt                *time.Time `json:"updatedAt" doc:"Time of the last PATCH /settings; null before the first one."`
}

// SessionBody is the state of the signed-in user.
type SessionBody struct {
	User      User      `json:"user"`
	CSRFToken string    `json:"csrfToken" doc:"Send as X-Silo-CSRF on every mutating request (cookie sessions only)."`
	Token     string    `json:"token,omitempty" doc:"Bearer token of a session opened with client.kind=bearer; returned once, by POST /session only."`
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
	Kind       string    `json:"kind" enum:"cookie,bearer" doc:"cookie: browser session; bearer: native client token."`
	ClientName string    `json:"clientName" doc:"Device name given by a native client at sign-in; empty for browsers."`
	IP         *string   `json:"ip"`
	Current    bool      `json:"current" doc:"Whether this is the session making the request."`
}

type loginInput struct {
	Body struct {
		Email    string       `json:"email" minLength:"1" maxLength:"254" doc:"Mailbox address (or local part, per SILO_DEFAULT_LOGIN_FORMAT)."`
		Password string       `json:"password" minLength:"1" maxLength:"1024"`
		Client   *LoginClient `json:"client,omitempty" doc:"Kind of session to open; a browser cookie session when omitted."`
		TOTPCode string       `json:"totpCode,omitempty" maxLength:"32" doc:"Mode local: 6-digit TOTP code or a backup code, for users with TOTP enabled (send it after a 401 totp_required)."`
	}
	remote, userAgent string
}

// LoginClient selects the session kind at sign-in (ADR 0007).
type LoginClient struct {
	Kind string `json:"kind" enum:"cookie,bearer" doc:"bearer: native client; the token is returned in the body and no cookie is set."`
	Name string `json:"name,omitempty" maxLength:"100" doc:"Device name shown in the session list, e.g. \"Pixel 8\"."`
}

// Resolve captures request details that are not part of the schema.
func (i *loginInput) Resolve(ctx huma.Context) []error {
	i.remote, i.userAgent = ctx.RemoteAddr(), ctx.Header("User-Agent")
	return nil
}

type sessionOutput struct {
	SetCookie string `header:"Set-Cookie"`
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
	SetCookie string `header:"Set-Cookie"` // empty: no header
}

func (d *deps) sessionCookie(token string, expires time.Time) string {
	c := http.Cookie{
		Name: SessionCookie, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: d.cookieSecure, SameSite: http.SameSiteLaxMode,
	}
	return c.String()
}

// clearedCookie drops the session cookie of a cookie session; bearer sessions
// get no Set-Cookie at all.
func (d *deps) clearedCookie(sess sqlcgen.Session) string {
	if sess.Kind == auth.KindBearer {
		return ""
	}
	c := http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: d.cookieSecure, SameSite: http.SameSiteLaxMode,
	}
	return c.String()
}

func (d *deps) registerSession(a huma.API) {
	huma.Register(a, huma.Operation{
		OperationID: "login", Method: http.MethodPost, Path: "/session", Tags: []string{"session"},
		Summary: "Sign in",
		Description: "In imap mode the credentials are verified with LOGIN on the default mail server. " +
			"A new user and first account are created on first success. When the mail server is " +
			"unreachable, a known user with the right password is let in and the account reports state `backoff`. " +
			"In local mode the Silo password is checked (no mail server, no user or account is created); a user with TOTP " +
			"enabled must also send totpCode: without it a correct password answers 401 totp_required, with a wrong code " +
			"401 invalid_credentials (a backup code works once). " +
			"Every failure answers 401 invalid_credentials after a constant minimum time. " +
			"Browsers get the silo_session cookie; with client.kind=bearer (native clients) no cookie is set and the " +
			"body carries a token for the Authorization: Bearer header instead (shown once).",
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusBadGateway},
		Middlewares: huma.Middlewares{d.checkOrigin},
	}, func(ctx context.Context, in *loginInput) (*sessionOutput, error) {
		noteLogin(ctx, in.Body.Email)
		client := auth.Client{IP: in.remote, UserAgent: in.userAgent, Kind: auth.KindCookie}
		if c := in.Body.Client; c != nil {
			client.Kind, client.Name = c.Kind, c.Name
		}
		res, err := d.auth.SignIn(ctx, auth.Credentials{Email: in.Body.Email, Password: in.Body.Password, TOTPCode: in.Body.TOTPCode}, client)
		var rl *auth.RateLimitError
		switch {
		case errors.As(err, &rl):
			return nil, errRateLimited(rl.RetryAfter)
		case errors.Is(err, auth.ErrInvalidCredentials):
			return nil, errInvalidCredentials()
		case errors.Is(err, auth.ErrTOTPRequired):
			return nil, errTOTPRequired()
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
		if res.Session.Kind == auth.KindBearer {
			body.Token = res.Token
			return &sessionOutput{Body: body}, nil
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
		return &logoutOutput{SetCookie: d.clearedCookie(sess)}, nil
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
				ExpiresAt: s.ExpiresAt.Time, UserAgent: s.UserAgent, Kind: s.Kind, ClientName: s.ClientName,
				Current: s.ID == cur.ID,
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
			out.SetCookie = d.clearedCookie(cur)
		}
		return out, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "revokeOtherSessions", Method: http.MethodDelete, Path: "/sessions", Tags: []string{"session"},
		Summary:       "Sign out everywhere else",
		Description:   "Revokes every session of the user except the current one (their open event streams get session.expired). others=true is required.",
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden},
	}), func(ctx context.Context, in *revokeOthersInput) (*struct{}, error) {
		if !in.Others {
			return nil, problem(http.StatusBadRequest, CodeValidation, "Only others=true is supported (DELETE /session signs out the current session).")
		}
		cur := sessionFrom(ctx)
		if _, err := d.auth.RevokeOthers(ctx, cur.UserID, cur.ID); err != nil {
			return nil, errInternal(err)
		}
		return nil, nil
	})
}

type revokeOthersInput struct {
	Others bool `query:"others" required:"true" doc:"Must be true: revoke every session but the current one."`
}

func (d *deps) sessionBody(ctx context.Context, user sqlcgen.User, sess sqlcgen.Session) (SessionBody, error) {
	accs, err := d.accts.List(ctx, user.ID)
	if err != nil {
		return SessionBody{}, errInternal(err)
	}
	return SessionBody{
		User:      User{ID: formatID("u_", user.ID), Email: user.Email, DisplayName: user.DisplayName, IsAdmin: user.IsAdmin, TOTPEnabled: user.TotpEnabledAt.Valid},
		CSRFToken: sess.CsrfToken,
		Settings:  settingsFrom(user.Settings, accs),
		Accounts:  accountList(accs),
	}, nil
}

// settingsFrom overlays users.settings (snake_case JSON) on the defaults.
func settingsFrom(raw []byte, accs []sqlcgen.Account) Settings {
	s := Settings{
		Density: "default", UndoSendSeconds: 5, Timezone: "UTC",
		KeyboardShortcutsEnabled: true, ConversationView: true, Language: DefaultLanguage,
		TrustedImageSenders: []string{},
	}
	var stored struct {
		Density                  *string  `json:"density"`
		UndoSendSeconds          *int     `json:"undo_send_seconds"`
		Signature                *string  `json:"signature"`
		Timezone                 *string  `json:"timezone"`
		KeyboardShortcutsEnabled *bool    `json:"keyboard_shortcuts_enabled"`
		ConversationView         *bool    `json:"conversation_view"`
		ActiveAccountID          *int64   `json:"active_account_id"`
		DesktopNotifications     *bool    `json:"desktop_notifications"`
		Language                 *string  `json:"language"`
		TrustedImageSenders      []string `json:"trusted_image_senders"`
		UpdatedAt                *string  `json:"updated_at"`
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
	if stored.DesktopNotifications != nil {
		s.DesktopNotifications = *stored.DesktopNotifications
	}
	if stored.Language != nil && slices.Contains(Languages, *stored.Language) {
		s.Language = *stored.Language
	}
	if stored.TrustedImageSenders != nil {
		s.TrustedImageSenders, _ = NormalizeTrustedSenders(stored.TrustedImageSenders)
	}
	if stored.UpdatedAt != nil {
		if t, err := time.Parse(time.RFC3339Nano, *stored.UpdatedAt); err == nil {
			s.UpdatedAt = &t
		}
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
