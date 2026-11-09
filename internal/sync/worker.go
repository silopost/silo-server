package sync

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"math/rand/v2"
	stdsync "sync"
	"time"

	"github.com/silopost/silo-server/internal/imapx"
	"github.com/silopost/silo-server/internal/mail"
)

// AccountWorker is the supervisor of one account (plan 3.5.1-3.5.2). Run it
// with Run; signal it with Reconnect, RequestResync and the internal "INBOX
// changed" signal from the idle connection.
type AccountWorker struct {
	id  int64
	d   *Deps
	log *slog.Logger

	// slots limits concurrent start-ups and backfill batches across accounts
	// (SILO_SYNC_INITIAL_CONCURRENCY); shared by the Manager.
	slots chan struct{}

	kick      chan struct{} // pending work was added
	reconnect chan struct{} // leave backoff / auth_failed now

	mu        stdsync.Mutex
	pend      pending
	connLimit int // connections this account may use (lowered on BYE/LIMIT)
	state     string
	live      *session // connected session that executes imap_op jobs
	rebuild   bool     // a cache rebuild was requested (account_rebuilds)
}

// pending is the incremental work requested since the last scheduler pass.
type pending struct {
	inbox    bool           // INBOX changed (IDLE) or INBOX poll
	folders  map[int64]bool // folders to sync now
	sweep    bool           // LIST(-STATUS) and sync the folders that changed
	all      bool           // incremental sync of every folder
	full     bool           // with full flag comparison (resync full)
	discover bool           // re-run discovery (folder mapping changed)
}

func (p pending) any() bool {
	return p.inbox || len(p.folders) > 0 || p.sweep || p.all || p.full || p.discover
}

// NewAccountWorker returns the worker of one account. slots may be nil (no
// cross-account limit).
func NewAccountWorker(d *Deps, accountID int64, slots chan struct{}) *AccountWorker {
	if slots == nil {
		slots = make(chan struct{}, max(d.Settings.withDefaults().InitialConcurrency, 1))
	}
	return &AccountWorker{
		id: accountID, d: d, log: d.log().With("component", "sync", "account", accountID), slots: slots,
		kick: make(chan struct{}, 1), reconnect: make(chan struct{}, 1),
		connLimit: d.Settings.withDefaults().MaxConnections,
	}
}

// ID returns the account id.
func (w *AccountWorker) ID() int64 { return w.id }

func (w *AccountWorker) nudge() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

func (w *AccountWorker) request(fn func(p *pending)) {
	w.mu.Lock()
	fn(&w.pend)
	w.mu.Unlock()
	w.nudge()
}

func (w *AccountWorker) take() pending {
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.pend
	w.pend = pending{}
	return p
}

// signalInbox is called from the idle connection's unilateral handlers; it
// must not block.
func (w *AccountWorker) signalInbox() { w.request(func(p *pending) { p.inbox = true }) }

// Reconnect makes a worker in backoff or auth_failed connect immediately
// (account.initial_sync / account.reconnect jobs, new password).
func (w *AccountWorker) Reconnect() {
	select {
	case w.reconnect <- struct{}{}:
	default:
	}
	w.nudge()
}

// RequestResync asks for an incremental pass over all folders, or a full
// flag comparison (mode full). Discovery runs first, so a changed folder
// mapping is applied.
func (w *AccountWorker) RequestResync(mode string) {
	w.request(func(p *pending) {
		p.discover, p.all = true, true
		if mode == ResyncFull {
			p.full = true
		}
	})
	w.Reconnect()
}

// RequestRebuild asks the worker to rebuild the cache (plan 3.3; the request
// itself is the account_rebuilds row): a live session ends as soon as no
// write-back is pending, and the next connect wipes the cache and runs the
// initial sync from scratch.
func (w *AccountWorker) RequestRebuild() {
	w.mu.Lock()
	w.rebuild = true
	w.mu.Unlock()
	w.nudge()
	w.Reconnect()
}

func (w *AccountWorker) takeRebuild() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.rebuild
	w.rebuild = false
	return r
}

// State returns the last state the worker wrote.
func (w *AccountWorker) State() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

func (w *AccountWorker) setState(ctx context.Context, state, code, detail string) error {
	if err := setState(ctx, w.d, w.id, state, code, detail); err != nil {
		return err
	}
	w.mu.Lock()
	w.state = state
	w.mu.Unlock()
	return nil
}

