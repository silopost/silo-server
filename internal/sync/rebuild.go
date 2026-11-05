package sync

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
)

// errRebuild ends a live session so that the next pass rebuilds the cache.
var errRebuild = errors.New("sync: cache rebuild requested")

// rebuildRetry is how often a live session re-checks a rebuild that waits for
// the write-back.
const rebuildRetry = 2 * time.Second

// wipeIfRequested wipes the cache of the account when a rebuild is requested
// and nothing waits for the server (mail.RebuildBlocker); a blocked request
// stays and the live session retries it.
func (w *AccountWorker) wipeIfRequested(ctx context.Context) (bool, error) {
	wiped := false
	err := inTx(ctx, w.d.Pool, func(tx pgx.Tx) error {
		st, err := mail.LoadRebuild(ctx, tx, w.id)
		if err != nil || !st.Requested {
			return err
		}
		if err := mail.LockAccount(ctx, tx, w.id); err != nil {
			return err
		}
		why, err := mail.RebuildBlocker(ctx, tx, w.id)
		if err != nil {
			return err
		}
		if why != "" {
			w.log.Info("cache rebuild waits for the write-back", "blocked_by", why)
			w.mu.Lock()
			w.rebuild = true
			w.mu.Unlock()
			return nil
		}
		res, err := mail.WipeCache(ctx, tx, w.id)
		if err != nil {
			return err
		}
		w.log.Info("cache wiped for a rebuild", "messages", res.Messages, "threads", res.Threads, "kept_local", res.KeptMessages,
			"snoozes", res.Snoozes, "local_label_assignments", res.LocalLabels)
		wiped = true
		return nil
	})
	return wiped, err
}

// rebuildReady reports whether a requested rebuild can start now; a blocked
// one is re-checked after rebuildRetry.
func (s *session) rebuildReady(ctx context.Context) (bool, error) {
	st, err := mail.LoadRebuild(ctx, s.d.Pool, s.acc.id)
	if err != nil || !st.Requested {
		return false, err
	}
	why, err := mail.RebuildBlocker(ctx, s.d.Pool, s.acc.id)
	if err != nil {
		return false, err
	}
	if why == "" {
		return true, nil
	}
	time.AfterFunc(rebuildRetry, func() {
		s.w.mu.Lock()
		s.w.rebuild = true
		s.w.mu.Unlock()
		s.w.nudge()
	})
	return false, nil
}

// restoreStep re-attaches what the last rebuild carried over (snoozes, local
// labels, reply drafts) to the messages that are back; once every synced
// folder finished its header backfill, what is still missing is given up.
func (s *session) restoreStep(ctx context.Context) error {
	s.lastRestore = time.Now()
	var final bool
	if err := s.d.Pool.QueryRow(ctx, `
SELECT COALESCE(bool_and(c.phase IN ('bodies','done')), false)
FROM mailbox_folders f LEFT JOIN sync_cursors c ON c.folder_id = f.id
WHERE f.account_id = $1 AND f.selectable AND f.sync_mode = 'sync'`, s.acc.id).Scan(&final); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		done, err := mail.RestoreAfterRebuild(ctx, tx, s.acc.id, final)
		if err == nil && done {
			s.restoring = false
			s.log.Info("cache rebuild finished")
		}
		return err
	})
}
