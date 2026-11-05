package api_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
	"github.com/silopost/silo-server/internal/testutil/smtptest"
	"github.com/silopost/silo-server/internal/worker"
)

// sendEnv is a mail environment whose account submits to an in-process SMTP
// server that accepts mail.
type sendEnv struct {
	*mailEnv
	smtp *smtptest.Server
}

func newSendEnv(t *testing.T) *sendEnv {
	m := newMailEnv(t)
	srv := smtptest.StartServer(t, imaptest.User, imaptest.Pass, 0)
	if _, err := m.pool.Exec(context.Background(), `UPDATE accounts SET smtp_host = $2, smtp_port = $3, smtp_tls = 'none' WHERE id = $1`,
		m.accID, srv.Host, srv.Port); err != nil {
		t.Fatal(err)
	}
	return &sendEnv{mailEnv: m, smtp: srv}
}

// draft creates a draft (a reply to replyTo when not 0) and saves body.
func (s *sendEnv) draft(replyTo int64, mode string, body map[string]any) api.Draft {
	s.t.Helper()
	create := map[string]any{}
	if replyTo != 0 {
		create["replyToMessageId"] = id36("m_", replyTo)
		create["mode"] = mode
	}
	var d api.Draft
	r := serve(s.web, s.as(req{method: "POST", path: "/drafts", body: create}))
	r.json(&d)
	if r.Code != http.StatusCreated {
		s.t.Fatalf("create draft: %d %s", r.Code, r.Body)
	}
	body["version"] = d.Version
	r = serve(s.web, s.as(req{method: "PUT", path: "/drafts/" + d.ID, body: body}))
	r.json(&d)
	if r.Code != http.StatusOK {
		s.t.Fatalf("save draft: %d %s", r.Code, r.Body)
	}
	return d
}

func (s *sendEnv) send(id string, body any) (resp, api.SendResult) {
	s.t.Helper()
	var out api.SendResult
	r := serve(s.web, s.as(req{method: "POST", path: "/drafts/" + id + "/send", body: body, headers: map[string]string{"X-Silo-Client": "tab-s"}}))
	r.json(&out)
	return r, out
}

func (s *sendEnv) cancel(id string) (resp, api.Draft) {
	s.t.Helper()
	var out api.Draft
	r := serve(s.web, s.as(req{method: "POST", path: "/outbound/" + id + "/cancel", headers: map[string]string{"X-Silo-Client": "tab-s"}}))
	r.json(&out)
	return r, out
}

func (s *sendEnv) labels(msgID int64) []string {
	s.t.Helper()
	rows, err := s.pool.Query(context.Background(), `
SELECT l.system_key FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE ml.message_id = $1 AND l.system_key IS NOT NULL ORDER BY 1`, msgID)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		out = append(out, k)
	}
	return out
}

func idOf(prefix, s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimPrefix(s, prefix), 36, 64)
	return n
}

func recipients(to, cc, bcc []string) map[string]any {
	l := func(xs []string) []map[string]string {
		out := []map[string]string{}
		for _, x := range xs {
			out = append(out, map[string]string{"email": x})
		}
		return out
	}
	return map[string]any{"to": l(to), "cc": l(cc), "bcc": l(bcc), "subject": "Re: Oferta", "bodyHtml": "<p>Odpowiedź</p>"}
}