// Run supervises the account until ctx is cancelled or the account is
// deleted or disabled. It also runs the account's job loop when Deps.Jobs is
// set. It returns after every goroutine it started has finished.
func (w *AccountWorker) Run(ctx context.Context) error {
	var wg stdsync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if w.d.Jobs != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				// Fails only while a previous worker of this account is still
				// shutting down its loop.
				if err := w.d.Jobs.RunAccount(ctx, w.id); err != nil {
					select {
					case <-ctx.Done():
					case <-time.After(time.Second):
					}
				}
			}
		}()
	}

	backoff := w.d.Backoff
	if len(backoff) == 0 {
		backoff = DefaultBackoff
	}
	attempt := 0
	for ctx.Err() == nil {
		err := w.connectAndRun(ctx, &attempt)
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, errAccountGone):
			w.log.Info("account deleted or disabled; worker stops")
			return nil
		case errors.Is(err, errRebuild):
			attempt = 0 // the next pass wipes the cache
		case errors.Is(err, imapx.ErrAuthFailed):
			w.log.Warn("authentication failed; waiting for new credentials", "err", err)
			if e := w.setState(ctx, StateAuthFailed, ErrCodeAuthFailed, authFailedDetail(err)); errors.Is(e, errAccountGone) {
				return nil
			}
			attempt = 0
			select { // no automatic retries (fail2ban, plan 3.5.2)
			case <-ctx.Done():
			case <-w.reconnect:
			}
		default:
			delay := backoff[min(attempt, len(backoff)-1)]
			delay = time.Duration(float64(delay) * (0.8 + 0.4*rand.Float64()))
			attempt++
			code := classify(err)
			w.log.Warn("sync session ended; backing off", "err", err, "in", delay.Round(time.Millisecond))
			if e := w.setState(ctx, StateBackoff, code, err.Error()); errors.Is(e, errAccountGone) {
				return nil
			}
			select {
			case <-ctx.Done():
			case <-w.reconnect:
			case <-time.After(delay):
			}
		}
	}
	return nil
}

// classify maps a session error to an account.error code.
func classify(err error) string {
	var certErr *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var recErr tls.RecordHeaderError
	if errors.As(err, &certErr) || errors.As(err, &unknownCA) || errors.As(err, &hostErr) || errors.As(err, &recErr) {
		return ErrCodeTLS
	}
	return ErrCodeUnreachable
}

// acquire takes a start-up/backfill slot; false when ctx ended.
func (w *AccountWorker) acquire(ctx context.Context) bool {
	select {
	case w.slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (w *AccountWorker) release() { <-w.slots }

// dial opens and authenticates one IMAP connection.
func (w *AccountWorker) dial(ctx context.Context, acc *account, opts ...imapx.Option) (*conn, error) {
	if dial := w.d.Settings.DialContext; dial != nil {
		opts = append(opts, imapx.WithDialContext(dial))
	}
	cl, err := imapx.Dial(ctx, acc.host, acc.port, imapx.TLSMode(acc.tls), w.d.Settings.InsecureTLS, opts...)
	if err != nil {
		return nil, err
	}
	if err := w.authenticate(ctx, cl, acc); err != nil {
		cl.Close()
		return nil, err
	}
	caps, err := cl.Capabilities(ctx)
	if err != nil {
		cl.Close()
		return nil, err
	}
	return &conn{cl: cl, caps: caps}, nil
}

// connectAndRun is one pass connecting -> discovering -> (initial_sync) ->
// live, returning when the session ends.
func (w *AccountWorker) connectAndRun(ctx context.Context, attempt *int) error {
	select { // a reconnect request is satisfied by this attempt
	case <-w.reconnect:
	default:
	}
	acc, err := loadAccount(ctx, w.d.Pool, w.id)
	if err != nil {
		return err
	}
	if acc.state == StateDisabled {
		return errAccountGone
	}
	if wiped, err := w.wipeIfRequested(ctx); err != nil {
		return err
	} else if wiped {
		if acc, err = loadAccount(ctx, w.d.Pool, w.id); err != nil {
			return err
		}
	}
	if err := w.setState(ctx, StateConnecting, "", ""); err != nil {
		return err
	}
	if !w.acquire(ctx) {
		return ctx.Err()
	}
	slotHeld := true
	defer func() {
		if slotHeld {
			w.release()
		}
	}()

	sc, err := w.dial(ctx, acc)
	if err != nil {
		return err
	}
	s := newSession(w, acc, sc)
	defer s.close()
	st, err := mail.LoadRebuild(ctx, w.d.Pool, w.id)
	if err != nil {
		return err
	}
	s.restoring = st.Restoring
	if err := s.saveCapabilities(ctx); err != nil {
		return err
	}
	if err := w.setState(ctx, StateDiscovering, "", ""); err != nil {
		return err
	}
	if _, err := s.discover(ctx); err != nil {
		return err
	}
	if !acc.initialDone {
		if err := w.setState(ctx, StateInitialSync, "", ""); err != nil {
			return err
		}
		if err := s.phaseA(ctx); err != nil {
			return err
		}
	}
	w.release()
	slotHeld = false
	if err := s.goLive(ctx); err != nil {
		return err
	}
	*attempt = 0
	w.mu.Lock()
	w.live = s
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.live = nil
		w.mu.Unlock()
		close(s.ended)
	}()
	return s.run(ctx)
}
