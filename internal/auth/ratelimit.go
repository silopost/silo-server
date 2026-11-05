package auth

import (
	"math"
	"sync"
	"time"
)

// Limit describes a token bucket: it holds up to Burst tokens and refills one
// token every Every.
type Limit struct {
	Burst int
	Every time.Duration
}

type bucket struct {
	tokens float64
	last   time.Time
}

// limiter is an in-memory token bucket per key. State is per process (plan:
// multiple instances are out of scope until later phases).
type limiter struct {
	mu        sync.Mutex
	limit     Limit
	now       func() time.Time
	buckets   map[string]*bucket
	lastSweep time.Time
}

func newLimiter(l Limit, now func() time.Time) *limiter {
	return &limiter{limit: l, now: now, buckets: map[string]*bucket{}, lastSweep: now()}
}

// allow takes one token for key. When none is left it returns false and how
// long until one is available.
func (l *limiter) allow(key string) (bool, time.Duration) {
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

// sweep drops buckets that have refilled completely (they carry no state).
func (l *limiter) sweep(now time.Time) {
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
