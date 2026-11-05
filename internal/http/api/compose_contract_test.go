package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/drafts"
	"github.com/silopost/silo-server/internal/http/api"
)

// TestDraftSyncNowAndCleanup covers syncNow of PUT /drafts/{id} (plan 3.8
// step 2: saved in Drafts when the compose window closes) and the clean-up of
// drafts that were never edited (blob_gc).
func TestDraftSyncNowAndCleanup(t *testing.T) {
	s := newSendEnv(t)
	ctx := context.Background()
	newDraft := func() api.Draft {
		t.Helper()
		var d api.Draft
		if r := serve(s.web, s.as(req{method: "POST", path: "/drafts", body: map[string]any{}})); r.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", r.Code, r.Body)
		} else {
			r.json(&d)
		}
		return d
	}
	appendRunAt := func(msgID int64) (n int, due bool) {
		t.Helper()
		err := s.pool.QueryRow(ctx, `
SELECT count(*), COALESCE(bool_and(j.run_at <= now()), false) FROM jobs j JOIN op_messages om ON om.job_id = j.id
WHERE om.message_id = $1 AND j.payload->>'op' = 'append' AND j.status = 'pending'`, msgID).Scan(&n, &due)
		if err != nil {
			t.Fatal(err)
		}
		return n, due
	}

	// ---- syncNow: the pending append job is due at once (and stays one).
	d := newDraft()
	msgID := idOf("d_", d.ID)
	body := recipients([]string{"jan@firma.pl"}, nil, nil)
	body["version"] = 1
	if r := serve(s.web, s.as(req{method: "PUT", path: "/drafts/" + d.ID, body: body})); r.Code != http.StatusOK {
		t.Fatalf("save: %d %s", r.Code, r.Body)
	}
	if n, due := appendRunAt(msgID); n != 1 || due {
		t.Fatalf("after an autosave: append jobs = %d, due = %v (want 1, throttled)", n, due)
	}
	body["version"], body["syncNow"] = 2, true
	if r := serve(s.web, s.as(req{method: "PUT", path: "/drafts/" + d.ID, body: body})); r.Code != http.StatusOK {
		t.Fatalf("save with syncNow: %d %s", r.Code, r.Body)
	}
	if n, due := appendRunAt(msgID); n != 1 || !due {
		t.Errorf("after syncNow: append jobs = %d, due = %v (want 1, due now)", n, due)
	}
	// A later autosave throttles it again.
	body["version"] = 3
	delete(body, "syncNow")
	if r := serve(s.web, s.as(req{method: "PUT", path: "/drafts/" + d.ID, body: body})); r.Code != http.StatusOK {
		t.Fatalf("save: %d %s", r.Code, r.Body)
	}
	if n, due := appendRunAt(msgID); n != 1 || due {
		t.Errorf("autosave after syncNow: append jobs = %d, due = %v", n, due)
	}

	// ---- empty drafts: never saved and empty -> deleted after 24 h.
	empty := newDraft()
	edited := newDraft()
	if r := serve(s.web, s.as(req{method: "PUT", path: "/drafts/" + edited.ID, body: map[string]any{
		"version": 1, "to": []any{}, "subject": "", "bodyHtml": ""}})); r.Code != http.StatusOK {
		t.Fatalf("save edited: %d %s", r.Code, r.Body)
	}
	var reply api.Draft
	origID, _ := s.ingest(1, "From: jan@firma.pl\r\nTo: silo@silo.test\r\nSubject: Oferta\r\nMessage-ID: <o@firma.pl>\r\n\r\n")
	serve(s.web, s.as(req{method: "POST", path: "/drafts", body: map[string]any{"replyToMessageId": id36("m_", origID)}})).json(&reply)

	if n, err := s.drafts.PurgeEmptyDrafts(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("purge of fresh drafts: %d, %v", n, err)
	}
	n, err := s.drafts.PurgeEmptyDrafts(ctx, time.Now().Add(drafts.EmptyDraftMaxAge+time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purge after 24 h: %d, %v (want 1)", n, err)
	}
	for _, c := range []struct {
		d    api.Draft
		want int
	}{{empty, http.StatusNotFound}, {edited, http.StatusOK}, {reply, http.StatusOK}, {d, http.StatusOK}} {
		if r := serve(s.web, s.as(req{method: "GET", path: "/drafts/" + c.d.ID})); r.Code != c.want {
			t.Errorf("draft %s after purge: %d, want %d", c.d.ID, r.Code, c.want)
		}
	}
	if n := s.count(`SELECT count(*) FROM threads WHERE id = $1`, idOf("t_", empty.ThreadID)); n != 0 {
		t.Error("thread of the purged draft left behind")
	}
	if n := s.count(`SELECT count(*) FROM events WHERE type = 'thread.deleted' AND (payload->>'threadId')::bigint = $1`, idOf("t_", empty.ThreadID)); n != 1 {
		t.Error("no thread.deleted for the purged draft")
	}
}

