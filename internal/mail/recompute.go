package mail

import (
	"context"
	"fmt"
	"slices"
)

// RecomputeThread rebuilds the denormalised columns of the given threads and
// their thread_labels rows from messages and message_labels (plan 4.5). Every
// thread is recomputed once however many messages changed, so batch callers
// collect the thread ids and call it once at the end of the transaction.
//
// Messages with orphaned_at set are ignored. A thread without any visible
// message keeps its row (message_count = 0, no thread_labels) until
// PurgeOrphans deletes it. Two statements: the aggregate UPDATE of threads and
// the upsert+delete of thread_labels.
func RecomputeThread(ctx context.Context, db DB, threadIDs ...int64) error {
	if len(threadIDs) == 0 {
		return nil
	}
	_, err := db.Exec(ctx, `
WITH per_sender AS (
  SELECT m.thread_id, m.from_addr::text AS email,
         (array_agg(m.from_name ORDER BY m.internal_date DESC, m.id DESC) FILTER (WHERE m.from_name <> ''))[1] AS name,
         bool_or(m.is_unread) AS unread,
         min(m.internal_date) AS first_at
  FROM messages m
  WHERE m.thread_id = ANY($1) AND m.orphaned_at IS NULL AND m.from_addr <> ''
  GROUP BY m.thread_id, m.from_addr
), parts AS (
  SELECT thread_id,
         jsonb_agg(jsonb_build_object('email', email, 'name', COALESCE(name, ''), 'unread', unread) ORDER BY first_at, email) AS p
  FROM per_sender GROUP BY thread_id
), agg AS (
  SELECT m.thread_id,
         count(*)::int AS cnt,
         (count(*) FILTER (WHERE m.is_unread))::int AS unread,
         bool_or(m.has_attachments) AS att,
         max(m.internal_date) AS last_at,
         COALESCE(sum(m.pending_ops), 0)::int AS pend,
         (array_agg(m.snippet ORDER BY m.internal_date DESC, m.id DESC))[1] AS snippet,
         (array_agg(m.subject ORDER BY m.internal_date, m.id))[1] AS subject,
         (array_agg(m.subject_norm ORDER BY m.internal_date, m.id))[1] AS subject_norm
  FROM messages m
  WHERE m.thread_id = ANY($1) AND m.orphaned_at IS NULL
  GROUP BY m.thread_id
)
UPDATE threads t SET
  message_count   = COALESCE(a.cnt, 0),
  unread_count    = COALESCE(a.unread, 0),
  has_attachments = COALESCE(a.att, false),
  pending_ops     = COALESCE(a.pend, 0),
  snippet         = COALESCE(a.snippet, ''),
  subject         = COALESCE(a.subject, t.subject),
  subject_norm    = COALESCE(a.subject_norm, t.subject_norm),
  participants    = COALESCE(p.p, '[]'::jsonb),
  last_message_at = COALESCE(a.last_at, t.last_message_at),
  sort_at         = CASE WHEN t.sort_at > t.last_message_at
                         THEN GREATEST(t.sort_at, COALESCE(a.last_at, t.last_message_at))
                         ELSE COALESCE(a.last_at, t.last_message_at) END,
  snoozed_until   = (SELECT s.wake_at FROM snoozes s WHERE s.thread_id = t.id)
FROM (SELECT unnest($1::bigint[]) AS id) ids
LEFT JOIN agg a ON a.thread_id = ids.id
LEFT JOIN parts p ON p.thread_id = ids.id
WHERE t.id = ids.id`, threadIDs)
	if err != nil {
		return fmt.Errorf("mail: recompute threads: %w", err)
	}

	_, err = db.Exec(ctx, "WITH "+threadLabelAgg+`,
up AS (
  INSERT INTO thread_labels (thread_id, label_id, user_id, message_count, unread_count, sort_at, hidden)
  SELECT a.thread_id, a.label_id, a.user_id, a.cnt, a.unread,
         CASE WHEN t.sort_at > t.last_message_at THEN GREATEST(a.mx, t.sort_at) ELSE a.mx END, a.hidden
  FROM agg a JOIN threads t ON t.id = a.thread_id
  ON CONFLICT (thread_id, label_id) DO UPDATE SET
    message_count = EXCLUDED.message_count, unread_count = EXCLUDED.unread_count,
    sort_at = EXCLUDED.sort_at, hidden = EXCLUDED.hidden
  WHERE (thread_labels.message_count, thread_labels.unread_count, thread_labels.sort_at, thread_labels.hidden)
        IS DISTINCT FROM (EXCLUDED.message_count, EXCLUDED.unread_count, EXCLUDED.sort_at, EXCLUDED.hidden)
  RETURNING 1
)
DELETE FROM thread_labels tl
WHERE tl.thread_id = ANY($1)
  AND NOT EXISTS (SELECT 1 FROM agg a WHERE a.thread_id = tl.thread_id AND a.label_id = tl.label_id)`, threadIDs)
	if err != nil {
		return fmt.Errorf("mail: recompute thread labels: %w", err)
	}
	return nil
}

