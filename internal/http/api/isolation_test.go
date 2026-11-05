package api_test

import (
	"encoding/json"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

// owned are the ids of every kind a user has: the targets of the other user.
type owned struct {
	name     string
	l        login
	acc      string // acc_
	thread   string // t_ in INBOX, with an attachment
	msg      string // m_ of that thread
	att      string // a_
	folder   string // f_ (INBOX)
	label    string // l_ user label
	draft    string // d_
	upload   string // u_ (pinned to draft)
	op       string // op_ (archive of another thread, in its undo window)
	outbound string // ob_ (queued: undoSendSeconds 30)
	session  string // s_
}

// TestUserIsolation is the second half of the first DoD item of phase 5:
// two users on one instance (both mailboxes on one Dovecot), and no endpoint
// or event stream lets one see or touch what the other has. Every id of the
// other user is 404 exactly as an id that does not exist (no 403, no empty
// 200), on every endpoint that takes that kind of id, in both directions.
func TestUserIsolation(t *testing.T) {
	s := newFullStack(t, nil)
	addr := net.JoinHostPort(s.cfg.DefaultIMAPHost, strconv.Itoa(s.cfg.DefaultIMAPPort))
	now := time.Now().Truncate(time.Second)
	for i, u := range []struct{ user, pass string }{{imaptest.User, imaptest.Pass}, {imaptest.User2, imaptest.Pass2}} {
		c := imapUser(t, addr, u.user, u.pass)
		appendRaw(t, c, "INBOX", htmlMsg(500+10*i, "Z załącznikiem "+u.user, "tekst", "<p>html</p>", now.Add(-2*time.Hour), true))
		appendRaw(t, c, "INBOX", plainMsg(501+10*i, "Do archiwum "+u.user, "x", now.Add(-time.Hour)))
	}
	la := s.login(s.h, imaptest.User, imaptest.Pass)
	lb := s.login(s.h, imaptest.User2, imaptest.Pass2)
	if la.body.User.ID == lb.body.User.ID {
		t.Fatal("one user for two logins")
	}
	s.waitLive(t, la, 1)
	s.waitLive(t, lb, 1)

	prepare := func(name string, l login) owned {
		t.Helper()
		o := owned{name: name, l: l, acc: l.body.Accounts[0].ID}
		do := func(method, path string, body any, want int, out any) {
			t.Helper()
			r := serve(s.h, as(l, req{method: method, path: path, body: body}))
			if r.Code != want {
				t.Fatalf("%s: %s %s: %d %s", name, method, path, r.Code, r.Body)
			}
			if out != nil {
				r.json(out)
			}
		}
		var list api.ThreadList
		do("GET", "/threads?labelId=INBOX", nil, http.StatusOK, &list)
		var archive string
		for _, th := range list.Threads {
			if th.HasAttachments {
				o.thread = th.ID
			} else {
				archive = th.ID
			}
		}
		if o.thread == "" || archive == "" {
			t.Fatalf("%s: INBOX %+v", name, list.Threads)
		}
		var det api.ThreadDetail
		do("GET", "/threads/"+o.thread, nil, http.StatusOK, &det)
		o.msg = det.Messages[0].ID
		for _, a := range det.Messages[0].Attachments {
			if !a.Inline {
				o.att = a.ID
			}
		}
		var fs struct {
			Folders []api.Folder `json:"folders"`
		}
		do("GET", "/accounts/"+o.acc+"/folders", nil, http.StatusOK, &fs)
		for _, f := range fs.Folders {
			if f.Role == "inbox" {
				o.folder = f.ID
			}
		}
		var lbl api.Label
		do("POST", "/labels", map[string]any{"name": "Tajne " + name, "type": "user"}, http.StatusCreated, &lbl)
		o.label = lbl.ID
		do("POST", "/threads/modify", map[string]any{"ids": []string{o.thread}, "addLabelIds": []string{o.label}}, http.StatusOK, nil)

		r := upload(s.h, l, "tajne.txt", []byte("sekret "+name))
		var up api.UploadInfo
		r.json(&up)
		if r.Code != http.StatusCreated {
			t.Fatalf("%s: upload: %d %s", name, r.Code, r.Body)
		}
		o.upload = up.ID
		var d api.Draft
		do("POST", "/drafts", map[string]any{}, http.StatusCreated, &d)
		do("PUT", "/drafts/"+d.ID, map[string]any{"version": d.Version, "to": []map[string]string{{"email": "x@x.test"}},
			"subject": "Szkic " + name, "bodyHtml": "<p>szkic</p>", "attachmentIds": []string{o.upload}}, http.StatusOK, &d)
		o.draft = d.ID

		var op api.OperationResult
		do("POST", "/threads/modify", map[string]any{"ids": []string{archive}, "removeLabelIds": []string{"INBOX"}}, http.StatusOK, &op)
		if op.OperationID == nil {
			t.Fatalf("%s: archive without operation", name)
		}
		o.op = *op.OperationID

		do("PATCH", "/settings", map[string]any{"undoSendSeconds": 30}, http.StatusOK, nil)
		var out api.Draft
		do("POST", "/drafts", map[string]any{}, http.StatusCreated, &out)
		do("PUT", "/drafts/"+out.ID, map[string]any{"version": out.Version, "to": []map[string]string{{"email": "y@x.test"}},
			"subject": "Wysyłka " + name, "bodyHtml": "<p>w</p>"}, http.StatusOK, &out)
		var sr api.SendResult
		do("POST", "/drafts/"+out.ID+"/send", map[string]any{}, http.StatusOK, &sr)
		o.outbound = sr.OutboundID

		var ss struct {
			Sessions []api.SessionInfo `json:"sessions"`
		}
		do("GET", "/sessions", nil, http.StatusOK, &ss)
		for _, x := range ss.Sessions {
			if x.Current {
				o.session = x.ID
			}
		}
		for k, v := range map[string]string{"att": o.att, "folder": o.folder, "session": o.session} {
			if v == "" {
				t.Fatalf("%s: no %s", name, k)
			}
		}
		return o
	}
	ua, ub := prepare("A", la), prepare("B", lb)

	// A case is one request with a foreign id. body may use the attacker's
	// own objects (own) to make the request valid apart from that id.
	type tcase struct {
		name, method, path string
		// ref: the foreign id is a reference inside the body of a request on
		// the attacker's own objects. Those keep their validation answer (400,
		// or 422 for folders): the same for a foreign and a missing id, so it
		// tells nothing either.
		ref  bool
		body func(own owned) any
	}
	cases := func(v owned) []tcase {
		fixed := func(b any) func(owned) any { return func(owned) any { return b } }
		ids := func(id string) func(owned) any { return fixed(map[string]any{"ids": []string{id}}) }
		saveOwn := func(extra map[string]any) func(owned) any {
			return func(own owned) any {
				var d api.Draft
				serve(s.h, as(own.l, req{method: "GET", path: "/drafts/" + own.draft})).json(&d)
				b := map[string]any{"version": d.Version, "to": []map[string]string{{"email": "x@x.test"}}, "subject": "s", "bodyHtml": "<p>b</p>",
					"attachmentIds": []string{own.upload}}
				for k, x := range extra {
					b[k] = x
				}
				return b
			}
		}
		return []tcase{
			// acc_
			{"sync-status", "GET", "/accounts/" + v.acc + "/sync-status", false, nil},
			{"resync", "POST", "/accounts/" + v.acc + "/resync", false, fixed(map[string]any{"mode": "incremental"})},
			{"folders", "GET", "/accounts/" + v.acc + "/folders", false, nil},
			{"put folders", "PUT", "/accounts/" + v.acc + "/folders", false, fixed(map[string]any{"folders": []any{}})},
			{"patch account", "PATCH", "/accounts/" + v.acc, false, fixed(map[string]any{"displayName": "x"})},
			{"delete account", "DELETE", "/accounts/" + v.acc, false, nil},
			{"labels of account", "GET", "/labels?accountId=" + v.acc, false, nil},
			{"create label in account", "POST", "/labels", false, fixed(map[string]any{"accountId": v.acc, "name": "x", "type": "user"})},
			{"threads of account", "GET", "/threads?accountId=" + v.acc, false, nil},
			{"INBOX of account", "GET", "/threads?labelId=INBOX&accountId=" + v.acc, false, nil},
			{"draft in account", "POST", "/drafts", false, fixed(map[string]any{"accountId": v.acc})},
			{"move draft to account", "PUT", "/drafts/{own}", false, saveOwn(map[string]any{"accountId": v.acc})},
			{"activate account", "PATCH", "/settings", false, fixed(map[string]any{"activeAccountId": v.acc})},
			// t_
			{"get thread", "GET", "/threads/" + v.thread, false, nil},
			{"modify thread", "POST", "/threads/modify", false, fixed(map[string]any{"ids": []string{v.thread}, "addLabelIds": []string{"STARRED"}})},
			{"trash thread", "POST", "/threads/trash", false, ids(v.thread)},
			{"untrash thread", "POST", "/threads/untrash", false, ids(v.thread)},
			{"delete thread", "POST", "/threads/delete", false, ids(v.thread)},
			// m_
			{"modify message", "POST", "/messages/modify", false, fixed(map[string]any{"ids": []string{v.msg}, "addLabelIds": []string{"STARRED"}})},
			{"html", "GET", "/messages/" + v.msg + "/html", false, nil},
			{"raw", "GET", "/messages/" + v.msg + "/raw", false, nil},
			{"reply", "POST", "/drafts", false, fixed(map[string]any{"replyToMessageId": v.msg})},
			{"forward", "POST", "/drafts", false, fixed(map[string]any{"replyToMessageId": v.msg, "mode": "forward"})},
			// a_
			{"attachment", "GET", "/attachments/" + v.att, false, nil},
			{"attachment inline", "GET", "/attachments/" + v.att + "?inline=1", false, nil},
			// f_
			{"map folder", "PUT", "/accounts/{ownacc}/folders", true, fixed(map[string]any{"folders": []map[string]any{{"id": v.folder, "syncMode": "ignore"}}})},
			// l_
			{"patch label", "PATCH", "/labels/" + v.label, false, fixed(map[string]any{"name": "x"})},
			{"delete label", "DELETE", "/labels/" + v.label, false, nil},
			{"threads of label", "GET", "/threads?labelId=" + v.label, false, nil},
			{"threads of label in own account", "GET", "/threads?labelId=" + v.label + "&accountId={ownacc}", false, nil},
			{"label own thread", "POST", "/threads/modify", true, func(own owned) any {
				return map[string]any{"ids": []string{own.thread}, "addLabelIds": []string{v.label}}
			}},
			{"unlabel own thread", "POST", "/threads/modify", true, func(own owned) any {
				return map[string]any{"ids": []string{own.thread}, "removeLabelIds": []string{v.label}}
			}},
			{"label own message", "POST", "/messages/modify", true, func(own owned) any {
				return map[string]any{"ids": []string{own.msg}, "addLabelIds": []string{v.label}}
			}},
			{"label parent", "POST", "/labels", true, fixed(map[string]any{"name": "dziecko", "type": "user", "parentId": v.label})},
			// d_
			{"get draft", "GET", "/drafts/" + v.draft, false, nil},
			{"save draft", "PUT", "/drafts/" + v.draft, false, fixed(map[string]any{"version": 1, "to": []any{}, "subject": "x", "bodyHtml": ""})},
			{"discard draft", "DELETE", "/drafts/" + v.draft, false, nil},
			{"send draft", "POST", "/drafts/" + v.draft + "/send", false, fixed(map[string]any{})},
			// u_
			{"attach upload", "PUT", "/drafts/{own}", true, saveOwn(map[string]any{"attachmentIds": []string{v.upload}})},
			// op_
			{"undo", "POST", "/operations/" + v.op + "/undo", false, nil},
			// ob_
			{"cancel send", "POST", "/outbound/" + v.outbound + "/cancel", false, nil},
			// s_
			{"revoke session", "DELETE", "/sessions/" + v.session, false, nil},
		}
	}
	// missing is an owned whose ids are well-formed but exist nowhere.
	missing := func() owned {
		n := func(p string) string { return id36(p, 987654321) }
		return owned{acc: n("acc_"), thread: n("t_"), msg: n("m_"), att: n("a_"), folder: n("f_"), label: n("l_"), draft: n("d_"),
			upload: n("u_"), op: n("op_"), outbound: n("ob_"), session: n("s_")}
	}()

	run := func(attacker owned, c tcase) resp {
		path := strings.ReplaceAll(strings.ReplaceAll(c.path, "{own}", attacker.draft), "{ownacc}", attacker.acc)
		var body any
		if c.body != nil {
			body = c.body(attacker)
		}
		return serve(s.h, as(attacker.l, req{method: c.method, path: path, body: body}))
	}
	digits := regexp.MustCompile(`[0-9]+`)
	for _, pair := range []struct{ attacker, victim owned }{{ub, ua}, {ua, ub}} {
		foreign, none := cases(pair.victim), cases(missing)
		for i, c := range foreign {
			t.Run(pair.attacker.name+" on "+pair.victim.name+": "+c.name, func(t *testing.T) {
				got, want := run(pair.attacker, c), run(pair.attacker, none[i])
				switch {
				case c.ref:
					if got.Code != http.StatusBadRequest && got.Code != http.StatusUnprocessableEntity || got.code() != api.CodeValidation {
						t.Errorf("foreign reference: %d %s", got.Code, got.Body)
					}
				case got.Code != http.StatusNotFound || got.code() != api.CodeNotFound:
					t.Errorf("foreign id: %d %s", got.Code, got.Body)
				}
				// The detail of a reference echoes the id that was sent;
				// apart from it the answers must be byte for byte the same.
				if want.Code != got.Code || digits.ReplaceAllString(want.Body.String(), "N") != digits.ReplaceAllString(got.Body.String(), "N") {
					t.Errorf("foreign id differs from a missing one:\n  foreign %d %s\n  missing %d %s", got.Code, got.Body, want.Code, want.Body)
				}
			})
		}
	}

	// Nothing of the victims was touched, and their ids were valid.
	for _, o := range []owned{ua, ub} {
		do := func(method, path string, want int) resp {
			t.Helper()
			r := serve(s.h, as(o.l, req{method: method, path: path}))
			if r.Code != want {
				t.Errorf("%s afterwards: %s %s: %d %s", o.name, method, path, r.Code, r.Body)
			}
			return r
		}
		var det api.ThreadDetail
		do("GET", "/threads/"+o.thread, http.StatusOK).json(&det)
		if len(det.LabelIDs) == 0 || !strings.Contains(strings.Join(det.LabelIDs, ","), "INBOX") ||
			!strings.Contains(strings.Join(det.LabelIDs, ","), o.label) || strings.Contains(strings.Join(det.LabelIDs, ","), "STARRED") {
			t.Errorf("%s: thread labels changed: %v", o.name, det.LabelIDs)
		}
		var d api.Draft
		do("GET", "/drafts/"+o.draft, http.StatusOK).json(&d)
		if d.Subject != "Szkic "+o.name || len(d.Attachments) != 1 || d.Attachments[0].ID != o.upload {
			t.Errorf("%s: draft changed: %+v", o.name, d)
		}
		do("GET", "/attachments/"+o.att, http.StatusOK)
		do("GET", "/messages/"+o.msg+"/raw", http.StatusOK)
		do("GET", "/accounts/"+o.acc+"/sync-status", http.StatusOK)
		var st api.Settings
		do("GET", "/settings", http.StatusOK).json(&st)
		if st.ActiveAccountID == nil || *st.ActiveAccountID != o.acc {
			t.Errorf("%s: active account %v", o.name, st.ActiveAccountID)
		}
		do("POST", "/operations/"+o.op+"/undo", http.StatusOK)
		do("POST", "/outbound/"+o.outbound+"/cancel", http.StatusOK)
	}

	// Search is per account too: A finds nothing of B's subjects.
	for _, c := range []struct {
		l    login
		want bool
	}{{la, false}, {lb, true}} {
		var list api.ThreadList
		serve(s.h, as(c.l, req{method: "GET", path: "/threads?q=silo2"})).json(&list)
		if (len(list.Threads) > 0) != c.want {
			t.Errorf("search silo2 as %s: %+v", c.l.body.User.Email, list.Threads)
		}
	}

	t.Run("events", func(t *testing.T) {
		sa := openSSE(t, s.srv.URL, la, "")
		sb := openSSE(t, s.srv.URL, lb, "")
		// B acts and gets new mail; B sees it, A does not.
		if r := serve(s.h, as(lb, req{method: "POST", path: "/threads/modify",
			body: map[string]any{"ids": []string{ub.thread}, "addLabelIds": []string{"STARRED"}}})); r.Code != http.StatusOK {
			t.Fatalf("B star: %d %s", r.Code, r.Body)
		}
		cb := imapUser(t, addr, imaptest.User2, imaptest.Pass2)
		appendRaw(t, cb, "INBOX", plainMsg(777, "Nowa poczta B", "hej", now))
		sb.wait(t, 60*time.Second, "message.new of B", ofType("message.new"))
		// Let anything misrouted arrive, then look at A's stream.
		time.Sleep(time.Second)
		for done := false; !done; {
			select {
			case e, ok := <-sa.events:
				if !ok {
					t.Fatal("A's stream closed")
				}
				var p struct {
					AccountID string `json:"accountId"`
				}
				_ = json.Unmarshal([]byte(e.data), &p)
				if p.AccountID != ua.acc || strings.Contains(e.data, ub.acc+`"`) || strings.Contains(e.data, ub.thread+`"`) {
					t.Errorf("A received %s %s", e.typ, e.data)
				}
			default:
				done = true
			}
		}

		poll := func(l login, after string) api.PollResult {
			t.Helper()
			var out api.PollResult
			r := serve(s.h, as(l, req{method: "GET", path: "/events/poll?limit=500&after=" + after}))
			if r.Code != http.StatusOK {
				t.Fatalf("poll: %d %s", r.Code, r.Body)
			}
			r.json(&out)
			return out
		}
		pa, pb := poll(la, "0"), poll(lb, "0")
		if len(pa.Events) == 0 || len(pb.Events) == 0 {
			t.Fatalf("no events: A %d, B %d", len(pa.Events), len(pb.Events))
		}
		seqA := map[int64]bool{}
		for _, e := range pa.Events {
			seqA[e.ID] = true
			if acc, ok := e.Data["accountId"]; ok && acc != ua.acc {
				t.Errorf("A polled an event of %v: %s %v", acc, e.Type, e.Data)
			}
		}
		var bSeq int64
		for _, e := range pb.Events {
			if seqA[e.ID] {
				t.Errorf("event %d in both polls", e.ID)
			}
			bSeq = max(bSeq, e.ID)
		}
		// after= a sequence number of B's: A gets only a resync (to A's own
		// head), never B's events after it.
		pr := poll(la, strconv.FormatInt(bSeq-1, 10))
		if len(pr.Events) != 1 || pr.Events[0].Type != "resync" || !seqA[pr.NextAfter] && pr.NextAfter != 0 {
			t.Errorf("A polling after B's seq: %+v", pr)
		}
		// The same through Last-Event-ID on the stream.
		sr := openSSE(t, s.srv.URL, la, strconv.FormatInt(bSeq-1, 10))
		first := sr.wait(t, 10*time.Second, "resync", func(sseEvent) bool { return true })
		if first.typ != "resync" {
			t.Errorf("A stream after B's seq starts with %s %s", first.typ, first.data)
		}
		select {
		case e := <-sr.events:
			n, _ := strconv.ParseInt(e.id, 10, 64)
			if !seqA[n] {
				t.Errorf("A stream replayed %s %s (seq %s)", e.typ, e.data, e.id)
			}
		case <-time.After(500 * time.Millisecond):
		}
	})
}
