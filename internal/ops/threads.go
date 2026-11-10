package ops

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/silopost/silo-server/internal/mail"
)

// Participant of a thread (threads.participants).
type Participant struct {
	Email  string `json:"email"`
	Name   string `json:"name"`
	Unread bool   `json:"unread"`
}

// Thread is the list shape of a thread (plan 5.2) with database ids; the API
// formats ids and label ids. Labels are the system keys and the ids of the
// user/folder labels of the thread; UNREAD is added when it has unread mail.
type Thread struct {
	ID             int64
	AccountID      int64
	Subject        string
	Snippet        string
	LastMessageAt  time.Time
	MessageCount   int
	UnreadCount    int
	HasAttachments bool
	SystemLabels   []string
	LabelIDs       []int64
	Participants   []Participant
	SnoozedUntil   *time.Time
	PendingSync    bool
	// DraftID is the newest message of the thread with the DRAFT label (0: none);
	// its d_ id opens it in compose. DraftCount counts the messages of the
	// thread with that label (0: none).
	DraftID    int64
	DraftCount int
	// ScheduledAt and OutboundID: the send of the thread waiting for its time
	// (outbound queued or sending with send_at ahead; a scheduled send or the
	// undo-send window), the earliest one; nil and 0 without one.
	ScheduledAt *time.Time
	OutboundID  int64
}

// LoadThreads returns the threads of the user, in id order.
func LoadThreads(ctx context.Context, db mail.DB, userID int64, ids []int64) ([]Thread, error) {
	out := []Thread{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.Query(ctx, `
SELECT t.id, t.account_id, t.subject, t.snippet, t.last_message_at, t.message_count, t.unread_count, t.has_attachments,
       t.participants, t.snoozed_until, t.pending_ops > 0,
       COALESCE(array_agg(l.system_key ORDER BY l.position, l.id) FILTER (WHERE l.system_key IS NOT NULL), '{}'),
       COALESCE(array_agg(l.id ORDER BY l.id) FILTER (WHERE l.id IS NOT NULL AND l.system_key IS NULL), '{}')
FROM threads t
LEFT JOIN thread_labels tl ON tl.thread_id = t.id
LEFT JOIN labels l ON l.id = tl.label_id
WHERE t.id = ANY($1) AND t.user_id = $2
GROUP BY t.id ORDER BY t.id`, ids, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t Thread
		var parts []byte
		if err := rows.Scan(&t.ID, &t.AccountID, &t.Subject, &t.Snippet, &t.LastMessageAt, &t.MessageCount, &t.UnreadCount,
			&t.HasAttachments, &parts, &t.SnoozedUntil, &t.PendingSync, &t.SystemLabels, &t.LabelIDs); err != nil {
			return nil, err
		}
		if t.UnreadCount > 0 {
			t.SystemLabels = append(t.SystemLabels, LabelUnread)
		}
		t.Participants = []Participant{}
		_ = json.Unmarshal(parts, &t.Participants)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return out, FillThreadExtras(ctx, db, out)
}

// FillThreadExtras sets DraftID, ScheduledAt and OutboundID of the threads.
func FillThreadExtras(ctx context.Context, db mail.DB, ts []Thread) error {
	if err := FillDraftIDs(ctx, db, ts); err != nil {
		return err
	}
	return fillScheduled(ctx, db, ts)
}

// FillDraftIDs sets DraftID and DraftCount of the threads that have the DRAFT
// label: the newest message of the thread with that label and the number of
// such messages (one query for all).
func FillDraftIDs(ctx context.Context, db mail.DB, ts []Thread) error {
	idx := map[int64]int{}
	var ids []int64
	for i, t := range ts {
		if slices.Contains(t.SystemLabels, "DRAFT") {
			idx[t.ID] = i
			ids = append(ids, t.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := db.Query(ctx, `
SELECT m.thread_id, (array_agg(m.id ORDER BY m.internal_date DESC, m.id DESC))[1], count(DISTINCT m.id) FROM messages m
JOIN message_labels ml ON ml.message_id = m.id
JOIN labels l ON l.id = ml.label_id AND l.system_key = 'DRAFT'
WHERE m.thread_id = ANY($1) AND m.orphaned_at IS NULL
GROUP BY m.thread_id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var tid, mid int64
		var n int
		if err := rows.Scan(&tid, &mid, &n); err != nil {
			return err
		}
		ts[idx[tid]].DraftID, ts[idx[tid]].DraftCount = mid, n
	}
	return rows.Err()
}

// fillScheduled sets ScheduledAt and OutboundID of the threads with the
// SCHEDULED label (one query for all), for the "Scheduled" list.
func fillScheduled(ctx context.Context, db mail.DB, ts []Thread) error {
	idx := map[int64]int{}
	var ids []int64
	for i, t := range ts {
		if slices.Contains(t.SystemLabels, mail.LabelScheduled) {
			idx[t.ID] = i
			ids = append(ids, t.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := db.Query(ctx, `
SELECT DISTINCT ON (m.thread_id) m.thread_id, o.id, o.send_at FROM outbound o
JOIN messages m ON m.id = o.message_id
WHERE m.thread_id = ANY($1) AND o.status IN ('queued', 'sending') AND o.send_at > now()
ORDER BY m.thread_id, o.send_at, o.id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var tid, oid int64
		var at time.Time
		if err := rows.Scan(&tid, &oid, &at); err != nil {
			return err
		}
		ts[idx[tid]].ScheduledAt, ts[idx[tid]].OutboundID = &at, oid
	}
	return rows.Err()
}
