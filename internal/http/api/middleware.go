package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

// Names fixed by the API contract (plan 3.15, 5).
const (
	// SessionCookie is the cookie that carries the session token.
	SessionCookie = "silo_session"
	// CSRFHeader must carry the session csrfToken on mutating requests.
	CSRFHeader = "X-Silo-CSRF"
)

type ctxKey struct{}

// sessionFrom returns the session authenticated by the requireSession middleware.
func sessionFrom(ctx context.Context) sqlcgen.Session {
	s, _ := ctx.Value(ctxKey{}).(sqlcgen.Session)
	return s
}

// writeProblem writes p as the response (for use inside middleware, where the
// operation handler never runs).
func writeProblem(ctx huma.Context, p *Problem) {
	for k, vs := range p.header {
		for _, v := range vs {
			ctx.AppendHeader(k, v)
		}
	}
	ctx.SetHeader("Content-Type", "application/problem+json")
	ctx.SetStatus(p.Status)
	_ = json.NewEncoder(ctx.BodyWriter()).Encode(p)
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// originAllowed rejects requests that a browser marks as cross-site, or whose
// Origin is neither SILO_PUBLIC_URL nor the host being served. Requests without
// Origin and Sec-Fetch-Site (non-browser clients) pass: they carry no ambient
// cookies an attacker could ride on.
func (d *deps) originAllowed(ctx huma.Context) bool {
	if sfs := ctx.Header("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "none" {
		return false
	}
	origin := ctx.Header("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if d.publicOrigin != "" && strings.EqualFold(u.Scheme+"://"+u.Host, d.publicOrigin) {
		return true
	}
	return strings.EqualFold(u.Host, ctx.Host())
}

// checkOrigin guards endpoints that have no session yet (POST /session).
func (d *deps) checkOrigin(ctx huma.Context, next func(huma.Context)) {
	if !d.originAllowed(ctx) {
		writeProblem(ctx, problem(http.StatusForbidden, CodeCSRF, "Cross-origin request rejected."))
		return
	}
	next(ctx)
}

// bearerToken returns the token of an "Authorization: Bearer" header and
// whether such a header is present. Other schemes (e.g. Basic added by a
// reverse proxy) are ignored, so cookie sessions keep working behind them.
func bearerToken(ctx huma.Context) (token string, ok bool) {
	scheme, tok, _ := strings.Cut(ctx.Header("Authorization"), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimSpace(tok), true
}

// requireSession authenticates the request and, for cookie sessions on mutating
// methods, enforces the CSRF header and the Origin check. A request with an
// Authorization header is authenticated by it alone (bearer session, ADR 0007):
// browsers never attach it on their own, so CSRF does not apply.
func (d *deps) requireSession(ctx huma.Context, next func(huma.Context)) {
	token, bearer := bearerToken(ctx)
	kind := auth.KindBearer
	if !bearer {
		kind = auth.KindCookie
		for _, c := range huma.ReadCookies(ctx) {
			if c.Name == SessionCookie {
				token = c.Value
				break
			}
		}
	}
	sess, err := d.auth.Authenticate(ctx.Context(), token, kind)
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		writeProblem(ctx, errUnauthenticated().(*Problem))
		return
	case err != nil:
		d.log.Error("authenticate session", "error", err)
		writeProblem(ctx, errInternal(err).(*Problem))
		return
	}
	if kind == auth.KindCookie && !isSafeMethod(ctx.Method()) {
		got := ctx.Header(CSRFHeader)
		if !d.originAllowed(ctx) || subtle.ConstantTimeCompare([]byte(got), []byte(sess.CsrfToken)) != 1 {
			writeProblem(ctx, problem(http.StatusForbidden, CodeCSRF, "Missing or invalid CSRF token."))
			return
		}
	}
	next(huma.WithValue(ctx, ctxKey{}, sess))
}

// secured marks op as requiring a session: the cookie (plus the CSRF token for
// mutations) or a bearer token.
func (d *deps) secured(op huma.Operation) huma.Operation {
	sec := map[string][]string{"cookieAuth": {}}
	if !isSafeMethod(op.Method) {
		sec["csrfHeader"] = []string{}
	}
	op.Security = []map[string][]string{sec, {"bearerAuth": {}}}
	op.Middlewares = append(op.Middlewares, d.requireSession)
	return op
}

// API ids are an opaque prefix plus the base36 database id.

func formatID(prefix string, id int64) string { return prefix + strconv.FormatInt(id, 36) }

func parseID(prefix, s string) (int64, bool) {
	rest, ok := strings.CutPrefix(s, prefix)
	if !ok || rest == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 36, 64)
	return n, err == nil && n > 0
}
