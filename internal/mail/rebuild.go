package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Rebuilding the cache of an account (plan 3.3, POST /accounts/{id}/resync
// {mode: rebuild}, silo-server rebuild-cache): everything that Silo mirrors
// from the server is dropped and the initial sync runs from scratch. The
// server is the source of truth for messages, folders, flags and the user
// labels written as IMAP keywords, so those come back on their own; what the
// server does not keep is carried over in account_rebuilds.restore, keyed by
// messages.dedupe_key, and re-attached while the messages come back.
//
// Kept as they are: labels (names, colours, tree, keywords), filters,
// search_history, contacts, settings, mailbox_folders (their sync state is
// reset), and the local messages of drafts and outbound (with their threads):
// the matcher re-attaches them to their server copies (X-Silo-Draft-Id,
// Message-ID of a sent message). Carried over: snoozes, assignments of user
// labels that are local only (no keyword, or local in the folder of the
// message), and the message a draft replies to. Lost: the undo of operations
// made before the rebuild (their messages are new rows).

// EventResync tells the clients to drop every cached query (plan 5.4).
const EventResync = "resync"

// Why a rebuild is refused now (ErrRebuildBlocked carries one of these).
const (
	RebuildBlockedOutbound   = "outbound"   // a message is being sent or due within RebuildSendMargin
	RebuildBlockedOperations = "operations" // imap_op jobs wait for write-back
)

// ErrRebuildBlocked means the cache cannot be rebuilt while local changes wait for
// the server; they would be lost with the messages they point to.
var ErrRebuildBlocked = errors.New("mail: rebuild blocked")

// RebuildSendMargin means a queued send due later than this does not block a
// rebuild. Scheduled mail (schedule send) stays local until its time and is
// kept by WipeCache like a draft; only sends in flight or about to start
// (undo window, a schedule that is due) wait.
const RebuildSendMargin = time.Minute

// RebuildBlocker returns why the cache of the account cannot be rebuilt now
// ("" when it can): sends in flight (outbound sending, or queued and due
// within RebuildSendMargin) and operations waiting for write-back (imap_op
// pending or running).
func RebuildBlocker(ctx context.Context, db DB, accountID int64) (string, error) {
	var outbound, ops bool
	err := db.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM outbound WHERE account_id = $1
                AND (status = 'sending' OR status = 'queued' AND send_at <= now() + make_interval(secs => $2))),
       EXISTS (SELECT 1 FROM jobs WHERE account_id = $1 AND kind = 'imap_op' AND status IN ('pending', 'running'))`, accountID,
		RebuildSendMargin.Seconds()).
		Scan(&outbound, &ops)
	switch {
	case err != nil:
		return "", fmt.Errorf("mail: rebuild blockers: %w", err)
	case outbound:
		return RebuildBlockedOutbound, nil
	case ops:
		return RebuildBlockedOperations, nil
	}
	return "", nil
}

// RequestRebuild records that the account's worker must rebuild the cache on
// its next pass (the caller enqueues the job that wakes it). A rebuild whose
// restore is still running starts again; its leftovers are kept.
func RequestRebuild(ctx context.Context, db DB, accountID int64) error {
	_, err := db.Exec(ctx, `
INSERT INTO account_rebuilds (account_id, user_id)
SELECT id, user_id FROM accounts WHERE id = $1
ON CONFLICT (account_id) DO UPDATE SET requested_at = now(), started_at = NULL`, accountID)
	if err != nil {
		return fmt.Errorf("mail: request rebuild: %w", err)
	}
	return nil
}

// RebuildState is the account_rebuilds row of an account.
type RebuildState struct {
	Requested bool // the cache is to be wiped (started_at NULL)
	Restoring bool // wiped; the restore is pending
}

// LoadRebuild returns the rebuild state of an account (zero when none).
func LoadRebuild(ctx context.Context, db DB, accountID int64) (RebuildState, error) {
	var started *time.Time
	err := db.QueryRow(ctx, `SELECT started_at FROM account_rebuilds WHERE account_id = $1`, accountID).Scan(&started)
	if errors.Is(err, pgx.ErrNoRows) {
		return RebuildState{}, nil
	}
	if err != nil {
		return RebuildState{}, fmt.Errorf("mail: load rebuild: %w", err)
	}
	return RebuildState{Requested: started == nil, Restoring: started != nil}, nil
}

// rebuildRestore is account_rebuilds.restore.
type rebuildRestore struct {
	Snoozes []snoozeSnap `json:"snoozes,omitempty"`
	Labels  []labelSnap  `json:"labels,omitempty"`
	Drafts  []draftSnap  `json:"drafts,omitempty"`
}

type snoozeSnap struct {
	WakeAt    time.Time    `json:"wakeAt"`
	CreatedAt time.Time    `json:"createdAt"`
	Items     []snoozeItem `json:"items"`
}

type snoozeItem struct {
	Key    string `json:"key"`
	Source *int64 `json:"source,omitempty"`
}

type labelSnap struct {
	LabelID int64    `json:"labelId"`
	Keys    []string `json:"keys"`
}

type draftSnap struct {
	MessageID int64  `json:"messageId"`
	ParentKey string `json:"parentKey"`
}

func loadRestore(ctx context.Context, db DB, accountID int64) (*rebuildRestore, error) {
	var raw []byte
	err := db.QueryRow(ctx, `SELECT restore FROM account_rebuilds WHERE account_id = $1`, accountID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mail: load restore: %w", err)
	}
	r := &rebuildRestore{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, r); err != nil {
			return nil, fmt.Errorf("mail: decode restore: %w", err)
		}
	}
	return r, nil
}

// cacheMessage selects the messages of $1 that a rebuild drops: everything
// but the local messages of drafts and outbound.
const cacheMessage = `m.account_id = $1 AND m.body_state <> 'local'
  AND NOT EXISTS (SELECT 1 FROM drafts d WHERE d.message_id = m.id)
  AND NOT EXISTS (SELECT 1 FROM outbound o WHERE o.message_id = m.id)`

// WipeResult counts what WipeCache dropped.
type WipeResult struct {
	Messages, Threads, KeptMessages, Snoozes, LocalLabels int
}

// WipeCache drops the mirror of an account for a rebuild; the caller holds
// the account lock (LockAccount) and has checked RebuildBlocker. The folders
// stay with their sync state reset (UIDVALIDITY 0: the next SELECT starts the
// folder from scratch), the account goes back to "initial sync not done",
// and account_rebuilds gets started_at and what to restore. It writes the
// resync event and label.counts for every label of the account.
func WipeCache(ctx context.Context, db DB, accountID int64) (*WipeResult, error) {
	res := &WipeResult{}
	var userID int64
	if err := db.QueryRow(ctx, `SELECT user_id FROM accounts WHERE id = $1`, accountID).Scan(&userID); err != nil {
		return nil, fmt.Errorf("mail: load account %d: %w", accountID, err)
	}
	restore, err := loadRestore(ctx, db, accountID)
	if err != nil {
		return nil, err
	}
	if restore == nil {
		restore = &rebuildRestore{}
	}

	// Snoozes: wake time and messages (with the folder they came from).
	rows, err := db.Query(ctx, `
SELECT s.wake_at, s.created_at, array_agg(m.dedupe_key ORDER BY m.id), array_agg(si.source_folder_id ORDER BY m.id)
FROM snoozes s JOIN snooze_items si ON si.snooze_id = s.id JOIN messages m ON m.id = si.message_id
WHERE s.account_id = $1 AND `+cacheMessage+`
GROUP BY s.id ORDER BY s.id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("mail: snapshot snoozes: %w", err)
	}
	for rows.Next() {
		var sn snoozeSnap
		var keys []string
		var srcs []*int64
		if err := rows.Scan(&sn.WakeAt, &sn.CreatedAt, &keys, &srcs); err != nil {
			rows.Close()
			return nil, err
		}
		for i, k := range keys {
			sn.Items = append(sn.Items, snoozeItem{Key: k, Source: srcs[i]})
		}
		restore.Snoozes = append(restore.Snoozes, sn)
		res.Snoozes++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// User labels that the server does not keep for the message: no keyword,
	// local_only, or local in a folder of the message (label_folder_state).
	rows, err = db.Query(ctx, `
SELECT ml.label_id, array_agg(DISTINCT m.dedupe_key)
FROM message_labels ml JOIN labels l ON l.id = ml.label_id JOIN messages m ON m.id = ml.message_id
WHERE l.account_id = $1 AND l.type = 'user' AND `+cacheMessage+`
  AND (l.imap_keyword IS NULL OR l.keyword_state <> 'mirrored'
       OR EXISTS (SELECT 1 FROM message_locations ml2 JOIN label_folder_state s ON s.folder_id = ml2.folder_id
                  WHERE ml2.message_id = m.id AND s.label_id = l.id))
GROUP BY ml.label_id ORDER BY ml.label_id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("mail: snapshot local labels: %w", err)
	}
	for rows.Next() {
		var ls labelSnap
		if err := rows.Scan(&ls.LabelID, &ls.Keys); err != nil {
			rows.Close()
			return nil, err
		}
		restore.Labels = append(restore.Labels, ls)
		res.LocalLabels += len(ls.Keys)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Drafts answering a message of the cache (in_reply_to would be cleared).
	rows, err = db.Query(ctx, `
SELECT d.message_id, m.dedupe_key FROM drafts d JOIN messages m ON m.id = d.in_reply_to_message_id
WHERE d.account_id = $1 AND `+cacheMessage, accountID)
	if err != nil {
		return nil, fmt.Errorf("mail: snapshot drafts: %w", err)
	}
	for rows.Next() {
		var ds draftSnap
		if err := rows.Scan(&ds.MessageID, &ds.ParentKey); err != nil {
			rows.Close()
			return nil, err
		}
		restore.Drafts = append(restore.Drafts, ds)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Drop the mirrored messages; their blobs lose the references (blob_gc
	// deletes them later; the backfill writes equal content to the same
	// content-addressed blob again).
	if _, err := db.Exec(ctx, `
WITH refs AS (
  SELECT m.raw_blob_sha256 AS sha FROM messages m WHERE `+cacheMessage+` AND m.raw_blob_sha256 IS NOT NULL
  UNION ALL
  SELECT a.blob_sha256 FROM attachments a JOIN messages m ON m.id = a.message_id WHERE `+cacheMessage+` AND a.blob_sha256 IS NOT NULL
), cnt AS (SELECT sha, count(*)::int AS n FROM refs GROUP BY sha)
UPDATE blobs b SET ref_count = GREATEST(b.ref_count - cnt.n, 0) FROM cnt WHERE b.sha256 = cnt.sha`, accountID); err != nil {
		return nil, fmt.Errorf("mail: release blobs: %w", err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM snoozes WHERE account_id = $1`, accountID); err != nil {
		return nil, fmt.Errorf("mail: drop snoozes: %w", err)
	}
	tag, err := db.Exec(ctx, `DELETE FROM messages m WHERE `+cacheMessage, accountID)
	if err != nil {
		return nil, fmt.Errorf("mail: drop messages: %w", err)
	}
	res.Messages = int(tag.RowsAffected())

	// Local messages stay, without their server copies (re-attached by the
	// matcher when the copies come back).
	if _, err := db.Exec(ctx, `DELETE FROM message_locations WHERE account_id = $1`, accountID); err != nil {
		return nil, fmt.Errorf("mail: drop locations: %w", err)
	}
	r, err := db.Query(ctx, `UPDATE messages SET orphaned_at = NULL WHERE account_id = $1 RETURNING id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("mail: kept messages: %w", err)
	}
	kept, err := pgx.CollectRows(r, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	res.KeptMessages = len(kept)
	tag, err = db.Exec(ctx, `
DELETE FROM threads t WHERE t.account_id = $1 AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.thread_id = t.id)`, accountID)
	if err != nil {
		return nil, fmt.Errorf("mail: drop threads: %w", err)
	}
	res.Threads = int(tag.RowsAffected())

	if _, err := db.Exec(ctx, `DELETE FROM sync_cursors c USING mailbox_folders f WHERE f.id = c.folder_id AND f.account_id = $1`, accountID); err != nil {
		return nil, fmt.Errorf("mail: drop cursors: %w", err)
	}
	if _, err := db.Exec(ctx, `
UPDATE mailbox_folders SET uidvalidity = 0, uidnext = 1, highestmodseq = NULL, messages_count = 0, last_sync_at = NULL,
  last_full_flags_at = NULL, last_error = NULL, error_count = 0
WHERE account_id = $1`, accountID); err != nil {
		return nil, fmt.Errorf("mail: reset folders: %w", err)
	}
	if _, err := db.Exec(ctx, `UPDATE accounts SET initial_sync_done_at = NULL, last_sync_at = NULL WHERE id = $1`, accountID); err != nil {
		return nil, fmt.Errorf("mail: reset account: %w", err)
	}
	raw, err := json.Marshal(restore)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(ctx, `
INSERT INTO account_rebuilds (account_id, user_id, started_at, restore) VALUES ($1, $2, now(), $3)
ON CONFLICT (account_id) DO UPDATE SET started_at = now(), restore = EXCLUDED.restore`, accountID, userID, raw); err != nil {
		return nil, fmt.Errorf("mail: record rebuild: %w", err)
	}

	ev := &eventBuf{userID: userID}
	if len(kept) > 0 {
		if err := DeriveSystemLabelsBatch(ctx, db, kept); err != nil {
			return nil, err
		}
		threads, err := threadsOfMessages(ctx, db, kept)
		if err != nil {
			return nil, err
		}
		if err := RecomputeThread(ctx, db, threads...); err != nil {
			return nil, err
		}
	}
	r, err = db.Query(ctx, `SELECT id FROM labels WHERE account_id = $1 ORDER BY id`, accountID)
	if err != nil {
		return nil, err
	}
	labels, err := pgx.CollectRows(r, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	ev.add(EventResync, map[string]any{"accountId": accountID})
	if err := ev.emitThreadEvents(ctx, db, accountID, nil, labels); err != nil {
		return nil, err
	}
	return res, ev.flush(ctx, db)
}

// RestoreAfterRebuild re-attaches what WipeCache carried over to the messages
// that are back in the cache (the caller holds the account lock). Snoozes wait
// until all their messages are back; with final set (every folder finished
// its header backfill) whatever is still missing is given up and the
// account_rebuilds row is deleted. It returns true when the restore is over.
func RestoreAfterRebuild(ctx context.Context, db DB, accountID int64, final bool) (bool, error) {
	restore, err := loadRestore(ctx, db, accountID)
	if err != nil || restore == nil {
		return restore == nil && err == nil, err
	}
	var userID int64
	if err := db.QueryRow(ctx, `SELECT user_id FROM accounts WHERE id = $1`, accountID).Scan(&userID); err != nil {
		return false, err
	}
	var keys []string
	for _, s := range restore.Snoozes {
		for _, it := range s.Items {
			keys = append(keys, it.Key)
		}
	}
	for _, l := range restore.Labels {
		keys = append(keys, l.Keys...)
	}
	for _, d := range restore.Drafts {
		keys = append(keys, d.ParentKey)
	}
	type found struct{ id, thread int64 }
	back := map[string]found{}
	if len(keys) > 0 {
		rows, err := db.Query(ctx, `SELECT dedupe_key, id, thread_id FROM messages WHERE account_id = $1 AND dedupe_key = ANY($2) AND orphaned_at IS NULL`, accountID, keys)
		if err != nil {
			return false, fmt.Errorf("mail: restore lookup: %w", err)
		}
		for rows.Next() {
			var k string
			var f found
			if err := rows.Scan(&k, &f.id, &f.thread); err != nil {
				rows.Close()
				return false, err
			}
			back[k] = f
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, err
		}
	}
	if len(back) == 0 && !final && len(keys) > 0 {
		return false, nil
	}

	var touched []int64
	before := map[int64]bool{}
	left := &rebuildRestore{}

	for _, ls := range restore.Labels {
		var ids, threads []int64
		var miss []string
		for _, k := range ls.Keys {
			if f, ok := back[k]; ok {
				ids, threads = append(ids, f.id), append(threads, f.thread)
			} else {
				miss = append(miss, k)
			}
		}
		if len(ids) > 0 {
			tag, err := db.Exec(ctx, `
INSERT INTO message_labels (message_id, label_id, user_id)
SELECT m, l.id, l.user_id FROM unnest($1::bigint[]) AS m, labels l WHERE l.id = $2 AND l.account_id = $3
ON CONFLICT DO NOTHING`, ids, ls.LabelID, accountID)
			if err != nil {
				return false, fmt.Errorf("mail: restore labels: %w", err)
			}
			if tag.RowsAffected() > 0 {
				touched = append(touched, threads...)
				before[ls.LabelID] = true
			}
		}
		if len(miss) > 0 && !final {
			left.Labels = append(left.Labels, labelSnap{LabelID: ls.LabelID, Keys: miss})
		}
	}

	wake := false
	for _, sn := range restore.Snoozes {
		var ids []int64
		var srcs []*int64
		thread := int64(0)
		for _, it := range sn.Items {
			if f, ok := back[it.Key]; ok {
				ids, srcs = append(ids, f.id), append(srcs, it.Source)
				if thread == 0 {
					thread = f.thread
				}
			}
		}
		if len(ids) < len(sn.Items) && !final {
			left.Snoozes = append(left.Snoozes, sn)
			continue
		}
		if len(ids) == 0 {
			continue
		}
		var snoozeID int64
		err := db.QueryRow(ctx, `
INSERT INTO snoozes (user_id, account_id, thread_id, wake_at, created_at) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (thread_id) DO UPDATE SET wake_at = LEAST(snoozes.wake_at, EXCLUDED.wake_at)
RETURNING id`, userID, accountID, thread, sn.WakeAt, sn.CreatedAt).Scan(&snoozeID)
		if err != nil {
			return false, fmt.Errorf("mail: restore snooze: %w", err)
		}
		if _, err := db.Exec(ctx, `
INSERT INTO snooze_items (snooze_id, message_id, source_folder_id)
SELECT $1, i.m, f.id FROM unnest($2::bigint[], $3::bigint[]) AS i(m, src)
LEFT JOIN mailbox_folders f ON f.id = i.src AND f.account_id = $4
ON CONFLICT DO NOTHING`, snoozeID, ids, srcs, accountID); err != nil {
			return false, fmt.Errorf("mail: restore snooze items: %w", err)
		}
		touched, wake = append(touched, thread), true
	}

	for _, ds := range restore.Drafts {
		f, ok := back[ds.ParentKey]
		if !ok {
			if !final {
				left.Drafts = append(left.Drafts, ds)
			}
			continue
		}
		if _, err := db.Exec(ctx, `
UPDATE drafts SET in_reply_to_message_id = $2 WHERE message_id = $1 AND account_id = $3 AND in_reply_to_message_id IS NULL`,
			ds.MessageID, f.id, accountID); err != nil {
			return false, fmt.Errorf("mail: restore draft parent: %w", err)
		}
	}

	ev := &eventBuf{userID: userID}
	if len(touched) > 0 {
		touched = uniqueSorted(touched)
		lb, err := labelsOfThreads(ctx, db, touched)
		if err != nil {
			return false, err
		}
		for id := range before {
			lb = append(lb, id)
		}
		if err := finishThreads(ctx, db, ev, accountID, touched, lb); err != nil {
			return false, err
		}
	}
	if wake {
		if err := EnqueueSnoozeWake(ctx, db); err != nil {
			return false, err
		}
	}
	done := final || len(left.Snoozes)+len(left.Labels)+len(left.Drafts) == 0
	if done {
		if _, err := db.Exec(ctx, `DELETE FROM account_rebuilds WHERE account_id = $1 AND started_at IS NOT NULL`, accountID); err != nil {
			return false, err
		}
	} else {
		raw, err := json.Marshal(left)
		if err != nil {
			return false, err
		}
		if _, err := db.Exec(ctx, `UPDATE account_rebuilds SET restore = $2 WHERE account_id = $1`, accountID, raw); err != nil {
			return false, err
		}
	}
	return done, ev.flush(ctx, db)
}
