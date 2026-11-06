package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/silopost/silo-server/internal/store"
	"github.com/silopost/silo-server/internal/testutil/pgtest"
)

// TestRebuildCacheCommand: --account queues one rebuild; --all queues every
// account that is not disabled and reports the refused ones.
func TestRebuildCacheCommand(t *testing.T) {
	ctx := context.Background()
	url := pgtest.New(t)
	if err := store.MigrateUp(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var user int64
	if err := pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ('r@x.test') RETURNING id`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	acc := func(name, state string) (id int64) {
		if err := pool.QueryRow(ctx, `
INSERT INTO accounts (user_id, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, secret_enc, sync_state)
VALUES ($1, $2, 'h', 1, 'none', 'h', 25, 'none', $3, '\x00', $4) RETURNING id`, user, name, name, state).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, b, busy, off := acc("a@x.test", "live"), acc("b@x.test", "live"), acc("busy@x.test", "live"), acc("off@x.test", "disabled")
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (kind, account_id, run_at) VALUES ('imap_op', $1, now() + interval '1 hour')`, busy); err != nil {
		t.Fatal(err)
	}
	requested := func() string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(string_agg(account_id::text, ',' ORDER BY account_id), '') FROM account_rebuilds`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	run := func(id int64, all bool) (string, string, error) {
		var out, errOut bytes.Buffer
		err := runRebuildCache(ctx, pool, id, all, &out, &errOut)
		return out.String(), errOut.String(), err
	}

	if _, _, err := run(a, false); err != nil || requested() != itoa(a) {
		t.Fatalf("--account: %v, requested %s", err, requested())
	}
	if _, _, err := run(busy, false); err == nil || !strings.Contains(err.Error(), "operations") {
		t.Errorf("--account with pending operations: %v", err)
	}
	if _, _, err := run(9999, false); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("--account unknown: %v", err)
	}
	out, errOut, err := run(0, true)
	if err == nil || !strings.Contains(err.Error(), "1 of 3") || !strings.Contains(errOut, itoa(busy)) || strings.Count(out, "queued") != 2 {
		t.Errorf("--all: %v\nout: %s\nerr: %s", err, out, errOut)
	}
	if got, want := requested(), itoa(a)+","+itoa(b); got != want {
		t.Errorf("requested after --all = %s, want %s (not the busy %d nor the disabled %d)", got, want, busy, off)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
