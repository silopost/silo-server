package mail

import (
	"context"
	"fmt"
)

// Discrepancy kinds returned by Reconcile.
const (
	DiscThreadAggregate = "thread_aggregate" // threads.* differs from messages
	DiscThreadLabel     = "thread_label"     // thread_labels differs from message_labels
	DiscMessageLabel    = "message_label"    // system/folder/keyword label differs from message_locations
	DiscMessageUnread   = "message_unread"   // is_unread differs from the \Seen flags
)

// Discrepancy is one difference found by Reconcile.
type Discrepancy struct {
	Kind      string
	ThreadID  int64 // 0 if not applicable
	MessageID int64 // 0 if not applicable
	LabelID   int64 // 0 if not applicable
	Detail    string
}

func (d Discrepancy) String() string {
	return fmt.Sprintf("%s thread=%d message=%d label=%d: %s", d.Kind, d.ThreadID, d.MessageID, d.LabelID, d.Detail)
}

// Reconcile compares the derived data of an account with its sources and
// returns every difference; it changes nothing. It checks that
//
//   - threads.message_count/unread_count/has_attachments match the visible
//     (non-orphaned) messages,
//   - thread_labels equals the aggregate of message_labels (counts, unread,
//     hidden),
//   - the system, folder and keyword labels and is_unread of every server
//     message match what DeriveSystemLabels would compute from
//     message_locations (local messages, body_state = 'local', are skipped).
//
// A difference in the last group is a bug in the derivation; the first two
// mean RecomputeThread was missed. `silo-server reconcile` and the weekly job
// are thin wrappers around this function.
func Reconcile(ctx context.Context, db DB, accountID int64) ([]Discrepancy, error) {
	var out []Discrepancy

	// Thread ids of the account, processed in chunks.
	rows, err := db.Query(ctx, `SELECT id FROM threads WHERE account_id = $1 ORDER BY id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("mail: reconcile list threads: %w", err)
	}
	var threadIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		threadIDs = append(threadIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for start := 0; start < len(threadIDs); start += 2000 {
		chunk := threadIDs[start:min(start+2000, len(threadIDs))]
		ds, err := reconcileThreads(ctx, db, chunk)
		if err != nil {
			return nil, err
		}
		out = append(out, ds...)
	}

	mrows, err := db.Query(ctx, `SELECT id FROM messages WHERE account_id = $1 AND body_state <> 'local' ORDER BY id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("mail: reconcile list messages: %w", err)
	}
	var msgIDs []int64
	for mrows.Next() {
		var id int64
		if err := mrows.Scan(&id); err != nil {
			mrows.Close()
			return nil, err
		}
		msgIDs = append(msgIDs, id)
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return nil, err
	}
	for start := 0; start < len(msgIDs); start += 1000 {
		chunk := msgIDs[start:min(start+1000, len(msgIDs))]
		plan, err := deriveLabels(ctx, db, chunk, false)
		if err != nil {
			return nil, err
		}
		for i := range plan.addMsg {
			out = append(out, Discrepancy{Kind: DiscMessageLabel, MessageID: plan.addMsg[i], LabelID: plan.addLabel[i], Detail: "label missing"})
		}
		for i := range plan.delMsg {
			out = append(out, Discrepancy{Kind: DiscMessageLabel, MessageID: plan.delMsg[i], LabelID: plan.delLabel[i], Detail: "label should not be set"})
		}
		for i, id := range plan.unreadMsg {
			out = append(out, Discrepancy{Kind: DiscMessageUnread, MessageID: id, Detail: fmt.Sprintf("is_unread should be %v", plan.unreadVal[i])})
		}
	}
	return out, nil
}

func reconcileThreads(ctx context.Context, db DB, threadIDs []int64) ([]Discrepancy, error) {
	var out []Discrepancy
	rows, err := db.Query(ctx, `
SELECT t.id, t.message_count, COALESCE(a.cnt, 0), t.unread_count, COALESCE(a.unread, 0),
       t.has_attachments, COALESCE(a.att, false)
FROM threads t
LEFT JOIN (SELECT thread_id, count(*)::int AS cnt, (count(*) FILTER (WHERE is_unread))::int AS unread,
                  bool_or(has_attachments) AS att
           FROM messages WHERE thread_id = ANY($1) AND orphaned_at IS NULL GROUP BY thread_id) a ON a.thread_id = t.id
WHERE t.id = ANY($1)
  AND (t.message_count <> COALESCE(a.cnt, 0) OR t.unread_count <> COALESCE(a.unread, 0)
       OR t.has_attachments <> COALESCE(a.att, false))`, threadIDs)
	if err != nil {
		return nil, fmt.Errorf("mail: reconcile threads: %w", err)
	}
	for rows.Next() {
		var id int64
		var c1, c2, u1, u2 int
		var a1, a2 bool
		if err := rows.Scan(&id, &c1, &c2, &u1, &u2, &a1, &a2); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, Discrepancy{Kind: DiscThreadAggregate, ThreadID: id,
			Detail: fmt.Sprintf("message_count %d/%d unread_count %d/%d has_attachments %v/%v (stored/expected)", c1, c2, u1, u2, a1, a2)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.Query(ctx, "WITH "+threadLabelAgg+`
SELECT COALESCE(a.thread_id, tl.thread_id), COALESCE(a.label_id, tl.label_id),
       COALESCE(tl.message_count, -1), COALESCE(a.cnt, -1),
       COALESCE(tl.unread_count, -1), COALESCE(a.unread, -1),
       tl.hidden, a.hidden
FROM agg a
FULL JOIN (SELECT * FROM thread_labels WHERE thread_id = ANY($1)) tl
  ON tl.thread_id = a.thread_id AND tl.label_id = a.label_id
WHERE a.thread_id IS NULL OR tl.thread_id IS NULL
   OR tl.message_count <> a.cnt OR tl.unread_count <> a.unread OR tl.hidden <> a.hidden`, threadIDs)
	if err != nil {
		return nil, fmt.Errorf("mail: reconcile thread labels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tid, lid int64
		var c1, c2, u1, u2 int
		var h1, h2 *bool
		if err := rows.Scan(&tid, &lid, &c1, &c2, &u1, &u2, &h1, &h2); err != nil {
			return nil, err
		}
		out = append(out, Discrepancy{Kind: DiscThreadLabel, ThreadID: tid, LabelID: lid,
			Detail: fmt.Sprintf("message_count %d/%d unread_count %d/%d (stored/expected, -1 = row missing)", c1, c2, u1, u2)})
	}
	return out, rows.Err()
}
