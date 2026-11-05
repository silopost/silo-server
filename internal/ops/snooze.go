package ops

import (
	"context"
	"errors"
	"fmt"
	"time"
	_ "time/tzdata" // users' time zones on hosts without a zoneinfo database

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/worker"
)

// Snooze (plan 3.10): the messages of a thread that are in INBOX move to the
// Snoozed folder (role snoozed, created by Silo on the first snooze) after
// the undo window, with $SiloSnoozed when the server keeps keywords, so that
// other clients do not show them in their inbox either. snoozes/snooze_items
// remember the wake time and the messages; snooze_wake (every 30 s, and at
// once when ingest sees a new message in the thread) moves them back to INBOX
// with the newest one unread and the thread on top of the list.

// Limits of the wake time of POST /threads/{id}/snooze.
const (
	SnoozeMinAhead = 30 * time.Second
	SnoozeMaxAhead = 366 * 24 * time.Hour
)

// SnoozeTick is how often snooze_wake is queued (plan 3.10: precision 30 s).
const SnoozeTick = 30 * time.Second

// wakeBatch is the number of due snoozes loaded at a time.
const wakeBatch = 100

const roleSnoozed = "snoozed"

// Errors of snooze.
var (
	ErrInvalidSnooze = errors.New("ops: the snooze time must be at least 30 seconds and at most a year ahead")
	ErrNotInInbox    = errors.New("ops: the thread has no message in the inbox")
	ErrNotSnoozed    = errors.New("ops: the thread is not snoozed")
)

