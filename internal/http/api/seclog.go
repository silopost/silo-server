package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/seclog"
)

// secNote collects what the security log line of a request needs; handlers
// and middleware fill it through the context, the transformer and
// writeProblem record the problem answered.
type secNote struct {
	status int
	code   string
	login  string
	userID int64
}

type secKey struct{}

func noteOf(ctx context.Context) *secNote {
	n, _ := ctx.Value(secKey{}).(*secNote)
	return n
}

// noteLogin records the login a request is about (the sign-in email).
func noteLogin(ctx context.Context, login string) {
	if n := noteOf(ctx); n != nil {
		n.login = login
	}
}

// noteUser records the authenticated user.
func noteUser(ctx context.Context, userID int64) {
	if n := noteOf(ctx); n != nil {
		n.userID = userID
	}
}

// noteProblem records the problem answered.
func noteProblem(ctx context.Context, p *Problem) {
	if n := noteOf(ctx); n != nil && p != nil {
		n.status, n.code = p.Status, p.Code
	}
}

// problemTransformer sees every error body huma writes (handler errors).
func problemTransformer(ctx huma.Context, _ string, v any) (any, error) {
	if p, ok := v.(*Problem); ok {
		noteProblem(ctx.Context(), p)
	}
	return v, nil
}

// useSecurityLog installs the middleware that writes one seclog line per
// failed authentication, 403 and 429 of the API. A missing or expired session
// (401 unauthenticated) is not logged: guessing 256-bit tokens is not an
// attack fail2ban can help with, and expired cookies are routine.
func (d *deps) useSecurityLog(a huma.API) {
	a.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		n := &secNote{}
		next(huma.WithValue(ctx, secKey{}, n))
		path := ctx.URL().Path
		event := securityEvent(n, path)
		if event == "" {
			return
		}
		login := n.login
		if login == "" && n.userID > 0 && d.auth != nil {
			if u, err := d.auth.User(ctx.Context(), n.userID); err == nil {
				login = u.Email
			}
		}
		var extra []slog.Attr
		if n.userID > 0 {
			extra = append(extra, slog.Int64("user_id", n.userID))
		}
		seclog.Log(ctx.Context(), d.log, event, hostOnly(ctx.RemoteAddr()), path, login, extra...)
	})
}

func securityEvent(n *secNote, path string) string {
	switch {
	case n.status == 0:
		return ""
	case n.status == http.StatusTooManyRequests:
		return seclog.RateLimited
	case n.code == CodeCSRF:
		return seclog.CSRFRejected
	case n.code == CodeInvalidTOTPCode:
		return seclog.TOTPFailed
	case n.code == CodeInvalidCredentials:
		if strings.Contains(path, "/totp") {
			return seclog.TOTPFailed
		}
		return seclog.AuthFailed
	case n.status == http.StatusForbidden:
		return seclog.Forbidden
	}
	return ""
}

// hostOnly strips the port of a remote address.
func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}
