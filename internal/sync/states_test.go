package sync_test

import (
	"context"
	"net"
	"runtime"
	"strconv"
	"testing"
	"time"

	silosync "github.com/silopost/silo-server/internal/sync"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
	"github.com/silopost/silo-server/internal/worker"
)

// TestAuthFailed: a wrong password ends in auth_failed with no retries; a
// new password and a reconnect request bring the account to live.
func TestAuthFailed(t *testing.T) {
	mb := newMailbox(t, imaptest.Full)
	px := newProxy(t, net.JoinHostPort(mb.host, strconv.Itoa(mb.port)))
	a := newAccount(t, "127.0.0.1", px.port(), mb.user, "wrong-password")
	r := a.start()
	a.waitState("auth_failed", 30*time.Second)
	n := px.accepts.Load()
	time.Sleep(1500 * time.Millisecond) // 15 backoff periods of the test schedule
	if got := px.accepts.Load(); got != n || a.state() != "auth_failed" {
		t.Fatalf("worker retried after auth_failed: %d -> %d connections, state %s", n, got, a.state())
	}
	if a.int(`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'account.error' AND payload->>'code' = 'auth_failed'`, a.userID) == 0 {
		t.Error("no account.error auth_failed")
	}
	a.setPassword(mb.pass)
	r.w.Reconnect()
	a.waitState("live", 30*time.Second)
}

// TestBackoffAndRecovery: the server goes away (connections dropped, new ones
// refused) -> backoff; it comes back -> live again, and the mirror catches up.
func TestBackoffAndRecovery(t *testing.T) {
	mb := newMailbox(t, imaptest.Full)
	mb.populate(spec{"INBOX", 3})
	px := newProxy(t, net.JoinHostPort(mb.host, strconv.Itoa(mb.port)))
	a := newAccount(t, "127.0.0.1", px.port(), mb.user, mb.pass)
	a.start()
	a.waitInitialDone(time.Minute)

	px.setUp(false)
	a.waitState("backoff", 30*time.Second)
	if a.int(`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'account.error' AND payload->>'code' = 'unreachable'`, a.userID) == 0 {
		t.Error("no account.error unreachable")
	}
	c := mb.client()
	appendMsg(t, c, "INBOX", msg(500, 0, "while offline", time.Now()), nil, time.Now())
	px.setUp(true)
	a.waitState("live", 30*time.Second)
	waitFor(t, 10*time.Second, "offline message synced", func() bool {
		_, _, _, ok := a.msgByHdr("m500@silo.test")
		return ok
	})
}

// TestGracefulShutdown: cancelling the worker closes its connections and
// leaves no goroutine behind.
func TestGracefulShutdown(t *testing.T) {
	mb := newMailbox(t, imaptest.Full)
	mb.populate(spec{"INBOX", 50})
	before := runtime.NumGoroutine()
	a := mb.account(mb.pass)
	r := a.start()
	a.waitState("live", 30*time.Second)
	t0 := time.Now()
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not stop within 10s")
	}
	t.Logf("worker stopped in %s", time.Since(t0).Round(time.Millisecond))
	var after int
	waitFor(t, 5*time.Second, "goroutines to finish", func() bool {
		after = runtime.NumGoroutine()
		return after <= before+1
	})
}

// TestManagerJobs: serve wiring. A new account's initial_sync job starts its
// worker through LISTEN/NOTIFY; resync jobs reach the worker.
func TestManagerJobs(t *testing.T) {
	mb := newMailbox(t, imaptest.Full)
	mb.populate(spec{"INBOX", 3})
	// The Manager starts every account that is not disabled: retire those of
	// the tests before (their Dovecots are gone).
	_, err := testPool.Exec(context.Background(), `UPDATE accounts SET sync_state = 'disabled'`)
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	jobs := worker.New(worker.Options{Pool: testPool, PollInterval: 200 * time.Millisecond})
	a := mb.account(mb.pass)
	deps := *a.deps
	m := silosync.NewManager(deps)
	m.RegisterJobs(jobs)
	jobsDone := make(chan struct{})
	mDone := make(chan struct{})
	go func() { defer close(jobsDone); _ = jobs.Run(ctx) }()
	go func() { defer close(mDone); _ = m.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-mDone; <-jobsDone })

	// The manager started with the account already present (startup path).
	a.waitInitialDone(time.Minute)

	// A second account appears later with its initial_sync job (sign-in path).
	b := mb.account(mb.pass)
	acctID := b.id
	_, err = worker.Enqueue(ctx, testPool, worker.NewJob{Kind: "account.initial_sync", AccountID: &acctID})
	must(t, err)
	b.waitInitialDone(time.Minute)
	waitFor(t, 10*time.Second, "initial_sync job consumed", func() bool {
		return a.int(`SELECT count(*) FROM jobs WHERE account_id = $1`, b.id) == 0
	})

	// resync full: a full pass that updates last_full_flags_at of every folder.
	_, err = testPool.Exec(ctx, `UPDATE mailbox_folders SET last_sync_at = NULL WHERE account_id = $1`, b.id)
	must(t, err)
	_, err = worker.Enqueue(ctx, testPool, worker.NewJob{Kind: "account.resync", AccountID: &acctID, Payload: map[string]string{"mode": "full"}})
	must(t, err)
	waitFor(t, 10*time.Second, "resync pass", func() bool {
		return a.int(`SELECT count(*) FROM mailbox_folders WHERE account_id = $1 AND last_sync_at IS NULL`, b.id) == 0
	})

	// Deleting the account stops its worker at its next pass (or reconcile).
	_, err = testPool.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, b.id)
	must(t, err)
	m.Worker(b.id).RequestResync("incremental")
	waitFor(t, 45*time.Second, "worker of the deleted account to stop", func() bool { return m.Worker(b.id) == nil })
}
