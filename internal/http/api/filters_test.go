package api_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/filters"
	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/ops"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
	"github.com/silopost/silo-server/internal/worker"
)

// runFilterJobs runs the pending filter jobs of kind with the env's filter
// service (no worker runs in mailEnv) and deletes them.
func (m *mailEnv) runFilterJobs(kind string) int {
	m.t.Helper()
	ctx := context.Background()
	rows, err := m.pool.Query(ctx, `SELECT id, kind, payload FROM jobs WHERE kind = $1 AND status = 'pending' ORDER BY id`, kind)
	if err != nil {
		m.t.Fatal(err)
	}
	var jobs []worker.Job
	for rows.Next() {
		var j worker.Job
		if err := rows.Scan(&j.ID, &j.Kind, &j.Payload); err != nil {
			m.t.Fatal(err)
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	for _, j := range jobs {
		if err := m.filters.Run(ctx, j); err != nil {
			m.t.Fatalf("%s job %d: %v", kind, j.ID, err)
		}
		if _, err := m.pool.Exec(ctx, `DELETE FROM jobs WHERE id = $1`, j.ID); err != nil {
			m.t.Fatal(err)
		}
	}
	return len(jobs)
}

func (m *mailEnv) labelsOfMsg(msgID int64) map[string]bool {
	m.t.Helper()
	rows, err := m.pool.Query(context.Background(), `
SELECT COALESCE(l.system_key, 'l_' || l.name) FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE ml.message_id = $1`, msgID)
	if err != nil {
		m.t.Fatal(err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		m.t.Fatal(err)
	}
	out := map[string]bool{}
	for _, k := range keys {
		out[k] = true
	}
	var unread bool
	_ = m.pool.QueryRow(context.Background(), `SELECT is_unread FROM messages WHERE id = $1`, msgID).Scan(&unread)
	out["UNREAD"] = unread
	return out
}

func TestFilters(t *testing.T) {
	m := newMailEnv(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := m.pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE accounts SET keywords_supported = true WHERE id = $1`, m.accID)
	label := func(name string) string {
		t.Helper()
		var l api.Label
		r := serve(m.web, m.as(req{method: "POST", path: "/labels", body: map[string]any{"name": name, "type": "user"}}))
		if r.Code != http.StatusCreated {
			t.Fatalf("label: %d %s", r.Code, r.Body)
		}
		r.json(&l)
		return l.ID
	}
	news := label("Newsletters")
	create := func(body map[string]any) (resp, api.Filter) {
		t.Helper()
		var f api.Filter
		r := serve(m.web, m.as(req{method: "POST", path: "/filters", body: body}))
		if r.Code == http.StatusCreated {
			r.json(&f)
		}
		return r, f
	}
	list := func() []api.Filter {
		t.Helper()
		var out struct{ Filters []api.Filter }
		r := serve(m.web, m.as(req{method: "GET", path: "/filters"}))
		if r.Code != http.StatusOK {
			t.Fatalf("list: %d %s", r.Code, r.Body)
		}
		r.json(&out)
		return out.Filters
	}

	t.Run("validation", func(t *testing.T) {
		// Another user's account and label.
		var otherUser, otherAcc, otherLabel int64
		if err := m.pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ('other@silo.test') RETURNING id`).Scan(&otherUser); err != nil {
			t.Fatal(err)
		}
		if err := m.pool.QueryRow(ctx, `
INSERT INTO accounts (user_id, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, secret_enc)
VALUES ($1, 'other@silo.test', 'h', 1, 'none', 'h', 25, 'none', 'other', '\x00') RETURNING id`, otherUser).Scan(&otherAcc); err != nil {
			t.Fatal(err)
		}
		if err := m.pool.QueryRow(ctx, `INSERT INTO labels (user_id, account_id, name, type) VALUES ($1, $2, 'Obca', 'user') RETURNING id`, otherUser, otherAcc).Scan(&otherLabel); err != nil {
			t.Fatal(err)
		}
		m.ingest(1, "From: a@silo.test\r\nSubject: Seed\r\nMessage-ID: <seed@silo.test>\r\n\r\n") // system labels
		var inboxLabel int64
		if err := m.pool.QueryRow(ctx, `SELECT id FROM labels WHERE account_id = $1 AND system_key = 'INBOX'`, m.accID).Scan(&inboxLabel); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			name   string
			body   map[string]any
			status int
			code   string
		}{
			{"bad syntax", map[string]any{"criteria": "from:a)", "actions": map[string]any{"star": true}}, 422, api.CodeInvalidQuery},
			{"no action", map[string]any{"criteria": "from:a", "actions": map[string]any{}}, 422, api.CodeInvalidFilter},
			{"archive and trash", map[string]any{"criteria": "from:a", "actions": map[string]any{"archive": true, "trash": true}}, 422, api.CodeInvalidFilter},
			{"in:anywhere only", map[string]any{"criteria": "in:anywhere", "actions": map[string]any{"star": true}}, 422, api.CodeInvalidFilter},
			{"blank criteria", map[string]any{"criteria": "   ", "actions": map[string]any{"star": true}}, 422, api.CodeInvalidFilter},
			{"empty criteria", map[string]any{"criteria": "", "actions": map[string]any{"star": true}}, 400, api.CodeValidation},
			{"label of another user", map[string]any{"criteria": "from:a", "actions": map[string]any{"addLabelIds": []string{id36("l_", otherLabel)}}}, 422, api.CodeInvalidLabel},
			{"system label", map[string]any{"criteria": "from:a", "actions": map[string]any{"addLabelIds": []string{id36("l_", inboxLabel)}}}, 422, api.CodeInvalidLabel},
			{"bad label id", map[string]any{"criteria": "from:a", "actions": map[string]any{"addLabelIds": []string{"INBOX"}}}, 422, api.CodeInvalidLabel},
			{"bad forward", map[string]any{"criteria": "from:a", "actions": map[string]any{"forwardTo": "not an address"}}, 422, api.CodeInvalidFilter},
			{"account of another user", map[string]any{"accountId": id36("acc_", otherAcc), "criteria": "from:a", "actions": map[string]any{"star": true}}, 404, api.CodeNotFound},
		} {
			if r, _ := create(c.body); r.Code != c.status || r.code() != c.code {
				t.Errorf("%s: %d %s", c.name, r.Code, r.Body)
			}
		}
		if n := m.count(`SELECT count(*) FROM filters`); n != 0 {
			t.Errorf("filters stored by refused requests: %d", n)
		}
		// Someone else's filter is not found.
		var foreign int64
		if err := m.pool.QueryRow(ctx, `INSERT INTO filters (user_id, account_id, criteria, actions) VALUES ($1, $2, 'from:x', '{"star":true}') RETURNING id`,
			otherUser, otherAcc).Scan(&foreign); err != nil {
			t.Fatal(err)
		}
		fid := id36("f_", foreign)
		for _, q := range []req{
			{method: "GET", path: "/filters/" + fid},
			{method: "PUT", path: "/filters/" + fid, body: map[string]any{"criteria": "from:y", "actions": map[string]any{"star": true}}},
			{method: "DELETE", path: "/filters/" + fid},
			{method: "POST", path: "/filters/" + fid + "/apply"},
		} {
			if r := serve(m.web, m.as(q)); r.Code != http.StatusNotFound {
				t.Errorf("%s %s of another user's filter: %d %s", q.method, q.path, r.Code, r.Body)
			}
		}
		if len(list()) != 0 {
			t.Error("another user's filter listed")
		}
	})

	t.Run("csrf and bearer", func(t *testing.T) {
		body := map[string]any{"criteria": "from:csrf", "actions": map[string]any{"star": true}}
		if r := serve(m.web, req{method: "POST", path: "/filters", body: body, cookie: m.l.cookie}); r.Code != http.StatusForbidden {
			t.Errorf("without CSRF: %d %s", r.Code, r.Body)
		}
		if r := serve(m.web, req{method: "GET", path: "/filters"}); r.Code != http.StatusUnauthorized {
			t.Errorf("anonymous: %d", r.Code)
		}
		r := serve(m.web, req{method: "POST", path: "/session", body: map[string]any{"email": imaptest.User, "password": imaptest.Pass,
			"client": map[string]string{"kind": "bearer", "name": "test"}}})
		var sb api.SessionBody
		r.json(&sb)
		if sb.Token == "" {
			t.Fatalf("bearer login: %d %s", r.Code, r.Body)
		}
		r = serve(m.web, req{method: "POST", path: "/filters", body: body, headers: bearerHdr(sb.Token)})
		var f api.Filter
		r.json(&f)
		if r.Code != http.StatusCreated {
			t.Fatalf("bearer create: %d %s", r.Code, r.Body)
		}
		if r := serve(m.web, req{method: "DELETE", path: "/filters/" + f.ID, headers: bearerHdr(sb.Token)}); r.Code != http.StatusNoContent {
			t.Errorf("bearer delete: %d %s", r.Code, r.Body)
		}
	})

	t.Run("crud and order", func(t *testing.T) {
		r, a := create(map[string]any{"name": " A ", "criteria": " from:(a@x.pl) ", "actions": map[string]any{"star": true, "addLabelIds": []string{news, news}}})
		if r.Code != http.StatusCreated || a.Name != "A" || a.Criteria != "from:(a@x.pl)" || a.CriteriaNormalized == "" || !a.Enabled ||
			a.Position != 0 || !a.Actions.Star || !slices.Equal(a.Actions.AddLabelIDs, []string{news}) || a.Actions.ForwardTo != nil {
			t.Fatalf("create a: %d %s", r.Code, r.Body)
		}
		_, b := create(map[string]any{"criteria": "subject:b", "actions": map[string]any{"markRead": true}, "enabled": false})
		_, c := create(map[string]any{"criteria": "subject:c", "actions": map[string]any{"trash": true}, "position": 0})
		ids := func() []string {
			var out []string
			for _, f := range list() {
				out = append(out, fmt.Sprintf("%s@%d", f.ID, f.Position))
			}
			return out
		}
		want := []string{c.ID + "@0", a.ID + "@1", b.ID + "@2"}
		if got := ids(); !slices.Equal(got, want) || b.Enabled {
			t.Fatalf("order after create = %v, want %v (b enabled %v)", got, want, b.Enabled)
		}
		// PUT replaces everything and moves the filter.
		var u api.Filter
		r = serve(m.web, m.as(req{method: "PUT", path: "/filters/" + c.ID, body: map[string]any{"criteria": "subject:c2", "actions": map[string]any{"archive": true}, "position": 9}}))
		r.json(&u)
		if r.Code != http.StatusOK || u.Criteria != "subject:c2" || u.Actions.Trash || !u.Actions.Archive || u.Position != 2 || !u.UpdatedAt.After(u.CreatedAt) && !u.UpdatedAt.Equal(u.CreatedAt) {
			t.Fatalf("put: %d %s", r.Code, r.Body)
		}
		if got, want := ids(), []string{a.ID + "@0", b.ID + "@1", c.ID + "@2"}; !slices.Equal(got, want) {
			t.Fatalf("order after put = %v, want %v", got, want)
		}
		if r := serve(m.web, m.as(req{method: "PUT", path: "/filters/" + c.ID, body: map[string]any{"criteria": "x", "actions": map[string]any{"archive": true, "trash": true}}})); r.Code != 422 {
			t.Errorf("put invalid: %d %s", r.Code, r.Body)
		}
		var g api.Filter
		r = serve(m.web, m.as(req{method: "GET", path: "/filters/" + a.ID}))
		r.json(&g)
		if r.Code != http.StatusOK || g.ID != a.ID || g.Criteria != a.Criteria {
			t.Errorf("get: %d %s", r.Code, r.Body)
		}
		for _, f := range []api.Filter{a, b} {
			if r := serve(m.web, m.as(req{method: "DELETE", path: "/filters/" + f.ID})); r.Code != http.StatusNoContent {
				t.Fatalf("delete: %d %s", r.Code, r.Body)
			}
		}
		if got := ids(); !slices.Equal(got, []string{c.ID + "@0"}) {
			t.Errorf("after delete = %v", got)
		}
		if r := serve(m.web, m.as(req{method: "DELETE", path: "/filters/" + a.ID})); r.Code != http.StatusNotFound {
			t.Errorf("second delete: %d", r.Code)
		}
		serve(m.web, m.as(req{method: "DELETE", path: "/filters/" + c.ID}))
	})

	t.Run("apply to existing mail", func(t *testing.T) {
		n1, t1 := m.ingest(11, "From: News <newsletter@shop.pl>\r\nSubject: Promo 1\r\nMessage-ID: <n1@silo.test>\r\n\r\n")
		n2, _ := m.ingest(12, "From: newsletter@shop.pl\r\nSubject: Promo 2\r\nMessage-ID: <n2@silo.test>\r\n\r\n")
		o1, _ := m.ingest(13, "From: jan@firma.pl\r\nSubject: Hello\r\nMessage-ID: <o1@silo.test>\r\n\r\n")
		_, f := create(map[string]any{"criteria": "from:newsletter", "actions": map[string]any{"archive": true, "markRead": true, "addLabelIds": []string{news}}})
		var res api.FilterApplyResult
		r := serve(m.web, m.as(req{method: "POST", path: "/filters/" + f.ID + "/apply", headers: map[string]string{"X-Silo-Client": "tab-f"}}))
		r.json(&res)
		if r.Code != http.StatusAccepted || res.JobID == "" || res.MatchedEstimate == nil || *res.MatchedEstimate != 2 {
			t.Fatalf("apply: %d %s", r.Code, r.Body)
		}
		if m.labelsOfMsg(n1)["l_Newsletters"] {
			t.Fatal("applied before the job ran")
		}
		if n := m.runFilterJobs("filter_apply"); n != 1 {
			t.Fatalf("filter_apply jobs = %d", n)
		}
		for _, id := range []int64{n1, n2} {
			if l := m.labelsOfMsg(id); l["INBOX"] || !l["l_Newsletters"] || l["UNREAD"] {
				t.Errorf("message %d after apply: %v", id, l)
			}
		}
		if l := m.labelsOfMsg(o1); !l["INBOX"] || l["l_Newsletters"] {
			t.Errorf("other message after apply: %v", l)
		}
		// Without undo: the move is due at once and no operation is recorded.
		if n := m.count(`SELECT count(*) FROM jobs WHERE account_id = $1 AND kind = 'imap_op' AND payload->>'op' = 'move' AND run_at <= now()`, m.accID); n == 0 {
			t.Error("no immediate move job")
		}
		if n := m.count(`SELECT count(*) FROM jobs j JOIN operations o ON o.id = j.group_id WHERE j.account_id = $1`, m.accID); n != 0 {
			t.Errorf("undoable operations recorded: %d", n)
		}
		fid := idOf("f_", f.ID)
		if n := m.count(`SELECT count(*) FROM events WHERE type = 'filter.applied' AND (payload->>'filterId')::bigint = $1
			AND payload->>'messages' = '2' AND payload->>'threads' = '2' AND payload->>'origin' = 'tab-f'`, fid); n != 1 {
			t.Error("no filter.applied event")
		}
		if n := m.count(`SELECT count(*) FROM events WHERE type = 'thread.updated' AND (payload->>'threadId')::bigint = $1`, t1); n == 0 {
			t.Error("no thread.updated for the filtered thread")
		}
		serve(m.web, m.as(req{method: "DELETE", path: "/filters/" + f.ID}))
	})

	t.Run("new mail after filters_since", func(t *testing.T) {
		_, f := create(map[string]any{"criteria": "from:alerts@bank.pl", "actions": map[string]any{"archive": true, "star": true, "addLabelIds": []string{news}}})
		// filters_since after the message: no filters (only apply).
		exec(`UPDATE accounts SET filters_since = now() WHERE id = $1`, m.accID)
		old, _ := m.ingest(21, "From: alerts@bank.pl\r\nSubject: Old\r\nMessage-ID: <a1@silo.test>\r\n\r\n")
		if n := m.count(`SELECT count(*) FROM jobs WHERE kind = 'filter_eval'`); n != 0 {
			t.Fatalf("filter_eval for a message older than filters_since: %d", n)
		}
		exec(`UPDATE accounts SET filters_since = now() - interval '2 hours' WHERE id = $1`, m.accID)
		nw, _ := m.ingest(22, "From: alerts@bank.pl\r\nSubject: New\r\nMessage-ID: <a2@silo.test>\r\n\r\n")
		other, _ := m.ingest(23, "From: jan@firma.pl\r\nSubject: New 2\r\nMessage-ID: <a3@silo.test>\r\n\r\n")
		// Ingesting the same message again (known location) is not new mail.
		m.ingest(22, "From: alerts@bank.pl\r\nSubject: New\r\nMessage-ID: <a2@silo.test>\r\n\r\n")
		if n := m.runFilterJobs("filter_eval"); n != 2 {
			t.Fatalf("filter_eval jobs = %d, want 2 (one per new batch)", n)
		}
		if l := m.labelsOfMsg(nw); l["INBOX"] || !l["STARRED"] || !l["l_Newsletters"] {
			t.Errorf("new message: %v", l)
		}
		if l := m.labelsOfMsg(old); !l["INBOX"] || l["STARRED"] {
			t.Errorf("message before filters_since: %v", l)
		}
		if l := m.labelsOfMsg(other); !l["INBOX"] || l["STARRED"] {
			t.Errorf("unmatched message: %v", l)
		}
		// A disabled filter does not run.
		serve(m.web, m.as(req{method: "PUT", path: "/filters/" + f.ID, body: map[string]any{"criteria": "from:alerts@bank.pl", "actions": map[string]any{"star": true}, "enabled": false}}))
		m.ingest(24, "From: alerts@bank.pl\r\nSubject: New 3\r\nMessage-ID: <a4@silo.test>\r\n\r\n")
		if n := m.count(`SELECT count(*) FROM jobs WHERE kind = 'filter_eval'`); n != 0 {
			t.Errorf("filter_eval with only disabled filters: %d", n)
		}
		serve(m.web, m.as(req{method: "DELETE", path: "/filters/" + f.ID}))
	})

	t.Run("archived new mail does not wake a snoozed thread", func(t *testing.T) {
		_, f := create(map[string]any{"criteria": "subject:snoozy", "actions": map[string]any{"archive": true}})
		_, th := m.ingest(31, "From: x@y.pl\r\nSubject: snoozy\r\nMessage-ID: <z1@silo.test>\r\n\r\n")
		exec(`INSERT INTO snoozes (user_id, account_id, thread_id, wake_at) SELECT user_id, id, $2, now() + interval '1 day' FROM accounts WHERE id = $1`, m.accID, th)
		m.ingest(32, "From: x@y.pl\r\nSubject: Re: snoozy\r\nIn-Reply-To: <z1@silo.test>\r\nMessage-ID: <z2@silo.test>\r\n\r\n")
		m.runFilterJobs("filter_eval")
		if n := m.count(`SELECT count(*) FROM snoozes WHERE thread_id = $1 AND wake_at > now()`, th); n != 1 {
			t.Error("a filtered-out reply woke the snoozed thread")
		}
		serve(m.web, m.as(req{method: "PUT", path: "/filters/" + f.ID, body: map[string]any{"criteria": "subject:snoozy", "actions": map[string]any{"star": true}}}))
		m.ingest(33, "From: x@y.pl\r\nSubject: Re: snoozy\r\nIn-Reply-To: <z1@silo.test>\r\nMessage-ID: <z3@silo.test>\r\n\r\n")
		m.runFilterJobs("filter_eval")
		if n := m.count(`SELECT count(*) FROM snoozes WHERE thread_id = $1 AND wake_at <= now()`, th); n != 1 {
			t.Error("a reply left in INBOX by the filters did not wake the thread")
		}
		serve(m.web, m.as(req{method: "DELETE", path: "/filters/" + f.ID}))
	})
}

