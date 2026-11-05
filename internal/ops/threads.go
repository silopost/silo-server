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
	// its d_ id opens it in compose.
	DraftID int64
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
	return out, FillDraftIDs(ctx, db, out)
}

// FillDraftIDs sets DraftID of the threads that have the DRAFT label: the
// newest message of the thread with that label (one query for all).
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
SELECT DISTINCT ON (m.thread_id) m.thread_id, m.id FROM messages m
JOIN message_labels ml ON ml.message_id = m.id
JOIN labels l ON l.id = ml.label_id AND l.system_key = 'DRAFT'
WHERE m.thread_id = ANY($1) AND m.orphaned_at IS NULL
ORDER BY m.thread_id, m.internal_date DESC, m.id DESC`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var tid, mid int64
		if err := rows.Scan(&tid, &mid); err != nil {
			return err
		}
		ts[idx[tid]].DraftID = mid
	}
	return rows.Err()
}
