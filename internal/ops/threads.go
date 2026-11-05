package ops

import (
	"context"
	"encoding/json"
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
	return out, rows.Err()
}
