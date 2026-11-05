// Package worker runs the Postgres-backed job queue (plan 3.13): a general pool
// that claims jobs without an account, and per-account loops that run one
// account's jobs strictly in (run_at, id) order. Jobs are claimed with
// FOR UPDATE SKIP LOCKED; idle loops are woken by LISTEN/NOTIFY with polling as
// a fallback.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

// Job is a row of the jobs table.
type Job = sqlcgen.Job

// Handler executes one job. Returning nil deletes the job; an error schedules a
// retry (or buries the job after max_attempts). Handlers should honour ctx,
// which is cancelled on shutdown; an interrupted job is given back without
// counting the attempt.
type Handler func(ctx context.Context, job Job) error

// Channel is the NOTIFY channel fired by a trigger on jobs inserts.
const Channel = "silo_jobs"

// Options configure a Worker. Zero values pick the defaults in brackets.
type Options struct {
	Pool         *pgxpool.Pool
	Logger       *slog.Logger
	Concurrency  int                              // general pool size [4]
	PollInterval time.Duration                    // fallback polling [5s]
	Backoff      func(attempts int) time.Duration // retry delay [DefaultBackoff]
}

// Worker owns the handler registry and runs the loops.
type Worker struct {
	opts     Options
	log      *slog.Logger
	handlers map[string]Handler

	mu      sync.Mutex
	started bool
	running map[int64]bool // accounts with a live RunAccount loop
	subs    []chan struct{}
	notify  func(payload string)
}

// OnNotify installs fn, called with the payload of every silo_jobs
// notification (the account id of the inserted job, or "" for a general job).
// The sync engine uses it to start the worker of an account that has none yet,
// so that its account loop can claim the job. fn must not block.
func (w *Worker) OnNotify(fn func(payload string)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.notify = fn
}

// New returns a Worker.
func New(opts Options) *Worker {
	if opts.Concurrency < 1 {
		opts.Concurrency = 4
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 5 * time.Second
	}
	if opts.Backoff == nil {
		opts.Backoff = DefaultBackoff
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Worker{opts: opts, log: log.With("component", "worker"), handlers: map[string]Handler{}, running: map[int64]bool{}}
}

// Register binds a handler to a job kind. Call it before Run.
func (w *Worker) Register(kind string, fn Handler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		panic("worker: Register after Run")
	}
	w.handlers[kind] = fn
}

// DefaultBackoff is 10s * 2^(attempts-1) with +-25% jitter, capped at one hour.
func DefaultBackoff(attempts int) time.Duration {
	const maxDelay = time.Hour
	d := maxDelay
	if attempts < 10 {
		d = min(10*time.Second<<max(attempts-1, 0), maxDelay)
	}
	return min(time.Duration(float64(d)*(0.75+rand.Float64()*0.5)), maxDelay)
}

// Run starts the general pool and the LISTEN loop and blocks until ctx is
// cancelled and every running handler has returned.
func (w *Worker) Run(ctx context.Context) error {
	w.mu.Lock()
	w.started = true
	w.mu.Unlock()

	q := sqlcgen.New(w.opts.Pool)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); w.listen(ctx) }()
	for range w.opts.Concurrency {
		wake := w.subscribe()
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx, wake, q.ClaimGeneralJob)
		}()
	}
	wg.Wait()
	return nil
}

// RunAccount runs the jobs of one account on the calling goroutine, strictly
// ordered by (run_at, id), until ctx is cancelled. Only one loop per account
// may run in this process; a second call fails. (The ordering guarantee assumes
// a single server instance, as does the rest of the sync design.)
func (w *Worker) RunAccount(ctx context.Context, accountID int64) error {
	w.mu.Lock()
	w.started = true
	if w.running[accountID] {
		w.mu.Unlock()
		return fmt.Errorf("worker: account %d already has a loop", accountID)
	}
	w.running[accountID] = true
	w.mu.Unlock()
	wake := w.subscribe()
	defer func() {
		w.unsubscribe(wake)
		w.mu.Lock()
		delete(w.running, accountID)
		w.mu.Unlock()
	}()

	q := sqlcgen.New(w.opts.Pool)
	w.loop(ctx, wake, func(ctx context.Context) (Job, error) { return q.ClaimAccountJob(ctx, pgInt8(accountID)) })
	return nil
}

