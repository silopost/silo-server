package api_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/http/api"
)

func TestScheduledThreads(t *testing.T) {
	s := newSendEnv(t)
	d := s.draft(0, "", map[string]any{"subject": "Later", "bodyHtml": "<p>x</p>", "to": []map[string]string{{"email": "jan@firma.pl"}}})
	at := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	r, res := s.send(d.ID, map[string]any{"sendAt": at.Format(time.RFC3339)})
	if r.Code != http.StatusAccepted && r.Code != http.StatusOK {
		t.Fatalf("send: %d %s", r.Code, r.Body)
	}
	var list api.ThreadList
	serve(s.web, s.as(req{method: "GET", path: "/threads?labelId=SCHEDULED"})).json(&list)
	if len(list.Threads) != 1 || list.Threads[0].ScheduledAt == nil || !list.Threads[0].ScheduledAt.Equal(at) ||
		list.Threads[0].OutboundID == nil || *list.Threads[0].OutboundID != res.OutboundID || list.Threads[0].DraftID != nil {
		t.Fatalf("Scheduled list: %+v", list.Threads)
	}
	var detail api.ThreadDetail
	serve(s.web, s.as(req{method: "GET", path: "/threads/" + list.Threads[0].ID})).json(&detail)
	if detail.ScheduledAt == nil || detail.OutboundID == nil {
		t.Errorf("thread detail: %+v", detail)
	}
	if r, _ := s.cancel(res.OutboundID); r.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", r.Code, r.Body)
	}
	serve(s.web, s.as(req{method: "GET", path: "/threads/" + list.Threads[0].ID})).json(&detail)
	if detail.ScheduledAt != nil || detail.OutboundID != nil || !slices.Contains(detail.LabelIDs, "DRAFT") {
		t.Errorf("thread after cancel: %+v", detail)
	}
}
