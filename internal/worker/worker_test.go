package worker

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/store"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
	"github.com/silopost/silo-server/internal/testutil/pgtest"
)

func newPool(t *testing.T) *pgxpool.Pool {
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
	return pool
}

// start runs w.Run in the background and stops it when the test ends.
func start(t *testing.T, w *Worker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("worker did not stop")
		}
	})
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func countJobs(t *testing.T, pool *pgxpool.Pool, where string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM jobs WHERE "+where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGeneralPoolRunsEveryJobExactlyOnce(t *testing.T) {
	const total = 10000
	ctx := context.Background()
	pool := newPool(t)
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (kind) SELECT 'count' FROM generate_series(1, $1)`, total); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	seen := map[int64]int{}
	w := New(Options{Pool: pool, Concurrency: 4, PollInterval: 50 * time.Millisecond})
	w.Register("count", func(_ context.Context, j Job) error {
		mu.Lock()
		seen[j.ID]++
		mu.Unlock()
		return nil
	})
	began := time.Now()
	start(t, w)
	waitFor(t, 60*time.Second, "queue to drain", func() bool { return countJobs(t, pool, "true") == 0 })
	t.Logf("%d jobs with 4 workers in %s", total, time.Since(began))

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != total {
		t.Fatalf("%d distinct jobs ran, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("job %d ran %d times", id, n)
		}
	}
}

func TestAccountJobsRunInOrder(t *testing.T) {
	const perAccount = 50
	ctx := context.Background()
	pool := newPool(t)
	q := sqlcgen.New(pool)

	user, err := q.InsertUser(ctx, sqlcgen.InsertUserParams{Email: "u@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	var accounts []int64
	for i := range 3 {
		acc, err := q.InsertAccount(ctx, sqlcgen.InsertAccountParams{
			UserID: user.ID, Email: "a@example.com", Provider: "generic",
			ImapHost: "imap.example.com", ImapPort: 993, ImapTls: "implicit",
			SmtpHost: "smtp.example.com", SmtpPort: 587, SmtpTls: "starttls",
			Username: fmt.Sprintf("u%d", i), AuthType: "password", SecretEnc: []byte("x"),
		})
		if err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, acc.ID)
	}

	// Random run_at in the past, with only 20 distinct values so ties exercise
	// the id tiebreak; inserted in shuffled order.
	type spec struct{ account, offset int64 }
	var specs []spec
	for _, a := range accounts {
		for range perAccount {
			specs = append(specs, spec{a, rand.Int64N(20) + 1})
		}
	}
	rand.Shuffle(len(specs), func(i, j int) { specs[i], specs[j] = specs[j], specs[i] })
	for _, s := range specs {
		acc := s.account
		_, err := Enqueue(ctx, pool, NewJob{Kind: "op", AccountID: &acc, RunAt: time.Now().Add(-time.Duration(s.offset) * time.Second)})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Expected order per account, as the database sees it.
	want := map[int64][]int64{}
	rows, err := pool.Query(ctx, `SELECT account_id, id FROM jobs ORDER BY run_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a, id int64
		if err := rows.Scan(&a, &id); err != nil {
			t.Fatal(err)
		}
		want[a] = append(want[a], id)
	}
	rows.Close()

	var mu sync.Mutex
	got := map[int64][]int64{}
	w := New(Options{Pool: pool, PollInterval: 50 * time.Millisecond})
	w.Register("op", func(_ context.Context, j Job) error {
		time.Sleep(time.Millisecond) // let the other accounts interleave
		mu.Lock()
		got[j.AccountID.Int64] = append(got[j.AccountID.Int64], j.ID)
		mu.Unlock()
		return nil
	})
	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, a := range accounts {
		wg.Add(1)
		go func() { defer wg.Done(); _ = w.RunAccount(runCtx, a) }()
	}
	t.Cleanup(func() { cancel(); wg.Wait() })

	waitFor(t, 30*time.Second, "account queues to drain", func() bool { return countJobs(t, pool, "true") == 0 })
	mu.Lock()
	defer mu.Unlock()
	for _, a := range accounts {
		if !slices.Equal(got[a], want[a]) {
			t.Errorf("account %d ran out of order:\n got %v\nwant %v", a, got[a], want[a])
		}
	}

	// A second loop for a live account is refused.
	if err := w.RunAccount(runCtx, accounts[0]); err == nil {
		t.Error("duplicate RunAccount accepted")
	}
}

func TestRetryAndDead(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	var mu sync.Mutex
	var attempts []int32
	w := New(Options{
		Pool: pool, PollInterval: 50 * time.Millisecond,
		Backoff: func(int) time.Duration { return 20 * time.Millisecond },
	})
	w.Register("flaky", func(_ context.Context, j Job) error {
		mu.Lock()
		defer mu.Unlock()
		attempts = append(attempts, j.Attempts)
		if len(attempts) < 3 {
			return errors.New("transient")
		}
		return nil
	})
	w.Register("broken", func(context.Context, Job) error { return errors.New("always fails") })
	w.Register("panicky", func(context.Context, Job) error { panic("oops") })

	if _, err := Enqueue(ctx, pool, NewJob{Kind: "flaky"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (kind, max_attempts) VALUES ('broken', 3), ('panicky', 2), ('nobody', 5)`); err != nil {
		t.Fatal(err)
	}
	start(t, w)
	waitFor(t, 15*time.Second, "retries to settle", func() bool {
		return countJobs(t, pool, "status = 'pending'") == 0
	})

	mu.Lock()
	if !slices.Equal(attempts, []int32{1, 2, 3}) {
		t.Errorf("flaky attempts = %v, want [1 2 3]", attempts)
	}
	mu.Unlock()
	if n := countJobs(t, pool, "kind = 'flaky'"); n != 0 {
		t.Errorf("flaky job still present after success")
	}

	rows, err := pool.Query(ctx, `SELECT kind, attempts, status, last_error FROM jobs ORDER BY kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	wantRows := []string{
		"broken 3 dead always fails",
		"nobody 1 dead no handler registered for kind \"nobody\"",
		"panicky 2 dead handler panic: oops",
	}
	var gotRows []string
	for rows.Next() {
		var kind, status, lastErr string
		var attempts int32
		if err := rows.Scan(&kind, &attempts, &status, &lastErr); err != nil {
			t.Fatal(err)
		}
		gotRows = append(gotRows, fmt.Sprintf("%s %d %s %s", kind, attempts, status, lastErr))
	}
	if !slices.Equal(gotRows, wantRows) {
		t.Errorf("dead jobs:\n got %q\nwant %q", gotRows, wantRows)
	}
}

func TestNotifyWakesIdleWorker(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	ran := make(chan int64, 1)
	// Polling is far too slow to be what wakes the worker.
	w := New(Options{Pool: pool, PollInterval: time.Minute})
	w.Register("ping", func(_ context.Context, j Job) error { ran <- j.ID; return nil })
	start(t, w)
	time.Sleep(500 * time.Millisecond) // workers idle, LISTEN established

	began := time.Now()
	job, err := Enqueue(ctx, pool, NewJob{Kind: "ping", Payload: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-ran:
		if id != job.ID {
			t.Fatalf("ran job %d, want %d", id, job.ID)
		}
		t.Logf("job started %s after insert", time.Since(began))
	case <-time.After(time.Second):
		t.Fatal("job not picked up within 1s")
	}
}

func TestEnqueueInTransactionRollsBack(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Enqueue(ctx, tx, NewJob{Kind: "x", Payload: []int{1}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countJobs(t, pool, "true"); n != 0 {
		t.Fatalf("%d jobs survived rollback", n)
	}
}
