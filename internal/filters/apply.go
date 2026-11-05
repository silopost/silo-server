package filters

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/mailview"
	"github.com/silopost/silo-server/internal/ops"
	"github.com/silopost/silo-server/internal/search"
	"github.com/silopost/silo-server/internal/worker"
)

// JobFilterApply is the general job of POST /filters/{id}/apply.
const JobFilterApply = "filter_apply"

// EventFilterApplied is written when filter_apply is done (plan 5.4):
// {filterId, accountId, messages, threads}.
const EventFilterApplied = "filter.applied"

type applyPayload struct {
	FilterID int64  `json:"filterId"`
	UserID   int64  `json:"userId"`
	Origin   string `json:"origin,omitempty"`
	// Progress, saved after every batch so that a retry goes on.
	LastID   int64   `json:"lastId,omitempty"`
	Messages int     `json:"messages,omitempty"`
	Threads  []int64 `json:"threads,omitempty"`
}

// ApplyResult is the answer of Apply.
type ApplyResult struct {
	JobID int64
	// MatchedEstimate is the number of conversations the criteria match now
	// (nil when counting took too long).
	MatchedEstimate *int
}

// applyFrom is the set of messages "apply to existing" acts on: messages of
// the account on the server or local (sent), not orphaned and not drafts.
// The default exclusion of Spam and Trash of the search syntax holds (unless
// the criteria name in:spam, in:trash or in:anywhere), as in Gmail.
const applyFrom = `FROM messages m WHERE m.user_id = $1 AND m.account_id = $2 AND m.orphaned_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM drafts d WHERE d.message_id = m.id) AND `

func (s *Service) where(ctx context.Context, f *Filter, argOffset int) (string, []any, error) {
	a, err := ParseCriteria(f.Criteria)
	if err != nil {
		return "", nil, err
	}
	scope, err := mailview.LoadSearchScope(ctx, s.o.Pool, f.UserID, f.AccountID)
	if err != nil {
		return "", nil, err
	}
	scope.ArgOffset = argOffset
	return search.ToSQL(a, scope)
}

// Apply queues filter_apply: the filter's actions on the existing messages
// that match its criteria now (POST /filters/{id}/apply). The actions run in
// the background in batches of ApplyBatch messages, as operations without
// undo; forwardTo is not applied to existing mail (as in Gmail).
func (s *Service) Apply(ctx context.Context, userID, id int64, origin string) (*ApplyResult, error) {
	f, err := s.Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	where, args, err := s.where(ctx, f, 2)
	if err != nil {
		return nil, err
	}
	res := &ApplyResult{}
	err = pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = 3000`); err != nil {
			return err
		}
		var n int
		err := tx.QueryRow(ctx, `SELECT count(DISTINCT m.thread_id) `+applyFrom+where, append([]any{userID, f.AccountID}, args...)...).Scan(&n)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "57014" { // query_canceled: no estimate
			return nil
		}
		if err != nil {
			return err
		}
		res.MatchedEstimate = &n
		return nil
	})
	if err != nil {
		return nil, err
	}
	job, err := worker.Enqueue(ctx, s.o.Pool, worker.NewJob{Kind: JobFilterApply, Payload: applyPayload{FilterID: id, UserID: userID, Origin: origin}})
	if err != nil {
		return nil, err
	}
	res.JobID = job.ID
	return res, nil
}

func (s *Service) runApply(ctx context.Context, job worker.Job) error {
	var p applyPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return worker.Permanent(err)
	}
	f, err := s.Get(ctx, p.UserID, p.FilterID)
	if errors.Is(err, ErrNotFound) {
		return nil // deleted meanwhile
	}
	if err != nil {
		return err
	}
	where, args, err := s.where(ctx, f, 4)
	if errors.Is(err, ErrInvalid) || errors.Is(err, search.ErrInvalidQuery) {
		return worker.Permanent(err)
	}
	if err != nil {
		return err
	}
	add, remove := f.Actions.labelChange()
	threads := map[int64]bool{}
	for _, t := range p.Threads {
		threads[t] = true
	}
	for {
		rows, err := s.o.Pool.Query(ctx, `SELECT m.id, m.thread_id `+applyFrom+`m.id > $3 AND `+where+` ORDER BY m.id LIMIT $4`,
			append([]any{p.UserID, f.AccountID, p.LastID, s.o.ApplyBatch}, args...)...)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id, th int64
			if err := rows.Scan(&id, &th); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
			threads[th] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		if len(add)+len(remove) > 0 {
			_, err := s.o.Ops.ModifyLabels(ctx, ops.Modify{UserID: p.UserID, MessageIDs: ids, Add: add, Remove: remove, Origin: p.Origin, Immediate: true})
			if err != nil && !errors.Is(err, ops.ErrNotFound) {
				if errors.Is(err, ops.ErrInvalid) {
					return worker.Permanent(err) // e.g. a label deleted meanwhile
				}
				return err
			}
		}
		p.LastID, p.Messages = ids[len(ids)-1], p.Messages+len(ids)
		p.Threads = p.Threads[:0]
		for t := range threads {
			p.Threads = append(p.Threads, t)
		}
		raw, _ := json.Marshal(p)
		if _, err := s.o.Pool.Exec(ctx, `UPDATE jobs SET payload = $2, locked_until = now() + interval '5 minutes' WHERE id = $1`, job.ID, raw); err != nil {
			return err
		}
		if len(ids) < s.o.ApplyBatch {
			break
		}
	}
	ev := map[string]any{"filterId": f.ID, "accountId": f.AccountID, "messages": p.Messages, "threads": len(threads)}
	if p.Origin != "" {
		ev["origin"] = p.Origin
	}
	return mail.WriteEvent(ctx, s.o.Pool, f.UserID, EventFilterApplied, ev)
}

// RegisterJobs binds filter_eval and filter_apply to the job worker.
func (s *Service) RegisterJobs(w *worker.Worker) {
	w.Register(mail.JobFilterEval, s.runEval)
	w.Register(JobFilterApply, s.runApply)
}

// Run executes one filter_eval or filter_apply job outside the worker (tests
// and tools); the worker calls the same handlers.
func (s *Service) Run(ctx context.Context, job worker.Job) error {
	switch job.Kind {
	case mail.JobFilterEval:
		return s.runEval(ctx, job)
	case JobFilterApply:
		return s.runApply(ctx, job)
	}
	return worker.Permanent(errors.New("filters: not a filter job: " + job.Kind))
}
