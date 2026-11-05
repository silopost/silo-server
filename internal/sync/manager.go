package sync

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	stdsync "sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/ops"
	"github.com/silopost/silo-server/internal/worker"
)

// Manager runs one AccountWorker per account that is not disabled.
//
// Jobs and workers: account jobs (account_id set) are claimed only by the
// account's own job loop, which runs inside its AccountWorker. The job
// handlers therefore just signal the running worker (Reconnect,
// RequestResync); the jobs are the durable, transactional trigger, the
// in-process registry is where the work happens. A job of an account without
// a worker yet (a new account) starts one: the job queue reports every
// insert through LISTEN/NOTIFY (worker.OnNotify), and a periodic reconcile
// covers missed notifications.
type Manager struct {
	d     *Deps
	slots chan struct{}
	log   *slog.Logger

	notify chan int64

	mu      stdsync.Mutex
	ctx     context.Context
	wg      stdsync.WaitGroup
	workers map[int64]*handle
}

type handle struct {
	w      *AccountWorker
	cancel context.CancelFunc
	done   chan struct{}
}

// Intervals of the Manager.
const (
	reconcileEvery = 30 * time.Second
	gcEvery        = time.Hour
)

// NewManager returns a Manager; call RegisterJobs before the job worker
// runs, then Run.
func NewManager(d Deps) *Manager {
	d.Settings = d.Settings.withDefaults()
	m := &Manager{
		d: &d, slots: make(chan struct{}, d.Settings.InitialConcurrency),
		notify: make(chan int64, 256), workers: map[int64]*handle{},
	}
	m.log = d.log().With("component", "sync")
	return m
}

// RegisterJobs binds the engine's job kinds and the insert notification.
func (m *Manager) RegisterJobs(jw *worker.Worker) {
	m.d.Jobs = jw
	wake := func(ctx context.Context, j worker.Job) error {
		if w := m.ensure(j.AccountID.Int64); w != nil {
			w.Reconnect()
		}
		return nil
	}
	jw.Register(JobInitialSync, wake)
	jw.Register(JobReconnect, wake)
	jw.Register(JobResync, func(ctx context.Context, j worker.Job) error {
		var p struct {
			Mode string `json:"mode"`
		}
		_ = json.Unmarshal(j.Payload, &p)
		if w := m.ensure(j.AccountID.Int64); w != nil {
			if p.Mode == ResyncRebuild {
				w.RequestRebuild()
			} else {
				w.RequestResync(p.Mode)
			}
		}
		return nil
	})
	jw.Register(ops.JobKind, func(ctx context.Context, j worker.Job) error {
		w := m.ensure(j.AccountID.Int64)
		if w == nil {
			return worker.Defer(5*time.Second, "sync engine stopping")
		}
		return w.RunOp(ctx, j)
	})
	jw.Register(JobOrphanGC, func(ctx context.Context, j worker.Job) error {
		res, err := OrphanGC(ctx, m.d, j.AccountID.Int64, m.d.now())
		if err == nil && res.Messages > 0 {
			m.log.Info("orphaned messages purged", "account", j.AccountID.Int64, "messages", res.Messages)
		}
		return err
	})
	jw.OnNotify(func(payload string) {
		id, err := strconv.ParseInt(payload, 10, 64)
		if err != nil || id <= 0 {
			return
		}
		select {
		case m.notify <- id:
		default: // the reconcile loop catches up
		}
	})
}

// Run starts the workers of all accounts that are not disabled and keeps the
// registry in step until ctx is cancelled; then it stops every worker
// (connections closed, current transaction finished) and returns.
func (m *Manager) Run(ctx context.Context) error {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	m.reconcile(ctx)
	rec := time.NewTicker(reconcileEvery)
	defer rec.Stop()
	gc := time.NewTicker(gcEvery)
	defer gc.Stop()
	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			for _, h := range m.workers {
				h.cancel()
			}
			m.mu.Unlock()
			m.wg.Wait()
			return nil
		case id := <-m.notify:
			var state string
			err := m.d.Pool.QueryRow(ctx, `SELECT sync_state FROM accounts WHERE id = $1`, id).Scan(&state)
			if err == nil && state != StateDisabled {
				m.ensure(id)
			}
		case <-rec.C:
			m.reconcile(ctx)
		case <-gc.C:
			m.mu.Lock()
			ids := make([]int64, 0, len(m.workers))
			for id := range m.workers {
				ids = append(ids, id)
			}
			m.mu.Unlock()
			for _, id := range ids {
				if err := enqueueOrphanGC(ctx, m.d, id); err != nil {
					m.log.Warn("enqueue orphan_gc", "account", id, "err", err)
				}
			}
		}
	}
}

// reconcile starts missing workers and stops those of disabled or deleted
// accounts.
func (m *Manager) reconcile(ctx context.Context) {
	rows, err := m.d.Pool.Query(ctx, `SELECT id FROM accounts WHERE sync_state <> 'disabled' ORDER BY id`)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("list accounts", "err", err)
		}
		return
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return
	}
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
		m.ensure(id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, h := range m.workers {
		if !want[id] {
			h.cancel()
		}
	}
}

// ensure returns the running worker of an account, starting it if needed.
// It returns nil while the Manager is not running.
func (m *Manager) ensure(id int64) *AccountWorker {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx == nil || m.ctx.Err() != nil {
		return nil
	}
	if h, ok := m.workers[id]; ok {
		select {
		case <-h.done:
		default:
			return h.w
		}
	}
	w := NewAccountWorker(m.d, id, m.slots)
	ctx, cancel := context.WithCancel(m.ctx)
	h := &handle{w: w, cancel: cancel, done: make(chan struct{})}
	m.workers[id] = h
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(h.done)
		defer cancel()
		_ = w.Run(ctx)
		m.mu.Lock()
		if m.workers[id] == h {
			delete(m.workers, id)
		}
		m.mu.Unlock()
	}()
	return w
}

// Worker returns the running worker of an account, or nil.
func (m *Manager) Worker(id int64) *AccountWorker {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.workers[id]; ok {
		return h.w
	}
	return nil
}
