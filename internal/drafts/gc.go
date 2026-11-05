package drafts

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

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

// RegisterJobs binds blob_gc to the job worker.
func (s *Service) RegisterJobs(w *worker.Worker) {
	w.Register(JobBlobGC, func(ctx context.Context, _ worker.Job) error {
		r, err := s.BlobGC(ctx, time.Now())
		if err == nil && (r.Uploads > 0 || r.Blobs > 0) {
			s.log.Info("blob gc", "uploads", r.Uploads, "blobs", r.Blobs)
		}
		return err
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