// fakeBodies stands for the sync engine: it either never delivers (waits
// for its deadline) or stores a body at once.
type fakeBodies struct {
	m       *mailEnv
	deliver bool
}

func (f *fakeBodies) FetchBody(ctx context.Context, _, messageID int64) error {
	if !f.deliver {
		<-ctx.Done()
		return ctx.Err()
	}
	_, err := f.m.pool.Exec(ctx, `UPDATE messages SET body_text = 'Kod: zebrakod 42', body_state = 'full' WHERE id = $1`, messageID)
	return err
}

// TestFilterBodyWait: a filter on a body word waits for the body of a new
// message up to BodyWait, then runs on the headers (plan 3.11).
func TestFilterBodyWait(t *testing.T) {
	m := newMailEnv(t)
	ctx := context.Background()
	var userID int64
	if err := m.pool.QueryRow(ctx, `UPDATE accounts SET filters_since = now() - interval '1 day' WHERE id = $1 RETURNING user_id`, m.accID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	const wait = 700 * time.Millisecond
	fb := &fakeBodies{m: m}
	m.filters = filters.New(filters.Options{Pool: m.pool, Ops: ops.NewService(m.pool, time.Second), Bodies: fb, BodyWait: wait})
	if _, err := m.filters.Create(ctx, userID, filters.Input{AccountID: m.accID, Criteria: "zebrakod", Enabled: true,
		Actions: filters.Actions{Star: true}}); err != nil {
		t.Fatal(err)
	}

	// Never delivered: evaluated on the headers once the wait is over.
	start := time.Now()
	slow, _ := m.ingest(1, "From: ala@firma.pl\r\nSubject: Plain\r\nMessage-ID: <b1@silo.test>\r\n\r\n")
	m.runFilterJobs("filter_eval")
	if d := time.Since(start); d < wait {
		t.Errorf("evaluated after %s, before the body wait of %s", d, wait)
	}
	if l := m.labelsOfMsg(slow); l["STARRED"] {
		t.Errorf("matched without a body: %v", l)
	}

	// Delivered: the body word matches.
	fb.deliver = true
	start = time.Now()
	fast, _ := m.ingest(2, "From: ala@firma.pl\r\nSubject: Plain 2\r\nMessage-ID: <b2@silo.test>\r\n\r\n")
	m.runFilterJobs("filter_eval")
	if d := time.Since(start); d >= wait {
		t.Errorf("waited %s although the body came at once", d)
	}
	if l := m.labelsOfMsg(fast); !l["STARRED"] {
		t.Errorf("body word not matched: %v", l)
	}
}

// TestFilterForward: forwardTo sends a copy of new mail at once through the
// compose path (no undo-send window), never of the account's own mail.
func TestFilterForward(t *testing.T) {
	m := newMailEnv(t)
	ctx := context.Background()
	var userID int64
	var self string
	if err := m.pool.QueryRow(ctx, `UPDATE accounts SET filters_since = now() - interval '1 day' WHERE id = $1 RETURNING user_id, email::text`, m.accID).Scan(&userID, &self); err != nil {
		t.Fatal(err)
	}
	r := serve(m.web, m.as(req{method: "POST", path: "/filters", body: map[string]any{"criteria": "subject:faktura",
		"actions": map[string]any{"forwardTo": " Ksiegowa@Biuro.pl ", "markRead": true}}}))
	var f api.Filter
	r.json(&f)
	if r.Code != http.StatusCreated || f.Actions.ForwardTo == nil || *f.Actions.ForwardTo != "ksiegowa@biuro.pl" {
		t.Fatalf("create: %d %s", r.Code, r.Body)
	}
	orig, _ := m.ingest(1, "From: sklep@x.pl\r\nSubject: Faktura 7\r\nMessage-ID: <fw1@silo.test>\r\n\r\n")
	m.ingest(2, "From: "+self+"\r\nSubject: Faktura od mnie\r\nMessage-ID: <fw2@silo.test>\r\n\r\n")
	m.runFilterJobs("filter_eval")
	if n := m.count(`SELECT count(*) FROM outbound o JOIN drafts d ON d.message_id = o.message_id
		WHERE o.status = 'queued' AND o.send_at <= now() AND d.to_addrs @> '[{"email":"ksiegowa@biuro.pl"}]' AND d.in_reply_to_message_id = $1`, orig); n != 1 {
		t.Errorf("forwards queued for the original = %d", n)
	}
	if n := m.count(`SELECT count(*) FROM outbound`); n != 1 {
		t.Errorf("outbound rows = %d (own mail must not be forwarded)", n)
	}
	if n := m.count(`SELECT count(*) FROM jobs WHERE kind = 'send' AND run_at <= now()`); n != 1 {
		t.Errorf("immediate send jobs = %d", n)
	}
	if l := m.labelsOfMsg(orig); l["UNREAD"] {
		t.Errorf("other actions of the forwarding filter not applied: %v", l)
	}
}
