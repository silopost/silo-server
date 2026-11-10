package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/events"
)

type eventsInput struct {
	LastEventID  string `header:"Last-Event-ID" doc:"Set by EventSource on reconnect: replays the events after it."`
	LastEventIDQ string `query:"lastEventId" doc:"Same as the Last-Event-ID header, for clients that cannot set headers."`
}

type pollInput struct {
	After int64 `query:"after" minimum:"0" doc:"Sequence number of the last event received; 0 starts from the oldest retained event."`
	Limit int   `query:"limit" minimum:"1" maximum:"500" default:"200"`
}

// PolledEvent is one event of GET /events/poll.
type PolledEvent struct {
	ID   int64          `json:"id"`
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// PollResult is the answer of GET /events/poll.
type PollResult struct {
	Events    []PolledEvent `json:"events" doc:"After a resync event, discard cached data and refetch."`
	NextAfter int64         `json:"nextAfter" doc:"Pass as after in the next poll."`
}

type pollOutput struct{ Body PollResult }

var eventStreamDoc = map[string]*huma.Response{"200": {
	Description: "A Server-Sent Events stream. Each event has id (sequence number), event (type) and data (JSON); " +
		"types: message.new, thread.updated (draftId and draftCount as in the thread list), thread.deleted, thread.merged, label.counts, label.changed, operation.failed, " +
		"filter.applied ({filterId, accountId, messages, threads}: POST /filters/{id}/apply is done), settings.updated " +
		"({updatedAt}: reload GET /settings), account.status, account.sync_progress, account.error, resync, session.expired " +
		"(the session was signed out, revoked, expired or its user disabled: the stream ends; sign in again), stream.limit " +
		"({retryAfter}: too many open streams of the user, the stream ends; fall back to /events/poll). Events caused by a request that carried " +
		"X-Silo-Client have origin set to that id.",
	Content: map[string]*huma.MediaType{"text/event-stream": {Schema: &huma.Schema{Type: huma.TypeString}}},
}}

func (d *deps) registerEvents(a huma.API) {
	huma.Register(a, d.secured(huma.Operation{
		OperationID: "streamEvents", Method: http.MethodGet, Path: "/events", Tags: []string{"events"},
		Summary: "Server-Sent Events stream",
		Description: "Pushes changes of the mailbox as they are committed. After a reconnect with Last-Event-ID the " +
			"missed events (24 h retained) are replayed first; an id older than the retention yields a resync event. " +
			"A ': ping' comment is sent every 25 s; with it the session is checked, and a stream whose session ended gets session.expired " +
			"and is closed. A client that cannot keep up is disconnected and replays on reconnect. At most 8 streams per user are open at " +
			"once; a stream over the limit answers 200 with a single stream.limit event ({retryAfter: 30}, seconds; a retry: 30000 " +
			"field slows down an EventSource that is not closed) and ends: close the EventSource and use /events/poll.",
		Responses: eventStreamDoc, Errors: []int{http.StatusUnauthorized},
	}), func(ctx context.Context, in *eventsInput) (*huma.StreamResponse, error) {
		sess := sessionFrom(ctx)
		userID := sess.UserID
		last := in.LastEventID
		if last == "" {
			last = in.LastEventIDQ
		}
		var after int64
		if last != "" {
			n, err := strconv.ParseInt(last, 10, 64)
			if err != nil || n < 0 {
				return nil, problem(http.StatusBadRequest, CodeValidation, "invalid Last-Event-ID")
			}
			after = n
		}
		if d.events == nil {
			return nil, errInternal(nil)
		}
		// Subscribe first, replay second: an event is then either replayed or
		// delivered live (duplicates are filtered below).
		cl := d.events.Subscribe(userID)
		var replay []events.Event
		resync := false
		var head int64
		if last != "" {
			var err error
			replay, resync, head, err = d.events.Since(ctx, userID, after, events.MaxReplay)
			if err != nil {
				cl.Close()
				return nil, errInternal(err)
			}
		}
		return &huma.StreamResponse{Body: func(c huma.Context) {
			defer cl.Close()
			release, ok := d.streams.acquire(userID)
			if !ok {
				writeStreamLimit(c)
				return
			}
			defer release()
			c.SetHeader("Content-Type", "text/event-stream")
			c.SetHeader("Cache-Control", "no-cache")
			c.SetHeader("X-Accel-Buffering", "no")
			c.SetStatus(http.StatusOK)
			w := bufio.NewWriter(c.BodyWriter())
			flush := func() bool {
				if w.Flush() != nil {
					return false
				}
				if f, ok := c.BodyWriter().(http.Flusher); ok {
					f.Flush()
				}
				return true
			}
			seen := map[int64]struct{}{}
			// Write errors of a bufio.Writer are sticky: flush() reports them.
			send := func(e events.Event) bool {
				if e.Seq > 0 {
					_, _ = w.WriteString("id: " + strconv.FormatInt(e.Seq, 10) + "\n")
				}
				_, _ = w.WriteString("event: " + e.Type + "\ndata: ")
				_, _ = w.Write(e.Data)
				_, _ = w.WriteString("\n\n")
				return true
			}
			_, _ = w.WriteString(": connected\n\n")
			if resync {
				send(events.Event{Seq: head, Type: "resync", Data: []byte("{}")})
			}
			for _, e := range replay {
				seen[e.Seq] = struct{}{}
				send(e)
			}
			if !flush() {
				return
			}
			tick := time.NewTicker(d.heartbeat)
			defer tick.Stop()
			for {
				select {
				case <-c.Context().Done():
					return
				case <-cl.Done():
					return
				case e := <-cl.C:
					if _, dup := seen[e.Seq]; dup {
						delete(seen, e.Seq)
						continue
					}
					send(e)
					// Drain what is queued before flushing once.
					for more := true; more; {
						select {
						case e := <-cl.C:
							if _, dup := seen[e.Seq]; dup {
								delete(seen, e.Seq)
								continue
							}
							send(e)
						default:
							more = false
						}
					}
					if !flush() {
						return
					}
				case <-tick.C:
					if d.auth != nil && !d.auth.SessionAlive(c.Context(), sess.ID) {
						send(events.Event{Type: "session.expired", Data: []byte("{}")})
						flush()
						return
					}
					_, _ = w.WriteString(": ping\n\n")
					if !flush() {
						return
					}
				}
			}
		}}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "pollEvents", Method: http.MethodGet, Path: "/events/poll", Tags: []string{"events"},
		Summary: "Events after a sequence number (fallback for /events)",
		Description: "The same events as the stream, for clients whose EventSource keeps failing. An after older than " +
			"the 24 h retention answers a single resync event.",
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized},
	}), func(ctx context.Context, in *pollInput) (*pollOutput, error) {
		if d.events == nil {
			return nil, errInternal(nil)
		}
		evs, resync, head, err := d.events.Since(ctx, sessionFrom(ctx).UserID, in.After, in.Limit)
		if err != nil {
			return nil, errInternal(err)
		}
		out := &pollOutput{}
		out.Body.Events = []PolledEvent{}
		out.Body.NextAfter = in.After
		if resync {
			out.Body.Events = append(out.Body.Events, PolledEvent{ID: head, Type: "resync", Data: map[string]any{}})
			out.Body.NextAfter = head
			return out, nil
		}
		for _, e := range evs {
			var data map[string]any
			_ = json.Unmarshal(e.Data, &data)
			out.Body.Events = append(out.Body.Events, PolledEvent{ID: e.Seq, Type: e.Type, Data: data})
			out.Body.NextAfter = max(out.Body.NextAfter, e.Seq)
		}
		return out, nil
	})
}
