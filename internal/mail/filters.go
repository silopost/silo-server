package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/silopost/silo-server/internal/search"
)

// JobFilterEval is the general job that evaluates the filters of an account
// on new messages of its INBOX (plan 3.5.7 step 7, 3.11); the filters
// package runs it. Payload: FilterEvalPayload.
const JobFilterEval = "filter_eval"

// FilterEvalPayload is the payload of a filter_eval job.
type FilterEvalPayload struct {
	AccountID  int64     `json:"accountId"`
	MessageIDs []int64   `json:"messageIds"`
	QueuedAt   time.Time `json:"queuedAt"`
}

// filterIngest is step 7 of plan 3.5.7: new messages of the INBOX received
// after the account went live (internal_date >= accounts.filters_since)
// are handed to the account's enabled filters by a filter_eval job queued
// in the ingest transaction. Only messages seen for the first time count
// (newIDs: a message moved into INBOX by another client is not filtered).
// It returns the messages handed over: they do not wake a snoozed thread
// here, filter_eval does it for those that stay in INBOX.
func filterIngest(ctx context.Context, db DB, accountID int64, folderRole string, newIDs []int64) ([]int64, error) {
	if folderRole != "inbox" || len(newIDs) == 0 {
		return nil, nil
	}
	var ids []int64
	err := db.QueryRow(ctx, `
SELECT COALESCE(array_agg(m.id ORDER BY m.id), '{}') FROM messages m JOIN accounts a ON a.id = m.account_id
WHERE m.id = ANY($2) AND a.id = $1 AND a.filters_since IS NOT NULL AND m.internal_date >= a.filters_since
  AND EXISTS (SELECT 1 FROM filters f WHERE f.account_id = $1 AND f.enabled)`, accountID, newIDs).Scan(&ids)
	if err != nil {
		return nil, fmt.Errorf("mail: select messages for filters: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	payload, _ := json.Marshal(FilterEvalPayload{AccountID: accountID, MessageIDs: ids, QueuedAt: time.Now().UTC()})
	if _, err := db.Exec(ctx, `INSERT INTO jobs (kind, payload) VALUES ($1, $2)`, JobFilterEval, payload); err != nil {
		return nil, fmt.Errorf("mail: enqueue %s: %w", JobFilterEval, err)
	}
	return ids, nil
}

// WakeSnoozedBy wakes the snoozed threads of new messages (step 6 of plan
// 3.5.7 for messages handed to filters first): wake_at = now and
// snooze_wake queued at once.
func WakeSnoozedBy(ctx context.Context, db DB, msgIDs []int64) error {
	if len(msgIDs) == 0 {
		return nil
	}
	tag, err := db.Exec(ctx, `
UPDATE snoozes s SET wake_at = now() FROM messages m
WHERE m.id = ANY($1) AND s.thread_id = m.thread_id AND s.wake_at > now()`, msgIDs)
	if err != nil {
		return fmt.Errorf("mail: wake snoozed threads: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	return EnqueueSnoozeWake(ctx, db)
}

// MessageView is a message as the search matcher sees it (plan 3.11), with
// what filters need besides.
type MessageView struct {
	ID, AccountID, UserID, ThreadID int64
	// BodyState is messages.body_state (headers, deferred, full, local).
	BodyState string
	// FromEmail is the sender's address (lower case).
	FromEmail string
	View      search.MessageView
}

// LoadMessageViews loads messages (orphans left out) in id order as the
// matcher sees them: the columns search.ToSQL reads and the visible labels
// (server state + pending operations) by type.
func LoadMessageViews(ctx context.Context, db DB, ids []int64) ([]MessageView, error) {
	rows, err := db.Query(ctx, `
SELECT m.id, m.account_id, m.user_id, m.thread_id, m.body_state, lower(m.from_addr::text),
       m.subject, m.from_text, m.recipients_text, m.cc_addrs, m.bcc_addrs, COALESCE(m.list_id, ''), m.message_id_hdr,
       m.internal_date, m.size_bytes, m.has_attachments, m.is_unread, m.body_text,
       ARRAY(SELECT a.filename FROM attachments a WHERE a.message_id = m.id ORDER BY a.id)::text[],
       ARRAY(SELECT l.system_key FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE ml.message_id = m.id AND l.type = 'system' ORDER BY l.id)::text[],
       ARRAY(SELECT l.name FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE ml.message_id = m.id AND l.type = 'folder' ORDER BY l.id)::text[],
       ARRAY(SELECT l.name FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE ml.message_id = m.id AND l.type = 'user' ORDER BY l.id)::text[]
FROM messages m WHERE m.id = ANY($1) AND m.orphaned_at IS NULL ORDER BY m.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("mail: load message views: %w", err)
	}
	defer rows.Close()
	var out []MessageView
	for rows.Next() {
		var r MessageView
		v := &r.View
		var cc, bcc []byte
		if err := rows.Scan(&r.ID, &r.AccountID, &r.UserID, &r.ThreadID, &r.BodyState, &r.FromEmail,
			&v.Subject, &v.FromText, &v.RecipientsText, &cc, &bcc, &v.ListID, &v.MessageIDHdr, &v.Date, &v.Size,
			&v.HasAttachments, &v.Unread, &v.Body, &v.Attachments, &v.SystemLabels, &v.FolderLabels, &v.UserLabels); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(cc, &v.Cc)
		_ = json.Unmarshal(bcc, &v.Bcc)
		out = append(out, r)
	}
	return out, rows.Err()
}

// withoutIDs returns ids minus drop.
func withoutIDs(ids, drop []int64) []int64 {
	if len(drop) == 0 {
		return ids
	}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !slices.Contains(drop, id) {
			out = append(out, id)
		}
	}
	return out
}
