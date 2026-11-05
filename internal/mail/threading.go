package mail

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// fallbackWindow is the time window of the subject fallback (plan 4.6, step 5).
const fallbackWindow = 7 * 24 * time.Hour

// threadInput is what the threading algorithm needs to know about a message.
type threadInput struct {
	accountID, userID int64
	messageID         string
	refs              []string
	subject           string
	subjectNorm       string
	replyPrefix       bool
	date              time.Time
	addrs             []string // from+to+cc without the account's own address
}

// threadState carries the side effects of threading within one transaction.
type threadState struct {
	ev           *eventBuf
	touched      map[int64]bool // threads that need RecomputeThread
	labelsBefore map[int64]bool // labels of threads that were merged away
	dropped      []int64        // messages that lost a location and need re-deriving
}

func newThreadState(ev *eventBuf) *threadState {
	return &threadState{ev: ev, touched: map[int64]bool{}, labelsBefore: map[int64]bool{}}
}

type threadRow struct {
	id          int64
	subjectNorm string
	size        int
}

// placeInThread implements plan 4.6: it returns the thread the message
// belongs to, creating or merging threads as needed. The message itself is
// inserted by the caller afterwards (see refreshThreadSubject).
func placeInThread(ctx context.Context, db DB, st *threadState, in threadInput) (int64, error) {
	if _, err := db.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`,
		strconv.FormatInt(in.accountID, 10)+":"+in.subjectNorm); err != nil {
		return 0, fmt.Errorf("mail: threading lock: %w", err)
	}

	// Steps 2 and 3: threads of the parents and of the children (messages that
	// were ingested earlier and reference this message).
	rows, err := db.Query(ctx, `
SELECT thread_id FROM messages WHERE account_id = $1 AND message_id_hdr = ANY($2)
UNION
SELECT m.thread_id FROM message_references r JOIN messages m ON m.id = r.message_id
WHERE r.account_id = $1 AND r.ref_message_id_hdr = $3`, in.accountID, in.refs, in.messageID)
	if err != nil {
		return 0, fmt.Errorf("mail: find related threads: %w", err)
	}
	var candidates []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var found []threadRow
	if len(candidates) > 0 {
		r, err := db.Query(ctx, `
SELECT t.id, t.subject_norm, (SELECT count(*) FROM messages m WHERE m.thread_id = t.id)
FROM threads t WHERE t.id = ANY($1) ORDER BY t.created_at, t.id FOR UPDATE OF t`, candidates)
		if err != nil {
			return 0, fmt.Errorf("mail: lock candidate threads: %w", err)
		}
		for r.Next() {
			var tr threadRow
			if err := r.Scan(&tr.id, &tr.subjectNorm, &tr.size); err != nil {
				r.Close()
				return 0, err
			}
			found = append(found, tr)
		}
		r.Close()
		if err := r.Err(); err != nil {
			return 0, err
		}
	}

	// Threads whose subject differs are not candidates: a changed subject
	// starts a new thread, and ignoring them keeps the result independent of
	// the arrival order.
	hadCandidates := len(found) > 0
	found = slices.DeleteFunc(found, func(t threadRow) bool { return t.subjectNorm != in.subjectNorm })
	switch {
	case hadCandidates && len(found) == 0:
		// only threads with another subject: new thread (step 4, no fallback)
	case len(found) == 0:
		// Step 5: subject fallback.
		id, ok, err := subjectFallback(ctx, db, in)
		if err != nil {
			return 0, err
		}
		if ok {
			return id, nil
		}
	case len(found) == 1:
		t := found[0]
		if t.size < maxThreadMessages {
			return t.id, nil
		}
	default:
		total := 0
		for _, t := range found {
			total += t.size
		}
		if total+1 <= maxThreadMessages {
			winner := found[0]
			var losers []int64
			for _, t := range found[1:] {
				losers = append(losers, t.id)
			}
			if err := mergeThreads(ctx, db, st, in.accountID, winner.id, losers); err != nil {
				return 0, err
			}
			return winner.id, nil
		}
		// Merging would exceed the thread size limit: join the thread of the
		// nearest known ancestor if it has room.
		for i := len(in.refs) - 1; i >= 0; i-- {
			var tid int64
			var size int
			err := db.QueryRow(ctx, `
SELECT m.thread_id, (SELECT count(*) FROM messages x WHERE x.thread_id = m.thread_id)
FROM messages m WHERE m.account_id = $1 AND m.message_id_hdr = $2 ORDER BY m.id LIMIT 1`, in.accountID, in.refs[i]).Scan(&tid, &size)
			if err != nil {
				continue // ancestor unknown (pgx.ErrNoRows)
			}
			if size < maxThreadMessages {
				return tid, nil
			}
			break
		}
	}

	// Step 6: a new thread.
	var id int64
	err = db.QueryRow(ctx, `
INSERT INTO threads (user_id, account_id, subject, subject_norm, last_message_at, sort_at)
VALUES ($1, $2, $3, $4, $5, $5) RETURNING id`,
		in.userID, in.accountID, in.subject, in.subjectNorm, in.date).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("mail: create thread: %w", err)
	}
	return id, nil
}

// subjectFallback implements step 5: same normalised subject (not a generic
// one), a message within 7 days sharing a participant, and a reply/forward
// prefix on the new message or on a message already in the thread (so the
// result does not depend on arrival order). Identical subjects without any
// prefix (periodic reports) never join.
func subjectFallback(ctx context.Context, db DB, in threadInput) (int64, bool, error) {
	if IsGenericSubject(in.subjectNorm) || len(in.addrs) == 0 {
		return 0, false, nil
	}
	var id int64
	rows, err := db.Query(ctx, `
SELECT t.id
FROM threads t
JOIN messages m ON m.thread_id = t.id
WHERE t.account_id = $1 AND t.subject_norm = $2
  AND m.orphaned_at IS NULL
  AND m.internal_date BETWEEN $3 AND $4
  AND (m.from_addr::text = ANY($5) OR EXISTS (
        SELECT 1 FROM jsonb_array_elements(m.to_addrs || m.cc_addrs) a WHERE a->>'email' = ANY($5)))
  AND ($6 OR EXISTS (SELECT 1 FROM messages p WHERE p.thread_id = t.id AND p.reply_prefix))
  AND (SELECT count(*) FROM messages c WHERE c.thread_id = t.id) < $7
GROUP BY t.id
ORDER BY max(m.internal_date) DESC, t.id
LIMIT 1`,
		in.accountID, in.subjectNorm, in.date.Add(-fallbackWindow), in.date.Add(fallbackWindow),
		in.addrs, in.replyPrefix, maxThreadMessages)
	if err != nil {
		return 0, false, fmt.Errorf("mail: subject fallback: %w", err)
	}
	hit := false
	if rows.Next() {
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, false, err
		}
		hit = true
	}
	rows.Close()
	if err := rows.Err(); err != nil || !hit {
		return 0, false, err
	}
	if _, err := db.Exec(ctx, `SELECT 1 FROM threads WHERE id = $1 FOR UPDATE`, id); err != nil {
		return 0, false, fmt.Errorf("mail: lock fallback thread: %w", err)
	}
	return id, true, nil
}

// mergeThreads folds the losers into the winner (the oldest thread): messages
// are re-pointed, losers' thread_labels dropped, snoozes combined (earliest
// wake_at), empty threads deleted, thread.merged emitted.
func mergeThreads(ctx context.Context, db DB, st *threadState, accountID, winner int64, losers []int64) error {
	all := append([]int64{winner}, losers...)
	before, err := labelsOfThreads(ctx, db, all)
	if err != nil {
		return err
	}
	for _, l := range before {
		st.labelsBefore[l] = true
	}
	if _, err := db.Exec(ctx, `UPDATE messages SET thread_id = $1 WHERE thread_id = ANY($2)`, winner, losers); err != nil {
		return fmt.Errorf("mail: merge move messages: %w", err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM thread_labels WHERE thread_id = ANY($1)`, losers); err != nil {
		return fmt.Errorf("mail: merge drop thread labels: %w", err)
	}
	if err := mergeSnoozes(ctx, db, all, winner); err != nil {
		return err
	}
	if _, err := db.Exec(ctx, `DELETE FROM threads WHERE id = ANY($1)`, losers); err != nil {
		return fmt.Errorf("mail: merge delete threads: %w", err)
	}
	for _, l := range losers {
		delete(st.touched, l)
	}
	st.touched[winner] = true
	st.ev.add(EventThreadMerged, map[string]any{"accountId": accountID, "fromIds": losers, "intoId": winner})
	return nil
}

