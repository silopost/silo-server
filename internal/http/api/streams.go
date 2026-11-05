package api

import (
	"net/http"
	"sync"
)

// MaxStreamsPerUser is the default cap of concurrent GET /events streams of
// one user (all sessions together).
const MaxStreamsPerUser = 8

// streamSlots counts the open event streams per user. A stream over the cap is
// refused with 429 rather than closing the oldest one: an EventSource that is
// closed reconnects at once, so evicting would make two tabs take turns
// forever, while a refused EventSource stops and the web client falls back
// to GET /events/poll.
type streamSlots struct {
	mu   sync.Mutex
	max  int
	open map[int64]int
}

func newStreamSlots(max int) *streamSlots {
	if max <= 0 {
		max = MaxStreamsPerUser
	}
	return &streamSlots{max: max, open: map[int64]int{}}
}

// acquire takes a slot of userID; release must be called once when ok.
func (s *streamSlots) acquire(userID int64) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open[userID] >= s.max {
		return nil, false
	}
	s.open[userID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.open[userID]--; s.open[userID] <= 0 {
				delete(s.open, userID)
			}
		})
	}, true
}

func errTooManyStreams() *Problem {
	p := problem(http.StatusTooManyRequests, CodeRateLimited, "Too many open event streams; close a tab or use GET /events/poll.")
	p.header = http.Header{"Retry-After": {"30"}}
	return p
}
