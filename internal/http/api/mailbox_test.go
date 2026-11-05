package api_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/go-chi/chi/v5"

	"github.com/silopost/silo-server/internal/accounts"
	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/blob"
	"github.com/silopost/silo-server/internal/events"
	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/imapx"
	"github.com/silopost/silo-server/internal/mailview"
	"github.com/silopost/silo-server/internal/ops"
	silosync "github.com/silopost/silo-server/internal/sync"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
	"github.com/silopost/silo-server/internal/worker"
)

// stack is the whole server over one Postgres and one Dovecot: sync engine,
// job worker, event hub and the HTTP API.
type stack struct {
	*env
	h     http.Handler // with content fetching from the mail server
	noFet http.Handler // same data, but no way to reach the mail server
	srv   *httptest.Server
	hub   *events.Hub
	mgr   *silosync.Manager
	l     login
}

type stackOpts struct {
	heartbeat time.Duration
	buffer    int
}

func newStack(t *testing.T, o stackOpts) *stack {
	t.Helper()
	e := newEnv(t)
	blobs, err := blob.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	jobs := worker.New(worker.Options{Pool: e.pool, Logger: logger, Concurrency: 2, PollInterval: 200 * time.Millisecond})
	mgr := silosync.NewManager(silosync.Deps{
		Pool: e.pool, Keys: e.ring, Blobs: blobs, Logger: logger,
		Settings: silosync.Settings{MaxConnections: 3, InitialConcurrency: 2, PollInterval: time.Minute, BackfillRate: 50 << 20,
			MaxMessageBytes: 50 << 20, InsecureTLS: true},
		Backoff: []time.Duration{100 * time.Millisecond, 200 * time.Millisecond},
	})
	mgr.RegisterJobs(jobs)
	hub := events.New(events.Options{Pool: e.pool, Logger: logger, Buffer: o.buffer, PollInterval: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 3)
	go func() { _ = jobs.Run(ctx); done <- struct{}{} }()
	go func() { _ = mgr.Run(ctx); done <- struct{}{} }()
	go func() { _ = hub.Run(ctx); done <- struct{}{} }()
	t.Cleanup(func() {
		cancel()
		for range 3 {
			<-done
		}
	})

	mk := func(fetcher mailview.BodyFetcher) http.Handler {
		ao := auth.Options{Pool: e.pool, Keys: e.ring, Config: e.cfg, MinLoginDuration: 20 * time.Millisecond,
			LoginLimit: auth.Limit{Burst: 1000, Every: time.Second}}
		r := chi.NewRouter()
		api.Mount(r, api.Options{
			Auth: auth.New(ao), Accounts: accounts.New(accounts.Options{Pool: e.pool, Keys: e.ring, Config: e.cfg}),
			Sync: silosync.NewService(e.pool), Ops: ops.NewService(e.pool, 100*time.Millisecond),
			View: mailview.New(e.pool, blobs, fetcher), Events: hub, SSEHeartbeat: o.heartbeat, PublicURL: publicURL,
		})
		return r
	}
	s := &stack{env: e, h: mk(mgr), noFet: mk(nil), hub: hub, mgr: mgr}
	s.srv = httptest.NewServer(s.h)
	t.Cleanup(s.srv.Close)
	return s
}

// ---- messages ---------------------------------------------------------------

const (
	crlf        = "\r\n"
	logoCID     = "logo@silo.test"
	evilSubject = "Evil HTML"
)

var (
	pdfBytes = append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte{0x00, 0xff, 0x10, 0x80, 'a', '\n'}, 300)...)
	pngBytes = append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte{1, 2, 3, 4, 0xfe}, 50)...)
)

func b64(b []byte) string {
	s := base64.StdEncoding.EncodeToString(b)
	var out strings.Builder
	for len(s) > 76 {
		out.WriteString(s[:76] + crlf)
		s = s[76:]
	}
	return out.String() + s + crlf
}

func hdr(id int, from, subject string, when time.Time) string {
	return fmt.Sprintf("From: %s%sTo: silo@silo.test%sSubject: %s%sMessage-ID: <m%d@silo.test>%sDate: %s%sMIME-Version: 1.0%s",
		from, crlf, crlf, subject, crlf, id, crlf, when.Format(time.RFC1123Z), crlf, crlf)
}

func plainMsg(id int, subject, body string, when time.Time) []byte {
	return []byte(hdr(id, "Jan Kowalski <jan@firma.pl>", subject, when) +
		"Content-Type: text/plain; charset=utf-8" + crlf + crlf + body + crlf)
}

// htmlMsg is multipart/alternative with an HTML part that refers to an inline
// image by cid, plus a PDF attachment.
func htmlMsg(id int, subject, text, html string, when time.Time, withPDF bool) []byte {
	var b strings.Builder
	b.WriteString(hdr(id, "Anna <anna@firma.pl>", subject, when))
	b.WriteString("Content-Type: multipart/mixed; boundary=\"MIX\"" + crlf + crlf)
	b.WriteString("--MIX" + crlf + "Content-Type: multipart/alternative; boundary=\"ALT\"" + crlf + crlf)
	b.WriteString("--ALT" + crlf + "Content-Type: text/plain; charset=utf-8" + crlf + crlf + text + crlf)
	b.WriteString("--ALT" + crlf + "Content-Type: multipart/related; boundary=\"REL\"" + crlf + crlf)
	b.WriteString("--REL" + crlf + "Content-Type: text/html; charset=utf-8" + crlf + crlf + html + crlf)
	b.WriteString("--REL" + crlf + "Content-Type: image/png; name=\"logo.png\"" + crlf + "Content-ID: <" + logoCID + ">" + crlf +
		"Content-Disposition: inline; filename=\"logo.png\"" + crlf + "Content-Transfer-Encoding: base64" + crlf + crlf + b64(pngBytes))
	b.WriteString("--REL--" + crlf + "--ALT--" + crlf)
	if withPDF {
		b.WriteString("--MIX" + crlf + "Content-Type: application/pdf; name=\"oferta.pdf\"" + crlf +
			"Content-Disposition: attachment; filename=\"oferta.pdf\"" + crlf + "Content-Transfer-Encoding: base64" + crlf + crlf + b64(pdfBytes))
	}
	b.WriteString("--MIX--" + crlf)
	return []byte(b.String())
}

