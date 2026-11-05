package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/ops"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
	"github.com/silopost/silo-server/internal/worker"
)

// JobRestore is the general job that applies the staged metadata of one
// account (payload {"accountId": N}) once the sync has ingested the messages.
const JobRestore = "metadata_restore"

// Kinds of metadata_restores rows.
const (
	restoreLabel  = "label"
	restoreSnooze = "snooze"
)

// Defaults of the restore job.
const (
	DefaultRetryEvery = time.Minute
	DefaultGiveUp     = 24 * time.Hour
)

// origin marks the events written by the restore (X-Silo-Client).
const restoreOrigin = "restore"

type labelPayload struct {
	Label         string   `json:"label"`
	MessageIDHdrs []string `json:"messageIdHdrs"`
}

type snoozePayload struct {
	WakeAt        time.Time         `json:"wakeAt"`
	CreatedAt     time.Time         `json:"createdAt"`
	Items         []string          `json:"items"`
	SourceFolders map[string]string `json:"sourceFolders,omitempty"`
}

type restorePayload struct {
	AccountID int64 `json:"accountId"`
}

// EnqueueRestore queues the metadata_restore job of an account in tx unless
// one is pending already. runAt zero means now.
func EnqueueRestore(ctx context.Context, tx pgx.Tx, accountID int64, runAt time.Time) error {
	var pending bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM jobs WHERE kind = $1 AND status IN ('pending', 'running') AND (payload->>'accountId')::bigint = $2)`,
		JobRestore, accountID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return nil
	}
	_, err := worker.Enqueue(ctx, tx, worker.NewJob{Kind: JobRestore, Payload: restorePayload{AccountID: accountID}, RunAt: runAt})
	return err
}

// RestoreOptions configure the restore job.
type RestoreOptions struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	// RetryEvery is the pause between runs while messages are missing
	// [DefaultRetryEvery]; GiveUp is how long the job keeps trying
	// [DefaultGiveUp].
	RetryEvery, GiveUp time.Duration
}

// RegisterJobs binds the metadata_restore job.
func RegisterJobs(w *worker.Worker, o RestoreOptions) {
	if o.RetryEvery <= 0 {
		o.RetryEvery = DefaultRetryEvery
	}
	if o.GiveUp <= 0 {
		o.GiveUp = DefaultGiveUp
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	w.Register(JobRestore, func(ctx context.Context, j worker.Job) error {
		var p restorePayload
		if err := json.Unmarshal(j.Payload, &p); err != nil || p.AccountID == 0 {
			return worker.Permanent(fmt.Errorf("bad payload %s", j.Payload))
		}
		res, err := Restore(ctx, o.Pool, p.AccountID)
		if err != nil {
			return err
		}
		log := o.Logger.With("component", "backup", "account", p.AccountID)
		if res.Applied > 0 {
			log.Info("metadata restored", "messages", res.Applied, "pending", res.Pending)
		}
		if res.Done {
			return nil
		}
		if j.CreatedAt.Valid && time.Since(j.CreatedAt.Time) > o.GiveUp {
			log.Warn("metadata restore gives up", "pending", res.Pending, "initialSyncDone", res.InitialSyncDone)
			return worker.Permanent(fmt.Errorf("gave up after %s: %d messages of staged labels and snoozes not found (initial sync done: %v); see metadata_restores",
				o.GiveUp, res.Pending, res.InitialSyncDone))
		}
		reason := "waiting for the initial sync"
		if res.InitialSyncDone {
			reason = strconv.Itoa(res.Pending) + " messages not found yet"
		}
		return worker.Defer(o.RetryEvery, reason)
	})
}

// RestoreResult reports one pass of Restore.
type RestoreResult struct {
	Applied         int  // messages that got a label or a snooze now
	Pending         int  // Message-IDs still unmatched
	InitialSyncDone bool // accounts.initial_sync_done_at is set
	// Done: nothing left to do (initial sync done, no staged rows), or the
	// account is gone.
	Done bool
}

// Restore applies what it can of the staged metadata of an account: user
// labels on the messages found by Message-ID (with the keyword written to
// the server for mirrored labels, as POST /threads/modify does), snoozes on
// the threads of their messages, and the link of reply drafts to their
// original. Matched Message-IDs leave the staging rows; fully applied rows
// are deleted. Safe to run any number of times.
func Restore(ctx context.Context, pool *pgxpool.Pool, accountID int64) (*RestoreResult, error) {
	res := &RestoreResult{}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var userID int64
		var keywords bool
		var doneAt pgtype.Timestamptz
		err := tx.QueryRow(ctx, `SELECT user_id, keywords_supported, initial_sync_done_at FROM accounts WHERE id = $1`, accountID).
			Scan(&userID, &keywords, &doneAt)
		if errors.Is(err, pgx.ErrNoRows) {
			res.Done = true
			return nil
		}
		if err != nil {
			return err
		}
		if err := mail.LockAccount(ctx, tx, accountID); err != nil {
			return err
		}
		res.InitialSyncDone = doneAt.Valid
		q := sqlcgen.New(tx)
		rows, err := q.ListMetadataRestores(ctx, sqlcgen.ListMetadataRestoresParams{AccountID: accountID, UserID: userID})
		if err != nil {
			return err
		}
		r := &restorer{ctx: ctx, tx: tx, accountID: accountID, userID: userID, keywords: keywords}
		for _, row := range rows {
			left, err := r.apply(row)
			if err != nil {
				return fmt.Errorf("backup: restore row %d: %w", row.ID, err)
			}
			if left == nil {
				if err := q.DeleteMetadataRestore(ctx, sqlcgen.DeleteMetadataRestoreParams{ID: row.ID, UserID: userID}); err != nil {
					return err
				}
				continue
			}
			msg := fmt.Sprintf("%d messages not found", left.n)
			if err := q.UpdateMetadataRestore(ctx, sqlcgen.UpdateMetadataRestoreParams{ID: row.ID, UserID: userID,
				Payload: left.payload, LastError: pgtype.Text{String: msg, Valid: true}}); err != nil {
				return err
			}
			res.Pending += left.n
		}
		if err := r.linkReplies(); err != nil {
			return err
		}
		if err := r.flush(); err != nil {
			return err
		}
		res.Applied = r.applied
		res.Done = res.InitialSyncDone && res.Pending == 0
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

type restorer struct {
	ctx       context.Context
	tx        pgx.Tx
	accountID int64
	userID    int64
	keywords  bool
	touched   []int64 // messages to re-derive and whose threads to recompute
	applied   int
}

// remainder is what is left of a row: its new payload and the number of
// unmatched Message-IDs.
type remainder struct {
	payload []byte
	n       int
}

// match returns the messages of the account with the given Message-IDs
// (orphans excluded) and the ids that matched none.
func (r *restorer) match(hdrs []string) (map[string][]int64, []string, error) {
	rows, err := r.tx.Query(r.ctx, `
