package http

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/ratelimit"
)

func TestRateLimit(t *testing.T) {
	var logs bytes.Buffer
	now := time.Unix(1000, 0)
	h := NewRouter(Deps{
		Logger:         slog.New(slog.NewTextHandler(&logs, nil)),
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.1/32")},
		RateLimits: RateLimits{
			API:  ratelimit.Limit{Burst: 3, Every: 1500 * time.Millisecond},
			Auth: ratelimit.Limit{Burst: 1, Every: time.Minute},
			Now:  func() time.Time { return now },
		},
	})
	do := func(method, path, client string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = "10.0.0.1:5555"
		r.Header.Set("X-Forwarded-For", client)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	for i := range 3 {
		if rec := do("GET", "/api/v1/meta", "1.2.3.4"); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	rec := do("GET", "/api/v1/meta", "1.2.3.4")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "2" { // 1.5 s rounded up
		t.Errorf("Retry-After = %q", ra)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var p struct {
		Code   string `json:"code"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p.Code != "rate_limited" || p.Status != 429 {
		t.Errorf("body %s: %+v %v", rec.Body, p, err)
	}
	if !strings.Contains(logs.String(), "event=rate_limited ip=1.2.3.4 path=/api/v1/meta login=-") {
		t.Errorf("no security event in log:\n%s", logs.String())
	}

	// Another client IP has its own bucket.
	if rec := do("GET", "/api/v1/meta", "5.6.7.8"); rec.Code != http.StatusOK {
		t.Errorf("other IP: %d", rec.Code)
	}
	// /events (a stream, capped per user by the API) and /healthz are not limited.
	if rec := do("GET", "/api/v1/events", "1.2.3.4"); rec.Code == http.StatusTooManyRequests {
		t.Errorf("/events limited")
	}
	if rec := do("GET", "/healthz", "1.2.3.4"); rec.Code != http.StatusOK {
		t.Errorf("/healthz: %d", rec.Code)
	}
	// The sign-in endpoints use the auth bucket: not exhausted by the API
	// requests above, exhausted after its own burst of 1.
	if rec := do("POST", "/api/v1/session", "1.2.3.4"); rec.Code == http.StatusTooManyRequests {
		t.Errorf("first sign-in limited")
	}
	for _, path := range []string{"/api/v1/session", "/api/v1/session/password", "/api/v1/totp/enable"} {
		if rec := do("POST", path, "1.2.3.4"); rec.Code != http.StatusTooManyRequests {
			t.Errorf("POST %s after the auth burst: %d", path, rec.Code)
		}
	}
	// The bucket refills.
	now = now.Add(1500 * time.Millisecond)
	if rec := do("GET", "/api/v1/meta", "1.2.3.4"); rec.Code != http.StatusOK {
		t.Errorf("after refill: %d", rec.Code)
	}
}
