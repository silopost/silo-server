package mail

import (
	"context"
	"fmt"
	"slices"
)

// JobSnoozeWake is the general job that wakes snoozed threads whose time has
// come and drops snoozes whose messages left the Snoozed folder (plan 3.10).
// The ops package runs it; it is queued every 30 s and by ingest.
const JobSnoozeWake = "snooze_wake"

// FlagSnoozed is the keyword set on snoozed messages on the server (when the
// server keeps keywords), so that other clients can tell them apart. It is
// never a label (IsLabelKeyword).
const FlagSnoozed = "$SiloSnoozed"

// EnqueueSnoozeWake queues snooze_wake to run now unless one is already
// waiting (a running one does not count: it may have missed the change).
func EnqueueSnoozeWake(ctx context.Context, db DB) error {
	_, err := db.Exec(ctx, `
INSERT INTO jobs (kind) SELECT $1
WHERE NOT EXISTS (SELECT 1 FROM jobs WHERE kind = $1 AND status = 'pending' AND (locked_until IS NULL OR locked_until < now()))`, JobSnoozeWake)
	if err != nil {
		return fmt.Errorf("mail: enqueue %s: %w", JobSnoozeWake, err)
	}
	return nil
}

// noWakeRoles are the folders whose new messages do not wake a snoozed
// thread: the user's own mail and mail thrown away.
var noWakeRoles = []string{"sent", "drafts", "trash", "junk", "snoozed"}

// snoozeIngest is step 6 of plan 3.5.7: a new message received in a snoozed
// thread wakes it (wake_at = now), as in Gmail; a known message of a snoozed
// thread seen outside the Snoozed folder may have been taken out of it by
// another client. Either way snooze_wake is queued to run now; it moves the
// messages and drops snoozes that are left without messages in Snoozed.
func snoozeIngest(ctx context.Context, db DB, folderRole string, newIDs, threads []int64) error {
	if len(threads) == 0 || folderRole == "snoozed" {
		return nil
	}
	if len(newIDs) > 0 && !slices.Contains(noWakeRoles, folderRole) {
		if _, err := db.Exec(ctx, `
UPDATE snoozes s SET wake_at = now() FROM messages m
WHERE m.id = ANY($1) AND s.thread_id = m.thread_id AND s.wake_at > now()`, newIDs); err != nil {
			return fmt.Errorf("mail: wake snoozed threads: %w", err)
		}
	}
	var snoozed bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM snoozes WHERE thread_id = ANY($1))`, threads).Scan(&snoozed); err != nil {
		return fmt.Errorf("mail: load snoozes: %w", err)
	}
	if !snoozed {
		return nil
	}
	return EnqueueSnoozeWake(ctx, db)
}
