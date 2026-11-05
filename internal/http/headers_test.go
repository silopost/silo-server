package http

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/silopost/silo-server/internal/http/api"
)

func TestSecurityHeaders(t *testing.T) {
	common := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Permissions-Policy":           "camera=(), microphone=(), geolocation=()",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'",
		"Cache-Control":                "no-store",
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		name      string
		publicURL string
		hsts      bool
		wantHSTS  bool
	}{
		{"https", "https://mail.example.com", true, true},
		{"https, SILO_HSTS=false", "https://mail.example.com", false, false},
		{"http", "http://localhost:8080", true, false},
	} {
		h := NewRouter(Deps{Logger: logger, HSTS: tc.hsts, API: api.Options{PublicURL: tc.publicURL}})
		// /api/v1/threads without a session is a 401 problem; it still carries
		// the headers.
		for _, path := range []string{"/healthz", "/api/v1/meta", "/api/v1/threads"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			for k, want := range common {
				if got := rec.Header().Get(k); got != want {
					t.Errorf("%s %s: %s = %q, want %q", tc.name, path, k, got, want)
				}
			}
			got := rec.Header().Get("Strict-Transport-Security")
			if tc.wantHSTS && got != "max-age=31536000; includeSubDomains" || !tc.wantHSTS && got != "" {
				t.Errorf("%s %s: HSTS = %q", tc.name, path, got)
			}
		}
	}

	// Handlers replace the defaults they need to (message HTML, attachments).
	h := SecurityHeaders(HeaderOptions{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/attachments/a_1", nil))
	if rec.Header().Get("Cache-Control") != "private, max-age=31536000, immutable" || rec.Header().Get("Content-Security-Policy") != "default-src 'none'; sandbox" ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("handler headers: %v", rec.Header())
	}
}