// Snooze snoozes a thread until the given time, in one transaction (plan
// 3.10): snoozes is upserted (snoozing a snoozed thread again changes its
// wake time), every message of the thread in INBOX gets a snooze_item with
// its folder and a move to the Snoozed folder after the undo window
// (+$SiloSnoozed when keywords are kept), so that INBOX turns SNOOZED at once
// through the pending operation. Only a thread with mail in INBOX can be
// snoozed (as in Gmail: snooze is an inbox action), else ErrNotInInbox; an
// already snoozed thread only gets the new time. The result is undoable like
// modify: inside the window the moves are cancelled, later they are reversed;
// either way the snooze is removed again (or gets its previous time back).
func (s *Service) Snooze(ctx context.Context, userID, threadID int64, until time.Time, origin string) (*Result, error) {
	now := time.Now()
	if until.Before(now.Add(SnoozeMinAhead)) || until.After(now.Add(SnoozeMaxAhead)) {
		return nil, ErrInvalidSnooze
	}
	return s.run(ctx, userID, []int64{threadID}, nil, origin, func(ctx context.Context, tx pgx.Tx, b *builder) error {
		var snoozeID int64
		var prev *time.Time
		var at time.Time
		err := tx.QueryRow(ctx, `SELECT id, wake_at FROM snoozes WHERE thread_id = $1 FOR UPDATE`, threadID).Scan(&snoozeID, &at)
		switch {
		case err == nil:
			prev = &at
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		ids := make([]int64, len(b.msgs))
		for i, m := range b.msgs {
			ids[i] = m.id
		}
		// Messages whose move to Snoozed is still pending (a snooze in its
		// undo window) are on their way already.
		r, err := tx.Query(ctx, `
SELECT DISTINCT om.message_id FROM op_messages om JOIN jobs j ON j.id = om.job_id
WHERE om.message_id = ANY($1) AND j.kind = 'imap_op' AND j.status = 'pending'
  AND j.payload->>'op' = 'move' AND j.payload->>'toRole' = $2`, ids, roleSnoozed)
		if err != nil {
			return err
		}
		moving, err := pgx.CollectRows(r, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		type pick struct {
			m    *msgInfo
			from location
		}
		var picks []pick
		for _, m := range b.msgs {
			from, ok := in(m, "inbox")
			if !ok || containsID(moving, m.id) {
				continue
			}
			picks = append(picks, pick{m, from})
		}
		if len(picks) == 0 && snoozeID == 0 {
			return ErrNotInInbox
		}
		if err := tx.QueryRow(ctx, `
INSERT INTO snoozes (user_id, account_id, thread_id, wake_at) VALUES ($1, $2, $3, $4)
ON CONFLICT (thread_id) DO UPDATE SET wake_at = EXCLUDED.wake_at
RETURNING id`, userID, b.accountID, threadID, until).Scan(&snoozeID); err != nil {
			return fmt.Errorf("ops: upsert snooze: %w", err)
		}
		var add []string
		if b.keywords {
			add = []string{mail.FlagSnoozed}
		}
		for _, p := range picks {
			b.add(mail.OpPayload{Op: mail.OpMove, ToRole: roleSnoozed, Add: add}, p.m.id, p.from.folderID, b.s.undoWindow)
			b.inv = append(b.inv, invItem{MessageID: p.m.id, MoveTo: p.from.folderID, ExpectRole: roleSnoozed, Remove: changedFlags(p.m, add, true)})
			if _, err := tx.Exec(ctx, `
INSERT INTO snooze_items (snooze_id, message_id, source_folder_id) VALUES ($1, $2, $3)
ON CONFLICT (snooze_id, message_id) DO UPDATE SET source_folder_id = EXCLUDED.source_folder_id`, snoozeID, p.m.id, p.from.folderID); err != nil {
				return fmt.Errorf("ops: insert snooze item: %w", err)
			}
		}
		b.inv = append(b.inv, invItem{MessageID: b.msgs[0].id, SnoozeThread: threadID, SnoozePrev: prev})
		b.record = true
		return nil
	})
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// restoreSnooze undoes a snooze recorded by invItem.SnoozeThread.
func restoreSnooze(ctx context.Context, tx pgx.Tx, userID int64, it invItem) error {
	if it.SnoozePrev == nil {
		_, err := tx.Exec(ctx, `DELETE FROM snoozes WHERE thread_id = $1 AND user_id = $2`, it.SnoozeThread, userID)
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE snoozes SET wake_at = $3 WHERE thread_id = $1 AND user_id = $2`, it.SnoozeThread, userID, *it.SnoozePrev)
	return err
}

// Unsnooze wakes a snoozed thread now (DELETE /threads/{id}/snooze): the
// same as the wake-up at its time. It is not an undoable operation
// (OperationID is empty): snoozing again is the way back. ErrNotSnoozed when
// the thread has no snooze.
func (s *Service) Unsnooze(ctx context.Context, userID, threadID int64, origin string) (*Result, error) {
	return s.wake(ctx, userID, threadID, false, origin)
}

// wake ends the snooze of a thread (plan 3.10) in one transaction: the
// snoozes row is locked (FOR UPDATE SKIP LOCKED, so that several instances
// never wake a thread twice; with due only when its time has come) and
// deleted, its messages move back to INBOX at once (no undo window) without
// $SiloSnoozed, the newest of them unread (-\Seen), and threads.sort_at =
// now() puts the thread on top of the inbox; thread.updated follows from the
// refresh.
func (s *Service) wake(ctx context.Context, userID, threadID int64, due bool, origin string) (*Result, error) {
	return s.run(ctx, userID, []int64{threadID}, nil, origin, func(ctx context.Context, tx pgx.Tx, b *builder) error {
		b.noUndo = true
		q := `SELECT id FROM snoozes WHERE thread_id = $1`
		if due {
			q += ` AND wake_at <= now()`
		}
		var snoozeID int64
		err := tx.QueryRow(ctx, q+` FOR UPDATE SKIP LOCKED`, threadID).Scan(&snoozeID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotSnoozed
		}
		if err != nil {
			return err
		}
		r, err := tx.Query(ctx, `
SELECT si.message_id FROM snooze_items si JOIN messages m ON m.id = si.message_id
WHERE si.snooze_id = $1 ORDER BY m.internal_date DESC, m.id DESC`, snoozeID)
		if err != nil {
			return err
		}
		items, err := pgx.CollectRows(r, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM snoozes WHERE id = $1`, snoozeID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE threads SET sort_at = now() WHERE id = $1`, threadID); err != nil {
			return err
		}
		byID := map[int64]*msgInfo{}
		for _, m := range b.msgs {
			byID[m.id] = m
		}
		newest := true
		for _, id := range items {
			m := byID[id]
			if m == nil {
				continue
			}
			from, ok := in(m, roleSnoozed)
			if !ok {
				// Still in INBOX on the server: the move to Snoozed is
				// pending (it is cancelled by coalescing) or being sent
				// (the move back runs after it).
				if from, ok = in(m, "inbox"); !ok {
					continue // taken elsewhere by another client
				}
			}
			var remove []string
			if b.keywords {
				remove = append(remove, mail.FlagSnoozed)
			}
			if newest {
				remove = append(remove, `\Seen`)
				if from.role == "inbox" {
					b.add(mail.OpPayload{Op: mail.OpFlags, Remove: []string{`\Seen`}}, m.id, 0, 0)
				}
				newest = false
			}
			b.add(mail.OpPayload{Op: mail.OpMove, ToRole: "inbox", Remove: remove}, m.id, from.folderID, 0)
		}
		return nil
	})
}

// WakeDue wakes every snoozed thread whose time has come (the snooze_wake
// job), after dropping snoozes whose messages all left the Snoozed folder.
// It is idempotent and safe with several instances: each thread is woken in
// its own transaction under the account lock with FOR UPDATE SKIP LOCKED.
// It returns the number of threads woken.
func (s *Service) WakeDue(ctx context.Context) (int, error) {
	if err := s.DropAbandonedSnoozes(ctx); err != nil {
		return 0, err
	}
	woken := 0
	for {
		r, err := s.pool.Query(ctx, `SELECT user_id, thread_id FROM snoozes WHERE wake_at <= now() ORDER BY wake_at, id LIMIT $1`, wakeBatch)
		if err != nil {
			return woken, err
		}
		type due struct{ user, thread int64 }
		var batch []due
		for r.Next() {
			var d due
			if err := r.Scan(&d.user, &d.thread); err != nil {
				r.Close()
				return woken, err
			}
			batch = append(batch, d)
		}
		r.Close()
		if err := r.Err(); err != nil {
			return woken, err
		}
		progress := false
		for _, d := range batch {
			_, err := s.wake(ctx, d.user, d.thread, true, "")
			switch {
			case errors.Is(err, ErrNotSnoozed):
				// Woken or being woken by another instance.
			case errors.Is(err, ErrNotFound):
				// The thread has no message left: nothing to move back.
				if _, err := s.pool.Exec(ctx, `DELETE FROM snoozes WHERE thread_id = $1 AND wake_at <= now()`, d.thread); err != nil {
					return woken, err
				}
				progress = true
			case err != nil:
				return woken, fmt.Errorf("ops: wake thread %d: %w", d.thread, err)
			default:
				woken++
				progress = true
			}
		}
		if len(batch) < wakeBatch || !progress {
			return woken, nil
		}
	}
}

// abandoned is true for a snooze_items row (si, message m) whose message is
// no longer in the Snoozed folder and is not on its way there: another
// client took it out (plan 3.10). Orphaned messages are kept: a message is
// orphaned for a moment while it moves, and while a folder is re-read after
// a UIDVALIDITY change or a delete + re-create; the matcher re-attaches it to
// the same row (dedupe_key), so the snooze survives.
const abandoned = `m.orphaned_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM message_locations l JOIN mailbox_folders f ON f.id = l.folder_id
                  WHERE l.message_id = m.id AND f.role = 'snoozed' AND NOT ('\Deleted' = ANY(l.flags)))
  AND NOT EXISTS (SELECT 1 FROM op_messages om JOIN jobs j ON j.id = om.job_id
                  WHERE om.message_id = m.id AND j.kind = 'imap_op' AND j.status = 'pending' AND j.payload->>'op' = 'move')`

// DropAbandonedSnoozes removes the snooze items of messages taken out of the
// Snoozed folder by another client and the snoozes left without items (plan
// 3.10); their threads are recomputed (snoozedUntil null, thread.updated).
func (s *Service) DropAbandonedSnoozes(ctx context.Context) error {
	r, err := s.pool.Query(ctx, `
SELECT DISTINCT s.account_id FROM snoozes s JOIN snooze_items si ON si.snooze_id = s.id JOIN messages m ON m.id = si.message_id
WHERE `+abandoned+`
UNION
SELECT s.account_id FROM snoozes s WHERE NOT EXISTS (SELECT 1 FROM snooze_items si WHERE si.snooze_id = s.id)`)
	if err != nil {
		return err
	}
	accounts, err := pgx.CollectRows(r, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, acc := range accounts {
		err := s.inTx(ctx, func(tx pgx.Tx) error {
			if err := mail.LockAccount(ctx, tx, acc); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
DELETE FROM snooze_items si USING snoozes s, messages m
WHERE s.id = si.snooze_id AND s.account_id = $1 AND m.id = si.message_id AND `+abandoned, acc); err != nil {
				return err
			}
			r, err := tx.Query(ctx, `
DELETE FROM snoozes s WHERE s.account_id = $1 AND NOT EXISTS (SELECT 1 FROM snooze_items si WHERE si.snooze_id = s.id)
RETURNING s.thread_id`, acc)
			if err != nil {
				return err
			}
			threads, err := pgx.CollectRows(r, pgx.RowTo[int64])
			if err != nil || len(threads) == 0 {
				return err
			}
			r, err = tx.Query(ctx, `SELECT id FROM messages WHERE thread_id = ANY($1)`, threads)
			if err != nil {
				return err
			}
			msgs, err := pgx.CollectRows(r, pgx.RowTo[int64])
			if err != nil {
				return err
			}
			if len(msgs) == 0 {
				return mail.RecomputeThread(ctx, tx, threads...)
			}
			_, err = mail.RefreshMessages(ctx, tx, acc, msgs, "")
			return err
		})
		if err != nil {
			return fmt.Errorf("ops: drop abandoned snoozes of account %d: %w", acc, err)
		}
	}
	return nil
}