// TestSendAndUndo covers POST /drafts/{id}/send and /outbound/{id}/cancel
// (plan 3.9 steps 1-2) and the send job's success path (step 5).
func TestSendAndUndo(t *testing.T) {
	s := newSendEnv(t)
	ctx := context.Background()
	origID, origThread := s.ingest(1, "From: Jan Kowalski <jan@firma.pl>\r\nTo: silo@silo.test\r\nSubject: Oferta\r\n"+
		"Message-ID: <orig@firma.pl>\r\nReferences: <older@firma.pl>\r\n\r\n")
	other := newUserSession(t, s.env, "other-send@silo.test")
	asOther := func(c req) req { c.cookie, c.csrf = other.cookie, other.csrf; return c }

	// ---- validation
	empty := s.draft(0, "", recipients(nil, nil, nil))
	if r, _ := s.send(empty.ID, nil); r.Code != http.StatusUnprocessableEntity || r.code() != api.CodeNoRecipients {
		t.Errorf("no recipients: %d %s", r.Code, r.Body)
	}
	bad := s.draft(0, "", recipients([]string{"jan@@firma.pl"}, nil, nil))
	if r, _ := s.send(bad.ID, nil); r.Code != http.StatusUnprocessableEntity || r.code() != api.CodeInvalidAddress {
		t.Errorf("invalid address: %d %s", r.Code, r.Body)
	} else {
		var p api.Problem
		r.json(&p)
		if p.Address == nil || *p.Address != "jan@@firma.pl" || p.Field == nil || *p.Field != "to" {
			t.Errorf("invalid address fields: %s", r.Body)
		}
	}
	badBcc := s.draft(0, "", recipients([]string{"jan@firma.pl"}, nil, []string{"x@y@z"}))
	if r, _ := s.send(badBcc.ID, nil); !strings.Contains(r.Body.String(), `"field":"bcc"`) || !strings.Contains(r.Body.String(), `"address":"x@y@z"`) {
		t.Errorf("invalid bcc address: %d %s", r.Code, r.Body)
	}
	var up api.UploadInfo
	upload(s.web, s.l, "big.bin", bytes.Repeat([]byte{3}, testMaxUpload)).json(&up)
	bigBody := recipients([]string{"jan@firma.pl"}, nil, nil)
	bigBody["attachmentIds"] = []string{up.ID}
	big := s.draft(0, "", bigBody)
	if r, _ := s.send(big.ID, nil); r.Code != http.StatusRequestEntityTooLarge || r.code() != api.CodeMessageTooLarge {
		t.Errorf("too large: %d %s", r.Code, r.Body)
	} else {
		var p api.Problem
		r.json(&p)
		if p.Limit == nil || *p.Limit != testMaxUpload || p.Size == nil || *p.Size <= testMaxUpload {
			t.Errorf("too large: limit/size: %s", r.Body)
		}
	}
	if n := s.count(`SELECT count(*) FROM outbound`); n != 0 {
		t.Errorf("outbound rows after refused sends: %d", n)
	}
	if r, _ := s.send("d_zzzz", nil); r.Code != http.StatusNotFound {
		t.Errorf("unknown draft: %d", r.Code)
	}

	// ---- send: queued, SCHEDULED, undo window
	d := s.draft(origID, "reply", recipients([]string{"jan@firma.pl"}, []string{"anna@firma.pl"}, []string{"tajny@firma.pl"}))
	msgID := idOf("d_", d.ID)
	if r := serve(s.web, asOther(req{method: "POST", path: "/drafts/" + d.ID + "/send"})); r.Code != http.StatusNotFound {
		t.Errorf("other user's send: %d", r.Code)
	}
	start := time.Now()
	r, res := s.send(d.ID, map[string]any{})
	if r.Code != http.StatusOK || res.MessageID != "m_"+strings.TrimPrefix(d.ID, "d_") || !strings.HasPrefix(res.OutboundID, "ob_") ||
		res.UndoUntil.Before(start.Add(4*time.Second)) || res.UndoUntil.After(time.Now().Add(6*time.Second)) {
		t.Fatalf("send: %d %s", r.Code, r.Body)
	}
	obID := idOf("ob_", res.OutboundID)
	if n := s.count(`SELECT count(*) FROM outbound WHERE id = $1 AND status = 'queued' AND message_id = $2`, obID, msgID); n != 1 {
		t.Error("outbound not queued")
	}
	if n := s.count(`SELECT count(*) FROM jobs WHERE kind = 'send' AND account_id IS NULL AND (payload->>'outboundId')::bigint = $1 AND run_at > now()`, obID); n != 1 {
		t.Error("no send job in the undo window")
	}
	if l := s.labels(msgID); !slices.Equal(l, []string{"SCHEDULED"}) {
		t.Errorf("labels while queued = %v", l)
	}
	if n := s.count(`SELECT count(*) FROM thread_labels tl JOIN labels l ON l.id = tl.label_id WHERE tl.thread_id = $1 AND l.system_key = 'SCHEDULED'`, origThread); n != 1 {
		t.Error("thread not SCHEDULED")
	}
	if n := s.count(`SELECT count(*) FROM events WHERE type = 'outbound.status' AND payload->>'status' = 'queued' AND payload->>'origin' = 'tab-s'
		AND payload->>'draftId' = payload->>'messageId'`); n != 1 {
		t.Error("no outbound.status queued event with origin")
	}
	if n := s.count(`SELECT count(*) FROM events WHERE type = 'thread.updated' AND payload->>'origin' = 'tab-s'`); n == 0 {
		t.Error("no thread.updated with origin")
	}
	if r := serve(s.web, s.as(req{method: "GET", path: "/drafts/" + d.ID})); r.Code != http.StatusNotFound {
		t.Errorf("GET of a queued draft: %d", r.Code)
	}
	putBody := recipients([]string{"x@firma.pl"}, nil, nil)
	putBody["version"] = d.Version
	if r := serve(s.web, s.as(req{method: "PUT", path: "/drafts/" + d.ID, body: putBody})); r.Code != http.StatusNotFound {
		t.Errorf("PUT of a queued draft: %d", r.Code)
	}
	if r, _ := s.send(d.ID, nil); r.Code != http.StatusConflict || r.code() != api.CodeSendInProgress {
		t.Errorf("second send: %d %s", r.Code, r.Body)
	}

	// ---- cancel in the window: the same draft comes back
	if r := serve(s.web, asOther(req{method: "POST", path: "/outbound/" + res.OutboundID + "/cancel"})); r.Code != http.StatusNotFound {
		t.Errorf("other user's cancel: %d", r.Code)
	}
	r, back := s.cancel(res.OutboundID)
	if r.Code != http.StatusOK || back.ID != d.ID || back.Version != d.Version || back.Subject != d.Subject || back.BodyHTML != d.BodyHTML ||
		len(back.Bcc) != 1 || back.Bcc[0].Email != "tajny@firma.pl" {
		t.Fatalf("cancel: %d %s", r.Code, r.Body)
	}
	if n := s.count(`SELECT count(*) FROM outbound WHERE id = $1 AND status = 'cancelled'`, obID); n != 1 {
		t.Error("outbound not cancelled")
	}
	if n := s.count(`SELECT count(*) FROM jobs WHERE kind = 'send'`); n != 0 {
		t.Error("send job left after cancel")
	}
	if n := s.count(`SELECT count(*) FROM messages WHERE id = $1 AND raw_blob_sha256 IS NULL`, msgID); n != 1 {
		t.Error("stored body not released")
	}
	if l := s.labels(msgID); !slices.Equal(l, []string{"DRAFT"}) {
		t.Errorf("labels after cancel = %v", l)
	}
	if r, again := s.cancel(res.OutboundID); r.Code != http.StatusOK || again.Version != d.Version {
		t.Errorf("second cancel: %d %s", r.Code, r.Body)
	}

	// ---- send again; a claimed job cannot be cancelled
	r, res2 := s.send(d.ID, nil)
	if r.Code != http.StatusOK || res2.OutboundID != res.OutboundID {
		t.Fatalf("send again: %d %s", r.Code, r.Body)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE jobs SET locked_until = now() + interval '1 minute' WHERE kind = 'send'`); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.cancel(res.OutboundID); r.Code != http.StatusConflict || r.code() != api.CodeSendInProgress {
		t.Errorf("cancel of a claimed send: %d %s", r.Code, r.Body)
	}
	if r, _ := s.cancel("ob_zzzz"); r.Code != http.StatusNotFound {
		t.Errorf("unknown outbound: %d", r.Code)
	}

	// ---- the send job delivers
	if err := s.drafts.SendOutbound(ctx, obID); err != nil {
		t.Fatal(err)
	}
	msgs := s.smtp.Messages()
	if len(msgs) != 1 {
		t.Fatalf("delivered messages = %d", len(msgs))
	}
	got := msgs[0]
	slices.Sort(got.To)
	if got.From != "silo@silo.test" || !slices.Equal(got.To, []string{"anna@firma.pl", "jan@firma.pl", "tajny@firma.pl"}) {
		t.Errorf("envelope: %s -> %v", got.From, got.To)
	}
	var msgIDHdr string
	if err := s.pool.QueryRow(ctx, `SELECT message_id_hdr FROM messages WHERE id = $1`, msgID).Scan(&msgIDHdr); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Message-Id: <" + msgIDHdr + ">", "In-Reply-To: <orig@firma.pl>", "References: <older@firma.pl> <orig@firma.pl>",
		"Date: ", "anna@firma.pl"} {
		if !bytes.Contains(got.Data, []byte(want)) {
			t.Errorf("sent message lacks %q:\n%s", want, got.Data)
		}
	}
	for _, not := range []string{"Bcc:", "tajny", "X-Silo-Draft-Id"} {
		if bytes.Contains(got.Data, []byte(not)) {
			t.Errorf("sent message contains %q", not)
		}
	}
	if n := s.count(`SELECT count(*) FROM outbound WHERE id = $1 AND status = 'sent' AND smtp_response LIKE '250 %' AND attempts = 1`, obID); n != 1 {
		t.Error("outbound not sent")
	}
	if l := s.labels(msgID); !slices.Equal(l, []string{"SENT"}) {
		t.Errorf("labels after sending = %v", l)
	}
	if n := s.count(`SELECT count(*) FROM drafts WHERE message_id = $1`, msgID); n != 0 {
		t.Error("drafts row left after sending")
	}
	if n := s.count(`SELECT count(*) FROM jobs j JOIN op_messages om ON om.job_id = j.id
	                 WHERE om.message_id = $1 AND j.payload->>'op' = 'flags' AND j.payload->'add' ? '\Answered'`, origID); n != 1 {
		t.Error("no \\Answered on the original")
	}
	sentAppend := s.count(`SELECT count(*) FROM accounts WHERE id = $1 AND sent_append`, s.accID) == 1
	appendSent := s.count(`SELECT count(*) FROM jobs j JOIN op_messages om ON om.job_id = j.id
	                      WHERE om.message_id = $1 AND j.payload->>'op' = 'append' AND j.payload->>'toRole' = 'sent'`, msgID)
	if !sentAppend || appendSent != 1 {
		t.Errorf("generic account sent_append = %v, append jobs to Sent = %d", sentAppend, appendSent)
	}
	if n := s.count(`SELECT count(*) FROM contacts WHERE email IN ('jan@firma.pl', 'anna@firma.pl', 'tajny@firma.pl') AND sent_count = 1`); n != 3 {
		t.Errorf("contacts with sent_count = %d", n)
	}
	if n := s.count(`SELECT count(*) FROM events WHERE type = 'outbound.status' AND payload->>'status' = 'sent'`); n != 1 {
		t.Error("no outbound.status sent event")
	}
	if r, _ := s.cancel(res.OutboundID); r.Code != http.StatusConflict {
		t.Errorf("cancel after sending: %d", r.Code)
	}
	if err := s.drafts.SendOutbound(ctx, obID); err != nil || len(s.smtp.Messages()) != 1 {
		t.Errorf("a second run of the job sent again: %v", err)
	}

	// ---- forward: $Forwarded
	fwd := s.draft(origID, "forward", recipients([]string{"ola@firma.pl"}, nil, nil))
	_, fres := s.send(fwd.ID, nil)
	if err := s.drafts.SendOutbound(ctx, idOf("ob_", fres.OutboundID)); err != nil {
		t.Fatal(err)
	}
	if n := s.count(`SELECT count(*) FROM jobs j JOIN op_messages om ON om.job_id = j.id
	                 WHERE om.message_id = $1 AND j.payload->>'op' = 'flags' AND j.payload->'add' ? '$Forwarded'`, origID); n != 1 {
		t.Error("no $Forwarded on the original")
	}
}

// TestSendFailures covers plan 3.9 step 6 with the send job.
func TestSendFailures(t *testing.T) {
	s := newSendEnv(t)
	ctx := context.Background()
	queue := func(to ...string) (msgID, obID int64) {
		t.Helper()
		d := s.draft(0, "", recipients(to, nil, nil))
		r, res := s.send(d.ID, nil)
		if r.Code != http.StatusOK {
			t.Fatalf("send: %d %s", r.Code, r.Body)
		}
		return idOf("d_", d.ID), idOf("ob_", res.OutboundID)
	}

	// 5xx for the whole message: failed at once, a draft again.
	s.smtp.RejectData(554)
	msgID, obID := queue("jan@firma.pl")
	if err := s.drafts.SendOutbound(ctx, obID); err != nil {
		t.Fatal(err)
	}
	if n := s.count(`SELECT count(*) FROM outbound WHERE id = $1 AND status = 'failed' AND last_error LIKE '%554%'`, obID); n != 1 {
		t.Error("5xx: outbound not failed")
	}
	if l := s.labels(msgID); !slices.Equal(l, []string{"DRAFT"}) {
		t.Errorf("5xx: labels = %v", l)
	}
	var back api.Draft
	if r := serve(s.web, s.as(req{method: "GET", path: "/drafts/" + id36("d_", msgID)})); r.Code != http.StatusOK {
		t.Errorf("5xx: draft not back: %d", r.Code)
	} else if r.json(&back); back.LastSend == nil || back.LastSend.Status != "failed" || back.LastSend.OutboundID != id36("ob_", obID) ||
		!strings.Contains(back.LastSend.Error, "554") || back.LastSend.FailedRecipients == nil {
		t.Errorf("5xx: lastSend = %+v", back.LastSend)
	}
	if n := s.count(`SELECT count(*) FROM events WHERE type = 'outbound.status' AND payload->>'status' = 'failed' AND payload->>'error' LIKE '%554%'`); n != 1 {
		t.Error("5xx: no outbound.status failed event")
	}

	// 4xx: retried with the configured delays, then failed.
	s.smtp.RejectData(451)
	msgID, obID = queue("jan@firma.pl")
	for attempt := 1; attempt <= 3; attempt++ {
		err := s.drafts.SendOutbound(ctx, obID)
		var de *worker.DeferError
		if attempt <= 2 {
			if !errors.As(err, &de) || time.Until(de.Until) < 0 {
				t.Fatalf("4xx attempt %d: %v", attempt, err)
			}
			if n := s.count(`SELECT count(*) FROM outbound WHERE id = $1 AND status = 'queued' AND attempts = $2 AND last_error LIKE '%451%'`, obID, attempt); n != 1 {
				t.Errorf("4xx attempt %d: outbound not queued for a retry", attempt)
			}
			if l := s.labels(msgID); !slices.Equal(l, []string{"SCHEDULED"}) {
				t.Errorf("4xx attempt %d: labels = %v", attempt, l)
			}
		} else if err != nil {
			t.Fatalf("4xx last attempt: %v", err)
		}
	}
	if n := s.count(`SELECT count(*) FROM outbound WHERE id = $1 AND status = 'failed' AND attempts = 3`, obID); n != 1 {
		t.Error("4xx: outbound not failed after the retries")
	}
	if len(s.smtp.Messages()) != 0 {
		t.Error("refused messages were recorded")
	}

	// 5xx on one RCPT: the others get it, failed_recipients.
	s.smtp.RejectData(0)
	s.smtp.RejectRcpt("nobody@firma.pl", 550)
	msgID, obID = queue("jan@firma.pl", "nobody@firma.pl")
	if err := s.drafts.SendOutbound(ctx, obID); err != nil {
		t.Fatal(err)
	}
	if msgs := s.smtp.Messages(); len(msgs) != 1 || !slices.Equal(msgs[0].To, []string{"jan@firma.pl"}) {
		t.Errorf("partial: delivered %+v", msgs)
	}
	if n := s.count(`SELECT count(*) FROM outbound WHERE id = $1 AND status = 'sent' AND failed_recipients = '{nobody@firma.pl}'`, obID); n != 1 {
		t.Error("partial: failed_recipients not recorded")
	}
	if l := s.labels(msgID); !slices.Equal(l, []string{"SENT"}) {
		t.Errorf("partial: labels = %v", l)
	}

	// Every recipient refused: failed.
	_, obID = queue("nobody@firma.pl")
	if err := s.drafts.SendOutbound(ctx, obID); err != nil {
		t.Fatal(err)
	}
	if n := s.count(`SELECT count(*) FROM outbound WHERE id = $1 AND status = 'failed'`, obID); n != 1 {
		t.Error("all recipients refused: not failed")
	}
}

// TestContactsAutocomplete covers GET /contacts/autocomplete (plan 5.1).
func TestContactsAutocomplete(t *testing.T) {
	m := newMailEnv(t)
	ctx := context.Background()
	if _, err := m.pool.Exec(ctx, `
INSERT INTO contacts (user_id, email, name, sent_count, received_count, last_interaction_at)
SELECT a.user_id, c.email, c.name, c.sent, 0, now() - make_interval(mins => c.age)
FROM accounts a, (VALUES ('jan@firma.pl', 'Jan Kowalski', 1, 10), ('janina@firma.pl', 'Janina Nowak', 5, 50),
                         ('jacek@firma.pl', '', 0, 1), ('anna@firma.pl', 'Anna Jankowska', 1, 5)) AS c(email, name, sent, age)
WHERE a.id = $1`, m.accID); err != nil {
		t.Fatal(err)
	}
	other := newUserSession(t, m.env, "other-contacts@silo.test")
	if _, err := m.pool.Exec(ctx, `INSERT INTO contacts (user_id, email, sent_count) SELECT id, 'janusz@firma.pl', 99 FROM users WHERE email = 'other-contacts@silo.test'`); err != nil {
		t.Fatal(err)
	}
	get := func(q string) []string {
		t.Helper()
		var out api.ContactSuggestions
		r := serve(m.web, m.as(req{method: "GET", path: "/contacts/autocomplete?q=" + q}))
		r.json(&out)
		if r.Code != http.StatusOK {
			t.Fatalf("autocomplete %q: %d %s", q, r.Code, r.Body)
		}
		var emails []string
		for _, c := range out.Contacts {
			emails = append(emails, c.Email)
		}
		return emails
	}
	if got := get("jan"); !slices.Equal(got, []string{"janina@firma.pl", "anna@firma.pl", "jan@firma.pl"}) {
		t.Errorf("jan: %v", got)
	}
	if got := get("NOWAK"); !slices.Equal(got, []string{"janina@firma.pl"}) {
		t.Errorf("NOWAK: %v", got)
	}
	if got := get("ja%25"); len(got) != 0 {
		t.Errorf("wildcard: %v", got)
	}
	if r := serve(m.web, m.as(req{method: "GET", path: "/contacts/autocomplete?q="})); r.Code != http.StatusUnprocessableEntity && r.Code != http.StatusBadRequest {
		t.Errorf("empty q: %d", r.Code)
	}
	for i := 0; i < 12; i++ {
		if _, err := m.pool.Exec(ctx, `INSERT INTO contacts (user_id, email) SELECT user_id, $2 FROM accounts WHERE id = $1`, m.accID, "zz"+strconv.Itoa(i)+"@x.pl"); err != nil {
			t.Fatal(err)
		}
	}
	if got := get("zz"); len(got) != 8 {
		t.Errorf("limit: %d", len(got))
	}
	_ = other
}
