// Package seclog writes the security events fail2ban-style tools match on: one
// WARN line per failed authentication, rejected request (403) or throttled
// request (429), always with the fields event, ip, path and login, in this
// order, so that the text format contains literally "event=auth_failed
// ip=1.2.3.4" (see the README for failregex examples). Passwords and tokens
// are never logged; the login is masked.
package seclog

import (
	"context"
	"log/slog"
	"strings"
)

// Event names (the value of the event field).
const (
	AuthFailed   = "auth_failed"   // wrong credentials at sign-in or re-authentication
	TOTPFailed   = "totp_failed"   // wrong TOTP/backup code on a /totp endpoint
	RateLimited  = "rate_limited"  // 429
	CSRFRejected = "csrf_rejected" // 403 csrf_failed (Origin or CSRF token)
	Forbidden    = "forbidden"     // any other 403
)

// Message is the msg of every security event line.
const Message = "security event"

// Log writes one security event. login may be empty (unknown); it is masked
// here. extra attributes follow the fixed ones.
func Log(ctx context.Context, logger *slog.Logger, event, ip, path, login string, extra ...slog.Attr) {
	if logger == nil {
		logger = slog.Default()
	}
	attrs := append([]slog.Attr{
		slog.String("event", event),
		slog.String("ip", ip),
		slog.String("path", path),
		slog.String("login", MaskLogin(login)),
	}, extra...)
	logger.LogAttrs(ctx, slog.LevelWarn, Message, attrs...)
}

// MaskLogin keeps the first character of the local part and the domain:
// "jan@example.com" -> "j***@example.com". An empty login is "-".
func MaskLogin(login string) string {
	login = strings.TrimSpace(login)
	if login == "" {
		return "-"
	}
	local, domain, hasAt := strings.Cut(login, "@")
	first := ""
	for _, r := range local {
		first = string(r)
		break
	}
	if strings.ContainsAny(domain, " \t\r\n\"=") {
		domain = "?"
	}
	if !hasAt {
		return first + "***"
	}
	return first + "***@" + domain
}
