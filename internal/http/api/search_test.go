package api_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/accounts"
	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/mailview"
	"github.com/silopost/silo-server/internal/ops"
	silosync "github.com/silopost/silo-server/internal/sync"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

func TestSearchAPI(t *testing.T) {
	e := newEnv(t)
	h := e.web
	ctx := context.Background()
	l := e.login(h, imaptest.User, imaptest.Pass)
	as := func(c req) req { c.cookie, c.csrf = l.cookie, l.csrf; return c }
	get := func(h http.Handler, path string) resp { return serve(h, as(req{method: "GET", path: path})) }
	accID, _ := strconv.ParseInt(strings.TrimPrefix(l.body.Accounts[0].ID, "acc_"), 36, 64)

	folders := map[string]int64{}
	for name, role := range map[string]string{"INBOX": "inbox", "Junk": "junk", "Trash": "trash"} {
		var id int64
		if err := e.pool.QueryRow(ctx, `
INSERT INTO mailbox_folders (account_id, user_id, name, name_raw, role, uidvalidity)
SELECT id, user_id, $2, $2, $3, 1 FROM accounts WHERE id = $1 RETURNING id`, accID, name, role).Scan(&id); err != nil {
			t.Fatal(err)
		}
		folders[name] = id
	}
	ingest := func(folder string, uid int, from, subject string, when time.Time) {
		t.Helper()
		err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
			_, err := mail.IngestBatch(ctx, tx, accID, folders[folder], []mail.FetchedMessage{{
				UID: uint32(uid), Size: 1000, InternalDate: when,
				Header: fmt.Appendf(nil, "From: %s\r\nSubject: %s\r\nMessage-ID: <s%d.%s@silo.test>\r\n\r\n", from, subject, uid, folder),
			}})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	for i := 1; i <= 55; i++ {
		ingest("INBOX", i, "Jan Kowalski <jan@firma.pl>", fmt.Sprintf("Faktura nr %d", i), base.Add(time.Duration(i)*time.Hour))
	}
	for i := 56; i <= 60; i++ {
		ingest("INBOX", i, "anna@nowak.pl", fmt.Sprintf("Spotkanie %d", i), base.Add(time.Duration(i)*time.Hour))
	}
	ingest("Junk", 1, "spam@spam.test", "Faktura do zapłaty", base)
	ingest("Trash", 1, "jan@firma.pl", "Faktura stara", base)

	// Search, page by page.
	var out api.ThreadList
	r := get(h, "/threads?q=faktura&limit=20")
	r.json(&out)
	if r.Code != http.StatusOK || len(out.Threads) != 20 || out.NextCursor == nil || out.ResultSizeEstimate != 55 ||
		out.Indexing == nil || out.Indexing.Done != 0 || out.Approximate {
		t.Fatalf("first page: %d %s", r.Code, r.Body)
	}
	seen := map[string]bool{}
	for page := 0; ; page++ {
		for _, th := range out.Threads {
			if seen[th.ID] || !strings.HasPrefix(th.Subject, "Faktura nr ") {
				t.Fatalf("page %d: thread %s %q", page, th.ID, th.Subject)
			}
			seen[th.ID] = true
		}
		if out.NextCursor == nil {
			break
		}
		get(h, "/threads?q=faktura&limit=20&cursor="+*out.NextCursor).json(&out)
	}
	if len(seen) != 55 {
		t.Errorf("paged %d threads, want 55", len(seen))
	}
	for q, want := range map[string]int{
		"faktura in:anywhere":          57,
		"faktura in:spam":              1,
		"faktura -in:trash":            56,
		"from:anna":                    5,
		`"faktura nr 7"`:               1,
		"subject:(faktura 1)":          11,
		"{spotkanie zapłaty}":          5,
		"from:jan -faktura":            0,
		"faktura before:2026/10/02":    15,
		"label:nieznana":               0,
		"has:attachment OR is:starred": 0,
		"is:unread from:(jan OR anna)": 60,
	} {
		get(h, "/threads?limit=100&q="+url.QueryEscape(q)).json(&out)
		if len(out.Threads) != want || out.ResultSizeEstimate != want {
			t.Errorf("%q: %d threads (estimate %d), want %d", q, len(out.Threads), out.ResultSizeEstimate, want)
		}
	}
	// labelId narrows the search like in:; SPAM and TRASH lift the exclusion.
	get(h, "/threads?q=faktura&labelId=SPAM").json(&out)
	if len(out.Threads) != 1 || out.Threads[0].Subject != "Faktura do zapłaty" {
		t.Errorf("faktura in SPAM: %+v", out.Threads)
	}
	get(h, "/threads?q=spotkanie&labelId=INBOX").json(&out)
	if len(out.Threads) != 5 {
		t.Errorf("spotkanie in INBOX: %d", len(out.Threads))
	}

	// indexing: the share of downloaded bodies; absent without q.
	if _, err := e.pool.Exec(ctx, `UPDATE messages SET body_state = 'full' WHERE account_id = $1 AND subject LIKE 'Spotkanie%'`, accID); err != nil {
		t.Fatal(err)
	}
	get(h, "/threads?q=spotkanie").json(&out)
	if out.Indexing == nil || out.Indexing.Done < 0.08 || out.Indexing.Done > 0.09 { // 5 of 57
		t.Errorf("indexing = %+v", out.Indexing)
	}
	if r := get(h, "/threads?labelId=INBOX"); strings.Contains(r.Body.String(), `"indexing"`) || strings.Contains(r.Body.String(), `"approximate"`) {
		t.Errorf("list without q: %s", r.Body)
	}

	// Refused queries.
	for _, q := range []string{"a)", "(a}", strings.Repeat("x ", 70)} {
		if r := get(h, "/threads?q="+url.QueryEscape(q)); r.Code != http.StatusUnprocessableEntity || r.code() != api.CodeInvalidQuery {
			t.Errorf("%q: %d %s", q, r.Code, r.Body)
		}
	}

	// A search over its time limit answers an empty, approximate page.
	view := mailview.New(e.pool, nil, nil)
	view.SearchTimeout = 200 * time.Millisecond
	slow := chi.NewRouter()
	api.Mount(slow, api.Options{
		Auth:     auth.New(auth.Options{Pool: e.pool, Keys: e.ring, Config: e.cfg}),
		Accounts: accounts.New(accounts.Options{Pool: e.pool, Keys: e.ring, Config: e.cfg}),
		Sync:     silosync.NewService(e.pool), Ops: ops.NewService(e.pool, 10*time.Second), View: view, Drafts: e.drafts,
		PublicURL: publicURL,
	})
	lock, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, `LOCK TABLE message_labels IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r = get(slow, "/threads?q=faktura")
	_ = lock.Rollback(ctx)
	r.json(&out)
	if r.Code != http.StatusOK || !out.Approximate || len(out.Threads) != 0 || out.NextCursor != nil || time.Since(start) > 5*time.Second {
		t.Errorf("timed out search: %d %s", r.Code, r.Body)
	}
	if r := get(slow, "/threads?q=faktura"); !strings.Contains(r.Body.String(), `"resultSizeEstimate":55`) || strings.Contains(r.Body.String(), `"approximate"`) {
		t.Errorf("search after the lock: %s", r.Body)
	}

	// Suggestions: history, contacts, labels, operators.
	if _, err := e.pool.Exec(ctx, `
INSERT INTO contacts (user_id, email, name, sent_count) SELECT user_id, 'janina@firma.pl', 'Janina Zielona', 9 FROM accounts WHERE id = $1
ON CONFLICT DO NOTHING`, accID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO labels (user_id, account_id, name, type) SELECT user_id, id, 'Projekt Alfa', 'user' FROM accounts WHERE id = $1`, accID); err != nil {
		t.Fatal(err)
	}
	suggest := func(q string) []api.SearchSuggestion {
		t.Helper()
		var s api.SearchSuggestions
		r := get(h, "/search/suggest?q="+url.QueryEscape(q))
		if r.Code != http.StatusOK {
			t.Fatalf("suggest %q: %d %s", q, r.Code, r.Body)
		}
		r.json(&s)
		return s.Suggestions
	}
	has := func(list []api.SearchSuggestion, kind, text string) bool {
		return slices.ContainsFunc(list, func(s api.SearchSuggestion) bool { return s.Kind == kind && s.Text == text })
	}
	s := suggest("fak")
	if !has(s, "history", "faktura") || !has(s, "history", "faktura in:anywhere") || len(s) > 5+5+5+5 {
		t.Errorf("history: %+v", s)
	}
	if s := suggest(""); len(s) != 5 || s[0].Kind != "history" || s[0].Text != "faktura" {
		t.Errorf("recent history: %+v", s)
	}
	if s := suggest("faktura from:jan"); !has(s, "contact", "faktura from:janina@firma.pl ") || !has(s, "contact", "faktura from:jan@firma.pl ") {
		t.Errorf("contacts: %+v", s)
	}
	if s := suggest("to:Zielo"); !has(s, "contact", "to:janina@firma.pl ") {
		t.Errorf("contacts by name: %+v", s)
	}
	if s := suggest("-proj"); !has(s, "label", "-label:Projekt-Alfa ") {
		t.Errorf("labels: %+v", s)
	}
	if s := suggest("in:alf"); !has(s, "label", "in:Projekt-Alfa ") {
		t.Errorf("labels with in: %+v", s)
	}
	if s := suggest("faktura has:a"); !has(s, "operator", "faktura has:attachment ") {
		t.Errorf("operators: %+v", s)
	}
	if s := suggest("is:u"); !has(s, "operator", "is:unread ") || has(s, "operator", "is:read ") {
		t.Errorf("is:u: %+v", s)
	}
	if s := suggest("fr"); !has(s, "operator", "from:") {
		t.Errorf("fr: %+v", s)
	}

	// History: at most 50 entries per account.
	for i := range 55 {
		get(h, "/threads?q="+url.QueryEscape(fmt.Sprintf("zz%d", i)))
	}
	if n := e.count(`SELECT count(*) FROM search_history WHERE account_id = $1`, accID); n != 50 {
		t.Errorf("history entries = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM search_history WHERE account_id = $1 AND query = 'zz54'`, accID); n != 1 {
		t.Error("newest query not kept")
	}

	// Forget one query, then everything; CSRF is required for a cookie session.
	if r := serve(h, req{method: "DELETE", path: "/search/history?query=zz54", cookie: l.cookie}); r.Code != http.StatusForbidden {
		t.Errorf("delete without CSRF: %d", r.Code)
	}
	if r := serve(h, as(req{method: "DELETE", path: "/search/history?query=zz54"})); r.Code != http.StatusNoContent {
		t.Errorf("delete one: %d %s", r.Code, r.Body)
	}
	if n := e.count(`SELECT count(*) FROM search_history WHERE account_id = $1`, accID); n != 49 {
		t.Errorf("after delete one: %d", n)
	}
	if r := serve(h, as(req{method: "DELETE", path: "/search/history"})); r.Code != http.StatusNoContent {
		t.Errorf("delete all: %d %s", r.Code, r.Body)
	}
	if s := suggest(""); len(s) != 0 {
		t.Errorf("history after delete: %+v", s)
	}

	// Native clients use a bearer token; no session is 401.
	r = serve(h, req{method: "POST", path: "/session", body: map[string]any{
		"email": imaptest.User, "password": imaptest.Pass, "client": map[string]string{"kind": "bearer", "name": "test"},
	}})
	var sb api.SessionBody
	r.json(&sb)
	if r := serve(h, req{method: "GET", path: "/search/suggest?q=is:s", headers: bearerHdr(sb.Token)}); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "is:starred") {
		t.Errorf("bearer suggest: %d %s", r.Code, r.Body)
	}
	if r := serve(h, req{method: "DELETE", path: "/search/history", headers: bearerHdr(sb.Token)}); r.Code != http.StatusNoContent {
		t.Errorf("bearer delete: %d %s", r.Code, r.Body)
	}
	if r := serve(h, req{method: "GET", path: "/search/suggest?q=x"}); r.Code != http.StatusUnauthorized {
		t.Errorf("suggest without session: %d", r.Code)
	}
}
