package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/drafts"
	"github.com/silopost/silo-server/internal/http/api"
)

// TestThreadDraftCount: Thread.draftCount (and draftId) in the thread list and
// in thread.updated follow replies started, discarded, sent, unsent and purged.
func TestThreadDraftCount(t *testing.T) {
	s := newSendEnv(t)
	ctx := context.Background()
	origID, threadID := s.ingest(1, "From: Jan Kowalski <jan@firma.pl>\r\nTo: silo@silo.test\r\nSubject: Szkice\r\n"+
		"Message-ID: <drafts@firma.pl>\r\n\r\n")
	tid := id36("t_", threadID)

	listed := func(path string) api.Thread {
		t.Helper()
		var list api.ThreadList
		r := serve(s.web, s.as(req{method: "GET", path: path}))
		r.json(&list)
		for _, th := range list.Threads {
			if th.ID == tid {
				return th
			}
		}
		t.Fatalf("thread %s not in %s: %d %s", tid, path, r.Code, r.Body)
		return api.Thread{}
	}
	// lastEvent: draftCount and draftId (d_ form, as the event hub maps it) of
	// the newest thread.updated of the thread.
	lastEvent := func() (int, *string) {
		t.Helper()
		var n int
		var raw *int64
		if err := s.pool.QueryRow(ctx, `SELECT (payload->>'draftCount')::int, (payload->>'draftId')::bigint FROM events
WHERE type = 'thread.updated' AND (payload->>'threadId')::bigint = $1 ORDER BY seq DESC LIMIT 1`, threadID).Scan(&n, &raw); err != nil {
			t.Fatal(err)
		}
		if raw == nil {
			return n, nil
		}
		id := id36("d_", *raw)
		return n, &id
	}
	check := func(step string, want int, wantDraft string) {
		t.Helper()
		for _, path := range []string{"/threads?labelId=INBOX", "/threads?labelId=INBOX&q=Szkice"} {
			th := listed(path)
			if th.DraftCount != want || (wantDraft == "") != (th.DraftID == nil) || th.DraftID != nil && *th.DraftID != wantDraft {
				t.Errorf("%s: %s: draftCount %d draftId %v, want %d %q", step, path, th.DraftCount, th.DraftID, want, wantDraft)
			}
		}
		if n, id := lastEvent(); n != want || (wantDraft == "") != (id == nil) || id != nil && *id != wantDraft {
			t.Errorf("%s: thread.updated draftCount %d draftId %v, want %d %q", step, n, id, want, wantDraft)
		}
	}

	if th := listed("/threads?labelId=INBOX"); th.DraftCount != 0 || th.DraftID != nil {
		t.Errorf("no drafts: draftCount %d draftId %v", th.DraftCount, th.DraftID)
	}
	first := s.draft(origID, "reply", recipients([]string{"jan@firma.pl"}, nil, nil))
	check("one draft", 1, first.ID)
	var second api.Draft
	r := serve(s.web, s.as(req{method: "POST", path: "/drafts", body: map[string]any{"replyToMessageId": id36("m_", origID), "mode": "replyAll"}}))
	r.json(&second)
	if r.Code != http.StatusCreated {
		t.Fatalf("second draft: %d %s", r.Code, r.Body)
	}
	check("two drafts", 2, second.ID)

	if r := serve(s.web, s.as(req{method: "DELETE", path: "/drafts/" + second.ID})); r.Code != http.StatusNoContent {
		t.Fatalf("discard: %d %s", r.Code, r.Body)
	}
	check("after discard", 1, first.ID)

	r, res := s.send(first.ID, map[string]any{})
	if r.Code != http.StatusOK {
		t.Fatalf("send: %d %s", r.Code, r.Body)
	}
	check("while sending (undo window)", 0, "")
	if r, _ := s.cancel(res.OutboundID); r.Code != http.StatusOK {
		t.Fatalf("undo send: %d %s", r.Code, r.Body)
	}
	check("after undo send", 1, first.ID)

	// An empty reply goes with the empty-draft GC.
	if r := serve(s.web, s.as(req{method: "DELETE", path: "/drafts/" + first.ID})); r.Code != http.StatusNoContent {
		t.Fatalf("discard first: %d %s", r.Code, r.Body)
	}
	check("all discarded", 0, "")
	var empty api.Draft
	serve(s.web, s.as(req{method: "POST", path: "/drafts", body: map[string]any{"replyToMessageId": id36("m_", origID)}})).json(&empty)
	check("empty reply", 1, empty.ID)
	if _, err := s.pool.Exec(ctx, `UPDATE drafts SET to_addrs = '[]', subject = '', body_html = '' WHERE message_id = $1`, idOf("d_", empty.ID)); err != nil {
		t.Fatal(err)
	}
	if n, err := s.drafts.PurgeEmptyDrafts(ctx, time.Now().Add(drafts.EmptyDraftMaxAge+time.Hour)); err != nil || n != 1 {
		t.Fatalf("purge: %d, %v", n, err)
	}
	check("after empty-draft GC", 0, "")

	// Sent for good: the message is SENT, no draft is left.
	d := s.draft(origID, "reply", recipients([]string{"jan@firma.pl"}, nil, nil))
	check("new draft", 1, d.ID)
	r, res = s.send(d.ID, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("send again: %d %s", r.Code, r.Body)
	}
	if err := s.drafts.SendOutbound(ctx, idOf("ob_", res.OutboundID)); err != nil {
		t.Fatal(err)
	}
	check("after send", 0, "")
}