// TestStuckSend covers sends left in sending by a restart (plan 3.9): failed
// with "interrupted", the draft back with lastSend, nothing sent again.
func TestStuckSend(t *testing.T) {
	s := newSendEnv(t)
	ctx := context.Background()
	queue := func() (msgID, obID int64) {
		t.Helper()
		d := s.draft(0, "", recipients([]string{"jan@firma.pl"}, nil, nil))
		r, res := s.send(d.ID, nil)
		if r.Code != http.StatusOK {
			t.Fatalf("send: %d %s", r.Code, r.Body)
		}
		return idOf("d_", d.ID), idOf("ob_", res.OutboundID)
	}
	// The process died during delivery: sending, the job's lock expired.
	stuckMsg, stuck := queue()
	// Still being delivered: its job is locked.
	busyMsg, busy := queue()
	// Recent: within 2 x SMTPTimeout.
	_, recent := queue()
	if _, err := s.pool.Exec(ctx, `UPDATE outbound SET status = 'sending', updated_at = now() - interval '10 minutes' WHERE id = ANY($1)`,
		[]int64{stuck, busy}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE outbound SET status = 'sending' WHERE id = $1`, recent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `
UPDATE jobs SET locked_until = CASE WHEN (payload->>'outboundId')::bigint = $1 THEN now() - interval '1 minute' ELSE now() + interval '5 minutes' END
WHERE kind = 'send'`, stuck); err != nil {
		t.Fatal(err)
	}

	n, err := s.drafts.FailStuckSends(ctx, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("FailStuckSends = %d, %v (want 1)", n, err)
	}
	if n := s.count(`SELECT count(*) FROM outbound WHERE id = $1 AND status = 'failed' AND last_error = 'interrupted'`, stuck); n != 1 {
		t.Error("stuck send not failed with interrupted")
	}
	if n := s.count(`SELECT count(*) FROM outbound WHERE id = ANY($1) AND status = 'sending'`, []int64{busy, recent}); n != 2 {
		t.Error("a send in progress was touched")
	}
	if n := s.count(`SELECT count(*) FROM jobs WHERE kind = 'send' AND (payload->>'outboundId')::bigint = $1`, stuck); n != 0 {
		t.Error("the send job of the interrupted send is left (it would deliver it later)")
	}
	if l := s.labels(stuckMsg); len(l) != 1 || l[0] != "DRAFT" {
		t.Errorf("labels of the interrupted send = %v", l)
	}
	if l := s.labels(busyMsg); len(l) != 1 || l[0] != "SCHEDULED" {
		t.Errorf("labels of the busy send = %v", l)
	}
	var d api.Draft
	r := serve(s.web, s.as(req{method: "GET", path: "/drafts/" + id36("d_", stuckMsg)}))
	r.json(&d)
	if r.Code != http.StatusOK || d.LastSend == nil || d.LastSend.Error != drafts.InterruptedError || d.LastSend.Status != "failed" {
		t.Errorf("draft after the interrupted send: %d %s", r.Code, r.Body)
	}
	if n := s.count(`SELECT count(*) FROM events WHERE type = 'outbound.status' AND (payload->>'outboundId')::bigint = $1
	  AND payload->>'status' = 'failed' AND payload->>'error' = 'interrupted'`, stuck); n != 1 {
		t.Error("no outbound.status failed for the interrupted send")
	}
	if len(s.smtp.Messages()) != 0 {
		t.Error("something was sent")
	}
	// Sending it again works and clears lastSend once queued.
	if r, _ := s.send(id36("d_", stuckMsg), nil); r.Code != http.StatusOK {
		t.Fatalf("send again: %d %s", r.Code, r.Body)
	}
	if n, err := s.drafts.FailStuckSends(ctx, time.Now()); err != nil || n != 0 {
		t.Errorf("second sweep: %d, %v", n, err)
	}
}
