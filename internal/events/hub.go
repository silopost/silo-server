// Package events publishes the rows of the events table to SSE clients
// (plan 3.14, 5.4).
//
// Delivery: every statement that inserts events fires NOTIFY silo_events from
// a trigger, which Postgres delivers only when the transaction commits. The
// Hub listens, reads the new rows and pushes them to the connected clients of
// each user; a slow poll covers a lost notification. Sequence numbers are
// assigned at insert and rows become visible at commit, so a row with a lower
// seq can appear after a higher one; the Hub remembers the gaps it saw for a
// while and rereads them, so such late rows are still delivered (possibly out
// of order; clients treat the ids as opaque).
//
// The payloads in the table carry database ids; Hub translates them to the
// ids of the API (t_, m_, acc_, l_ or a system label key) in one place.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Channel is the NOTIFY channel fired by the trigger on events inserts.
const Channel = "silo_events"

// Defaults.
const (
	DefaultRetention = 24 * time.Hour
	DefaultBuffer    = 64
	DefaultPoll      = 5 * time.Second
	gapTTL           = time.Minute
	maxGap           = 10000
	pruneEvery       = time.Hour
	// MaxReplay bounds the events one replay or poll returns.
	MaxReplay = 500
)

// Event is one event as the client receives it.
type Event struct {
	// Seq is the SSE id; 0 for events that have none (a resync with no history).
	Seq  int64
	Type string
	// Data is the JSON payload with API ids.
	Data json.RawMessage
}

// Options configure a Hub; zero values pick the defaults.
type Options struct {
	Pool         *pgxpool.Pool
	Logger       *slog.Logger
	Retention    time.Duration
	Buffer       int
	PollInterval time.Duration
}

// Hub fans events out to the connected clients of each user.
type Hub struct {
	opts Options
	log  *slog.Logger

	mu      sync.Mutex
	clients map[int64]map[*Client]struct{}

	// state of the dispatcher (only the Run goroutine)
	last int64
	gaps map[int64]time.Time

	labels sync.Map // label id -> API id
}

// New returns a Hub.
func New(o Options) *Hub {
	if o.Retention <= 0 {
		o.Retention = DefaultRetention
	}
	if o.Buffer <= 0 {
		o.Buffer = DefaultBuffer
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPoll
	}
	h := &Hub{opts: o, clients: map[int64]map[*Client]struct{}{}, gaps: map[int64]time.Time{}}
	h.log = o.Logger
	if h.log == nil {
		h.log = slog.Default()
	}
	return h
}

// Client is one SSE connection.
type Client struct {
	UserID int64
	// C delivers events; it is never closed, watch Done.
	C    chan Event
	done chan struct{}
	hub  *Hub
	once sync.Once
}

// Done is closed when the client is dropped: the buffer overflowed (the
// connection must be closed; the browser reconnects and replays) or Close was
// called.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close unsubscribes the client.
func (c *Client) Close() {
	c.hub.drop(c)
}

func (h *Hub) drop(c *Client) {
	h.mu.Lock()
	if m := h.clients[c.UserID]; m != nil {
		delete(m, c)
		if len(m) == 0 {
			delete(h.clients, c.UserID)
		}
	}
	h.mu.Unlock()
	c.once.Do(func() { close(c.done) })
}

// Subscribe registers a client of a user. Subscribe before replaying history
// so that nothing falls between the replay and the live stream.
func (h *Hub) Subscribe(userID int64) *Client {
	c := &Client{UserID: userID, C: make(chan Event, h.opts.Buffer), done: make(chan struct{}), hub: h}
	h.mu.Lock()
	if h.clients[userID] == nil {
		h.clients[userID] = map[*Client]struct{}{}
	}
	h.clients[userID][c] = struct{}{}
	h.mu.Unlock()
	return c
}

// Clients returns the number of connected clients (tests, metrics).
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.clients {
		n += len(m)
	}
	return n
}

func (h *Hub) hasClients(userID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients[userID]) > 0
}

// publish gives e to every client of the user; a client whose buffer is full
// is dropped.
func (h *Hub) publish(userID int64, e Event) {
	h.mu.Lock()
	var slow []*Client
	for c := range h.clients[userID] {
		select {
		case c.C <- e:
		default:
			slow = append(slow, c)
		}
	}
	h.mu.Unlock()
	for _, c := range slow {
		h.log.Warn("sse client too slow; closing", "user", userID)
		h.drop(c)
	}
}

