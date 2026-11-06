package http

import (
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/ratelimit"
	"github.com/silopost/silo-server/internal/seclog"
)

// RateLimits are the per-client-IP limits of the API (plan 3.15).
type RateLimits struct {
	// API applies to /api/v1/* except /api/v1/events (a long-lived stream,
	// capped per user by the API instead) and the Auth endpoints
	// (SILO_RATE_LIMIT_API).
	API ratelimit.Limit
	// Auth applies to POST /session, POST /session/password and /totp/*
	// (SILO_RATE_LIMIT_AUTH). The per-login limit stays in internal/auth.
	Auth ratelimit.Limit
	// Now is the clock (time.Now when nil).
	Now func() time.Time
}

// rateLimit answers 429 rate_limited with Retry-After once the client IP
// (after RealIP) has used up its bucket.
func rateLimit(rl RateLimits, logger *slog.Logger) func(http.Handler) http.Handler {
	apiLim := ratelimit.New(rl.API, rl.Now)
	authLim := ratelimit.New(rl.Auth, rl.Now)
	return func(next http.Handler) http.Handler {
		if !rl.API.Enabled() && !rl.Auth.Enabled() {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			lim := limiterFor(r, apiLim, authLim)
			if lim == nil {
				next.ServeHTTP(w, r)
				return
			}
			ip := remoteIP(r)
			if ok, wait := lim.Allow(ip); !ok {
				seclog.Log(r.Context(), logger, seclog.RateLimited, ip, r.URL.Path, "")
				writeRateLimited(w, wait)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// limiterFor picks the bucket set of a request; nil when it is not limited.
func limiterFor(r *http.Request, apiLim, authLim *ratelimit.Limiter) *ratelimit.Limiter {
	p := r.URL.Path
	rest, ok := strings.CutPrefix(p, api.Prefix+"/")
	if !ok || rest == "events" {
		return nil
	}
	if (r.Method == http.MethodPost && (rest == "session" || rest == "session/password")) ||
		rest == "totp" || strings.HasPrefix(rest, "totp/") || strings.HasPrefix(rest, "oauth/") {
		return authLim
	}
	return apiLim
}

// writeRateLimited writes the problem the API uses for 429 (plan 5.3).
func writeRateLimited(w http.ResponseWriter, wait time.Duration) {
	secs := max(1, int(math.Ceil(wait.Seconds())))
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "https://silo.dev/errors/rate-limited",
		"title":  http.StatusText(http.StatusTooManyRequests),
		"status": http.StatusTooManyRequests,
		"detail": "Too many requests, try again later.",
		"code":   api.CodeRateLimited,
	})
}
