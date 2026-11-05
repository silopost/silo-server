package api_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/http/api"
)

func TestSnoozeThread(t *testing.T) {
	m := newMailEnv(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := m.pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE accounts SET keywords_supported = true WHERE id = $1`, m.accID)
	_, threadID := m.ingest(1, "From: a@silo.test\r\nSubject: Later\r\nMessage-ID: <s1@silo.test>\r\n\r\n")
	_, other := m.ingest(2, "From: b@silo.test\r\nSubject: Archived\r\nMessage-ID: <s2@silo.test>\r\n\r\n")
	tid := id36("t_", threadID)
	snooze := func(id string, until time.Time, client string) (resp, api.SnoozeResult) {
		t.Helper()
		var out api.SnoozeResult
		r := serve(m.web, m.as(req{method: "POST", path: "/threads/" + id + "/snooze", body: map[string]any{"until": until.Format(time.RFC3339)},
			headers: map[string]string{"X-Silo-Client": client}}))
		if r.Code == http.StatusOK {
			r.json(&out)
		}
		return r, out
	}
	undo := func(id string) (resp, api.OperationResult) {
		t.Helper()
		var out api.OperationResult
		r := serve(m.web, m.as(req{method: "POST", path: "/operations/" + id + "/undo"}))
		if r.Code == http.StatusOK {
			r.json(&out)
		}
		return r, out
	}
	same := func(a *time.Time, b time.Time) bool {
		return a != nil && a.Truncate(time.Second).Equal(b.Truncate(time.Second))
	}

	// Validation.
	for _, until := range []time.Time{time.Now().Add(10 * time.Second), time.Now().Add(-time.Hour), time.Now().AddDate(2, 0, 0)} {
		if r, _ := snooze(tid, until, ""); r.Code != http.StatusUnprocessableEntity || r.code() != api.CodeInvalidSnooze {
			t.Errorf("snooze until %s: %d %s", until, r.Code, r.Body)
		}
	}
	if r, _ := snooze("t_zzzz", time.Now().Add(time.Hour), ""); r.Code != http.StatusNotFound {
		t.Errorf("unknown thread: %d %s", r.Code, r.Body)
	}
	if r := serve(m.web, m.as(req{method: "POST", path: "/threads/modify", body: map[string]any{"ids": []string{tid}, "addLabelIds": []string{"SNOOZED"}}})); r.Code != http.StatusBadRequest {
		t.Errorf("modify +SNOOZED: %d %s", r.Code, r.Body)
	}

	// Snooze: INBOX turns SNOOZED at once, the move waits for the window.
	until := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	r, op := snooze(tid, until, "tab-z")
	th := op.Thread
	if r.Code != http.StatusOK || th.ID != tid || !slices.Contains(th.LabelIDs, "SNOOZED") || slices.Contains(th.LabelIDs, "INBOX") ||
		!same(th.SnoozedUntil, until) || !th.PendingSync || op.OperationID == nil || op.UndoUntil == nil || !op.UndoUntil.After(time.Now()) {
		t.Fatalf("snooze: %d %s", r.Code, r.Body)
	}
	if n := m.count(`SELECT count(*) FROM snooze_items si JOIN snoozes s ON s.id = si.snooze_id WHERE s.thread_id = $1 AND si.source_folder_id = $2`, threadID, m.inbox); n != 1 {
		t.Errorf("snooze items = %d", n)
	}
	if n := m.count(`SELECT count(*) FROM jobs WHERE group_id = $1::uuid AND payload->>'op' = 'move' AND payload->>'toRole' = 'snoozed'
		AND payload->'add' ? '$SiloSnoozed' AND run_at > now()`, *op.OperationID); n != 1 {
		t.Errorf("move jobs to snoozed = %d", n)
	}
	if n := m.count(`SELECT count(*) FROM events WHERE type = 'thread.updated' AND payload->>'origin' = 'tab-z' AND payload->>'snoozedUntil' IS NOT NULL`); n == 0 {
		t.Error("no thread.updated with snoozedUntil")
	}
	var list api.ThreadList
	serve(m.web, m.as(req{method: "GET", path: "/threads?labelId=SNOOZED"})).json(&list)
	if len(list.Threads) != 1 || list.Threads[0].ID != tid || !same(list.Threads[0].SnoozedUntil, until) {
		t.Errorf("SNOOZED list: %+v", list.Threads)
	}

	// Snoozing again changes the time only; undoing that puts the first time back.
	later := until.Add(time.Hour)
	r, op2 := snooze(tid, later, "")
	if r.Code != http.StatusOK || !same(op2.Thread.SnoozedUntil, later) || op2.OperationID == nil {
		t.Fatalf("snooze again: %d %s", r.Code, r.Body)
	}
	if n := m.count(`SELECT count(*) FROM jobs WHERE account_id = $1 AND kind = 'imap_op'`, m.accID); n != 1 {
		t.Errorf("jobs after the second snooze = %d, want 1", n)
	}
	if r, out := undo(*op2.OperationID); r.Code != http.StatusOK || len(out.Threads) != 1 || !same(out.Threads[0].SnoozedUntil, until) {
		t.Fatalf("undo of the new time: %d %s", r.Code, r.Body)
	}

	// Undo inside the window: back in INBOX, no snooze, nothing for the server.
	r, out := undo(*op.OperationID)
	if r.Code != http.StatusOK || len(out.Threads) != 1 || !slices.Contains(out.Threads[0].LabelIDs, "INBOX") ||
		slices.Contains(out.Threads[0].LabelIDs, "SNOOZED") || out.Threads[0].SnoozedUntil != nil {
		t.Fatalf("undo: %d %s", r.Code, r.Body)
	}
	if n := m.count(`SELECT count(*) FROM jobs WHERE account_id = $1 AND kind = 'imap_op'`, m.accID) + m.count(`SELECT count(*) FROM snoozes`); n != 0 {
		t.Errorf("jobs or snoozes left after undo: %d", n)
	}

	// Only threads with mail in INBOX; unsnooze only snoozed threads.
	var archive int64
	if err := m.pool.QueryRow(ctx, `
INSERT INTO mailbox_folders (account_id, user_id, name, name_raw, role, uidvalidity)
SELECT id, user_id, 'Archive', 'Archive', 'archive', 1 FROM accounts WHERE id = $1 RETURNING id`, m.accID).Scan(&archive); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE message_locations SET folder_id = $2 WHERE message_id IN (SELECT id FROM messages WHERE thread_id = $1)`, other, archive)
	if r, _ := snooze(id36("t_", other), time.Now().Add(time.Hour), ""); r.Code != http.StatusConflict || r.code() != api.CodeNotInInbox {
		t.Errorf("snooze of an archived thread: %d %s", r.Code, r.Body)
	}
	if r := serve(m.web, m.as(req{method: "DELETE", path: "/threads/" + tid + "/snooze"})); r.Code != http.StatusConflict || r.code() != api.CodeNotSnoozed {
		t.Errorf("unsnooze of a thread that is not snoozed: %d %s", r.Code, r.Body)
	}

	// Snooze, the move ran (the message is in Snoozed, read), then wake now.
	r, op = snooze(tid, until, "")
	if r.Code != http.StatusOK {
		t.Fatalf("snooze: %d %s", r.Code, r.Body)
	}
	var snoozed int64
	if err := m.pool.QueryRow(ctx, `
INSERT INTO mailbox_folders (account_id, user_id, name, name_raw, role, uidvalidity)
SELECT id, user_id, 'Snoozed', 'Snoozed', 'snoozed', 1 FROM accounts WHERE id = $1 RETURNING id`, m.accID).Scan(&snoozed); err != nil {
		t.Fatal(err)
	}
	exec(`DELETE FROM jobs WHERE group_id = $1::uuid`, *op.OperationID)
	exec(`UPDATE message_locations SET folder_id = $2, flags = '{\Seen,$SiloSnoozed}' WHERE message_id IN (SELECT id FROM messages WHERE thread_id = $1)`, threadID, snoozed)
	exec(`UPDATE messages SET is_unread = false WHERE thread_id = $1`, threadID)
	var woke api.SnoozeResult
	r = serve(m.web, m.as(req{method: "DELETE", path: "/threads/" + tid + "/snooze", headers: map[string]string{"X-Silo-Client": "tab-w"}}))
	r.json(&woke)
	if r.Code != http.StatusOK || !slices.Contains(woke.Thread.LabelIDs, "INBOX") || !slices.Contains(woke.Thread.LabelIDs, "UNREAD") ||
		slices.Contains(woke.Thread.LabelIDs, "SNOOZED") || woke.Thread.SnoozedUntil != nil || woke.OperationID != nil || woke.UndoUntil != nil {
		t.Fatalf("unsnooze: %d %s", r.Code, r.Body)
	}
	if n := m.count(`SELECT count(*) FROM jobs WHERE account_id = $1 AND payload->>'op' = 'move' AND payload->>'toRole' = 'inbox'
		AND payload->'remove' ? '\Seen' AND payload->'remove' ? '$SiloSnoozed' AND run_at <= now()`, m.accID); n != 1 {
		t.Errorf("wake move jobs = %d", n)
	}
	if n := m.count(`SELECT count(*) FROM threads WHERE id = $1 AND sort_at > last_message_at`, threadID); n != 1 {
		t.Error("the woken thread is not on top")
	}
	serve(m.web, m.as(req{method: "GET", path: "/threads?labelId=INBOX"})).json(&list)
	if len(list.Threads) == 0 || list.Threads[0].ID != tid {
		t.Errorf("INBOX after wake: %+v", list.Threads)
	}
	if n := m.count(`SELECT count(*) FROM snoozes`); n != 0 {
		t.Errorf("snoozes after unsnooze = %d", n)
	}
}

