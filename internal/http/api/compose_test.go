package api_test

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/http/api"
)

// upload posts one file as multipart/form-data.
func upload(h http.Handler, l login, filename string, data []byte) resp {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.WriteField("purpose", "attachment")
	fw, _ := w.CreateFormFile("file", filename)
	_, _ = fw.Write(data)
	_ = w.Close()
	r := httptest.NewRequest("POST", "/api/v1/uploads", &b)
	r.Header.Set("Content-Type", w.FormDataContentType())
	r.AddCookie(l.cookie)
	r.Header.Set(api.CSRFHeader, l.csrf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return resp{rec}
}

func TestDraftsAndUploads(t *testing.T) {
	m := newMailEnv(t)
	ctx := context.Background()
	origID, origThread := m.ingest(1, "From: Jan Kowalski <jan@firma.pl>\r\nTo: silo@silo.test, ewa@firma.pl\r\nCc: anna@firma.pl\r\n"+
		"Subject: Oferta\r\nMessage-ID: <orig@firma.pl>\r\nReferences: <older@firma.pl>\r\n\r\n")
	other := newUserSession(t, m.env, "other-drafts@silo.test")
	asOther := func(c req) req { c.cookie, c.csrf = other.cookie, other.csrf; return c }

	// ---- uploads
	pdf := bytes.Repeat([]byte("%PDF-1.4 "), 100)
	var up api.UploadInfo
	r := upload(m.web, m.l, "C:\\docs\\umowa.pdf", pdf)
	r.json(&up)
	if r.Code != http.StatusCreated || !strings.HasPrefix(up.ID, "u_") || up.Filename != "umowa.pdf" || up.Size != int64(len(pdf)) ||
		up.ContentType != "application/octet-stream" && up.ContentType != "application/pdf" {
		t.Fatalf("upload: %d %s", r.Code, r.Body)
	}
	upID, _ := strconv.ParseInt(strings.TrimPrefix(up.ID, "u_"), 36, 64)
	if n := m.count(`SELECT count(*) FROM attachments a JOIN blobs b ON b.sha256 = a.blob_sha256 WHERE a.id = $1 AND a.message_id IS NULL AND b.kind = 'local' AND b.ref_count = 1`, upID); n != 1 {
		t.Error("upload not recorded as an unpinned local blob")
	}
	before := m.count(`SELECT count(*) FROM attachments`)
	r = upload(m.web, m.l, "big.bin", bytes.Repeat([]byte{1}, testMaxUpload+1))
	if r.Code != http.StatusRequestEntityTooLarge || r.code() != api.CodeUploadTooLarge {
		t.Errorf("too large: %d %s", r.Code, r.Body)
	} else {
		var p api.Problem
		r.json(&p)
		if p.Limit == nil || *p.Limit != testMaxUpload || p.Size == nil || *p.Size <= testMaxUpload {
			t.Errorf("too large: limit/size: %s", r.Body)
		}
	}
	if n := m.count(`SELECT count(*) FROM attachments`); n != before {
		t.Error("a too large upload left a row")
	}
	if r := upload(m.web, m.l, "exact.bin", bytes.Repeat([]byte{2}, testMaxUpload)); r.Code != http.StatusCreated {
		t.Errorf("upload at the limit: %d %s", r.Code, r.Body)
	}
	if r := serve(m.web, m.as(req{method: "POST", path: "/uploads", body: map[string]string{"x": "y"}})); r.Code != http.StatusBadRequest {
		t.Errorf("json body: %d %s", r.Code, r.Body)
	}
	var otherUp api.UploadInfo
	upload(m.web, other, "secret.txt", []byte("other user")).json(&otherUp)

	// ---- reply all: prefilled, same thread, DRAFT label
	var dr api.Draft
	r = serve(m.web, m.as(req{method: "POST", path: "/drafts", headers: map[string]string{"X-Silo-Client": "tab-d"},
		body: map[string]any{"replyToMessageId": id36("m_", origID), "mode": "replyAll"}}))
	r.json(&dr)
	emails := func(l []api.Address) []string {
		var out []string
		for _, a := range l {
			out = append(out, a.Email)
		}
		return out
	}
	if r.Code != http.StatusCreated || !strings.HasPrefix(dr.ID, "d_") || dr.ThreadID != id36("t_", origThread) || dr.Subject != "Re: Oferta" ||
		!slices.Equal(emails(dr.To), []string{"jan@firma.pl", "ewa@firma.pl"}) || !slices.Equal(emails(dr.Cc), []string{"anna@firma.pl"}) ||
		dr.InReplyToMessageID == nil || *dr.InReplyToMessageID != id36("m_", origID) || dr.Version != 1 || dr.SyncedVersion != 0 ||
		dr.MessageID != "m_"+strings.TrimPrefix(dr.ID, "d_") || !strings.Contains(dr.BodyHTML, "<blockquote>") {
		t.Fatalf("create reply all: %d %s", r.Code, r.Body)
	}
	draftMsg, _ := strconv.ParseInt(strings.TrimPrefix(dr.ID, "d_"), 36, 64)
	hasLabel := func(table, col string, id int64, key string) bool {
		return m.count(`SELECT count(*) FROM `+table+` x JOIN labels l ON l.id = x.label_id WHERE x.`+col+` = $1 AND l.system_key = $2`, id, key) == 1
	}
	if !hasLabel("message_labels", "message_id", draftMsg, "DRAFT") || !hasLabel("thread_labels", "thread_id", origThread, "DRAFT") ||
		!hasLabel("thread_labels", "thread_id", origThread, "INBOX") {
		t.Error("draft not labelled DRAFT in the thread of the original")
	}
	// The draft is found in the thread and in the list without mapping ids.
	var td api.ThreadDetail
	serve(m.web, m.as(req{method: "GET", path: "/threads/" + id36("t_", origThread)})).json(&td)
	if len(td.Messages) != 2 || td.Messages[0].DraftID != nil || td.Messages[1].DraftID == nil || *td.Messages[1].DraftID != dr.ID {
		t.Errorf("thread messages draftId: %+v", td.Messages)
	}
	var list api.ThreadList
	serve(m.web, m.as(req{method: "GET", path: "/threads?labelId=DRAFT"})).json(&list)
	if len(list.Threads) != 1 || list.Threads[0].DraftID == nil || *list.Threads[0].DraftID != dr.ID {
		t.Errorf("thread list draftId: %+v", list.Threads)
	}
	if n := m.count(`SELECT count(*) FROM messages WHERE id = $1 AND in_reply_to_hdr = 'orig@firma.pl' AND NOT is_unread AND body_state = 'local'`, draftMsg); n != 1 {
		t.Error("draft message not a read local reply")
	}
	if n := m.count(`SELECT count(*) FROM jobs WHERE kind = 'imap_op'`); n != 0 {
		t.Errorf("jobs after create: %d", n)
	}
	var reply api.Draft
	serve(m.web, m.as(req{method: "POST", path: "/drafts", body: map[string]any{"replyToMessageId": id36("m_", origID)}})).json(&reply)
	if !slices.Equal(emails(reply.To), []string{"jan@firma.pl"}) || len(reply.Cc) != 0 {
		t.Errorf("reply recipients: %+v", reply)
	}
	var fwd api.Draft
	serve(m.web, m.as(req{method: "POST", path: "/drafts", body: map[string]any{"replyToMessageId": id36("m_", origID), "mode": "forward"}})).json(&fwd)
	if fwd.Subject != "Fwd: Oferta" || len(fwd.To) != 0 || fwd.ThreadID == dr.ThreadID || !strings.Contains(fwd.BodyHTML, "Forwarded message") {
		t.Errorf("forward: %+v", fwd)
	}

	// ---- save
	put := func(h func(req) req, id string, body map[string]any) resp {
		return serve(m.web, h(req{method: "PUT", path: "/drafts/" + id, body: body, headers: map[string]string{"X-Silo-Client": "tab-d"}}))
	}
	body := map[string]any{
		"version": 1, "to": []map[string]string{{"name": "Jan", "email": "Jan@Firma.pl"}}, "bcc": []map[string]string{{"email": "boss@firma.pl"}},
		"subject": "Re: Oferta (v2)", "bodyHtml": "<p>Dziękuję za ofertę.</p>", "bodyJson": map[string]any{"type": "doc"},
		"attachmentIds": []string{up.ID},
	}
	r = put(m.as, dr.ID, body)
	r.json(&dr)
	if r.Code != http.StatusOK || dr.Version != 2 || dr.Subject != "Re: Oferta (v2)" || len(dr.Attachments) != 1 || dr.Attachments[0].ID != up.ID ||
		!slices.Equal(emails(dr.To), []string{"jan@firma.pl"}) || !slices.Equal(emails(dr.Bcc), []string{"boss@firma.pl"}) || len(dr.Cc) != 0 ||
		string(dr.BodyJSON) != `{"type":"doc"}` {
		t.Fatalf("save: %d %s", r.Code, r.Body)
	}
	if n := m.count(`SELECT count(*) FROM attachments WHERE id = $1 AND message_id = $2`, upID, draftMsg); n != 1 {
		t.Error("upload not pinned to the draft")
	}
	if n := m.count(`SELECT count(*) FROM messages WHERE id = $1 AND subject = 'Re: Oferta (v2)' AND snippet = 'Dziękuję za ofertę.' AND has_attachments`, draftMsg); n != 1 {
		t.Error("draft message not updated")
	}
	appendJobs := `SELECT count(*) FROM jobs j JOIN op_messages om ON om.job_id = j.id WHERE om.message_id = $1 AND j.payload->>'op' = 'append' AND j.run_at > now() + interval '30 seconds'`
	if n := m.count(appendJobs, draftMsg); n != 1 {
		t.Errorf("throttled append jobs = %d", n)
	}
	if n := m.count(`SELECT count(*) FROM events WHERE type = 'draft.updated' AND payload->>'origin' = 'tab-d' AND (payload->>'version')::int = 2`); n != 1 {
		t.Error("no draft.updated with origin")
	}
	if n := m.count(`SELECT count(*) FROM threads WHERE id = $1 AND pending_ops = 0 AND snippet = 'Dziękuję za ofertę.'`, origThread); n != 1 {
		t.Error("thread not refreshed (or a saved draft shows as pending)")
	}

	// Stale version: conflict; the save is not applied.
	r = put(m.as, dr.ID, body)
	if r.Code != http.StatusConflict || r.code() != api.CodeDraftVersionConflict {
		t.Errorf("stale version: %d %s", r.Code, r.Body)
	} else {
		var p api.Problem
		r.json(&p)
		if p.CurrentVersion == nil || *p.CurrentVersion != 2 || p.Current == nil || p.Current.ID != dr.ID || p.Current.Version != 2 ||
			p.Current.Subject != "Re: Oferta (v2)" || len(p.Current.Attachments) != 1 {
			t.Errorf("conflict without the current draft: %s", r.Body)
		}
	}
	body["version"], body["subject"], body["attachmentIds"] = 2, "Re: Oferta (v3)", []string{}
	r = put(m.as, dr.ID, body)
	r.json(&dr)
	if r.Code != http.StatusOK || dr.Version != 3 || len(dr.Attachments) != 0 {
		t.Fatalf("second save: %d %s", r.Code, r.Body)
	}
	if n := m.count(appendJobs, draftMsg); n != 1 {
		t.Errorf("append jobs after the second save = %d (want one, postponed)", n)
	}
	if n := m.count(`SELECT count(*) FROM attachments WHERE id = $1 AND message_id IS NULL`, upID); n != 1 {
		t.Error("detached upload not unpinned")
	}
	body["version"], body["attachmentIds"] = 3, []string{up.ID}
	if r := put(m.as, dr.ID, body); r.Code != http.StatusOK {
		t.Fatalf("third save: %d %s", r.Code, r.Body)
	}
	for name, ids := range map[string][]string{"other user's upload": {otherUp.ID}, "unknown upload": {"u_zzzz"}, "bad id": {"x"}} {
		body["version"], body["attachmentIds"] = 4, ids
		if r := put(m.as, dr.ID, body); r.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, r.Code, r.Body)
		}
	}
	body["version"], body["attachmentIds"] = 4, []string{up.ID}
	body["to"] = []map[string]string{{"email": "jan@firma.pl"}}
	body["cc"] = []map[string]string{{"email": "not-an-address"}}
	if r := put(m.as, dr.ID, body); r.Code != http.StatusBadRequest || r.code() != api.CodeValidation ||
		!strings.Contains(r.Body.String(), `"address":"not-an-address"`) || !strings.Contains(r.Body.String(), `"field":"cc"`) {
		t.Errorf("bad address: %d %s", r.Code, r.Body)
	}
	delete(body, "cc")

	var got api.Draft
	r = serve(m.web, m.as(req{method: "GET", path: "/drafts/" + dr.ID}))
	r.json(&got)
	if r.Code != http.StatusOK || got.Version != 4 || got.Subject != "Re: Oferta (v3)" || len(got.Attachments) != 1 || got.ServerChanged {
		t.Errorf("get: %d %s", r.Code, r.Body)
	}

	// Another user sees nothing.
	if r := serve(m.web, asOther(req{method: "GET", path: "/drafts/" + dr.ID})); r.Code != http.StatusNotFound {
		t.Errorf("other user GET: %d", r.Code)
	}
	body["to"] = []map[string]string{}
	if r := put(asOther, dr.ID, body); r.Code != http.StatusNotFound {
		t.Errorf("other user PUT: %d", r.Code)
	}
	if r := serve(m.web, asOther(req{method: "DELETE", path: "/drafts/" + dr.ID})); r.Code != http.StatusNotFound {
		t.Errorf("other user DELETE: %d", r.Code)
	}
	if r := serve(m.web, asOther(req{method: "POST", path: "/drafts", body: map[string]any{"replyToMessageId": id36("m_", origID)}})); r.Code != http.StatusNotFound {
		t.Errorf("other user reply: %d", r.Code)
	}
	if r := serve(m.web, m.as(req{method: "GET", path: "/drafts/" + id36("d_", origID)})); r.Code != http.StatusNotFound {
		t.Errorf("a message that is not a draft: %d", r.Code)
	}
	if r := serve(m.web, m.as(req{method: "POST", path: "/drafts", body: map[string]any{"mode": "reply"}})); r.Code != http.StatusBadRequest {
		t.Errorf("mode without message: %d", r.Code)
	}

	// ---- a new message: own thread, deleted with the draft
	var nd api.Draft
	r = serve(m.web, m.as(req{method: "POST", path: "/drafts", body: map[string]any{}}))
	r.json(&nd)
	if r.Code != http.StatusCreated || nd.ThreadID == dr.ThreadID || nd.Subject != "" || nd.InReplyToMessageID != nil || nd.AccountID != m.l.body.Accounts[0].ID {
		t.Fatalf("new draft: %d %s", r.Code, r.Body)
	}
	if r := serve(m.web, m.as(req{method: "DELETE", path: "/drafts/" + nd.ID})); r.Code != http.StatusNoContent {
		t.Fatalf("delete new draft: %d %s", r.Code, r.Body)
	}
	ndThread, _ := strconv.ParseInt(strings.TrimPrefix(nd.ThreadID, "t_"), 36, 64)
	if n := m.count(`SELECT count(*) FROM threads WHERE id = $1`, ndThread); n != 0 {
		t.Error("thread of a discarded new draft left behind")
	}
	if n := m.count(`SELECT count(*) FROM events WHERE type = 'thread.deleted' AND (payload->>'threadId')::bigint = $1`, ndThread); n != 1 {
		t.Error("no thread.deleted for the discarded draft")
	}

	// ---- discard the reply: thread keeps the original, upload released
	if r := serve(m.web, m.as(req{method: "DELETE", path: "/drafts/" + dr.ID})); r.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", r.Code, r.Body)
	}
	if r := serve(m.web, m.as(req{method: "GET", path: "/drafts/" + dr.ID})); r.Code != http.StatusNotFound {
		t.Errorf("get after delete: %d", r.Code)
	}
	if n := m.count(`SELECT count(*) FROM threads WHERE id = $1 AND message_count = 2`, origThread); n != 1 {
		t.Error("thread of the original not recomputed (original + the other reply draft)")
	}
	if n := m.count(`SELECT count(*) FROM jobs WHERE kind = 'imap_op'`); n != 0 {
		t.Errorf("jobs after discarding drafts that never reached the server: %d", n)
	}
	if n := m.count(`SELECT count(*) FROM attachments WHERE id = $1`, upID); n != 0 {
		t.Error("attachment of a discarded draft left behind")
	}
	if n := m.count(`SELECT count(*) FROM blobs WHERE kind = 'local' AND ref_count = 0`); n != 1 {
		t.Errorf("released local blobs = %d", n)
	}

	// A draft that reached the server: discarding it queues the removal.
	if _, err := m.pool.Exec(ctx, `UPDATE drafts SET server_synced_version = 1 WHERE message_id = $1`,
		mustID(t, reply.ID, "d_")); err != nil {
		t.Fatal(err)
	}
	if r := serve(m.web, m.as(req{method: "DELETE", path: "/drafts/" + reply.ID})); r.Code != http.StatusNoContent {
		t.Fatalf("delete synced draft: %d %s", r.Code, r.Body)
	}
	if n := m.count(`SELECT count(*) FROM jobs WHERE kind = 'imap_op' AND payload->>'op' = 'delete_draft' AND payload->>'draftId' LIKE 'd\_%'`); n != 1 {
		t.Errorf("delete_draft jobs = %d", n)
	}

	// ---- blob_gc: unpinned uploads older than 24 h and unreferenced blobs go.
	res, err := m.drafts.BlobGC(ctx, time.Now().Add(25*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploads != 2 || res.Blobs != 3 { // the exact-limit and the other user's uploads; their blobs and umowa.pdf's
		t.Errorf("blob gc: %+v", res)
	}
	if n := m.count(`SELECT count(*) FROM attachments WHERE message_id IS NULL`); n != 0 {
		t.Errorf("unpinned uploads after gc: %d", n)
	}
	if n := m.count(`SELECT count(*) FROM blobs`); n != 0 {
		t.Errorf("blobs after gc: %d", n)
	}
}

func mustID(t *testing.T, s, prefix string) int64 {
	t.Helper()
	id, err := strconv.ParseInt(strings.TrimPrefix(s, prefix), 36, 64)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
