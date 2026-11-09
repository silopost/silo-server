package drafts

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
	"github.com/silopost/silo-server/internal/worker"
)

// JobBlobGC is the general job that deletes unpinned uploads and
// unreferenced blobs (plan 3.13, 3.16).
const JobBlobGC = "blob_gc"

// blobMinAge: unreferenced blobs younger than this are kept (an upload or a
// body download may be about to reference them).
const blobMinAge = 24 * time.Hour

// GCResult counts what BlobGC deleted.
type GCResult struct {
	Uploads int // unpinned uploads older than UploadMaxAge
	Blobs   int // unreferenced blobs (rows and bytes)
}

// BlobGC deletes uploads that were never pinned to a draft (or were unpinned)
// more than UploadMaxAge before now, releasing their blobs, and then blobs
// with ref_count = 0 created more than 24 h before now: the row first, the
// bytes after. A row still referenced by a foreign key (a counting drift) is
// kept. now is the current time (tests move it).
func (s *Service) BlobGC(ctx context.Context, now time.Time) (*GCResult, error) {
	res := &GCResult{}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `DELETE FROM attachments WHERE message_id IS NULL AND created_at < $1 RETURNING blob_sha256`, now.Add(-UploadMaxAge))
		if err != nil {
			return err
		}
		shas, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			return err
		}
		res.Uploads = len(shas)
		_, err = tx.Exec(ctx, `
UPDATE blobs b SET ref_count = GREATEST(b.ref_count - c.n, 0)
FROM (SELECT sha, count(*)::int AS n FROM unnest($1::bytea[]) AS sha WHERE sha IS NOT NULL GROUP BY sha) c
WHERE b.sha256 = c.sha`, shas)
		if err != nil {
			return err
		}
		// Thumbnails of blobs nobody references any more go with them: the
		// row is deleted and the thumbnail blob released (deleted below once
		// old enough).
		_, err = tx.Exec(ctx, `
WITH d AS (
  DELETE FROM thumbnails t USING blobs b WHERE b.sha256 = t.source_sha256 AND b.ref_count = 0 RETURNING t.thumb_sha256
)
UPDATE blobs b SET ref_count = GREATEST(b.ref_count - c.n, 0)
FROM (SELECT thumb_sha256 AS sha, count(*)::int AS n FROM d GROUP BY thumb_sha256) c
WHERE b.sha256 = c.sha`)
		return err
	})
	if err != nil {
		return nil, err
	}
	q := sqlcgen.New(s.o.Pool)
	for {
		list, err := q.ListUnreferencedBlobs(ctx, sqlcgen.ListUnreferencedBlobsParams{CreatedAt: pgtype.Timestamptz{Time: now.Add(-blobMinAge), Valid: true}, Limit: 500})
		if err != nil {
			return res, err
		}
		deleted := 0
		for _, b := range list {
			tag, err := s.o.Pool.Exec(ctx, `DELETE FROM blobs WHERE sha256 = $1 AND ref_count = 0`, b.Sha256)
			if err != nil {
				s.log.Warn("blob still referenced; kept", "sha256", hex.EncodeToString(b.Sha256), "error", err)
				continue
			}
			if tag.RowsAffected() == 0 {
				continue
			}
			deleted++
			if err := s.o.Blobs.Delete(ctx, hex.EncodeToString(b.Sha256)); err != nil {
				s.log.Warn("delete blob bytes", "sha256", hex.EncodeToString(b.Sha256), "error", err)
			}
		}
		res.Blobs += deleted
		if len(list) < 500 || deleted == 0 {
			return res, nil
		}
	}
}

