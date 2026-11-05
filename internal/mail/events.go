package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Event types written to the events table (plan 5.4). Payloads carry numeric
// database ids; the SSE hub translates them to API ids when publishing.
const (
	EventMessageNew   = "message.new"
	EventThreadUpdate = "thread.updated"
	EventThreadDelete = "thread.deleted"
	EventThreadMerged = "thread.merged"
	EventLabelCounts  = "label.counts"
)

type eventBuf struct {
	userID   int64
	origin   string // X-Silo-Client of the request that caused the events
	types    []string
	payloads []string
}

func (b *eventBuf) add(typ string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil { // payloads are plain maps/structs; cannot fail
		raw = []byte("{}")
	}
	if b.origin != "" {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			m["origin"] = b.origin
			raw, _ = json.Marshal(m)
		}
	}
	b.types = append(b.types, typ)
	b.payloads = append(b.payloads, string(raw))
}

// flush inserts all buffered events, in order, with one statement.
func (b *eventBuf) flush(ctx context.Context, db DB) error {
	if len(b.types) == 0 {
		return nil
	}
	_, err := db.Exec(ctx, `
INSERT INTO events (user_id, type, payload)
SELECT $1, t.type, t.payload::jsonb
FROM unnest($2::text[], $3::text[]) WITH ORDINALITY AS t(type, payload, n)
ORDER BY t.n`, b.userID, b.types, b.payloads)
	if err != nil {
		return fmt.Errorf("mail: write events: %w", err)
	}
	b.types, b.payloads = nil, nil
	return nil
}

type threadUpdatedPayload struct {
	ThreadID      int64     `json:"threadId"`
	AccountID     int64     `json:"accountId"`
	LabelIDs      []int64   `json:"labelIds"`
	UnreadCount   int       `json:"unreadCount"`
	MessageCount  int       `json:"messageCount"`
	LastMessageAt time.Time `json:"lastMessageAt"`
	Snippet       string    `json:"snippet"`
	PendingSync   bool      `json:"pendingSync"`
}

type labelCount struct {
	ID            int64 `json:"id"`
	ThreadsUnread int   `json:"threadsUnread"`
	ThreadsTotal  int   `json:"threadsTotal"`
}

// emitThreadEvents adds thread.updated for every given thread that still has
// messages (thread.deleted for threads that no longer exist) and one
// label.counts event for the labels in labelIDs.
func (b *eventBuf) emitThreadEvents(ctx context.Context, db DB, accountID int64, threadIDs, labelIDs []int64) error {
	if len(threadIDs) > 0 {
		rows, err := db.Query(ctx, `
SELECT t.id, t.unread_count, t.message_count, t.last_message_at, t.snippet, t.pending_ops > 0,
       COALESCE((SELECT array_agg(tl.label_id ORDER BY tl.label_id) FROM thread_labels tl
                 WHERE tl.thread_id = t.id AND NOT tl.hidden), '{}')
FROM threads t WHERE t.id = ANY($1) ORDER BY t.id`, threadIDs)
		if err != nil {
			return fmt.Errorf("mail: load thread events: %w", err)
		}
		found := map[int64]bool{}
		for rows.Next() {
			p := threadUpdatedPayload{AccountID: accountID}
			if err := rows.Scan(&p.ThreadID, &p.UnreadCount, &p.MessageCount, &p.LastMessageAt, &p.Snippet, &p.PendingSync, &p.LabelIDs); err != nil {
				rows.Close()
				return err
			}
			found[p.ThreadID] = true
			b.add(EventThreadUpdate, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range threadIDs {
			if !found[id] {
				b.add(EventThreadDelete, map[string]any{"threadId": id, "accountId": accountID})
			}
		}
	}
	if len(labelIDs) == 0 {
		return nil
	}
	rows, err := db.Query(ctx, `
SELECT l.id,
       count(*) FILTER (WHERE tl.unread_count > 0 AND NOT tl.hidden),
       count(*) FILTER (WHERE NOT tl.hidden)
FROM labels l LEFT JOIN thread_labels tl ON tl.label_id = l.id
WHERE l.id = ANY($1) GROUP BY l.id ORDER BY l.id`, labelIDs)
	if err != nil {
		return fmt.Errorf("mail: load label counts: %w", err)
	}
	defer rows.Close()
	var counts []labelCount
	for rows.Next() {
		var c labelCount
		if err := rows.Scan(&c.ID, &c.ThreadsUnread, &c.ThreadsTotal); err != nil {
			return err
		}
		counts = append(counts, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(counts) > 0 {
		b.add(EventLabelCounts, map[string]any{"accountId": accountID, "labels": counts})
	}
	return nil
}

// WriteEvent inserts one event of the given type for a user, in the caller's
// transaction. Packages other than mail (the sync engine's account.status,
// account.error and account.sync_progress) use it so that every event goes
// through the same table and encoding.
func WriteEvent(ctx context.Context, db DB, userID int64, typ string, payload any) error {
	b := &eventBuf{userID: userID}
	b.add(typ, payload)
	return b.flush(ctx, db)
}

// EventOperationFailed is written when a write-back operation is given up
// (plan 5.4): {operationId, accountId, kind, reason, threadIds}; kind is
// sync.OpKind of the job (move, flags, labels, ...).
const EventOperationFailed = "operation.failed"

// RefreshMessages re-derives the labels of messages of one account (server
// state + pending operations), recomputes their threads and writes
// thread.updated (with pendingSync) and label.counts, in the caller's
// transaction. origin (X-Silo-Client) is echoed in the events. The ops
// package calls it after enqueueing, executing or cancelling operations.
func RefreshMessages(ctx context.Context, db DB, accountID int64, msgIDs []int64, origin string) ([]int64, error) {
	msgIDs = uniqueSorted(msgIDs)
	if len(msgIDs) == 0 {
		return nil, nil
	}
	var userID int64
	if err := db.QueryRow(ctx, `SELECT user_id FROM accounts WHERE id = $1`, accountID).Scan(&userID); err != nil {
		return nil, fmt.Errorf("mail: load account %d: %w", accountID, err)
	}
	ev := &eventBuf{userID: userID, origin: origin}
	threads, err := refreshMessages(ctx, db, ev, accountID, msgIDs)
	if err != nil {
		return nil, err
	}
	return threads, ev.flush(ctx, db)
}

// LockAccount serialises the write transactions of one account between the
// sync engine, the write-back and the API (pg_advisory_xact_lock; released
// at commit). Take it first in the transaction.
func LockAccount(ctx context.Context, db DB, accountID int64) error {
	_, err := db.Exec(ctx, `SELECT pg_advisory_xact_lock(1397312335, $1::int)`, accountID)
	return err
}

// EventLabelChanged is written when a label is created, renamed, recoloured
// or deleted (plan 5.4): {accountId, labelId}.
const EventLabelChanged = "label.changed"
