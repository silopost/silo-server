package http

import (
	"net/http"
	"net/url"
)

// HeaderOptions configure SecurityHeaders.
type HeaderOptions struct {
	// PublicURL is SILO_PUBLIC_URL; HSTS is sent only when it is https.
	PublicURL string
	// HSTS is SILO_HSTS (default true): Strict-Transport-Security on an https
	// instance. Turn it off when the proxy in front already sends it.
	HSTS bool
}

// apiCSP is the policy of everything the server answers by default (JSON,
// problems, healthz): nothing may load, nothing may frame it. Message HTML and
// attachments set their own policy in internal/http/api.
const apiCSP = "default-src 'none'; frame-ancestors 'none'"

// SecurityHeaders sets the response headers of plan 3.15 on every response.
// They are set before the handler runs, so handlers with other needs replace
// them: message HTML, attachments and raw messages keep their immutable
// Cache-Control and their own Content-Security-Policy, the event stream its
// no-cache.
func SecurityHeaders(o HeaderOptions) func(http.Handler) http.Handler {
	hsts := false
	if u, err := url.Parse(o.PublicURL); err == nil && u.Scheme == "https" && u.Host != "" {
		hsts = o.HSTS
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("Content-Security-Policy", apiCSP)
			// Every answer of the server is per user or live state; nothing
			// may be stored by shared or browser caches unless the handler
			// says otherwise.
			h.Set("Cache-Control", "no-store")
			if hsts {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}