func TestSnoozePresets(t *testing.T) {
	m := newMailEnv(t)
	get := func(path string) (resp, api.SnoozePresets) {
		t.Helper()
		var out api.SnoozePresets
		r := serve(m.web, m.as(req{method: "GET", path: path}))
		if r.Code == http.StatusOK {
			r.json(&out)
		}
		return r, out
	}
	keys := func(p api.SnoozePresets) []string {
		var out []string
		for _, x := range p.Presets {
			out = append(out, x.Key+"="+x.Until.Format(time.RFC3339))
		}
		return out
	}
	// 2026-11-05 is a Thursday.
	r, p := get("/snooze/presets?now=2026-11-05T10:00:00Z")
	want := []string{"later_today=2026-11-05T18:00:00Z", "tomorrow=2026-11-06T08:00:00Z", "this_weekend=2026-11-07T08:00:00Z", "next_week=2026-11-09T08:00:00Z"}
	if r.Code != http.StatusOK || p.Timezone != "UTC" || !slices.Equal(keys(p), want) {
		t.Fatalf("presets in UTC: %d %s", r.Code, r.Body)
	}
	if _, err := m.pool.Exec(context.Background(), `UPDATE users SET settings = settings || '{"timezone": "Europe/Warsaw"}' WHERE email = $1`, m.l.body.User.Email); err != nil {
		t.Fatal(err)
	}
	r, p = get("/snooze/presets?now=2026-11-05T17:30:00Z") // 18:30 in Warsaw
	want = []string{"tomorrow=2026-11-06T08:00:00+01:00", "this_weekend=2026-11-07T08:00:00+01:00", "next_week=2026-11-09T08:00:00+01:00"}
	if r.Code != http.StatusOK || p.Timezone != "Europe/Warsaw" || !slices.Equal(keys(p), want) || p.Presets[0].Label == "" {
		t.Fatalf("presets in Warsaw: %d %s", r.Code, r.Body)
	}
	if r, _ := get("/snooze/presets"); r.Code != http.StatusOK {
		t.Errorf("presets now: %d %s", r.Code, r.Body)
	}
	if r, _ := get("/snooze/presets?now=yesterday"); r.Code != http.StatusBadRequest {
		t.Errorf("bad now: %d %s", r.Code, r.Body)
	}
}