SELECT id, message_id_hdr FROM messages
WHERE account_id = $1 AND user_id = $2 AND message_id_hdr = ANY($3) AND orphaned_at IS NULL
ORDER BY id`, r.accountID, r.userID, hdrs)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	found := map[string][]int64{}
	for rows.Next() {
		var id int64
		var h string
		if err := rows.Scan(&id, &h); err != nil {
			return nil, nil, err
		}
		found[h] = append(found[h], id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var missing []string
	for _, h := range hdrs {
		if found[h] == nil && !slices.Contains(missing, h) {
			missing = append(missing, h)
		}
	}
	return found, missing, nil
}

func (r *restorer) apply(row sqlcgen.MetadataRestore) (*remainder, error) {
	switch row.Kind {
	case restoreLabel:
		var p labelPayload
		if err := json.Unmarshal(row.Payload, &p); err != nil {
			return nil, err
		}
		missing, err := r.applyLabel(p)
		if err != nil || len(missing) == 0 {
			return nil, err
		}
		p.MessageIDHdrs = missing
		b, _ := json.Marshal(p)
		return &remainder{payload: b, n: len(missing)}, nil
	case restoreSnooze:
		var p snoozePayload
		if err := json.Unmarshal(row.Payload, &p); err != nil {
			return nil, err
		}
		missing, err := r.applySnooze(p)
		if err != nil || len(missing) == 0 {
			return nil, err
		}
		p.Items = missing
		b, _ := json.Marshal(p)
		return &remainder{payload: b, n: len(missing)}, nil
	}
	return nil, fmt.Errorf("unknown kind %q", row.Kind)
}

// applyLabel puts a user label on the messages found. Mirrored labels also
// get their keyword on the server (an imap_op flags operation, the path of
// POST /threads/modify) where the message does not carry it yet.
func (r *restorer) applyLabel(p labelPayload) ([]string, error) {
	var labelID int64
	var kw *string
	var state string
	err := r.tx.QueryRow(r.ctx, `
SELECT id, imap_keyword, keyword_state FROM labels
WHERE account_id = $1 AND user_id = $2 AND type = 'user' AND lower(name) = lower($3)`, r.accountID, r.userID, p.Label).
		Scan(&labelID, &kw, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("metadata restore: label is gone; assignments dropped", "account", r.accountID, "label", p.Label)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	found, missing, err := r.match(p.MessageIDHdrs)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, l := range found {
		ids = append(ids, l...)
	}
	if len(ids) == 0 {
		return missing, nil
	}
	rows, err := r.tx.Query(r.ctx, `
