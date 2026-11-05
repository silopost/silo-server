package events

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/store"
	"github.com/silopost/silo-server/internal/testutil/pgtest"
)

func newHub(t *testing.T, buffer int) (*Hub, *pgxpool.Pool, int64) {
	t.Helper()
	ctx := context.Background()
	url := pgtest.New(t)
	if err := store.MigrateUp(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var uid int64
	if err := pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ('hub@silo.test') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	h := New(Options{Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Buffer: buffer, PollInterval: time.Hour})
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Run(rctx) }()
	t.Cleanup(func() { cancel(); <-done })
	time.Sleep(300 * time.Millisecond) // LISTEN established
	return h, pool, uid
}

func recv(t *testing.T, c *Client, d time.Duration) Event {
	t.Helper()
	select {
	case e := <-c.C:
		return e
	case <-time.After(d):
		t.Fatal("no event")
	}
	return Event{}
}

func TestDeliveryAfterCommitWithMappedIDs(t *testing.T) {
	h, pool, uid := newHub(t, 8)
	ctx := context.Background()
	var inbox int64
	var acc int64
	if err := pool.QueryRow(ctx, `INSERT INTO accounts (user_id, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, secret_enc)
VALUES ($1, 'a@silo.test', 'h', 1, 'none', 'h', 1, 'none', 'u', '\x00') RETURNING id`, uid).Scan(&acc); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO labels (user_id, account_id, name, type, system_key) VALUES ($1, $2, 'Inbox', 'system', 'INBOX') RETURNING id`, uid, acc).Scan(&inbox); err != nil {
		t.Fatal(err)
	}
	c := h.Subscribe(uid)
	defer c.Close()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"threadId":35,"accountId":` + itoa(acc) + `,"labelIds":[` + itoa(inbox) + `,999],"unreadCount":2,"origin":"tab-1"}`
	if _, err := tx.Exec(ctx, `INSERT INTO events (user_id, type, payload) VALUES ($1, 'thread.updated', $2::jsonb)`, uid, payload); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-c.C:
		t.Fatalf("delivered before commit: %+v", e)
	case <-time.After(400 * time.Millisecond):
	}
	t0 := time.Now()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	e := recv(t, c, 2*time.Second)
	t.Logf("delivered %v after commit", time.Since(t0))
	var m map[string]any
	if err := json.Unmarshal(e.Data, &m); err != nil {
		t.Fatal(err)
	}
	labels, _ := m["labelIds"].([]any)
	if e.Type != "thread.updated" || m["threadId"] != "t_z" || m["accountId"] != "acc_"+strings.ToLower(itoa36(acc)) || m["origin"] != "tab-1" ||
		len(labels) != 3 || labels[0] != "INBOX" || labels[1] != "l_"+itoa36(999) || labels[2] != "UNREAD" {
		t.Errorf("event: %s %s", e.Type, e.Data)
	}
}

func TestLateCommitIsStillDelivered(t *testing.T) {
	h, pool, uid := newHub(t, 8)
	ctx := context.Background()
	c := h.Subscribe(uid)
	defer c.Close()
	slow, _ := pool.Begin(ctx)
	if _, err := slow.Exec(ctx, `INSERT INTO events (user_id, type, payload) VALUES ($1, 'thread.deleted', '{"threadId":1}')`, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO events (user_id, type, payload) VALUES ($1, 'thread.deleted', '{"threadId":2}')`, uid); err != nil {
		t.Fatal(err)
	}
	second := recv(t, c, 2*time.Second)
	if err := slow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	first := recv(t, c, 2*time.Second)
	if !strings.Contains(string(second.Data), "t_2") || !strings.Contains(string(first.Data), "t_1") || first.Seq >= second.Seq {
		t.Errorf("first %d %s, second %d %s", first.Seq, first.Data, second.Seq, second.Data)
	}
}

func TestSlowClientIsDropped(t *testing.T) {
	h, pool, uid := newHub(t, 2)
	ctx := context.Background()
	c := h.Subscribe(uid)
	other := h.Subscribe(uid + 1000)
	defer other.Close()
	if _, err := pool.Exec(ctx, `INSERT INTO events (user_id, type, payload) SELECT $1, 'thread.deleted', '{}' FROM generate_series(1, 5)`, uid); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("slow client not closed")
	}
	if h.Clients() != 1 {
		t.Errorf("clients left: %d", h.Clients())
	}
	// It reconnects and replays from its last event.
	evs, resync, _, err := h.Since(ctx, uid, 0, 100)
	if err != nil || resync || len(evs) != 5 {
		t.Errorf("replay: %d events, resync %v, %v", len(evs), resync, err)
	}
}

func TestSinceAndPrune(t *testing.T) {
	h, pool, uid := newHub(t, 8)
	ctx := context.Background()
	for range 3 {
		if _, err := pool.Exec(ctx, `INSERT INTO events (user_id, type, payload) VALUES ($1, 'resync', '{}')`, uid); err != nil {
			t.Fatal(err)
		}
	}
	evs, resync, _, _ := h.Since(ctx, uid, 0, 100)
	if len(evs) != 3 || resync {
		t.Fatalf("since 0: %d %v", len(evs), resync)
	}
	evs, resync, _, _ = h.Since(ctx, uid, evs[0].Seq, 100)
	if len(evs) != 2 || resync {
		t.Fatalf("since first: %d %v", len(evs), resync)
	}
	// Events older than the retention are pruned; asking after one means resync.
	if _, err := pool.Exec(ctx, `UPDATE events SET created_at = now() - interval '25 hours' WHERE seq = (SELECT min(seq) FROM events)`); err != nil {
		t.Fatal(err)
	}
	var oldest int64
	_ = pool.QueryRow(ctx, `SELECT min(seq) FROM events`).Scan(&oldest)
	h.pruneNow(ctx)
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&n)
	if n != 2 {
		t.Errorf("events after prune: %d", n)
	}
	evs, resync, head, _ := h.Since(ctx, uid, oldest, 100)
	if !resync || len(evs) != 0 || head == 0 {
		t.Errorf("after pruned id: resync %v events %d head %d", resync, len(evs), head)
	}
}

func itoa(n int64) string   { return strconv.FormatInt(n, 10) }
func itoa36(n int64) string { return strconv.FormatInt(n, 36) }