const evilHTML = `<html><head><title>t</title><meta http-equiv="refresh" content="0;url=http://evil"><style>@import url(http://evil/x.css); b {color:red}</style></head>` +
	`<body onload="steal()"><p onclick="x()" style="color:red">Hello <b>world</b></p><script>alert(1)</script>` +
	`<img src=x onerror=alert(1)><a href="javascript:alert(1)">bad</a><a href=" JaVa&#x09;script:alert(2)">bad2</a>` +
	`<a href="https://ok.example/p">fine</a><form action="/x"><input name="a"></form><iframe src="//e"></iframe>` +
	`<object data="x"></object><embed src="x"><img src="cid:` + logoCID + `"><img src="https://remote.example/pic.png"></body></html>`

type mailbox struct {
	t        *testing.T
	host     string
	port     int
	raw      map[string][]byte // Message-ID -> bytes as appended
	uids     map[string]imap.UID
	inboxIDs []int
}

func (m *mailbox) client() *imapclient.Client {
	m.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cl, err := imapx.Dial(ctx, m.host, m.port, imapx.TLSNone, false)
	if err != nil {
		m.t.Fatal(err)
	}
	m.t.Cleanup(func() { cl.Close() })
	if err := cl.Login(ctx, imaptest.User, imaptest.Pass); err != nil {
		m.t.Fatal(err)
	}
	return cl.IMAP()
}

func (m *mailbox) appendTo(c *imapclient.Client, folder string, id int, raw []byte, seen bool, when time.Time) imap.UID {
	m.t.Helper()
	var flags []imap.Flag
	if seen {
		flags = append(flags, imap.FlagSeen)
	}
	cmd := c.Append(folder, int64(len(raw)), &imap.AppendOptions{Flags: flags, Time: when})
	if _, err := cmd.Write(raw); err != nil {
		m.t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		m.t.Fatal(err)
	}
	data, err := cmd.Wait()
	if err != nil {
		m.t.Fatalf("APPEND %s: %v", folder, err)
	}
	m.raw[fmt.Sprintf("m%d@silo.test", id)] = raw
	return data.UID
}

// ---- SSE client ---------------------------------------------------------------

type sseEvent struct {
	id, typ string
	data    string
}

type sseStream struct {
	events   chan sseEvent
	comments chan string
	cancel   context.CancelFunc
	status   int
}

func openSSE(t *testing.T, base string, l login, lastID string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	rq, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/events", nil)
	rq.AddCookie(l.cookie)
	if lastID != "" {
		rq.Header.Set("Last-Event-ID", lastID)
	}
	res, err := http.DefaultClient.Do(rq)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	s := &sseStream{events: make(chan sseEvent, 1000), comments: make(chan string, 100), cancel: cancel, status: res.StatusCode}
	t.Cleanup(cancel)
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		cancel()
		t.Fatalf("GET /events: %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	go func() {
		defer res.Body.Close()
		defer close(s.events)
		sc := bufio.NewScanner(res.Body)
		var cur sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.typ != "" {
					s.events <- cur
				}
				cur = sseEvent{}
			case strings.HasPrefix(line, ":"):
				select {
				case s.comments <- line:
				default:
				}
			case strings.HasPrefix(line, "id: "):
				cur.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				cur.typ = line[7:]
			case strings.HasPrefix(line, "data: "):
				cur.data = line[6:]
			}
		}
	}()
	return s
}

func (s *sseStream) wait(t *testing.T, timeout time.Duration, what string, pred func(sseEvent) bool) sseEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case e, ok := <-s.events:
			if !ok {
				t.Fatalf("stream closed while waiting for %s", what)
			}
			if pred(e) {
				return e
			}
		case <-deadline:
			t.Fatalf("no %s within %v", what, timeout)
		}
	}
}

func ofType(typ string) func(sseEvent) bool { return func(e sseEvent) bool { return e.typ == typ } }

