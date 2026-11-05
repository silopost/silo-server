package sync

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
)

// OrphanGC is the orphan_gc job of one account (plan 3.5.6 step 4): messages
// orphaned for more than 24 h are deleted, but only once every synced folder
// of the account has finished its header backfill and has been synced after
// the message was orphaned (then it was deleted, not moved). now is the
// current time (tests simulate it).
func OrphanGC(ctx context.Context, d *Deps, accountID int64, now time.Time) (*mail.PurgeResult, error) {
	var res *mail.PurgeResult
	err := inTx(ctx, d.Pool, func(tx pgx.Tx) error {
		var complete bool
		var minSync *time.Time
		err := tx.QueryRow(ctx, `
SELECT COALESCE(bool_and(c.phase IN ('bodies','done') AND f.last_sync_at IS NOT NULL), false), min(f.last_sync_at)
FROM mailbox_folders f LEFT JOIN sync_cursors c ON c.folder_id = f.id
WHERE f.account_id = $1 AND f.selectable AND f.sync_mode = 'sync'`, accountID).Scan(&complete, &minSync)
		if err != nil {
			return err
		}
		if !complete || minSync == nil {
			res = &mail.PurgeResult{}
			return nil
		}
		cutoff := now.Add(-orphanMinAge)
		if minSync.Before(cutoff) {
			cutoff = *minSync
		}
		res, err = mail.PurgeOrphans(ctx, tx, accountID, cutoff)
		return err
	})
	return res, err
}

// enqueueOrphanGC schedules orphan_gc for an account unless one is pending.
func enqueueOrphanGC(ctx context.Context, d *Deps, accountID int64) error {
	_, err := d.Pool.Exec(ctx, `
INSERT INTO jobs (kind, account_id) SELECT $2, $1
WHERE NOT EXISTS (SELECT 1 FROM jobs WHERE kind = $2 AND account_id = $1 AND status = 'pending')`, accountID, JobOrphanGC)
	return err
}