INSERT INTO message_labels (message_id, label_id, user_id)
SELECT unnest($1::bigint[]), $2, $3 ON CONFLICT DO NOTHING RETURNING message_id`, ids, labelID, r.userID)
	if err != nil {
		return nil, err
	}
	added, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	if len(added) == 0 {
		return missing, nil
	}
	if r.keywords && state == "mirrored" && kw != nil {
		// Messages on the server without the keyword yet.
		rows, err := r.tx.Query(r.ctx, `
SELECT DISTINCT l.message_id FROM message_locations l
WHERE l.message_id = ANY($1) AND l.user_id = $2 AND NOT EXISTS (
  SELECT 1 FROM unnest(l.flags) f WHERE lower(f) = lower($3))
ORDER BY 1`, added, r.userID, *kw)
		if err != nil {
			return nil, err
		}
		need, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return nil, err
		}
		if err := ops.EnqueueFlags(r.ctx, r.tx, r.accountID, r.userID, []string{*kw}, need); err != nil {
			return nil, err
		}
	}
	r.touched = append(r.touched, added...)
	r.applied += len(added)
	return missing, nil
}

// applySnooze snoozes the thread of the messages found (the first one's
// thread; an existing snooze of that thread is extended). The messages are
// already in the Snoozed folder on the server, so nothing moves.
func (r *restorer) applySnooze(p snoozePayload) ([]string, error) {
	found, missing, err := r.match(p.Items)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, h := range p.Items {
		ids = append(ids, found[h]...)
	}
	if len(ids) == 0 {
		return missing, nil
	}
	var threadID int64
	if err := r.tx.QueryRow(r.ctx, `SELECT thread_id FROM messages WHERE id = $1`, ids[0]).Scan(&threadID); err != nil {
		return nil, err
	}
	var snoozeID int64
	err = r.tx.QueryRow(r.ctx, `SELECT id FROM snoozes WHERE thread_id = $1 AND user_id = $2`, threadID, r.userID).Scan(&snoozeID)
	if errors.Is(err, pgx.ErrNoRows) {
		created := p.CreatedAt
		if created.IsZero() {
			created = time.Now()
		}
		err = r.tx.QueryRow(r.ctx, `
INSERT INTO snoozes (user_id, account_id, thread_id, wake_at, created_at) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			r.userID, r.accountID, threadID, p.WakeAt, created).Scan(&snoozeID)
	}
	if err != nil {
		return nil, err
	}
	for _, h := range p.Items {
		var folder *int64
		if name := p.SourceFolders[h]; name != "" {
			var id int64
			err := r.tx.QueryRow(r.ctx, `SELECT id FROM mailbox_folders WHERE account_id = $1 AND user_id = $2 AND name = $3`,
				r.accountID, r.userID, name).Scan(&id)
			if err == nil {
				folder = &id
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
		}
		for _, id := range found[h] {
			tag, err := r.tx.Exec(r.ctx, `
INSERT INTO snooze_items (snooze_id, message_id, source_folder_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, snoozeID, id, folder)
			if err != nil {
				return nil, err
			}
			r.applied += int(tag.RowsAffected())
		}
	}
	r.touched = append(r.touched, ids...)
	return missing, nil
}

// linkReplies sets drafts.in_reply_to_message_id of reply drafts whose
// original arrived (the link is local and lost by an import or a rebuild).
func (r *restorer) linkReplies() error {
	_, err := r.tx.Exec(r.ctx, `
UPDATE drafts d SET in_reply_to_message_id = (
  SELECT o.id FROM messages o
  WHERE o.account_id = d.account_id AND o.message_id_hdr = m.in_reply_to_hdr AND o.id <> m.id AND o.orphaned_at IS NULL
  ORDER BY o.id LIMIT 1)
FROM messages m
WHERE m.id = d.message_id AND d.account_id = $1 AND d.user_id = $2 AND d.in_reply_to_message_id IS NULL
  AND m.in_reply_to_hdr IS NOT NULL
  AND EXISTS (SELECT 1 FROM messages o WHERE o.account_id = d.account_id AND o.message_id_hdr = m.in_reply_to_hdr AND o.id <> m.id)`,
		r.accountID, r.userID)
	return err
}

// flush re-derives the touched messages and recomputes their threads (thread
// aggregates, thread_labels, snoozed_until) with the usual events.
func (r *restorer) flush() error {
	if len(r.touched) == 0 {
		return nil
	}
	_, err := mail.RefreshMessages(r.ctx, r.tx, r.accountID, r.touched, restoreOrigin)
	return err
}
