package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

// NewJob describes a job to enqueue.
type NewJob struct {
	Kind      string
	AccountID *int64      // nil: general job
	GroupID   pgtype.UUID // one user operation (undo); zero value: none
	Payload   any         // JSON-encoded; nil means {}
	RunAt     time.Time   // zero means now
}

// Enqueue inserts a job. Pass a pgx.Tx as db to create the job in the same
// transaction as the domain change that needs it.
func Enqueue(ctx context.Context, db sqlcgen.DBTX, j NewJob) (Job, error) {
	payload := []byte("{}")
	if j.Payload != nil {
		var err error
		if payload, err = json.Marshal(j.Payload); err != nil {
			return Job{}, fmt.Errorf("enqueue %s: payload: %w", j.Kind, err)
		}
	}
	p := sqlcgen.EnqueueJobParams{Kind: j.Kind, GroupID: j.GroupID, Payload: payload}
	if j.AccountID != nil {
		p.AccountID = pgInt8(*j.AccountID)
	}
	if !j.RunAt.IsZero() {
		p.RunAt = pgTime(j.RunAt)
	}
	job, err := sqlcgen.New(db).EnqueueJob(ctx, p)
	if err != nil {
		return Job{}, fmt.Errorf("enqueue %s: %w", j.Kind, err)
	}
	return job, nil
}

func pgInt8(v int64) pgtype.Int8            { return pgtype.Int8{Int64: v, Valid: true} }
func pgText(s string) pgtype.Text           { return pgtype.Text{String: s, Valid: true} }
func pgTime(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
