package mail

import (
	"context"
	"fmt"
	"time"
)

// FlagUpdate is a flag change reported by the server for one UID.
type FlagUpdate struct {
	UID    uint32
	Flags  []string
	ModSeq uint64 // 0 = unknown
}

// FlagsResult is returned by ApplyFlagsBatch.
type FlagsResult struct {
	// Changed is the number of locations whose flags or modseq changed.
	Changed int
	// Unknown lists UIDs without a location in the folder; the sync engine
	// should FETCH them as new messages.
	Unknown []uint32
	// ThreadIDs are the threads recomputed.
	ThreadIDs []int64
}

// ApplyFlags records a flag change made by another client on one UID of a
// folder (incremental sync): it stores the flags and modseq, then re-derives
// the system labels and keyword labels, recomputes the thread and writes
// thread.updated / label.counts events.
func ApplyFlags(ctx context.Context, db DB, folderID int64, uid uint32, flags []string, modseq uint64) error {
	_, err := ApplyFlagsBatch(ctx, db, folderID, []FlagUpdate{{UID: uid, Flags: flags, ModSeq: modseq}})
	return err
}

// ApplyFlagsBatch is ApplyFlags for many UIDs of one folder with a constant
// number of queries; use it for CHANGEDSINCE / full flag sweeps.
func ApplyFlagsBatch(ctx context.Context, db DB, folderID int64, updates []FlagUpdate) (*FlagsResult, error) {
	res := &FlagsResult{}
	if len(updates) == 0 {
		return res, nil
	}
	var accountID, userID int64
	if err := db.QueryRow(ctx, `SELECT account_id, user_id FROM mailbox_folders WHERE id = $1`, folderID).Scan(&accountID, &userID); err != nil {
		return nil, fmt.Errorf("mail: load folder %d: %w", folderID, err)
	}
	uids := make([]int64, len(updates))
	flagJSON := make([][]string, len(updates))
	modseqs := make([]*int64, len(updates))
	for i, u := range updates {
		uids[i] = int64(u.UID)
		flagJSON[i] = normFlags(u.Flags)
		modseqs[i] = nullModseq(u.ModSeq)
	}
	// pgx cannot encode [][]string as text[][] with ragged rows; send the flag
	// sets as one comma-joined text each and split them in SQL.
	joined := make([]string, len(updates))
	for i, f := range flagJSON {
		joined[i] = joinFlags(f)
	}
	rows, err := db.Query(ctx, `
WITH incoming AS (
  SELECT u.uid, CASE WHEN u.flags = '' THEN '{}'::text[] ELSE string_to_array(u.flags, E'\x1f') END AS flags, u.modseq
  FROM unnest($2::bigint[], $3::text[], $4::bigint[]) AS u(uid, flags, modseq)
), upd AS (
  UPDATE message_locations l SET flags = i.flags, modseq = COALESCE(i.modseq, l.modseq)
  FROM incoming i
  WHERE l.folder_id = $1 AND l.uid = i.uid
    AND (l.flags IS DISTINCT FROM i.flags OR l.modseq IS DISTINCT FROM COALESCE(i.modseq, l.modseq))
  RETURNING l.message_id
)
SELECT 'u', message_id FROM upd
UNION ALL
SELECT 'x', i.uid FROM incoming i WHERE NOT EXISTS (SELECT 1 FROM message_locations l WHERE l.folder_id = $1 AND l.uid = i.uid)`,
		folderID, uids, joined, modseqs)
	if err != nil {
		return nil, fmt.Errorf("mail: apply flags: %w", err)
	}
	var msgIDs []int64
	for rows.Next() {
		var kind string
		var v int64
		if err := rows.Scan(&kind, &v); err != nil {
			rows.Close()
			return nil, err
		}
		if kind == "u" {
			msgIDs = append(msgIDs, v)
		} else {
			res.Unknown = append(res.Unknown, uint32(v))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res.Changed = len(msgIDs)
	msgIDs = uniqueSorted(msgIDs)
	if len(msgIDs) == 0 {
		return res, nil
	}
	ev := &eventBuf{userID: userID}
	res.ThreadIDs, err = refreshMessages(ctx, db, ev, accountID, msgIDs)
	if err != nil {
		return nil, err
	}
	return res, ev.flush(ctx, db)
}

func joinFlags(f []string) string {
	out := ""
	for i, s := range f {
		if i > 0 {
			out += "\x1f"
		}
		out += s
	}
	return out
}

// refreshMessages re-derives labels for the messages, recomputes their
// threads and buffers the resulting events. It returns the threads touched.
func refreshMessages(ctx context.Context, db DB, ev *eventBuf, accountID int64, msgIDs []int64) ([]int64, error) {
	threads, err := threadsOfMessages(ctx, db, msgIDs)
	if err != nil {
		return nil, err
	}
	before, err := labelsOfThreads(ctx, db, threads)
	if err != nil {
		return nil, err
	}
	if err := DeriveSystemLabelsBatch(ctx, db, msgIDs); err != nil {
		return nil, err
	}
	if err := finishThreads(ctx, db, ev, accountID, threads, before); err != nil {
		return nil, err
	}
	return uniqueSorted(threads), nil
}

// VanishedResult is returned by LocationVanished.
type VanishedResult struct {
	Removed  int // locations deleted
	Orphaned int // messages left without a location (orphaned_at set)
}

// LocationVanished handles UIDs that disappeared from a folder (EXPUNGE,
// VANISHED, STATUS/UID sweep) as in plan 3.5.6 steps 1-2: the location rows are
// deleted; a message with other locations only gets its labels re-derived, a
// message with none is marked orphaned_at = now() (it leaves the lists but
// keeps its row, user labels and thread). The STATUS sweep and the orphan_gc
// job (PurgeOrphans) are the sync engine's business.
func LocationVanished(ctx context.Context, db DB, folderID int64, uids []uint32) (*VanishedResult, error) {
	res := &VanishedResult{}
	if len(uids) == 0 {
		return res, nil
	}
	var accountID, userID int64
	if err := db.QueryRow(ctx, `SELECT account_id, user_id FROM mailbox_folders WHERE id = $1`, folderID).Scan(&accountID, &userID); err != nil {
		return nil, fmt.Errorf("mail: load folder %d: %w", folderID, err)
	}
	ids := make([]int64, len(uids))
	for i, u := range uids {
		ids[i] = int64(u)
	}
	rows, err := db.Query(ctx, `DELETE FROM message_locations WHERE folder_id = $1 AND uid = ANY($2) RETURNING message_id`, folderID, ids)
	if err != nil {
		return nil, fmt.Errorf("mail: delete locations: %w", err)
	}
	var msgIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		msgIDs = append(msgIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res.Removed = len(msgIDs)
	msgIDs = uniqueSorted(msgIDs)
	if len(msgIDs) == 0 {
		return res, nil
	}
	tag, err := db.Exec(ctx, `
UPDATE messages m SET orphaned_at = now()
WHERE m.id = ANY($1) AND m.orphaned_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM message_locations l WHERE l.message_id = m.id)`, msgIDs)
	if err != nil {
		return nil, fmt.Errorf("mail: orphan messages: %w", err)
	}
	res.Orphaned = int(tag.RowsAffected())
	ev := &eventBuf{userID: userID}
	if _, err := refreshMessages(ctx, db, ev, accountID, msgIDs); err != nil {
		return nil, err
	}
	return res, ev.flush(ctx, db)
}

// PurgeResult is returned by PurgeOrphans.
type PurgeResult struct {
	Messages       int // messages deleted
	Threads        int // empty threads deleted
	BlobsReleased  int // blob references released (raw .eml and attachments)
	RecomputedThrs int
}

// PurgeOrphans permanently deletes messages of the account that have been
// orphaned since before olderThan and still have no location, releasing the
// ref_count of their raw and attachment blobs (the blob gc deletes unreferenced
// blobs later). Threads left without messages are deleted; the others are
// recomputed. It does not check that a full sync cycle has completed since the
// message was orphaned - the orphan_gc job must verify that before calling.
func PurgeOrphans(ctx context.Context, db DB, accountID int64, olderThan time.Time) (*PurgeResult, error) {
	res := &PurgeResult{}
	var userID int64
	if err := db.QueryRow(ctx, `SELECT user_id FROM accounts WHERE id = $1`, accountID).Scan(&userID); err != nil {
		return nil, fmt.Errorf("mail: load account %d: %w", accountID, err)
	}
	rows, err := db.Query(ctx, `
SELECT m.id, m.thread_id FROM messages m
WHERE m.account_id = $1 AND m.orphaned_at IS NOT NULL AND m.orphaned_at < $2
  AND NOT EXISTS (SELECT 1 FROM message_locations l WHERE l.message_id = m.id)
ORDER BY m.id FOR UPDATE OF m`, accountID, olderThan)
	if err != nil {
		return nil, fmt.Errorf("mail: find orphans: %w", err)
	}
	var msgIDs, threadIDs []int64
	for rows.Next() {
		var id, tid int64
		if err := rows.Scan(&id, &tid); err != nil {
			rows.Close()
			return nil, err
		}
		msgIDs, threadIDs = append(msgIDs, id), append(threadIDs, tid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(msgIDs) == 0 {
		return res, nil
	}
	threadIDs = uniqueSorted(threadIDs)
	before, err := labelsOfThreads(ctx, db, threadIDs)
	if err != nil {
		return nil, err
	}
	tag, err := db.Exec(ctx, `
WITH refs AS (
  SELECT raw_blob_sha256 AS sha FROM messages WHERE id = ANY($1) AND raw_blob_sha256 IS NOT NULL
  UNION ALL
  SELECT blob_sha256 FROM attachments WHERE message_id = ANY($1) AND blob_sha256 IS NOT NULL
), cnt AS (SELECT sha, count(*)::int AS n FROM refs GROUP BY sha)
UPDATE blobs b SET ref_count = GREATEST(b.ref_count - cnt.n, 0) FROM cnt WHERE b.sha256 = cnt.sha`, msgIDs)
	if err != nil {
		return nil, fmt.Errorf("mail: release blobs: %w", err)
	}
	res.BlobsReleased = int(tag.RowsAffected())
	tag, err = db.Exec(ctx, `DELETE FROM messages WHERE id = ANY($1)`, msgIDs)
	if err != nil {
		return nil, fmt.Errorf("mail: delete orphans: %w", err)
	}
	res.Messages = int(tag.RowsAffected())

	// Threads without any message left are deleted, the rest recomputed.
	var gone []int64
	r2, err := db.Query(ctx, `
DELETE FROM threads t WHERE t.id = ANY($1) AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.thread_id = t.id)
RETURNING t.id`, threadIDs)
	if err != nil {
		return nil, fmt.Errorf("mail: delete empty threads: %w", err)
	}
	for r2.Next() {
		var id int64
		if err := r2.Scan(&id); err != nil {
			r2.Close()
			return nil, err
		}
		gone = append(gone, id)
	}
	r2.Close()
	if err := r2.Err(); err != nil {
		return nil, err
	}
	res.Threads = len(gone)
	var remain []int64
	for _, id := range threadIDs {
		found := false
		for _, g := range gone {
			found = found || g == id
		}
		if !found {
			remain = append(remain, id)
		}
	}
	res.RecomputedThrs = len(remain)
	ev := &eventBuf{userID: userID}
	if err := finishThreads(ctx, db, ev, accountID, remain, before); err != nil {
		return nil, err
	}
	for _, id := range gone {
		ev.add(EventThreadDelete, map[string]any{"threadId": id, "accountId": accountID})
	}
	return res, ev.flush(ctx, db)
}
