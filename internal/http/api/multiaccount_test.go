package api_test

import (
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
	"github.com/silopost/silo-server/internal/testutil/smtptest"
)

// TestMultipleAccounts is the first DoD item of phase 5 for one user: two
// accounts on two Dovecots (each with its own SMTP server), the active
// account (PATCH /settings), counts of both, the From choice of a draft
// (PUT /drafts/{id} accountId), sending from the right account, label.counts
// of both, removing the active account and SILO_MAX_ACCOUNTS_PER_USER.
func TestMultipleAccounts(t *testing.T) {
	s := newFullStack(t, func(c *config.Config) { c.MaxAccountsPerUser = 2 })
	addrA := net.JoinHostPort(s.cfg.DefaultIMAPHost, strconv.Itoa(s.cfg.DefaultIMAPPort))
	addrB, _, _ := imaptest.Start(t, imaptest.Full)
	hostB, portBStr, _ := net.SplitHostPort(addrB)
	portB, _ := strconv.Atoi(portBStr)
	const emailB = "zwei@b.test"
	smtpB := smtptest.StartServer(t, imaptest.User2, imaptest.Pass2, 0)

	// Different mailboxes: 3 messages on A, 2 on B.
	ca := imapUser(t, addrA, imaptest.User, imaptest.Pass)
	for i := range 3 {
		appendMsg(t, ca, "INBOX", "a"+strconv.Itoa(i)+"@a.test", "Ala <ala@a.test>", "silo@silo.test", "Konto A "+strconv.Itoa(i))
	}
	cb := imapUser(t, addrB, imaptest.User2, imaptest.Pass2)
	for i := range 2 {
		appendMsg(t, cb, "INBOX", "b"+strconv.Itoa(i)+"@b.test", "Bob <bob@b.test>", emailB, "Konto B "+strconv.Itoa(i))
	}

	l := s.login(s.h, imaptest.User, imaptest.Pass)
	do := func(method, path string, body any) resp {
		t.Helper()
		return serve(s.h, as(l, req{method: method, path: path, body: body}))
	}
	acc1 := l.body.Accounts[0].ID
	accountB := func(email, user, pass string) map[string]any {
		return map[string]any{"email": email, "username": user, "password": pass, "displayName": "Zwei",
			"imapHost": hostB, "imapPort": portB, "imapTls": "none", "smtpHost": smtpB.Host, "smtpPort": smtpB.Port, "smtpTls": "none"}
	}
	r := do("POST", "/accounts", accountB(emailB, imaptest.User2, imaptest.Pass2))
	var a2 api.Account
	r.json(&a2)
	if r.Code != http.StatusCreated || a2.Position != 1 || a2.IMAPPort != portB || strings.Contains(r.Body.String(), imaptest.Pass2) {
		t.Fatalf("add account 2: %d %s", r.Code, r.Body)
	}
	acc2 := a2.ID
	// The secret is stored encrypted only.
	if n := s.count(`SELECT count(*) FROM accounts WHERE position(convert_to($1, 'UTF8') in secret_enc) > 0`, imaptest.Pass2); n != 0 {
		t.Error("plain password in accounts.secret_enc")
	}

	t.Run("account limit", func(t *testing.T) {
		r := do("POST", "/accounts", accountB("drei@b.test", imaptest.User, imaptest.Pass))
		if r.Code != http.StatusConflict || r.code() != api.CodeAccountLimit {
			t.Errorf("third account with SILO_MAX_ACCOUNTS_PER_USER=2: %d %s", r.Code, r.Body)
		}
	})

	accs := s.waitLive(t, l, 2)
	if accs[0].ID != acc1 || accs[0].Position != 0 || accs[1].ID != acc2 || accs[1].Position != 1 || accs[1].Email != emailB {
		t.Fatalf("GET /accounts order: %+v", accs)
	}

	inbox := func(acc string) api.Label {
		t.Helper()
		var out struct {
			Labels []api.Label `json:"labels"`
		}
		path := "/labels"
		if acc != "" {
			path += "?accountId=" + acc
		}
		r := do("GET", path, nil)
		r.json(&out)
		for _, lb := range out.Labels {
			if lb.ID == "INBOX" {
				return lb
			}
		}
		t.Fatalf("GET %s: no INBOX: %d %s", path, r.Code, r.Body)
		return api.Label{}
	}
	if in := inbox(acc1); in.AccountID != acc1 || in.ThreadsTotal != 3 || in.ThreadsUnread != 3 {
		t.Errorf("INBOX of account 1: %+v", in)
	}
	if in := inbox(acc2); in.AccountID != acc2 || in.ThreadsTotal != 2 || in.ThreadsUnread != 2 {
		t.Errorf("INBOX of account 2: %+v", in)
	}

	threads := func(query string) api.ThreadList {
		t.Helper()
		var out api.ThreadList
		r := do("GET", "/threads?"+query, nil)
		if r.Code != http.StatusOK {
			t.Fatalf("GET /threads?%s: %d %s", query, r.Code, r.Body)
		}
		r.json(&out)
		return out
	}
	only := func(list api.ThreadList, acc string, n int) bool {
		if len(list.Threads) != n {
			return false
		}
		for _, th := range list.Threads {
			if th.AccountID != acc {
				return false
			}
		}
		return true
	}
	setActive := func(acc any) api.Settings {
		t.Helper()
		var st api.Settings
		r := do("PATCH", "/settings", map[string]any{"activeAccountId": acc})
		if r.Code != http.StatusOK {
			t.Fatalf("PATCH /settings activeAccountId=%v: %d %s", acc, r.Code, r.Body)
		}
		r.json(&st)
		return st
	}

	t.Run("active account", func(t *testing.T) {
		if l := threads("labelId=INBOX"); !only(l, acc1, 3) {
			t.Errorf("default (first account): %+v", l.Threads)
		}
		setActive(acc2)
		if l := threads("labelId=INBOX"); !only(l, acc2, 2) {
			t.Errorf("after activating account 2: %+v", l.Threads)
		}
		if l := threads(""); !only(l, acc2, 2) {
			t.Errorf("all mail of account 2: %+v", l.Threads)
		}
		if in := inbox(""); in.AccountID != acc2 || in.ThreadsTotal != 2 {
			t.Errorf("GET /labels without accountId: %+v", in)
		}
		// An explicit accountId wins over the active account.
		if l := threads("labelId=INBOX&accountId=" + acc1); !only(l, acc1, 3) {
			t.Errorf("explicit account 1: %+v", l.Threads)
		}
		setActive(acc1)
		if l := threads("labelId=INBOX"); !only(l, acc1, 3) {
			t.Errorf("back to account 1: %+v", l.Threads)
		}
	})

	draftIn := func(acc, draftID string) bool {
		for _, th := range threads("labelId=DRAFT&accountId=" + acc).Threads {
			if th.DraftID != nil && *th.DraftID == draftID {
				return true
			}
		}
		return false
	}

	var moved api.Draft
	t.Run("draft from another account", func(t *testing.T) {
		var d api.Draft
		r := do("POST", "/drafts", map[string]any{})
		r.json(&d)
		if r.Code != http.StatusCreated || d.AccountID != acc1 {
			t.Fatalf("create draft: %d %s", r.Code, r.Body)
		}
		body := map[string]any{"version": d.Version, "to": []map[string]string{{"email": "odbiorca@x.test"}},
			"subject": "Z drugiego konta", "bodyHtml": "<p>Treść</p>", "syncNow": true}
		r = do("PUT", "/drafts/"+d.ID, body)
		r.json(&d)
		if r.Code != http.StatusOK {
			t.Fatalf("save: %d %s", r.Code, r.Body)
		}
		waitFor(t, 30*time.Second, "draft copy in Drafts of server A", func() bool { return folderCount(t, ca, "Drafts") == 1 })
		if !draftIn(acc1, d.ID) {
			t.Fatal("draft not in DRAFT of account 1")
		}

		// Unknown (or another user's: TestUserIsolation) account: 404, nothing moves.
		body["version"], body["accountId"] = d.Version, "acc_zzzz"
		if r := do("PUT", "/drafts/"+d.ID, body); r.Code != http.StatusNotFound || r.code() != api.CodeNotFound {
			t.Errorf("unknown account: %d %s", r.Code, r.Body)
		}
		// A stale version is a conflict, as for any save.
		body["version"], body["accountId"] = d.Version-1, acc2
		if r := do("PUT", "/drafts/"+d.ID, body); r.Code != http.StatusConflict || r.code() != api.CodeDraftVersionConflict {
			t.Errorf("stale version: %d %s", r.Code, r.Body)
		}

		body["version"], body["accountId"] = d.Version, acc2
		r = do("PUT", "/drafts/"+d.ID, body)
		r.json(&moved)
		if r.Code != http.StatusOK || moved.AccountID != acc2 || moved.ID == d.ID || moved.ThreadID == d.ThreadID ||
			moved.Version != d.Version+1 || moved.Subject != "Z drugiego konta" || len(moved.To) != 1 {
			t.Fatalf("move to account 2: %d %s", r.Code, r.Body)
		}
		if r := do("GET", "/drafts/"+d.ID, nil); r.Code != http.StatusNotFound {
			t.Errorf("old draft id after the move: %d", r.Code)
		}
		if r := do("GET", "/threads/"+d.ThreadID, nil); r.Code != http.StatusNotFound {
			t.Errorf("old thread after the move: %d", r.Code)
		}
		if draftIn(acc1, d.ID) || draftIn(acc1, moved.ID) || !draftIn(acc2, moved.ID) {
			t.Errorf("DRAFT lists after the move: acc1 %+v, acc2 %+v", threads("labelId=DRAFT&accountId="+acc1).Threads,
				threads("labelId=DRAFT&accountId="+acc2).Threads)
		}
		var got api.Draft
		do("GET", "/drafts/"+moved.ID, nil).json(&got)
		if got.ID != moved.ID || got.AccountID != acc2 {
			t.Errorf("GET moved draft: %+v", got)
		}
		// The server copies follow: gone from A, saved on B.
		waitFor(t, 30*time.Second, "copy removed from Drafts of server A", func() bool { return folderCount(t, ca, "Drafts") == 0 })
		waitFor(t, 30*time.Second, "copy in Drafts of server B", func() bool { return folderCount(t, cb, "Drafts") == 1 })
		if n := s.count(`SELECT count(*) FROM events WHERE type = 'draft.updated' AND (payload->>'previousDraftId')::bigint = $1 AND (payload->>'draftId')::bigint = $2`,
			idOf("d_", d.ID), idOf("d_", moved.ID)); n != 1 {
			t.Errorf("draft.updated with previousDraftId: %d", n)
		}

		// A reply belongs to the account of the original: 409.
		orig := threads("labelId=INBOX&accountId=" + acc1)
		var det api.ThreadDetail
		do("GET", "/threads/"+orig.Threads[0].ID, nil).json(&det)
		var reply api.Draft
		r = do("POST", "/drafts", map[string]any{"replyToMessageId": det.Messages[0].ID, "accountId": acc2})
		r.json(&reply)
		if r.Code != http.StatusCreated || reply.AccountID != acc1 {
			t.Fatalf("reply draft: %d %s", r.Code, r.Body)
		}
		save := map[string]any{"version": reply.Version, "to": []map[string]string{{"email": "ala@a.test"}},
			"subject": reply.Subject, "bodyHtml": "<p>x</p>", "accountId": acc2}
		r = do("PUT", "/drafts/"+reply.ID, save)
		if r.Code != http.StatusConflict || r.code() != api.CodeDraftAccountLocked {
			t.Errorf("move a reply: %d %s", r.Code, r.Body)
		}
		// Naming the draft's own account is a normal save.
		save["accountId"] = acc1
		if r := do("PUT", "/drafts/"+reply.ID, save); r.Code != http.StatusOK {
			t.Errorf("save with the same account: %d %s", r.Code, r.Body)
		}
		if r := do("DELETE", "/drafts/"+reply.ID, nil); r.Code != http.StatusNoContent {
			t.Errorf("discard reply: %d", r.Code)
		}
	})

	t.Run("send from account 2", func(t *testing.T) {
		if moved.ID == "" {
			t.Skip("no moved draft")
		}
		r := do("POST", "/drafts/"+moved.ID+"/send", map[string]any{})
		if r.Code != http.StatusOK {
			t.Fatalf("send: %d %s", r.Code, r.Body)
		}
		waitFor(t, 30*time.Second, "delivery to the SMTP server of account 2", func() bool { return len(smtpB.Messages()) == 1 })
		msg := smtpB.Messages()[0]
		data := string(msg.Data)
		if msg.From != emailB || !slices.Equal(msg.To, []string{"odbiorca@x.test"}) ||
			!strings.Contains(data, "From: \"Zwei\" <"+emailB+">") && !strings.Contains(data, "From: Zwei <"+emailB+">") {
			t.Errorf("delivered: from %q to %v:\n%s", msg.From, msg.To, data)
		}
		if n := len(s.smtp.Messages()); n != 0 {
			t.Errorf("SMTP server of account 1 got %d messages", n)
		}
		waitFor(t, 30*time.Second, "copy in Sent of server B", func() bool { return folderCount(t, cb, "Sent") == 1 })
		if n := folderCount(t, ca, "Sent"); n != 0 {
			t.Errorf("Sent of server A: %d", n)
		}
		waitFor(t, 30*time.Second, "Drafts of server B emptied", func() bool { return folderCount(t, cb, "Drafts") == 0 })
	})

	t.Run("label.counts of both accounts", func(t *testing.T) {
		seen := map[string]bool{}
		waitFor(t, 10*time.Second, "label.counts of both accounts", func() bool {
			var poll api.PollResult
			serve(s.h, as(l, req{method: "GET", path: "/events/poll?after=0&limit=500"})).json(&poll)
			for _, e := range poll.Events {
				if acc, _ := e.Data["accountId"].(string); e.Type == "label.counts" && acc != "" {
					seen[acc] = true
				}
			}
			return seen[acc1] && seen[acc2]
		})
		// Live over SSE too: a new message on B updates the counts of acc2.
		sse := openSSE(t, s.srv.URL, l, "")
		appendMsg(t, cb, "INBOX", "b-live@b.test", "Bob <bob@b.test>", emailB, "Konto B live")
		sse.wait(t, 30*time.Second, "label.counts of account 2", func(e sseEvent) bool {
			var p struct {
				AccountID string `json:"accountId"`
			}
			_ = json.Unmarshal([]byte(e.data), &p)
			return e.typ == "label.counts" && p.AccountID == acc2
		})
	})

	t.Run("remove the active account", func(t *testing.T) {
		if st := setActive(acc2); *st.ActiveAccountID != acc2 {
			t.Fatalf("activate account 2: %+v", st)
		}
		if r := do("DELETE", "/accounts/"+acc2, nil); r.Code != http.StatusNoContent {
			t.Fatalf("delete account 2: %d %s", r.Code, r.Body)
		}
		var st api.Settings
		do("GET", "/settings", nil).json(&st)
		if st.ActiveAccountID == nil || *st.ActiveAccountID != acc1 {
			t.Errorf("activeAccountId after removing it: %v", st.ActiveAccountID)
		}
		var sess api.SessionBody
		do("GET", "/session", nil).json(&sess)
		if len(sess.Accounts) != 1 || sess.Accounts[0].ID != acc1 || *sess.Settings.ActiveAccountID != acc1 {
			t.Errorf("GET /session: %+v %+v", sess.Accounts, sess.Settings)
		}
		if l := threads("labelId=INBOX"); !only(l, acc1, 3) {
			t.Errorf("list after removing the active account: %+v", l.Threads)
		}
		if in := inbox(""); in.AccountID != acc1 {
			t.Errorf("labels after removing the active account: %+v", in)
		}
		for _, p := range []string{"/labels?accountId=" + acc2, "/threads?accountId=" + acc2, "/accounts/" + acc2 + "/sync-status"} {
			if r := do("GET", p, nil); r.Code != http.StatusNotFound {
				t.Errorf("GET %s of the removed account: %d", p, r.Code)
			}
		}
		if n := s.count(`SELECT count(*) FROM messages WHERE account_id = $1`, idOf("acc_", acc2)); n != 0 {
			t.Errorf("messages of the removed account left: %d", n)
		}
		// Room for an account again.
		if r := do("POST", "/accounts", accountB(emailB, imaptest.User2, imaptest.Pass2)); r.Code != http.StatusCreated {
			t.Errorf("add an account after removing one: %d %s", r.Code, r.Body)
		}
	})
}