// Run delivers events until ctx is cancelled and prunes the table. It
// returns nil on cancellation.
func (h *Hub) Run(ctx context.Context) error {
	if err := h.opts.Pool.QueryRow(ctx, `SELECT COALESCE(max(seq), 0) FROM events`).Scan(&h.last); err != nil {
		return fmt.Errorf("events: initial position: %w", err)
	}
	wake := make(chan struct{}, 1)
	go h.listen(ctx, wake)
	poll := time.NewTicker(h.opts.PollInterval)
	defer poll.Stop()
	prune := time.NewTicker(pruneEvery)
	defer prune.Stop()
	h.pruneNow(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
		case <-poll.C:
		case <-prune.C:
			h.pruneNow(ctx)
			continue
		}
		if err := h.dispatch(ctx); err != nil && ctx.Err() == nil {
			h.log.Warn("events: dispatch", "err", err)
		}
	}
}

func (h *Hub) pruneNow(ctx context.Context) {
	tag, err := h.opts.Pool.Exec(ctx, `DELETE FROM events WHERE created_at < now() - make_interval(secs => $1)`, h.opts.Retention.Seconds())
	if err != nil {
		if ctx.Err() == nil {
			h.log.Warn("events: prune", "err", err)
		}
		return
	}
	if n := tag.RowsAffected(); n > 0 {
		h.log.Debug("events pruned", "rows", n)
	}
}

// listen keeps a dedicated connection with LISTEN silo_events.
func (h *Hub) listen(ctx context.Context, wake chan<- struct{}) {
	for ctx.Err() == nil {
		if err := h.listenOnce(ctx, wake); err != nil && ctx.Err() == nil {
			h.log.Warn("events: listener lost, retrying", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (h *Hub) listenOnce(ctx context.Context, wake chan<- struct{}) error {
	conn, err := pgx.ConnectConfig(ctx, h.opts.Pool.Config().ConnConfig)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return err
	}
	// A notification may have been missed while not listening.
	select {
	case wake <- struct{}{}:
	default:
	}
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// dispatch reads the rows after the last seen seq (and the gaps) and
// publishes those of users with connected clients.
func (h *Hub) dispatch(ctx context.Context) error {
	now := time.Now()
	gapIDs := make([]int64, 0, len(h.gaps))
	for s, until := range h.gaps {
		if now.After(until) {
			delete(h.gaps, s)
			continue
		}
		gapIDs = append(gapIDs, s)
	}
	rows, err := h.opts.Pool.Query(ctx, `
SELECT seq, user_id, type, payload FROM events WHERE seq > $1 OR seq = ANY($2) ORDER BY seq LIMIT 5000`, h.last, gapIDs)
	if err != nil {
		return err
	}
	type row struct {
		seq, user int64
		typ       string
		payload   []byte
	}
	var batch []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.seq, &r.user, &r.typ, &r.payload); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range batch {
		if r.seq > h.last {
			if r.seq-h.last <= maxGap {
				for s := h.last + 1; s < r.seq; s++ {
					h.gaps[s] = now.Add(gapTTL)
				}
			}
			h.last = r.seq
		} else {
			delete(h.gaps, r.seq)
		}
		if !h.hasClients(r.user) {
			continue
		}
		data, err := h.mapPayload(ctx, r.typ, r.payload)
		if err != nil {
			h.log.Warn("events: map payload", "seq", r.seq, "type", r.typ, "err", err)
			continue
		}
		h.publish(r.user, Event{Seq: r.seq, Type: r.typ, Data: data})
	}
	if len(batch) == 5000 { // more waiting
		return h.dispatch(ctx)
	}
	return nil
}

// Since returns the events of a user after seq `after` (at most limit, in
// order), mapped to API ids. resync is true when `after` is not a known event
// of the user any more (older than the retention, or from another database):
// the client must refetch everything; the returned events are then empty and
// head is the user's newest seq to continue from.
func (h *Hub) Since(ctx context.Context, userID, after int64, limit int) (evs []Event, resync bool, head int64, err error) {
	if limit <= 0 || limit > MaxReplay {
		limit = MaxReplay
	}
	if after > 0 {
		var ok bool
		if err := h.opts.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE seq = $1 AND user_id = $2)`, after, userID).Scan(&ok); err != nil {
			return nil, false, 0, err
		}
		if !ok {
			err := h.opts.Pool.QueryRow(ctx, `SELECT COALESCE(max(seq), 0) FROM events WHERE user_id = $1`, userID).Scan(&head)
			return []Event{}, true, head, err
		}
	}
	rows, err := h.opts.Pool.Query(ctx, `SELECT seq, type, payload FROM events WHERE user_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`, userID, after, limit)
	if err != nil {
		return nil, false, 0, err
	}
	defer rows.Close()
	evs = []Event{}
	for rows.Next() {
		var e Event
		var raw []byte
		if err := rows.Scan(&e.Seq, &e.Type, &raw); err != nil {
			return nil, false, 0, err
		}
		if e.Data, err = h.mapPayload(ctx, e.Type, raw); err != nil {
			h.log.Warn("events: map payload", "seq", e.Seq, "err", err)
			continue
		}
		evs = append(evs, e)
	}
	return evs, false, after, rows.Err()
}