// RegisterJobs binds snooze_wake to the job worker.
func (s *Service) RegisterJobs(w *worker.Worker) {
	w.Register(mail.JobSnoozeWake, func(ctx context.Context, _ worker.Job) error {
		_, err := s.WakeDue(ctx)
		return err
	})
}

// RunSnoozeWake queues snooze_wake at start and then every interval (unless
// one is waiting) until ctx ends.
func (s *Service) RunSnoozeWake(ctx context.Context, every time.Duration, onError func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := mail.EnqueueSnoozeWake(ctx, s.pool); err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SnoozePreset is a suggested wake time (GET /snooze/presets).
type SnoozePreset struct {
	Key   string
	Label string
	Until time.Time
}

// SnoozePresets are Gmail's snooze suggestions at now in loc (plan 2.6: the
// client does no date arithmetic): later_today (18:00 today, only when that
// is more than an hour away), tomorrow (08:00), this_weekend (Saturday 08:00,
// not on Saturday or Sunday) and next_week (Monday 08:00). A preset at the
// same time as an earlier one (this_weekend on Friday, next_week on Sunday)
// is left out.
func SnoozePresets(now time.Time, loc *time.Location) []SnoozePreset {
	if loc == nil {
		loc = time.UTC
	}
	n := now.In(loc)
	at := func(days, hour int) time.Time {
		d := time.Date(n.Year(), n.Month(), n.Day()+days, 0, 0, 0, 0, loc)
		return time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, loc)
	}
	var out []SnoozePreset
	push := func(key, label string, t time.Time) {
		for _, p := range out {
			if p.Until.Equal(t) {
				return
			}
		}
		out = append(out, SnoozePreset{Key: key, Label: label, Until: t})
	}
	if t := at(0, 18); t.After(n.Add(time.Hour)) {
		push("later_today", "Later today", t)
	}
	push("tomorrow", "Tomorrow", at(1, 8))
	wd := n.Weekday()
	if wd != time.Saturday && wd != time.Sunday {
		push("this_weekend", "This weekend", at(int(time.Saturday-wd), 8))
	}
	days := (int(time.Monday) - int(wd) + 7) % 7
	if days == 0 {
		days = 7
	}
	push("next_week", "Next week", at(days, 8))
	return out
}

// UserLocation is the time zone of users.settings.timezone (UTC when unset
// or unknown).
func (s *Service) UserLocation(ctx context.Context, userID int64) (*time.Location, error) {
	var tz string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(settings->>'timezone', '') FROM users WHERE id = $1`, userID).Scan(&tz)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc, nil
		}
	}
	return time.UTC, nil
}