// mergeSnoozes keeps the snooze with the earliest wake_at (moving it to the
// winner thread) and folds the items of the others into it.
func mergeSnoozes(ctx context.Context, db DB, threads []int64, winner int64) error {
	rows, err := db.Query(ctx, `SELECT id FROM snoozes WHERE thread_id = ANY($1) ORDER BY wake_at, id`, threads)
	if err != nil {
		return fmt.Errorf("mail: merge load snoozes: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return err
	}
	keeper, others := ids[0], ids[1:]
	if len(others) > 0 {
		if _, err := db.Exec(ctx, `
INSERT INTO snooze_items (snooze_id, message_id, source_folder_id)
SELECT $1, message_id, source_folder_id FROM snooze_items WHERE snooze_id = ANY($2)
ON CONFLICT DO NOTHING`, keeper, others); err != nil {
			return fmt.Errorf("mail: merge snooze items: %w", err)
		}
		if _, err := db.Exec(ctx, `DELETE FROM snoozes WHERE id = ANY($1)`, others); err != nil {
			return fmt.Errorf("mail: merge delete snoozes: %w", err)
		}
	}
	if _, err := db.Exec(ctx, `UPDATE snoozes SET thread_id = $1 WHERE id = $2`, winner, keeper); err != nil {
		return fmt.Errorf("mail: merge move snooze: %w", err)
	}
	return nil
}

// refreshThreadSubject sets threads.subject/subject_norm from the earliest
// visible message so the thread key does not depend on arrival order.
func refreshThreadSubject(ctx context.Context, db DB, threadID int64) error {
	_, err := db.Exec(ctx, `
UPDATE threads t SET subject = s.subject, subject_norm = s.subject_norm
FROM (SELECT subject, subject_norm FROM messages
      WHERE thread_id = $1 AND orphaned_at IS NULL ORDER BY internal_date, id LIMIT 1) s
WHERE t.id = $1 AND (t.subject, t.subject_norm) IS DISTINCT FROM (s.subject, s.subject_norm)`, threadID)
	if err != nil {
		return fmt.Errorf("mail: refresh thread subject: %w", err)
	}
	return nil
}

func participantAddrs(own string, lists ...[]Address) []string {
	var out []string
	for _, l := range lists {
		for _, a := range l {
			if a.Email != "" && a.Email != own && !slices.Contains(out, a.Email) {
				out = append(out, a.Email)
			}
		}
	}
	return out
}
