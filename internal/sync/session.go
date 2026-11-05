package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/imapx"
	"github.com/silopost/silo-server/internal/mail"
)

// session is one connected period of an account. The sync connection and all
// fields are used only from the supervisor goroutine (s.run); the idle
// connection lives on its own goroutine and only calls w.signalInbox.
type session struct {
	w   *AccountWorker
	d   *Deps
	acc *account
	sc  *conn
	log *slog.Logger

	folders []*folder

	idleDone chan error // result of the idle goroutine; nil when not started
	idleStop context.CancelFunc
	idleOn   bool

	candidates map[int64][]uint32  // phase B: UIDs below the cursor still to process, ascending
	bodySkip   map[int64]time.Time // bodies the server did not return; retried later
	bodyWork   bool                // phase C may have work
	backfillAt time.Time           // body backfill pacing (SILO_SYNC_BACKFILL_RATE)
	slowUntil  time.Time           // after NO [OVERQUOTA]/[LIMIT]
	quotaHit   bool
	lastProg   time.Time
}

func newSession(w *AccountWorker, acc *account, sc *conn) *session {
	return &session{
		w: w, d: w.d, acc: acc, sc: sc, log: w.log,
		candidates: map[int64][]uint32{}, bodySkip: map[int64]time.Time{}, bodyWork: true,
	}
}

// close stops the idle goroutine and logs out of the sync connection.
func (s *session) close() {
	if s.idleStop != nil {
		s.idleStop()
		<-s.idleDone
	}
	_ = s.sc.cl.Logout(context.Background())
}

func (s *session) saveCapabilities(ctx context.Context) error {
	names := s.sc.caps.Names()
	slices.Sort(names)
	return inTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE accounts SET capabilities = $2 WHERE id = $1`, s.acc.id, names)
		return err
	})
}

func (s *session) folderByRole(role string) *folder {
	for _, f := range s.folders {
		if f.role == role {
			return f
		}
	}
	return nil
}

func (s *session) folderByID(id int64) *folder {
	for _, f := range s.folders {
		if f.id == id {
			return f
		}
	}
	return nil
}

func (s *session) reloadFolders(ctx context.Context) error {
	fs, err := loadFolders(ctx, s.d.Pool, s.acc.id)
	if err != nil {
		return err
	}
	s.folders = fs
	return nil
}

// goLive switches the account to live (plan 3.5.2: after phase A), starts
// the idle connection and schedules a catch-up pass over all folders.
func (s *session) goLive(ctx context.Context) error {
	if err := inTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE accounts SET filters_since = now() WHERE id = $1 AND filters_since IS NULL`, s.acc.id)
		return err
	}); err != nil {
		return err
	}
	if err := s.w.setState(ctx, StateLive, "", ""); err != nil {
		return err
	}
	s.startIdle(ctx)
	s.w.request(func(p *pending) { p.all = true })
	return nil
}

// startIdle opens the idle connection when the server has IDLE and the
// account may use a second connection.
func (s *session) startIdle(ctx context.Context) {
	s.w.mu.Lock()
	limit := s.w.connLimit
	s.w.mu.Unlock()
	if !s.sc.caps.HasIdle() || limit < 2 {
		return
	}
	ictx, cancel := context.WithCancel(ctx)
	ready := make(chan error, 1)
	s.idleStop, s.idleDone = cancel, make(chan error, 1)
	go func() { s.idleDone <- s.runIdle(ictx, ready) }()
	select {
	case err := <-ready:
		if err != nil {
			<-s.idleDone
			s.idleStop, s.idleDone = nil, nil
			cancel()
			if errors.Is(err, imapx.ErrUnavailable) || isCommandError(err) {
				// BYE / [LIMIT]: too many connections; keep one and poll INBOX.
				s.w.mu.Lock()
				s.w.connLimit = 1
				s.w.mu.Unlock()
			}
			s.log.Warn("idle connection unavailable; polling INBOX instead", "err", err)
			return
		}
		s.idleOn = true
	case <-ctx.Done():
	}
}

// runIdle keeps SELECT INBOX + IDLE on a dedicated connection. Unilateral
// EXISTS / EXPUNGE / FETCH only signal "INBOX changed" (ADR 0006: no UID in
// the unilateral FETCH); the handlers never block the reader.
func (s *session) runIdle(ctx context.Context, ready chan<- error) error {
	h := &imapclient.UnilateralDataHandler{
		Mailbox: func(d *imapclient.UnilateralDataMailbox) {
			if d.NumMessages != nil {
				s.w.signalInbox()
			}
		},
		Expunge: func(uint32) { s.w.signalInbox() },
		Fetch: func(m *imapclient.FetchMessageData) {
			_, _ = m.Collect()
			s.w.signalInbox()
		},
	}
	c, err := s.w.dial(ctx, s.acc, imapx.WithUnilateralDataHandler(h))
	if err != nil {
		ready <- err
		return err
	}
	defer func() { _ = c.cl.Logout(context.Background()) }()
	if _, err := c.selectFolder(ctx, "INBOX"); err != nil {
		ready <- err
		return err
	}
	idle, err := c.imap().Idle()
	if err != nil {
		ready <- err
		return err
	}
	ready <- nil
	select {
	case <-ctx.Done():
		_ = c.await(context.Background(), 5*time.Second, func() error {
			if err := idle.Close(); err != nil {
				return err
			}
			return idle.Wait()
		})
		return nil
	case <-c.imap().Closed():
		return fmt.Errorf("%w: idle connection closed", imapx.ErrUnavailable)
	}
}