// labelsOfThreads returns the distinct label ids currently in thread_labels
// for the given threads.
func labelsOfThreads(ctx context.Context, db DB, threadIDs []int64) ([]int64, error) {
	rows, err := db.Query(ctx, `SELECT DISTINCT label_id FROM thread_labels WHERE thread_id = ANY($1)`, threadIDs)
	if err != nil {
		return nil, fmt.Errorf("mail: load thread labels: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// finishThreads is the common tail of every mutation: recompute the threads
// and write thread.updated / label.counts for them. labelsBefore must be taken
// (by labelsOfThreads) before the message labels changed.
func finishThreads(ctx context.Context, db DB, ev *eventBuf, accountID int64, threadIDs, labelsBefore []int64) error {
	threadIDs = uniqueSorted(threadIDs)
	if len(threadIDs) == 0 {
		if len(labelsBefore) == 0 {
			return nil
		}
		return ev.emitThreadEvents(ctx, db, accountID, nil, uniqueSorted(labelsBefore))
	}
	if err := RecomputeThread(ctx, db, threadIDs...); err != nil {
		return err
	}
	after, err := labelsOfThreads(ctx, db, threadIDs)
	if err != nil {
		return err
	}
	labels := uniqueSorted(append(slices.Clone(labelsBefore), after...))
	return ev.emitThreadEvents(ctx, db, accountID, threadIDs, labels)
}

func uniqueSorted(ids []int64) []int64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// threadLabelAgg is the common table expressions that compute the expected
// thread_labels rows of the threads in $1 from messages and message_labels.
// RecomputeThread writes them, Reconcile compares against them.
const threadLabelAgg = `per_msg AS (
  SELECT m.id, m.thread_id, m.is_unread, m.internal_date, m.user_id,
         EXISTS (SELECT 1 FROM message_labels x JOIN labels lx ON lx.id = x.label_id
                 WHERE x.message_id = m.id AND lx.system_key IN ('TRASH', 'SPAM')) AS binned
  FROM messages m
  WHERE m.thread_id = ANY($1) AND m.orphaned_at IS NULL
), binned AS (
  SELECT thread_id, bool_and(binned) AS all_binned FROM per_msg GROUP BY thread_id
), agg AS (
  SELECT pm.thread_id, ml.label_id, pm.user_id,
         count(*)::int AS cnt,
         (count(*) FILTER (WHERE pm.is_unread))::int AS unread,
         max(pm.internal_date) AS mx,
         (b.all_binned AND l.system_key IS DISTINCT FROM 'TRASH' AND l.system_key IS DISTINCT FROM 'SPAM') AS hidden
  FROM per_msg pm
  JOIN message_labels ml ON ml.message_id = pm.id
  JOIN labels l ON l.id = ml.label_id
  JOIN binned b ON b.thread_id = pm.thread_id
  GROUP BY pm.thread_id, ml.label_id, pm.user_id, b.all_binned, l.system_key
)`
