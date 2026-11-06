package api

import (
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/danielgtaylor/huma/v2"
)

// MaxStreamsPerUser is the default cap of concurrent GET /events streams of
// one user (all sessions together).
const MaxStreamsPerUser = 8

// streamSlots counts the open event streams per user. A stream over the cap is
// refused with stream.limit rather than closing the oldest one: an
// EventSource whose stream is closed reconnects at once, so evicting would
// make two tabs take turns forever, while the refused client closes its
// EventSource and falls back to GET /events/poll.
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

// StreamLimitRetryAfter is the retryAfter (seconds) of stream.limit.
const StreamLimitRetryAfter = 30

// writeStreamLimit answers a stream over the cap. A 429 would be invisible to
// an EventSource (it only reports a generic error), so the answer is a
// normal event stream with one stream.limit event that the client can act
// on, after which the stream ends. retry: makes an EventSource that is not
// closed reconnect after 30 s instead of 3 s. The request is still noted as
// 429 rate_limited for the security log.
func writeStreamLimit(c huma.Context) {
	noteProblem(c.Context(), problem(http.StatusTooManyRequests, CodeRateLimited, "Too many open event streams."))
	c.SetHeader("Content-Type", "text/event-stream")
	c.SetHeader("Cache-Control", "no-cache")
	c.SetHeader("X-Accel-Buffering", "no")
	c.SetStatus(http.StatusOK)
	retry := strconv.Itoa(StreamLimitRetryAfter)
	_, _ = io.WriteString(c.BodyWriter(), "retry: "+retry+"000\nevent: stream.limit\ndata: {\"retryAfter\":"+retry+"}\n\n")
	if f, ok := c.BodyWriter().(http.Flusher); ok {
		f.Flush()
	}
}