// RegisterJobs binds send and blob_gc to the job worker. The periodic job (run at
// start and hourly) also deletes empty drafts (PurgeEmptyDrafts), fails sends
// interrupted by a restart (FailStuckSends) and runs
// the extra clean-up steps (e.g. ops.Service.PurgeOperations) after BlobGC.
func (s *Service) RegisterJobs(w *worker.Worker, extra ...func(ctx context.Context, now time.Time) error) {
	w.Register(JobSend, s.runSend)
	w.Register(JobBlobGC, func(ctx context.Context, _ worker.Job) error {
		now := time.Now()
		r, err := s.BlobGC(ctx, now)
		if err != nil {
			return err
		}
		if r.Uploads > 0 || r.Blobs > 0 {
			s.log.Info("blob gc", "uploads", r.Uploads, "blobs", r.Blobs)
		}
		if n, err := s.PurgeEmptyDrafts(ctx, now); err != nil {
			return err
		} else if n > 0 {
			s.log.Info("empty drafts deleted", "drafts", n)
		}
		if _, err := s.FailStuckSends(ctx, now); err != nil {
			return err
		}
		for _, fn := range extra {
			if err := fn(ctx, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// RunGC enqueues blob_gc at start and then every interval (unless one is
// pending) until ctx ends.
func (s *Service) RunGC(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := s.o.Pool.Exec(ctx, `
INSERT INTO jobs (kind) SELECT $1 WHERE NOT EXISTS (SELECT 1 FROM jobs WHERE kind = $1 AND status = 'pending')`, JobBlobGC); err != nil && ctx.Err() == nil {
			s.log.Warn("enqueue blob_gc", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// EmptyDraftMaxAge is the age after which a draft never saved and still empty
// is deleted (the compose window was closed without typing; plan 3.8).
const EmptyDraftMaxAge = 24 * time.Hour

// PurgeEmptyDrafts deletes drafts created by POST /drafts more than
// EmptyDraftMaxAge before now and never saved since: version 1, no
// recipients, empty subject and body, no attachments, no copy on the server
// and no send. They go like a discarded draft (thread.updated/deleted). It
// returns how many were deleted.
func (s *Service) PurgeEmptyDrafts(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.o.Pool.Query(ctx, `
SELECT d.account_id, d.message_id FROM drafts d
WHERE d.version = 1 AND d.server_synced_version = 0 AND d.updated_at < $1
  AND d.to_addrs = '[]' AND d.cc_addrs = '[]' AND d.bcc_addrs = '[]' AND d.subject = '' AND d.body_html = ''
  AND d.attachment_ids = '{}'
  AND NOT EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = d.message_id)
  AND NOT EXISTS (SELECT 1 FROM message_locations l WHERE l.message_id = d.message_id)
  AND NOT EXISTS (SELECT 1 FROM outbound o WHERE o.message_id = d.message_id)
ORDER BY d.account_id, d.message_id`, now.Add(-EmptyDraftMaxAge))
	if err != nil {
		return 0, err
	}
	type item struct{ account, msg int64 }
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (item, error) {
		var it item
		return it, r.Scan(&it.account, &it.msg)
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for len(list) > 0 {
		acc := list[0].account
		var ids []int64
		for len(list) > 0 && list[0].account == acc {
			ids = append(ids, list[0].msg)
			list = list[1:]
		}
		err := s.inTx(ctx, func(tx pgx.Tx) error {
			if err := mail.LockAccount(ctx, tx, acc); err != nil {
				return err
			}
			// Re-check under the lock: a save may have come in meanwhile.
			r, err := tx.Query(ctx, `
SELECT message_id FROM drafts WHERE message_id = ANY($1) AND version = 1 AND server_synced_version = 0
  AND NOT EXISTS (SELECT 1 FROM outbound o WHERE o.message_id = drafts.message_id)`, ids)
			if err != nil {
				return err
			}
			still, err := pgx.CollectRows(r, pgx.RowTo[int64])
			if err != nil || len(still) == 0 {
				return err
			}
			for _, id := range still {
				if _, err := removeServerCopy(ctx, tx, acc, id); err != nil {
					return err
				}
			}
			n += len(still)
			return mail.DeleteMessages(ctx, tx, acc, still, "")
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// InterruptedError is outbound.last_error of a send found stuck in sending.
const InterruptedError = "interrupted"

// FailStuckSends fails sends left in sending (the server stopped during the
// delivery): rows not updated for 2 x SMTPTimeout whose send job is not
// running. They are not sent again automatically, since the message may have
// left (a duplicate is worse than a failed send the user can repeat): the
// send fails with last_error "interrupted" and the message is a draft again,
// as after any failure (outbound.status, lastSend). It returns how many.
func (s *Service) FailStuckSends(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.o.Pool.Query(ctx, `
SELECT o.id, o.account_id, o.user_id, o.message_id FROM outbound o
WHERE o.status = 'sending' AND o.updated_at < $1
  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.kind = $2 AND j.status = 'pending' AND j.locked_until > $3
                    AND (j.payload->>'outboundId')::bigint = o.id)
ORDER BY o.id`, now.Add(-2*s.o.SMTPTimeout), JobSend, now)
	if err != nil {
		return 0, err
	}
	type item struct{ id, account, user, msg int64 }
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (item, error) {
		var it item
		return it, r.Scan(&it.id, &it.account, &it.user, &it.msg)
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, it := range list {
		err := s.inTx(ctx, func(tx pgx.Tx) error {
			if err := mail.LockAccount(ctx, tx, it.account); err != nil {
				return err
			}
			var status string
			if err := tx.QueryRow(ctx, `SELECT status FROM outbound WHERE id = $1 FOR UPDATE`, it.id).Scan(&status); err != nil {
				return err
			}
			if status != mail.OutboundSending {
				return nil
			}
			// A leftover job must not deliver it later, outside any undo window.
			if _, err := tx.Exec(ctx, `DELETE FROM jobs WHERE kind = $1 AND status = 'pending' AND (payload->>'outboundId')::bigint = $2`, JobSend, it.id); err != nil {
				return err
			}
			n++
			return s.backToDraft(ctx, tx, it.account, it.user, it.msg, it.id, mail.OutboundFailed, InterruptedError, "")
		})
		if err != nil {
			return n, err
		}
		s.log.Warn("send interrupted; marked failed", "outbound", it.id)
	}
	return n, nil
}