// loop claims and runs jobs until ctx is done, sleeping when the queue is empty.
func (w *Worker) loop(ctx context.Context, wake <-chan struct{}, claim func(context.Context) (Job, error)) {
	timer := time.NewTimer(w.opts.PollInterval)
	defer timer.Stop()
	for ctx.Err() == nil {
		job, err := claim(ctx)
		switch {
		case err == nil:
			w.execute(ctx, job)
			continue
		case errors.Is(err, pgx.ErrNoRows):
		case ctx.Err() != nil:
			return
		default:
			w.log.Error("claim job", "err", err)
		}
		timer.Reset(w.opts.PollInterval)
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-timer.C:
		}
	}
}

// execute runs one claimed job and records the outcome. Bookkeeping uses a
// context that survives shutdown so a finished job is never left locked.
func (w *Worker) execute(ctx context.Context, job Job) {
	db := sqlcgen.New(w.opts.Pool)
	bg := context.WithoutCancel(ctx)
	log := w.log.With("job", job.ID, "kind", job.Kind, "attempt", job.Attempts)

	w.mu.Lock()
	fn := w.handlers[job.Kind]
	w.mu.Unlock()

	switch {
	case fn == nil:
		w.bury(bg, db, job, fmt.Sprintf("no handler registered for kind %q", job.Kind))
		return
	case job.Attempts > job.MaxAttempts:
		// Only reachable when earlier runs crashed without reporting back.
		w.bury(bg, db, job, "max attempts exceeded (worker crashed while running the job)")
		return
	}

	err := safeCall(ctx, fn, job)
	switch {
	case err == nil:
		if err := db.CompleteJob(bg, job.ID); err != nil {
			log.Error("delete finished job", "err", err)
		}
	case ctx.Err() != nil:
		log.Info("job interrupted by shutdown", "err", err)
		if err := db.ReleaseJob(bg, job.ID); err != nil {
			log.Error("release job", "err", err)
		}
	case isDeferred(err):
		d, _ := asDefer(err)
		log.Debug("job deferred", "reason", d.Reason, "until", d.Until)
		if err := db.DeferJob(bg, sqlcgen.DeferJobParams{ID: job.ID, RunAt: pgTime(d.Until), LastError: pgText(d.Error())}); err != nil {
			log.Error("defer job", "err", err)
		}
	case isPermanent(err) || job.Attempts >= job.MaxAttempts:
		log.Error("job failed for good", "err", err)
		w.bury(bg, db, job, err.Error())
	default:
		delay := w.opts.Backoff(int(job.Attempts))
		log.Warn("job failed, will retry", "err", err, "in", delay)
		retry := sqlcgen.RetryJobParams{
			ID:        job.ID,
			RunAt:     pgTime(time.Now().Add(delay)),
			LastError: pgText(err.Error()),
		}
		if err := db.RetryJob(bg, retry); err != nil {
			log.Error("reschedule job", "err", err)
		}
	}
}

func (w *Worker) bury(ctx context.Context, db *sqlcgen.Queries, job Job, msg string) {
	w.log.Error("job is dead", "job", job.ID, "kind", job.Kind, "reason", msg)
	if err := db.BuryJob(ctx, sqlcgen.BuryJobParams{ID: job.ID, LastError: pgText(msg)}); err != nil {
		w.log.Error("bury job", "job", job.ID, "err", err)
	}
}

func safeCall(ctx context.Context, fn Handler, job Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return fn(ctx, job)
}

// listen holds a dedicated connection with LISTEN silo_jobs and wakes every
// idle loop on each notification. It reconnects after errors; polling covers
// any gap.
func (w *Worker) listen(ctx context.Context) {
	for ctx.Err() == nil {
		if err := w.listenOnce(ctx); err != nil && ctx.Err() == nil {
			w.log.Warn("job listener lost, retrying", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (w *Worker) listenOnce(ctx context.Context) error {
	conn, err := pgx.ConnectConfig(ctx, w.opts.Pool.Config().ConnConfig)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return err
	}
	w.broadcast() // catch jobs inserted before LISTEN took effect
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		w.broadcast()
		w.mu.Lock()
		fn := w.notify
		w.mu.Unlock()
		if fn != nil {
			fn(n.Payload)
		}
	}
}

func (w *Worker) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	w.subs = append(w.subs, ch)
	w.mu.Unlock()
	return ch
}

func (w *Worker) unsubscribe(ch chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, c := range w.subs {
		if c == ch {
			w.subs = append(w.subs[:i], w.subs[i+1:]...)
			return
		}
	}
}

// broadcast nudges every loop; a loop that is busy already has a pending nudge.
func (w *Worker) broadcast() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ch := range w.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
