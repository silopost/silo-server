package mailview

import (
	"context"
	"encoding/json"
	"time"

	"github.com/silopost/silo-server/internal/ops"
)

// Address is a mailbox of a message header.
type Address struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// Attachment of a message.
type Attachment struct {
	ID          int64
	Filename    string
	ContentType string
	Size        int64
	Inline      bool
	Downloaded  bool
}

// Folder is where a message is located on the server.
type Folder struct {
	ID   int64
	Name string
	Role string
}

// Message is one message of a thread (plan 5.2) with database ids.
type Message struct {
	ID              int64
	From            Address
	To, Cc, Bcc     []Address
	ReplyTo         *Address
	Date            time.Time
	Subject         string
	Snippet         string
	SystemLabels    []string
	LabelIDs        []int64
	Folder          *Folder
	BodyState       string
	HasHTML         bool
	BodyText        string
	Attachments     []Attachment
	ListUnsubscribe *string
	PendingSync     bool
}

// ThreadDetail is a thread with its messages in date order.
type ThreadDetail struct {
	Thread   ops.Thread
	Messages []Message
}

// GetThread returns a thread of the user; orphaned messages (no location on
// the server) are left out.
func (v *View) GetThread(ctx context.Context, userID, threadID int64) (*ThreadDetail, error) {
	ts, err := ops.LoadThreads(ctx, v.pool, userID, []int64{threadID})
	if err != nil {
		return nil, err
	}
	if len(ts) == 0 {
		return nil, ErrNotFound
	}
	d := &ThreadDetail{Thread: ts[0], Messages: []Message{}}
	keys, err := v.labelKeys(ctx, v.pool, d.Thread.AccountID)
	if err != nil {
		return nil, err
	}
	rows, err := v.pool.Query(ctx, `
SELECT m.id, m.from_addr::text, m.from_name, m.to_addrs, m.cc_addrs, m.bcc_addrs, m.reply_to,
       COALESCE(m.header_date, m.internal_date), m.subject, m.snippet, m.body_state, m.has_html, m.body_text,
       m.list_unsubscribe, m.pending_ops > 0, m.is_unread,
       f.id, f.name, f.role
FROM messages m
LEFT JOIN LATERAL (
  SELECT f.id, f.name, f.role FROM message_locations l JOIN mailbox_folders f ON f.id = l.folder_id
  WHERE l.message_id = m.id AND NOT l.stale AND NOT (l.flags @> ARRAY['\Deleted'])
  ORDER BY l.folder_id LIMIT 1) f ON true
WHERE m.thread_id = $1 AND m.user_id = $2 AND m.orphaned_at IS NULL
ORDER BY m.internal_date, m.id`, threadID, userID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	byID := map[int64]int{}
	unread := map[int64]bool{}
	for rows.Next() {
		var m Message
		var to, cc, bcc, reply []byte
		var isUnread bool
		var fid *int64
		var fname, frole *string
		if err := rows.Scan(&m.ID, &m.From.Email, &m.From.Name, &to, &cc, &bcc, &reply, &m.Date, &m.Subject, &m.Snippet,
			&m.BodyState, &m.HasHTML, &m.BodyText, &m.ListUnsubscribe, &m.PendingSync, &isUnread, &fid, &fname, &frole); err != nil {
			rows.Close()
			return nil, err
		}
		m.To, m.Cc, m.Bcc = addrs(to), addrs(cc), addrs(bcc)
		if r := addrs(reply); len(r) > 0 {
			m.ReplyTo = &r[0]
		}
		if fid != nil {
			m.Folder = &Folder{ID: *fid, Name: *fname, Role: *frole}
		}
		m.SystemLabels, m.LabelIDs, m.Attachments = []string{}, []int64{}, []Attachment{}
		unread[m.ID] = isUnread
		byID[m.ID] = len(d.Messages)
		ids = append(ids, m.ID)
		d.Messages = append(d.Messages, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return d, nil
	}
	lr, err := v.pool.Query(ctx, `SELECT message_id, label_id FROM message_labels WHERE message_id = ANY($1) ORDER BY message_id, label_id`, ids)
	if err != nil {
		return nil, err
	}
	for lr.Next() {
		var mid, lid int64
		if err := lr.Scan(&mid, &lid); err != nil {
			lr.Close()
			return nil, err
		}
		m := &d.Messages[byID[mid]]
		if key := keys[lid]; key != "" {
			m.SystemLabels = append(m.SystemLabels, key)
		} else {
			m.LabelIDs = append(m.LabelIDs, lid)
		}
	}
	lr.Close()
	if err := lr.Err(); err != nil {
		return nil, err
	}
	for id, u := range unread {
		if u {
			m := &d.Messages[byID[id]]
			m.SystemLabels = append(m.SystemLabels, ops.LabelUnread)
		}
	}
	ar, err := v.pool.Query(ctx, `
SELECT message_id, id, filename, content_type, size_bytes, is_inline, blob_sha256 IS NOT NULL
FROM attachments WHERE message_id = ANY($1) ORDER BY message_id, id`, ids)
	if err != nil {
		return nil, err
	}
	defer ar.Close()
	for ar.Next() {
		var mid int64
		var a Attachment
		if err := ar.Scan(&mid, &a.ID, &a.Filename, &a.ContentType, &a.Size, &a.Inline, &a.Downloaded); err != nil {
			return nil, err
		}
		m := &d.Messages[byID[mid]]
		m.Attachments = append(m.Attachments, a)
	}
	return d, ar.Err()
}

func addrs(raw []byte) []Address {
	out := []Address{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}