// ---- the test ----------------------------------------------------------------------

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	start := time.Now()
	for time.Since(start) < timeout {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestMailboxAPI(t *testing.T) {
	s := newStack(t, stackOpts{heartbeat: 300 * time.Millisecond, buffer: 64})
	mb := &mailbox{t: t, host: s.cfg.DefaultIMAPHost, port: s.cfg.DefaultIMAPPort, raw: map[string][]byte{}, uids: map[string]imap.UID{}}
	c := mb.client()
	for _, f := range []string{"Sent", "Projekty"} {
		if err := c.Create(f, nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}

	// 30 messages: 20 in INBOX (a third read; some with HTML, attachment, the
	// evil one), 5 in Sent, 5 in Projekty.
	now := time.Now().Truncate(time.Second)
	when := func(i int) time.Time { return now.Add(-time.Duration(40-i) * time.Hour) }
	var evilID, pdfID int
	for i := 0; i < 20; i++ {
		id := 100 + i
		subj := fmt.Sprintf("Inbox subject %02d", i)
		var raw []byte
		switch i {
		case 5:
			evilID = id
			raw = htmlMsg(id, evilSubject, "plain evil", evilHTML, when(i), false)
		case 7:
			pdfID = id
			raw = htmlMsg(id, subj, "needleword in text", "<p>needleword in <b>html</b></p>", when(i), true)
		default:
			raw = plainMsg(id, subj, "Body "+subj, when(i))
		}
		mb.appendTo(c, "INBOX", id, raw, i%3 == 0, when(i))
		mb.inboxIDs = append(mb.inboxIDs, id)
	}
	for i := 0; i < 5; i++ {
		id := 200 + i
		raw := []byte(hdr(id, "silo@silo.test", fmt.Sprintf("Sent subject %d", i), when(i)) + "Content-Type: text/plain" + crlf + crlf + "sent" + crlf)
		mb.appendTo(c, "Sent", id, raw, true, when(i))
	}
	for i := 0; i < 5; i++ {
		id := 300 + i
		mb.appendTo(c, "Projekty", id, plainMsg(id, fmt.Sprintf("Project subject %d", i), "project body", when(i)), false, when(i))
	}

	s.l = s.login(s.h, imaptest.User, imaptest.Pass)
	l := s.l
	as := func(q req) req { q.cookie, q.csrf = l.cookie, l.csrf; return q }
	get := func(h http.Handler, path string) resp { return serve(h, as(req{method: "GET", path: path})) }
	accStr := l.body.Accounts[0].ID
	accID, _ := strconv.ParseInt(strings.TrimPrefix(accStr, "acc_"), 36, 64)

	waitFor(t, 120*time.Second, "initial sync and bodies", func() bool {
		return s.count(`SELECT count(*) FROM messages WHERE account_id = $1 AND body_state = 'full'`, accID) == 30 &&
			s.count(`SELECT count(*) FROM accounts WHERE id = $1 AND sync_state = 'live'`, accID) == 1
	})

	t.Run("labels", func(t *testing.T) {
		var out struct{ Labels []api.Label }
		r := get(s.h, "/labels?accountId="+accStr)
		r.json(&out)
		if r.Code != http.StatusOK {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		byID := map[string]api.Label{}
		for _, lb := range out.Labels {
			byID[lb.ID] = lb
		}
		if lb := byID["INBOX"]; lb.Type != "system" || lb.ThreadsTotal != 20 || lb.ThreadsUnread != 13 || lb.KeywordState != nil {
			t.Errorf("INBOX: %+v", lb)
		}
		if lb := byID["SENT"]; lb.ThreadsTotal != 5 || lb.ThreadsUnread != 0 {
			t.Errorf("SENT: %+v", lb)
		}
		if lb, ok := byID["UNREAD"]; !ok || lb.ThreadsTotal != 18 {
			t.Errorf("UNREAD: %+v", lb)
		}
		var proj api.Label
		for _, lb := range out.Labels {
			if lb.Type == "folder" && lb.Name == "Projekty" {
				proj = lb
			}
		}
		if proj.ID == "" || !strings.HasPrefix(proj.ID, "l_") || proj.ThreadsTotal != 5 || proj.ThreadsUnread != 5 || proj.FolderID == nil {
			t.Errorf("Projekty: %+v", proj)
		}
		// Without accountId: the active account.
		r = get(s.h, "/labels")
		if r.Code != http.StatusOK {
			t.Errorf("default account: %d", r.Code)
		}
		if r := get(s.h, "/labels?accountId=acc_zzzz"); r.Code != http.StatusNotFound {
			t.Errorf("foreign account: %d", r.Code)
		}
		if r := serve(s.h, req{method: "GET", path: "/labels"}); r.Code != http.StatusUnauthorized {
			t.Errorf("no session: %d", r.Code)
		}
	})

	t.Run("thread list pagination", func(t *testing.T) {
		seen := map[string]bool{}
		var order []time.Time
		cursor, pages := "", 0
		for {
			path := "/threads?labelId=INBOX&limit=10&accountId=" + accStr
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			var out api.ThreadList
			r := get(s.h, path)
			r.json(&out)
			if r.Code != http.StatusOK {
				t.Fatalf("%d %s", r.Code, r.Body)
			}
			pages++
			if out.ResultSizeEstimate != 20 {
				t.Errorf("estimate %d", out.ResultSizeEstimate)
			}
			for _, th := range out.Threads {
				if seen[th.ID] {
					t.Errorf("duplicate %s", th.ID)
				}
				seen[th.ID] = true
				order = append(order, th.LastMessageAt)
				if !slices.Contains(th.LabelIDs, "INBOX") || th.AccountID != accStr || th.Participants == nil {
					t.Errorf("thread shape: %+v", th)
				}
			}
			if out.NextCursor == nil {
				break
			}
			cursor = *out.NextCursor
			if pages > 5 {
				t.Fatal("too many pages")
			}
		}
		if pages != 2 || len(seen) != 20 {
			t.Errorf("pages %d, threads %d", pages, len(seen))
		}
		if !slices.IsSortedFunc(order, func(a, b time.Time) int { return b.Compare(a) }) {
			t.Errorf("not newest first: %v", order)
		}
		// Exactly one page: no cursor.
		var out api.ThreadList
		get(s.h, "/threads?labelId=SENT&limit=50").json(&out)
		if len(out.Threads) != 5 || out.NextCursor != nil {
			t.Errorf("SENT: %d threads, cursor %v", len(out.Threads), out.NextCursor)
		}
		// All mail.
		get(s.h, "/threads?limit=100").json(&out)
		if len(out.Threads) != 30 || out.ResultSizeEstimate != 30 {
			t.Errorf("all mail: %d threads, estimate %d", len(out.Threads), out.ResultSizeEstimate)
		}
		// Full-text.
		get(s.h, "/threads?q=needleword").json(&out)
		if len(out.Threads) != 1 || out.Threads[0].Subject != "Inbox subject 07" || !out.Threads[0].HasAttachments {
			t.Errorf("search: %+v", out.Threads)
		}
		get(s.h, "/threads?q=needleword&labelId=SENT").json(&out)
		if len(out.Threads) != 0 {
			t.Errorf("search in SENT: %+v", out.Threads)
		}
		for name, path := range map[string]string{
			"limit too big": "/threads?limit=101", "bad cursor": "/threads?cursor=%21%21", "limit zero": "/threads?limit=0",
		} {
			if r := get(s.h, path); r.Code != http.StatusBadRequest {
				t.Errorf("%s: %d %s", name, r.Code, r.Body)
			}
		}
		if r := get(s.h, "/threads?labelId=l_zzzz"); r.Code != http.StatusNotFound {
			t.Errorf("unknown label: %d", r.Code)
		}
	})

	threadOfSubject := func(subject string) string {
		var id int64
		if err := s.pool.QueryRow(context.Background(), `SELECT thread_id FROM messages WHERE account_id = $1 AND subject = $2`, accID, subject).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return "t_" + strconv.FormatInt(id, 36)
	}
	var evilMsg, pdfMsg api.Message
	var pdfAtt api.MessageAttachment

	t.Run("thread detail", func(t *testing.T) {
		var th api.ThreadDetail
		r := get(s.h, "/threads/"+threadOfSubject(evilSubject))
		r.json(&th)
		if r.Code != http.StatusOK || len(th.Messages) != 1 || th.AccountID != accStr || th.Subject != evilSubject {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		evilMsg = th.Messages[0]
		m := evilMsg
		if m.BodyState != "full" || !m.HasHTML || m.From.Email != "anna@firma.pl" || m.From.Name != "Anna" ||
			len(m.To) != 1 || m.To[0].Email != "silo@silo.test" || m.Bcc == nil || m.Cc == nil ||
			m.Folder == nil || m.Folder.Role != "inbox" || m.Folder.Name != "INBOX" || !strings.HasPrefix(m.Folder.ID, "f_") ||
			!slices.Contains(m.LabelIDs, "INBOX") || m.BodyText == "" || m.Date.IsZero() || m.PendingSync {
			t.Errorf("message: %+v", m)
		}
		if len(m.Attachments) != 1 || !m.Attachments[0].Inline || m.Attachments[0].Filename != "logo.png" || !m.Attachments[0].Downloaded {
			t.Errorf("attachments: %+v", m.Attachments)
		}
		r = get(s.h, "/threads/"+threadOfSubject("Inbox subject 07"))
		r.json(&th)
		pdfMsg = th.Messages[0]
		for _, a := range pdfMsg.Attachments {
			if a.Filename == "oferta.pdf" {
				pdfAtt = a
			}
		}
		if pdfAtt.ID == "" || pdfAtt.Inline || pdfAtt.ContentType != "application/pdf" || !pdfAtt.Downloaded {
			t.Errorf("pdf attachment: %+v", pdfMsg.Attachments)
		}
		if r := get(s.h, "/threads/t_zzzzzz"); r.Code != http.StatusNotFound {
			t.Errorf("unknown thread: %d", r.Code)
		}
		if r := get(s.h, "/threads/garbage"); r.Code != http.StatusNotFound {
			t.Errorf("bad thread id: %d", r.Code)
		}
	})

	t.Run("html sanitising and headers", func(t *testing.T) {
		r := get(s.h, "/messages/"+evilMsg.ID+"/html")
		if r.Code != http.StatusOK {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		out := r.Body.String()
		for _, bad := range []string{"<script", "alert(", "onerror", "onclick", "onload", "javascript", "java", "<form", "<input", "<iframe", "<object", "<embed",
			"http-equiv", "<meta", "@import", "evil", "steal", "<title"} {
			if strings.Contains(strings.ToLower(out), bad) {
				t.Errorf("output contains %q:\n%s", bad, out)
			}
		}
		for _, good := range []string{"Hello", "<b>world</b>", `href="https://ok.example/p"`, `src="https://remote.example/pic.png"`,
			"color:red", "/api/v1/attachments/a_", "?inline=1"} {
			if !strings.Contains(out, good) {
				t.Errorf("output lacks %q:\n%s", good, out)
			}
		}
		h := r.Header()
		if csp := h.Get("Content-Security-Policy"); csp != "default-src 'none'; img-src data: https: http: cid:; style-src 'unsafe-inline'" {
			t.Errorf("CSP %q", csp)
		}
		if cc := h.Get("Cache-Control"); !strings.Contains(cc, "immutable") || h.Get("ETag") == "" ||
			!strings.HasPrefix(h.Get("Content-Type"), "text/html") || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("headers: %v", h)
		}
		// The cid image resolves to a servable attachment.
		i := strings.Index(out, "/api/v1/attachments/")
		end := strings.Index(out[i:], `"`)
		u := strings.TrimPrefix(out[i:i+end], "/api/v1")
		r = get(s.h, u)
		if r.Code != http.StatusOK || !bytes.Equal(r.Body.Bytes(), pngBytes) || !strings.HasPrefix(r.Header().Get("Content-Disposition"), "inline") ||
			r.Header().Get("Content-Type") != "image/png" {
			t.Errorf("inline image: %d %v", r.Code, r.Header())
		}
		// A plain-text message is shown as preformatted text.
		var th api.ThreadDetail
		get(s.h, "/threads/"+threadOfSubject("Inbox subject 01")).json(&th)
		r = get(s.h, "/messages/"+th.Messages[0].ID+"/html")
		if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "<pre") || !strings.Contains(r.Body.String(), "Body Inbox subject 01") {
			t.Errorf("text message: %d %s", r.Code, r.Body)
		}
	})

	t.Run("raw and attachments", func(t *testing.T) {
		r := get(s.h, "/messages/"+evilMsg.ID+"/raw")
		want := mb.raw[fmt.Sprintf("m%d@silo.test", evilID)]
		if r.Code != http.StatusOK || !bytes.Equal(r.Body.Bytes(), want) || r.Header().Get("Content-Type") != "message/rfc822" ||
			!strings.HasPrefix(r.Header().Get("Content-Disposition"), "attachment") {
			t.Errorf("raw: %d %v (%d bytes, want %d)", r.Code, r.Header(), r.Body.Len(), len(want))
		}
		_ = pdfID
		r = get(s.h, "/attachments/"+pdfAtt.ID)
		if r.Code != http.StatusOK || !bytes.Equal(r.Body.Bytes(), pdfBytes) || r.Header().Get("Content-Type") != "application/pdf" ||
			!strings.Contains(r.Header().Get("Content-Disposition"), `attachment; filename=oferta.pdf`) {
			t.Errorf("attachment: %d %v (%d bytes)", r.Code, r.Header(), r.Body.Len())
		}
		// inline=1 never shows a PDF in the browser.
		r = get(s.h, "/attachments/"+pdfAtt.ID+"?inline=1")
		if !strings.HasPrefix(r.Header().Get("Content-Disposition"), "attachment") {
			t.Errorf("inline pdf: %v", r.Header())
		}
	})

	t.Run("tenancy", func(t *testing.T) {
		other := newUserSession(t, s.env, "other@silo.test")
		bad := []string{
			"/threads/" + threadOfSubject(evilSubject), "/messages/" + evilMsg.ID + "/html", "/messages/" + evilMsg.ID + "/raw",
			"/attachments/" + pdfAtt.ID, "/labels?accountId=" + accStr, "/threads?accountId=" + accStr,
		}
		for _, p := range bad {
			if r := serve(s.h, req{method: "GET", path: p, cookie: other.cookie, csrf: other.csrf}); r.Code != http.StatusNotFound {
				t.Errorf("GET %s as another user: %d %s", p, r.Code, r.Body)
			}
		}
		var lbl string
		var out struct{ Labels []api.Label }
		get(s.h, "/labels").json(&out)
		for _, lb := range out.Labels {
			if lb.ID == "INBOX" {
				lbl = lb.ID
			}
		}
		if r := serve(s.h, req{method: "PATCH", path: "/labels/" + lbl, cookie: other.cookie, csrf: other.csrf, body: map[string]any{"showInList": "hide"}}); r.Code != http.StatusNotFound {
			t.Errorf("patch label of another user: %d", r.Code)
		}
	})

	t.Run("content on demand", func(t *testing.T) {
		mid, _ := strconv.ParseInt(strings.TrimPrefix(pdfMsg.ID, "m_"), 36, 64)
		wantAtt := func() {
			// Back to the state of a message whose body was never downloaded.
			if _, err := s.pool.Exec(context.Background(), `
UPDATE messages SET body_state = 'headers', raw_blob_sha256 = NULL, body_text = '' WHERE id = $1`, mid); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(context.Background(), `UPDATE attachments SET blob_sha256 = NULL WHERE message_id = $1`, mid); err != nil {
				t.Fatal(err)
			}
		}
		wantAtt()
		r := get(s.noFet, "/messages/"+pdfMsg.ID+"/html")
		if r.Code != http.StatusServiceUnavailable || r.code() != api.CodeContentUnavailable || r.Header().Get("Retry-After") == "" {
			t.Errorf("without mail server: %d %v %s", r.Code, r.Header(), r.Body)
		}
		if r := get(s.noFet, "/attachments/"+pdfAtt.ID); r.Code != http.StatusServiceUnavailable {
			t.Errorf("attachment without mail server: %d", r.Code)
		}
		start := time.Now()
		r = get(s.h, "/messages/"+pdfMsg.ID+"/html")
		if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "needleword in <b>html</b>") {
			t.Fatalf("on demand: %d %s", r.Code, r.Body)
		}
		t.Logf("body fetched on demand in %v", time.Since(start))
		if n := s.count(`SELECT count(*) FROM messages WHERE id = $1 AND body_state = 'full' AND raw_blob_sha256 IS NOT NULL AND has_html`, mid); n != 1 {
			t.Error("message not stored as full")
		}
		wantAtt()
		r = get(s.h, "/attachments/"+pdfAtt.ID)
		if r.Code != http.StatusOK || !bytes.Equal(r.Body.Bytes(), pdfBytes) {
			t.Errorf("attachment on demand: %d (%d bytes)", r.Code, r.Body.Len())
		}
		wantAtt()
		r = get(s.h, "/messages/"+pdfMsg.ID+"/raw")
		if r.Code != http.StatusOK || !bytes.Equal(r.Body.Bytes(), mb.raw["m107@silo.test"]) {
			t.Errorf("raw on demand: %d", r.Code)
		}
	})

	t.Run("events", func(t *testing.T) {
		st := openSSE(t, s.srv.URL, l, "")
		// A heartbeat comes without any event.
		select {
		case c := <-st.comments:
			_ = c
		case <-time.After(2 * time.Second):
			t.Fatal("no comment")
		}
		deadline := time.After(3 * time.Second)
		for ping := false; !ping; {
			select {
			case c := <-st.comments:
				ping = c == ": ping"
			case <-deadline:
				t.Fatal("no heartbeat")
			}
		}

		// DoD phase 2: a message appended by another client shows up within 2 s.
		raw := plainMsg(900, "Fresh from Thunderbird", "hello fresh", time.Now())
		mb.appendTo(c, "INBOX", 900, raw, false, time.Now())
		t0 := time.Now()
		ev := st.wait(t, 2*time.Second, "message.new", ofType("message.new"))
		took := time.Since(t0)
		t.Logf("message.new after APPEND: %v", took)
		if !strings.Contains(ev.data, `"subject":"Fresh from Thunderbird"`) || !strings.Contains(ev.data, `"threadId":"t_`) ||
			!strings.Contains(ev.data, `"messageId":"m_`) || !strings.Contains(ev.data, `"accountId":"`+accStr+`"`) ||
			!strings.Contains(ev.data, `"INBOX"`) || ev.id == "" {
			t.Errorf("message.new: %+v", ev)
		}
		// label.counts carries API ids.
		lc := st.wait(t, 3*time.Second, "label.counts", ofType("label.counts"))
		if !strings.Contains(lc.data, `"id":"INBOX"`) {
			t.Errorf("label.counts: %s", lc.data)
		}

		// origin: a modify with X-Silo-Client.
		var th api.ThreadDetail
		get(s.h, "/threads/"+threadOfSubject("Fresh from Thunderbird")).json(&th)

		tid := th.ID
		r := serve(s.h, as(req{method: "POST", path: "/threads/modify", headers: map[string]string{"X-Silo-Client": "tab-9"},
			body: map[string]any{"ids": []string{tid}, "removeLabelIds": []string{"UNREAD"}}}))
		if r.Code != http.StatusOK {
			t.Fatalf("modify: %d %s", r.Code, r.Body)
		}
		up := st.wait(t, 3*time.Second, "thread.updated with origin", func(e sseEvent) bool {
			return e.typ == "thread.updated" && strings.Contains(e.data, `"origin":"tab-9"`)
		})
		if !strings.Contains(up.data, `"threadId":"`+tid+`"`) || !strings.Contains(up.data, `"labelIds":["INBOX"`) {
			t.Errorf("thread.updated: %s", up.data)
		}

		// Replay after the id of message.new returns what followed, including the origin event.
		st2 := openSSE(t, s.srv.URL, l, ev.id)
		got := map[string]bool{}
		for range 3 {
			e := st2.wait(t, 3*time.Second, "replayed event", func(sseEvent) bool { return true })
			got[e.typ] = true
			if e.id == ev.id {
				t.Errorf("replay repeated the last event id")
			}
		}
		if !got["label.counts"] && !got["thread.updated"] {
			t.Errorf("replay: %v", got)
		}
		st2.cancel()

		// Poll returns the same events.
		var poll api.PollResult
		r = get(s.h, "/events/poll?after="+ev.id)
		r.json(&poll)
		if r.Code != http.StatusOK || len(poll.Events) < 2 {
			t.Fatalf("poll: %d %s", r.Code, r.Body)
		}
		foundOrigin := false
		for _, e := range poll.Events {
			if e.Type == "thread.updated" && e.Data["origin"] == "tab-9" {
				foundOrigin = true
			}
			if e.ID <= 0 || e.Data == nil {
				t.Errorf("poll event: %+v", e)
			}
		}
		last := poll.Events[len(poll.Events)-1]
		if !foundOrigin || poll.NextAfter != last.ID {
			t.Errorf("poll: origin %v next %d last %d", foundOrigin, poll.NextAfter, last.ID)
		}
		get(s.h, "/events/poll?after="+strconv.FormatInt(poll.NextAfter, 10)).json(&poll)
		if len(poll.Events) != 0 && poll.Events[0].Type == "message.new" {
			t.Errorf("poll after the end repeated events: %+v", poll.Events)
		}

		// An id the server no longer knows (older than the retention) means resync.
		st3 := openSSE(t, s.srv.URL, l, "999999999")
		if e := st3.wait(t, 2*time.Second, "resync", ofType("resync")); e.data != "{}" {
			t.Errorf("resync: %+v", e)
		}
		st3.cancel()
		get(s.h, "/events/poll?after=999999999").json(&poll)
		if len(poll.Events) != 1 || poll.Events[0].Type != "resync" {
			t.Errorf("poll resync: %+v", poll)
		}

		// Another user's stream does not see our events.
		other := newUserSession(t, s.env, "other2@silo.test")
		so := openSSE(t, s.srv.URL, other, "")
		mb.appendTo(c, "INBOX", 901, plainMsg(901, "Second fresh", "x", time.Now()), false, time.Now())
		st.wait(t, 3*time.Second, "second message.new", func(e sseEvent) bool { return e.typ == "message.new" && strings.Contains(e.data, "Second fresh") })
		select {
		case e := <-so.events:
			t.Errorf("another user got %s", e.typ)
		case <-time.After(300 * time.Millisecond):
		}
		if r := serve(s.h, req{method: "GET", path: "/events"}); r.Code != http.StatusUnauthorized {
			t.Errorf("events without session: %d", r.Code)
		}
		st.cancel()
		so.cancel()
		waitFor(t, 3*time.Second, "clients to leave", func() bool { return s.hub.Clients() == 0 })
	})

	t.Run("label management", func(t *testing.T) {
		jobsDone := func() bool {
			return s.count(`SELECT count(*) FROM jobs WHERE account_id = $1 AND kind = 'imap_op'`, accID) == 0
		}
		serverFlags := func(subject string) []string {
			var folder string
			var uid int64
			if err := s.pool.QueryRow(context.Background(), `
SELECT f.name, l.uid FROM messages m JOIN message_locations l ON l.message_id = m.id JOIN mailbox_folders f ON f.id = l.folder_id
WHERE m.account_id = $1 AND m.subject = $2`, accID, subject).Scan(&folder, &uid); err != nil {
				t.Fatal(err)
			}
			cl := mb.client()
			defer cl.Close()
			if _, err := cl.Select(folder, nil).Wait(); err != nil {
				t.Fatal(err)
			}
			bufs, err := cl.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{Flags: true, UID: true}).Collect()
			if err != nil || len(bufs) != 1 {
				t.Fatalf("fetch flags: %v %d", err, len(bufs))
			}
			var out []string
			for _, f := range bufs[0].Flags {
				out = append(out, string(f))
			}
			return out
		}

		var lb api.Label
		r := serve(s.h, as(req{method: "POST", path: "/labels", headers: map[string]string{"X-Silo-Client": "tab-1"},
			body: map[string]any{"accountId": accStr, "name": "Klienci", "type": "user", "colorBg": "#ff0000"}}))
		r.json(&lb)
		if r.Code != http.StatusCreated || lb.Type != "user" || lb.Name != "Klienci" || lb.KeywordState == nil || *lb.KeywordState != "mirrored" ||
			lb.ColorBG == nil || *lb.ColorBG != "#ff0000" || !strings.HasPrefix(lb.ID, "l_") {
			t.Fatalf("create: %d %s", r.Code, r.Body)
		}
		var kw string
		lid, _ := strconv.ParseInt(strings.TrimPrefix(lb.ID, "l_"), 36, 64)
		if err := s.pool.QueryRow(context.Background(), `SELECT imap_keyword FROM labels WHERE id = $1`, lid).Scan(&kw); err != nil || kw != "Klienci" {
			t.Fatalf("keyword %q %v", kw, err)
		}
		if n := s.count(`SELECT count(*) FROM events WHERE type = 'label.changed' AND payload->>'origin' = 'tab-1'`); n != 1 {
			t.Errorf("label.changed events: %d", n)
		}
		if r := serve(s.h, as(req{method: "POST", path: "/labels", body: map[string]any{"name": "klienci", "type": "user"}})); r.Code != http.StatusConflict || r.code() != api.CodeLabelExists {
			t.Errorf("duplicate: %d %s", r.Code, r.Body)
		}
		if r := serve(s.h, as(req{method: "POST", path: "/labels", body: map[string]any{"name": " ", "type": "user"}})); r.Code != http.StatusBadRequest {
			t.Errorf("blank: %d", r.Code)
		}

		// Apply it to a thread: the keyword reaches the server.
		tid := threadOfSubject("Inbox subject 03")
		if r := serve(s.h, as(req{method: "POST", path: "/threads/modify", body: map[string]any{"ids": []string{tid}, "addLabelIds": []string{lb.ID}}})); r.Code != http.StatusOK {
			t.Fatalf("apply: %d %s", r.Code, r.Body)
		}
		waitFor(t, 20*time.Second, "keyword on the server", func() bool { return slices.Contains(serverFlags("Inbox subject 03"), "Klienci") })

		// Rename: a new keyword replaces the old one on the server.
		r = serve(s.h, as(req{method: "PATCH", path: "/labels/" + lb.ID, body: map[string]any{"name": "Partnerzy", "colorText": "#ffffff"}}))
		r.json(&lb)
		if r.Code != http.StatusOK || lb.Name != "Partnerzy" || lb.ColorText == nil || *lb.ColorText != "#ffffff" || lb.ThreadsTotal != 1 {
			t.Fatalf("rename: %d %s", r.Code, r.Body)
		}
		waitFor(t, 20*time.Second, "renamed keyword on the server", func() bool {
			f := serverFlags("Inbox subject 03")
			return slices.Contains(f, "Partnerzy") && !slices.Contains(f, "Klienci")
		})
		waitFor(t, 20*time.Second, "jobs to finish", jobsDone)
		// The label survived the round trip through sync (no duplicate label from the old keyword).
		if n := s.count(`SELECT count(*) FROM labels WHERE account_id = $1 AND type = 'user'`, accID); n != 1 {
			t.Errorf("user labels after rename: %d", n)
		}
		if n := s.count(`SELECT count(*) FROM message_labels WHERE label_id = $1`, lid); n != 1 {
			t.Errorf("messages with the label: %d", n)
		}

		// Delete: the keyword goes from the server.
		if r := serve(s.h, as(req{method: "DELETE", path: "/labels/" + lb.ID})); r.Code != http.StatusNoContent {
			t.Fatalf("delete: %d %s", r.Code, r.Body)
		}
		waitFor(t, 20*time.Second, "keyword removed on the server", func() bool { return !slices.Contains(serverFlags("Inbox subject 03"), "Partnerzy") })
		waitFor(t, 20*time.Second, "jobs to finish", jobsDone)
		if n := s.count(`SELECT count(*) FROM labels WHERE account_id = $1 AND type = 'user'`, accID); n != 0 {
			t.Errorf("user labels after delete: %d", n)
		}
		if r := serve(s.h, as(req{method: "DELETE", path: "/labels/" + lb.ID})); r.Code != http.StatusNotFound {
			t.Errorf("delete twice: %d", r.Code)
		}

		// System labels are not deletable or renamable.
		if r := serve(s.h, as(req{method: "DELETE", path: "/labels/INBOX"})); r.Code != http.StatusBadRequest {
			t.Errorf("delete INBOX: %d", r.Code)
		}
		if r := serve(s.h, as(req{method: "PATCH", path: "/labels/INBOX?accountId=" + accStr, body: map[string]any{"name": "Skrzynka"}})); r.Code != http.StatusBadRequest {
			t.Errorf("rename INBOX: %d %s", r.Code, r.Body)
		}
		if r := serve(s.h, as(req{method: "PATCH", path: "/labels/INBOX?accountId=" + accStr, body: map[string]any{"showInList": "hide"}})); r.Code != http.StatusOK {
			t.Errorf("hide INBOX: %d %s", r.Code, r.Body)
		}

		// A folder is created on the server and shows up as a folder label after discovery.
		r = serve(s.h, as(req{method: "POST", path: "/labels", body: map[string]any{"name": "Nowy", "type": "folder"}}))
		var fl api.Label
		r.json(&fl)
		if r.Code != http.StatusCreated || fl.Type != "folder" || fl.KeywordState != nil || fl.FolderID == nil {
			t.Fatalf("create folder: %d %s", r.Code, r.Body)
		}
		folderExists := func(name string) bool {
			cl := mb.client()
			defer cl.Close()
			list, err := cl.List("", "*", nil).Collect()
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range list {
				if e.Mailbox == name {
					return true
				}
			}
			return false
		}
		waitFor(t, 20*time.Second, "folder on the server", func() bool { return folderExists("Nowy") })
		waitFor(t, 30*time.Second, "folder discovered", func() bool {
			return s.count(`SELECT count(*) FROM mailbox_folders WHERE account_id = $1 AND name = 'Nowy' AND uidvalidity > 0`, accID) == 1
		})
		if n := s.count(`SELECT count(*) FROM labels WHERE account_id = $1 AND type = 'folder' AND name = 'Nowy'`, accID); n != 1 {
			t.Errorf("folder labels named Nowy: %d", n)
		}
		if r := serve(s.h, as(req{method: "POST", path: "/labels", body: map[string]any{"name": "Nowy", "type": "folder"}})); r.Code != http.StatusConflict {
			t.Errorf("duplicate folder: %d", r.Code)
		}
		if r := serve(s.h, as(req{method: "PATCH", path: "/labels/" + fl.ID, body: map[string]any{"name": "Inny"}})); r.Code != http.StatusBadRequest {
			t.Errorf("rename folder: %d %s", r.Code, r.Body)
		}
		// A non-empty folder cannot be deleted; an empty one can.
		var proj string
		var out struct{ Labels []api.Label }
		get(s.h, "/labels?accountId="+accStr).json(&out)
		for _, lb := range out.Labels {
			if lb.Name == "Projekty" {
				proj = lb.ID
			}
		}
		if r := serve(s.h, as(req{method: "DELETE", path: "/labels/" + proj})); r.Code != http.StatusConflict || r.code() != api.CodeFolderNotEmpty {
			t.Errorf("delete non-empty folder: %d %s", r.Code, r.Body)
		}
		if r := serve(s.h, as(req{method: "DELETE", path: "/labels/" + fl.ID})); r.Code != http.StatusNoContent {
			t.Fatalf("delete empty folder: %d %s", r.Code, r.Body)
		}
		waitFor(t, 20*time.Second, "folder removed on the server", func() bool { return !folderExists("Nowy") })
		waitFor(t, 30*time.Second, "folder label gone after discovery", func() bool {
			return s.count(`SELECT count(*) FROM mailbox_folders WHERE account_id = $1 AND name = 'Nowy'`, accID) == 0
		})
	})
}

// newUserSession makes another Silo user with a session (no mail account).
func newUserSession(t *testing.T, e *env, email string) login {
	t.Helper()
	ctx := context.Background()
	var uid int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	token := "token-" + email
	sum := sha256.Sum256([]byte(token))
	csrf := "csrf-" + email
	if _, err := e.pool.Exec(ctx, `INSERT INTO sessions (user_id, token_hash, csrf_token, expires_at) VALUES ($1, $2, $3, now() + interval '1 hour')`, uid, sum[:], csrf); err != nil {
		t.Fatal(err)
	}
	return login{cookie: &http.Cookie{Name: api.SessionCookie, Value: token}, csrf: csrf}
}