// run is the scheduler of the sync connection: incremental work first, then
// one backfill batch at a time (phase B headers, then phase C bodies), so a
// signal from IDLE never waits for more than one batch.
func (s *session) run(ctx context.Context) error {
	poll := time.NewTicker(s.d.Settings.withDefaults().PollInterval)
	defer poll.Stop()
	var inboxTick <-chan time.Time
	if !s.idleOn {
		t := time.NewTicker(inboxPollNoIdle)
		defer t.Stop()
		inboxTick = t.C
	}
	var idleErr <-chan error = s.idleDone
	for {
		select { // events that arrived while working
		case <-ctx.Done():
			return nil
		case err := <-idleErr:
			s.idleStop, s.idleDone = nil, nil
			return err
		case <-poll.C:
			s.w.request(func(p *pending) { p.sweep = true })
		case <-inboxTick:
			s.w.request(func(p *pending) { p.inbox = true })
		default:
		}
		if p := s.w.take(); p.any() {
			if err := s.incremental(ctx, p); err != nil {
				return err
			}
			continue
		}
		did, err := s.backfillStep(ctx)
		if err != nil {
			return err
		}
		if did {
			continue
		}
		var wait <-chan time.Time
		if s.bodyWork && time.Now().Before(s.backfillAt) {
			wait = time.After(time.Until(s.backfillAt))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.w.kick:
		case err := <-idleErr:
			s.idleStop, s.idleDone = nil, nil
			return err
		case <-poll.C:
			s.w.request(func(p *pending) { p.sweep = true })
		case <-inboxTick:
			s.w.request(func(p *pending) { p.inbox = true })
		case <-wait:
		}
	}
}

// incremental runs the requested incremental work (plan 3.5.4).
func (s *session) incremental(ctx context.Context, p pending) error {
	var statuses map[int64]*imap.StatusData
	todo := map[int64]bool{}
	for id := range p.folders {
		todo[id] = true
	}
	if p.discover || p.sweep {
		var err error
		if statuses, err = s.discover(ctx); err != nil {
			return err
		}
		if p.sweep {
			for _, f := range s.folders {
				if s.changed(f, statuses[f.id]) {
					todo[f.id] = true
				}
			}
		}
	}
	if p.all || p.full {
		for _, f := range s.folders {
			todo[f.id] = true
		}
	}
	if p.inbox {
		if in := s.folderByRole(RoleInbox); in != nil {
			if err := s.syncFolderSafe(ctx, in, true, nil); err != nil {
				return err
			}
			delete(todo, in.id)
		}
	}
	order := make([]*folder, 0, len(todo))
	for id := range todo {
		if f := s.folderByID(id); f != nil {
			order = append(order, f)
		}
	}
	slices.SortFunc(order, func(a, b *folder) int {
		if d := backfillOrder(a.role) - backfillOrder(b.role); d != 0 {
			return d
		}
		return int(a.id - b.id)
	})
	for _, f := range order {
		if ctx.Err() != nil {
			return nil
		}
		if err := s.syncFolderSafe(ctx, f, p.full, statuses[f.id]); err != nil {
			return err
		}
	}
	return inTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE accounts SET last_sync_at = now() WHERE id = $1 AND sync_state <> 'disabled'`, s.acc.id)
		if err == nil && tag.RowsAffected() == 0 {
			return errAccountGone
		}
		return err
	})
}

// changed decides from a STATUS whether a folder needs a sync pass now.
func (s *session) changed(f *folder, st *imap.StatusData) bool {
	if !f.synced() {
		return false
	}
	if st == nil || f.uidValidity == 0 {
		return true
	}
	msgs := -1
	if st.NumMessages != nil {
		msgs = int(*st.NumMessages)
	}
	switch {
	case st.UIDValidity != f.uidValidity, uint32(st.UIDNext) != f.uidNext, msgs != f.messages:
		return true
	case s.sc.caps.HasCondstore():
		return st.HighestModSeq != f.highestModSeq
	}
	return f.lastFullFlags == nil || time.Since(*f.lastFullFlags) > fullFlagsMaxAge
}

// syncFolderSafe isolates command errors of one folder (plan 3.5.4): the
// folder gets last_error and an error count, the session goes on.
func (s *session) syncFolderSafe(ctx context.Context, f *folder, force bool, st *imap.StatusData) error {
	err := s.syncFolder(ctx, f, force, st)
	if err == nil || !isCommandError(err) {
		return err
	}
	s.sc.selected = ""
	s.log.Warn("folder sync failed", "folder", f.name, "err", err)
	return s.folderError(ctx, f, err)
}

func (s *session) folderError(ctx context.Context, f *folder, err error) error {
	return inTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE mailbox_folders SET last_error = $2, error_count = error_count + 1 WHERE id = $1`, f.id, err.Error())
		return e
	})
}

// progress writes account.sync_progress at most once per second (always when
// force is set), in the caller's transaction.
func (s *session) progress(ctx context.Context, tx pgx.Tx, force bool) error {
	if !force && time.Since(s.lastProg) < progressEvery {
		return nil
	}
	p, err := loadProgress(ctx, tx, s.acc.id)
	if err != nil {
		return err
	}
	s.lastProg = time.Now()
	return mail.WriteEvent(ctx, tx, s.acc.userID, EventAccountProgress, map[string]any{
		"accountId": s.acc.id, "phase": p.Phase, "foldersDone": p.FoldersDone, "foldersTotal": p.FoldersTotal,
		"messagesDone": p.MessagesDone, "messagesTotal": p.MessagesTotal, "bytesDone": p.BytesDone, "bytesTotal": p.BytesTotal,
	})
}

func (s *session) afterBatch(phase string, n int) {
	if h := s.d.Hooks.AfterBatch; h != nil {
		h(phase, n)
	}
}
