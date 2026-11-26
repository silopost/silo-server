package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// JobPushNotify is the general job that sends the push notification of new
// INBOX messages of an account to the user's devices (phase 6); the push
// package runs it. Payload: PushNotifyPayload.
const JobPushNotify = "push.notify"

// PushDelay is how long push.notify waits before it runs, so that a burst of
// new messages ends up in one notification per device and account (new
// messages join the job while it waits).
const PushDelay = 2 * time.Second

// PushNotifyPayload is the payload of a push.notify job.
type PushNotifyPayload struct {
	AccountID  int64   `json:"accountId"`
	MessageIDs []int64 `json:"messageIds"`
}

// EnqueuePushNotify queues push.notify for new INBOX messages of an account:
// those found by the incremental sync (UID >= uidnext) and those the filters
// left in INBOX. It costs one query when no device of the user wants the
// account's mail. A job of the account that has not started yet takes the
// messages instead of a new job (one notification for a burst).
func EnqueuePushNotify(ctx context.Context, db DB, accountID int64, msgIDs []int64) error {
	if len(msgIDs) == 0 {
		return nil
	}
	var wanted bool
	if err := db.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM push_devices d JOIN accounts a ON a.user_id = d.user_id
               WHERE a.id = $1 AND (d.account_ids IS NULL OR $1 = ANY(d.account_ids)))`, accountID).Scan(&wanted); err != nil {
		return fmt.Errorf("mail: check push devices: %w", err)
	}
	if !wanted {
		return nil
	}
	ids, _ := json.Marshal(msgIDs)
	tag, err := db.Exec(ctx, `
UPDATE jobs SET payload = jsonb_set(payload, '{messageIds}', COALESCE(payload->'messageIds', '[]'::jsonb) || $3::jsonb)
WHERE id = (SELECT j.id FROM jobs j
            WHERE j.kind = $1 AND j.status = 'pending' AND j.attempts = 0 AND j.locked_until IS NULL
              AND j.account_id IS NULL AND (j.payload->>'accountId')::bigint = $2
            ORDER BY j.id LIMIT 1 FOR UPDATE SKIP LOCKED)`, JobPushNotify, accountID, ids)
	if err != nil {
		return fmt.Errorf("mail: merge %s: %w", JobPushNotify, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	payload, _ := json.Marshal(PushNotifyPayload{AccountID: accountID, MessageIDs: msgIDs})
	if _, err := db.Exec(ctx, `INSERT INTO jobs (kind, payload, run_at) VALUES ($1, $2, now() + make_interval(secs => $3))`,
		JobPushNotify, payload, PushDelay.Seconds()); err != nil {
		return fmt.Errorf("mail: enqueue %s: %w", JobPushNotify, err)
	}
	return nil
}
