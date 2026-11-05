// Package ratelimit is an in-memory token bucket per key, shared by the
// sign-in throttling (internal/auth) and the per-IP API limits (internal/http).
// State is per process: several instances behind a load balancer each keep
// their own buckets.
package ratelimit

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Limit describes a token bucket: it holds up to Burst tokens and refills one
// token every Every. The zero Limit is "no limit".
type Limit struct {
	Burst int
	Every time.Duration
}

// Enabled reports whether l limits anything.
func (l Limit) Enabled() bool { return l.Burst > 0 && l.Every > 0 }

// String formats l as Parse accepts it (N per Burst*Every).
func (l Limit) String() string {
	if !l.Enabled() {
		return "off"
	}
	return strconv.Itoa(l.Burst) + "/" + (time.Duration(l.Burst) * l.Every).String()
}

// Parse reads "N/s", "N/m", "N/h" or "N/<Go duration>" (e.g. "20/1m30s"):
// N requests per period, all of which may come at once (burst N), refilled
// evenly over the period. "off" (or "0") disables the limit.
func Parse(s string) (Limit, error) {
	s = strings.TrimSpace(s)
	if s == "off" || s == "0" {
		return Limit{}, nil
	}
	ns, per, ok := strings.Cut(s, "/")
	n, err := strconv.Atoi(strings.TrimSpace(ns))
	if !ok || err != nil || n < 1 {
		return Limit{}, fmt.Errorf("rate %q: want N/s, N/m, N/h or N/<duration> with N >= 1, or off", s)
	}
	var d time.Duration
	switch per = strings.TrimSpace(per); per {
	case "s":
		d = time.Second
	case "m":
		d = time.Minute
	case "h":
		d = time.Hour
	default:
		if d, err = time.ParseDuration(per); err != nil || d <= 0 {
			return Limit{}, fmt.Errorf("rate %q: invalid period %q", s, per)
		}
	}
	every := d / time.Duration(n)
	if every <= 0 {
		return Limit{}, fmt.Errorf("rate %q: too many requests per period", s)
	}
	return Limit{Burst: n, Every: every}, nil
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter keeps one bucket per key.
type Limiter struct {
	mu        sync.Mutex
	limit     Limit
	now       func() time.Time
	buckets   map[string]*bucket
	lastSweep time.Time
}

// New returns a Limiter; now is the clock (time.Now when nil).
func New(l Limit, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{limit: l, now: now, buckets: map[string]*bucket{}, lastSweep: now()}
}

// Allow takes one token for key. When none is left it returns false and how
// long until one is available. A disabled Limit allows everything.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if !l.limit.Enabled() {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)

	burst := float64(l.limit.Burst)
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(burst, b.tokens+float64(now.Sub(b.last))/float64(l.limit.Every))
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) * float64(l.limit.Every))
	return false, wait
}

// Len is the number of buckets held (tests).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// sweep drops buckets that have refilled completely (they carry no state), at
// most once a minute.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	full := time.Duration(l.limit.Burst) * l.limit.Every
	for k, b := range l.buckets {
		if now.Sub(b.last) >= full {
			delete(l.buckets, k)
		}
	}
}
