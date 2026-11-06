package api_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/silopost/silo-server/internal/accounts"
	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/blob"
	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/drafts"
	"github.com/silopost/silo-server/internal/events"
	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/imapx"
	"github.com/silopost/silo-server/internal/mailview"
	"github.com/silopost/silo-server/internal/oauth"
	"github.com/silopost/silo-server/internal/ops"
	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/store"
	silosync "github.com/silopost/silo-server/internal/sync"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
	"github.com/silopost/silo-server/internal/testutil/oauthtest"
	"github.com/silopost/silo-server/internal/testutil/pgtest"
	"github.com/silopost/silo-server/internal/testutil/smtptest"
	"github.com/silopost/silo-server/internal/worker"
)

// oauthEmail is the mailbox the fake provider signs in.
const oauthEmail = "oauth@silo.test"

// oauthStack is the whole server with Google sign-in against the fake
// provider of oauthtest: Dovecot (profile oauth2) checks XOAUTH2 tokens by
// introspection at the fake, the SMTP server accepts the fake's tokens. The
// provider's IMAP server is reached through a proxy that the test can cut.
type oauthStack struct {
	*env
	h     http.Handler
	fake  *oauthtest.Server
	smtp  *smtptest.Server
	imap  string // Dovecot host:port
	proxy *tcpProxy
}

func newOAuthStack(t *testing.T) *oauthStack {
	t.Helper()
	ctx := context.Background()
	u := pgtest.New(t)
	if err := store.MigrateUp(ctx, u); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ring, err := secrets.NewKeyring(bytes.Repeat([]byte{7}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	fake := oauthtest.Start(t)
	// Access tokens live 61 s: within the refresh margin, so every
	// connection refreshes (the cache itself is tested in package oauth).
	fake.Set(func(s *oauthtest.Server) { s.Email, s.ExpiresIn = oauthEmail, 61 })
	addr := imaptest.StartOAuth2(t, fake.DockerURL()+"/introspect")
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	smtp := smtptest.StartServer(t, imaptest.User, imaptest.Pass, 0)
	smtp.AcceptXOAuth2(oauthEmail, fake.ValidAccessToken)
	px := newTCPProxy(t, addr)

	e := &env{t: t, pool: pool, ring: ring, cfg: config.Config{
		DefaultIMAPHost: host, DefaultIMAPPort: port, DefaultIMAPTLS: "none",
		DefaultSMTPHost: smtp.Host, DefaultSMTPPort: smtp.Port, DefaultSMTPTLS: "none",
		DefaultLoginFormat: config.LoginFormatLocalpart, DefaultDomain: "silo.test",
		AllowInsecureTransport: true, MaxAccountsPerUser: 5,
	}}
	providers := oauth.NewProviders(config.OAuthConfig{
		GoogleClientID: oauthtest.ClientID, GoogleClientSecret: oauthtest.ClientSecret,
		GoogleAuthURL: fake.URL + "/authorize", GoogleTokenURL: fake.URL + "/token", GoogleUserinfoURL: fake.URL + "/userinfo",
	})
	g := providers[config.OAuthGoogle]
	g.IMAP = oauth.Server{Host: "127.0.0.1", Port: px.port(), TLS: config.TLSNone}
	g.SMTP = oauth.Server{Host: smtp.Host, Port: smtp.Port, TLS: config.TLSNone}
	osvc := oauth.New(oauth.Options{Pool: pool, Keys: ring, Providers: providers, PublicURL: publicURL})

	blobs, err := blob.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	jobs := worker.New(worker.Options{Pool: pool, Logger: logger, Concurrency: 4, PollInterval: 100 * time.Millisecond})
	mgr := silosync.NewManager(silosync.Deps{
		Pool: pool, Keys: ring, Blobs: blobs, Logger: logger, OAuth: osvc,
		Settings: silosync.Settings{MaxConnections: 3, InitialConcurrency: 2, PollInterval: time.Minute, BackfillRate: 50 << 20,
			MaxMessageBytes: 50 << 20, InsecureTLS: true},
		Backoff: []time.Duration{100 * time.Millisecond, 200 * time.Millisecond},
	})
	mgr.RegisterJobs(jobs)
	view := mailview.New(pool, blobs, mgr)
	dr := drafts.New(drafts.Options{Pool: pool, Blobs: blobs, MaxUploadBytes: testMaxUpload, SyncDelay: 300 * time.Millisecond,
		Keys: ring, InsecureTransport: true, SendRetry: []time.Duration{time.Second}, Logger: logger, OAuth: osvc})
	dr.RegisterJobs(jobs, osvc.PurgeStates)
	hub := events.New(events.Options{Pool: pool, Logger: logger, PollInterval: time.Minute})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{}, 3)
	go func() { _ = jobs.Run(runCtx); done <- struct{}{} }()
	go func() { _ = mgr.Run(runCtx); done <- struct{}{} }()
	go func() { _ = hub.Run(runCtx); done <- struct{}{} }()
	t.Cleanup(func() {
		cancel()
		for range 3 {
			<-done
		}
	})
	r := chi.NewRouter()
	api.Mount(r, api.Options{
		Auth: auth.New(auth.Options{Pool: pool, Keys: ring, Config: e.cfg, MinLoginDuration: 20 * time.Millisecond,
			LoginLimit: auth.Limit{Burst: 1000, Every: time.Second}}),
		Accounts: accounts.New(accounts.Options{Pool: pool, Keys: ring, Config: e.cfg}),
		Sync:     silosync.NewService(pool), Ops: ops.NewService(pool, 30*time.Second),
		View: view, Drafts: dr, Events: hub, OAuth: osvc, PublicURL: publicURL,
	})
	return &oauthStack{env: e, h: r, fake: fake, smtp: smtp, imap: addr, proxy: px}
}

// signIn runs start (returnTo, accountId) -> provider -> callback and returns
// the Location of the callback's redirect.
func (s *oauthStack) signIn(l login, body map[string]any) *url.URL {
	s.t.Helper()
	r := serve(s.h, as(l, req{method: "POST", path: "/oauth/google/start", body: body}))
	if r.Code != http.StatusOK {
		s.t.Fatalf("start: %d %s", r.Code, r.Body)
	}
	var st struct{ URL, State string }
	r.json(&st)
	var bound *http.Cookie
	for _, c := range r.Result().Cookies() {
		if c.Name == "silo_oauth_state" {
			bound = c
		}
	}
	if bound == nil || bound.Value != st.State || !bound.HttpOnly || bound.Path != "/api/v1/oauth/" {
		s.t.Fatalf("state cookie = %+v", bound)
	}
	return s.callback(s.fake.Authorize(s.t, st.URL), bound)
}

func (s *oauthStack) callback(cb *url.URL, bound *http.Cookie) *url.URL {
	s.t.Helper()
	if !strings.HasPrefix(cb.String(), publicURL+"/api/v1/oauth/google/callback?") {
		s.t.Fatalf("redirect_uri = %s", cb)
	}
	r := serve(s.h, req{method: "GET", path: strings.TrimPrefix(cb.Path, "/api/v1") + "?" + cb.RawQuery, cookie: bound})
	if r.Code != http.StatusSeeOther {
		s.t.Fatalf("callback: %d %s", r.Code, r.Body)
	}
	loc, err := url.Parse(r.Header().Get("Location"))
	if err != nil {
		s.t.Fatal(err)
	}
	return loc
}

func (s *oauthStack) account(l login, id string) api.Account {
	s.t.Helper()
	var list struct{ Accounts []api.Account }
	serve(s.h, as(l, req{method: "GET", path: "/accounts"})).json(&list)
	for _, a := range list.Accounts {
		if a.ID == id {
			return a
		}
	}
	s.t.Fatalf("account %s not listed", id)
	return api.Account{}
}

// TestOAuthAccount is the end-to-end flow of an OAuth account: sign in with
// the (fake) provider, initial sync over XOAUTH2, INBOX, sending with AUTH
// XOAUTH2, a refused refresh token (auth_failed) and the re-authorization.
func TestOAuthAccount(t *testing.T) {
	s := newOAuthStack(t)
	l := s.login(s.h, imaptest.User, imaptest.Pass)

	var meta api.Meta
	serve(s.h, req{method: "GET", path: "/meta"}).json(&meta)
	if len(meta.OAuthProviders) != 1 || meta.OAuthProviders[0] != "google" {
		t.Fatalf("meta oauthProviders = %v", meta.OAuthProviders)
	}

	t.Run("start errors", func(t *testing.T) {
		if r := serve(s.h, as(l, req{method: "POST", path: "/oauth/microsoft/start", body: map[string]any{}})); r.Code != http.StatusNotFound || r.code() != api.CodeNotFound {
			t.Errorf("unconfigured provider: %d %s", r.Code, r.Body)
		}
		if r := serve(s.h, req{method: "POST", path: "/oauth/google/start", body: map[string]any{}}); r.Code != http.StatusUnauthorized {
			t.Errorf("no session: %d", r.Code)
		}
		for _, bad := range []string{"//evil.example/x", "https://evil.example", "/\\evil"} {
			if r := serve(s.h, as(l, req{method: "POST", path: "/oauth/google/start", body: map[string]any{"returnTo": bad}})); r.Code != http.StatusBadRequest || r.code() != api.CodeValidation {
				t.Errorf("returnTo %q: %d %s", bad, r.Code, r.Body)
			}
		}
		var accs struct{ Accounts []api.Account }
		serve(s.h, as(l, req{method: "GET", path: "/accounts"})).json(&accs)
		if r := serve(s.h, as(l, req{method: "POST", path: "/oauth/google/start", body: map[string]any{"accountId": accs.Accounts[0].ID}})); r.Code != http.StatusNotFound {
			t.Errorf("re-authorizing a password account: %d", r.Code)
		}
		// No body at all is a new account with returnTo /.
		if r := serve(s.h, as(l, req{method: "POST", path: "/oauth/google/start"})); r.Code != http.StatusOK {
			t.Errorf("start without body: %d %s", r.Code, r.Body)
		}
	})

	var accID string
	t.Run("sign in", func(t *testing.T) {
		loc := s.signIn(l, map[string]any{"returnTo": "/settings/accounts?tab=1"})
		q := loc.Query()
		if loc.Scheme+"://"+loc.Host != publicURL || loc.Path != "/settings/accounts" || q.Get("tab") != "1" || q.Get("oauth") != "ok" || q.Get("accountId") == "" {
			t.Fatalf("redirect = %s", loc)
		}
		accID = q.Get("accountId")
		a := s.account(l, accID)
		if a.AuthType != "xoauth2" || a.Provider != "gmail" || a.Email != oauthEmail || a.Username != oauthEmail ||
			a.DisplayName != "OAuth User" || a.SentAppend || a.IMAPPort != s.proxy.port() {
			t.Fatalf("account = %+v", a)
		}
		// The refresh token is stored encrypted with the account id as AAD.
		var sealed []byte
		if err := s.pool.QueryRow(context.Background(), `SELECT secret_enc FROM accounts WHERE id = $1`, idOf("acc_", accID)).Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ring.Decrypt(sealed, []byte(strconv.FormatInt(idOf("acc_", accID), 10))); err != nil || bytes.Contains(sealed, []byte("ya29")) {
			t.Fatalf("secret_enc: %v", err)
		}
		waitFor(t, 60*time.Second, "OAuth account live", func() bool {
			return s.account(l, accID).State == "live" && s.count(`SELECT count(*) FROM accounts WHERE id = $1 AND initial_sync_done_at IS NOT NULL`, idOf("acc_", accID)) == 1
		})
		if n := s.count(`SELECT count(*) FROM oauth_states`); n != 1 { // the one of "start without body"
			t.Errorf("oauth_states left: %d", n)
		}
	})
	if accID == "" {
		t.FailNow()
	}

	t.Run("INBOX over XOAUTH2", func(t *testing.T) {
		host, portStr, _ := net.SplitHostPort(s.imap)
		port, _ := strconv.Atoi(portStr)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cl, err := imapx.Dial(ctx, host, port, imapx.TLSNone, false)
		if err != nil {
			t.Fatal(err)
		}
		defer cl.Close()
		if err := cl.Authenticate(ctx, oauth.NewXOAuth2Client(oauthEmail, s.fake.IssueAccessToken(oauthEmail))); err != nil {
			t.Fatal(err)
		}
		appendMsg(t, cl.IMAP(), "INBOX", "oauth-1@x.test", "Ann <ann@x.test>", oauthEmail, "Hello OAuth")
		waitFor(t, 60*time.Second, "message in INBOX of the OAuth account", func() bool {
			var list api.ThreadList
			serve(s.h, as(l, req{method: "GET", path: "/threads?labelId=INBOX&accountId=" + accID})).json(&list)
			return len(list.Threads) == 1 && list.Threads[0].Subject == "Hello OAuth"
		})
	})

	t.Run("send with XOAUTH2", func(t *testing.T) {
		var d api.Draft
		r := serve(s.h, as(l, req{method: "POST", path: "/drafts", body: map[string]any{"accountId": accID}}))
		r.json(&d)
		if r.Code != http.StatusCreated || d.AccountID != accID {
			t.Fatalf("create draft: %d %s", r.Code, r.Body)
		}
		r = serve(s.h, as(l, req{method: "PUT", path: "/drafts/" + d.ID, body: map[string]any{"version": d.Version,
			"to": []map[string]string{{"email": "bob@x.test"}}, "subject": "Via XOAUTH2", "bodyHtml": "<p>Hi</p>"}}))
		if r.Code != http.StatusOK {
			t.Fatalf("save: %d %s", r.Code, r.Body)
		}
		if r := serve(s.h, as(l, req{method: "POST", path: "/drafts/" + d.ID + "/send", body: map[string]any{}})); r.Code != http.StatusOK {
			t.Fatalf("send: %d %s", r.Code, r.Body)
		}
		waitFor(t, 30*time.Second, "delivery with AUTH XOAUTH2", func() bool { return len(s.smtp.Messages()) == 1 })
		if m := s.smtp.Messages()[0]; m.From != oauthEmail || !strings.Contains(string(m.Data), "Subject: Via XOAUTH2") {
			t.Errorf("delivered from %q:\n%s", m.From, m.Data)
		}
		// sent_append is off (Gmail stores sent mail itself): no append job.
		if n := s.count(`SELECT count(*) FROM jobs WHERE account_id = $1 AND payload->>'op' = 'append'`, idOf("acc_", accID)); n != 0 {
			t.Errorf("append jobs: %d", n)
		}
	})

	t.Run("callback errors", func(t *testing.T) {
		// Replay, missing or foreign state cookie, denied consent, wrong address.
		r := serve(s.h, as(l, req{method: "POST", path: "/oauth/google/start", body: map[string]any{"returnTo": "/x"}}))
		var st struct{ URL, State string }
		r.json(&st)
		cb := s.fake.Authorize(t, st.URL)
		if loc := s.callback(cb, nil); loc.Query().Get("code") != "oauth_state_invalid" || loc.Path != "/" {
			t.Errorf("no state cookie: %s", loc)
		}
		bound := &http.Cookie{Name: "silo_oauth_state", Value: st.State}
		if loc := s.callback(cb, bound); loc.Query().Get("oauth") != "ok" || loc.Path != "/x" {
			t.Errorf("same mailbox again: %s", loc) // the existing account is updated
		}
		if loc := s.callback(cb, bound); loc.Query().Get("code") != "oauth_state_invalid" {
			t.Errorf("replayed state: %s", loc)
		}
		if n := s.count(`SELECT count(*) FROM accounts WHERE username = $1`, oauthEmail); n != 1 {
			t.Errorf("accounts of the mailbox: %d", n)
		}

		s.fake.Set(func(f *oauthtest.Server) { f.Deny = true })
		if loc := s.signIn(l, map[string]any{"returnTo": "/y"}); loc.Query().Get("oauth") != "error" || loc.Query().Get("code") != "oauth_denied" || loc.Path != "/y" {
			t.Errorf("denied: %s", loc)
		}
		s.fake.Set(func(f *oauthtest.Server) { f.Deny = false; f.Email = "someone-else@silo.test" })
		if loc := s.signIn(l, map[string]any{"accountId": accID}); loc.Query().Get("code") != "oauth_account_mismatch" {
			t.Errorf("other address on re-authorization: %s", loc)
		}
		// A new address the servers do not accept: no account is created.
		if loc := s.signIn(l, map[string]any{}); loc.Query().Get("code") != "account_test_failed" {
			t.Errorf("mailbox the servers refuse: %s", loc)
		}
		if n := s.count(`SELECT count(*) FROM accounts WHERE username = 'someone-else@silo.test'`); n != 0 {
			t.Errorf("account created despite the failed test")
		}
		s.fake.Set(func(f *oauthtest.Server) { f.Email = oauthEmail })

		// A password cannot be set on an OAuth account.
		if r := serve(s.h, as(l, req{method: "PATCH", path: "/accounts/" + accID, body: map[string]any{"password": "x"}})); r.Code != http.StatusConflict || r.code() != api.CodeNotAvailable {
			t.Errorf("PATCH password: %d %s", r.Code, r.Body)
		}
	})

	t.Run("expired authorization and re-authorization", func(t *testing.T) {
		// The provider refuses the refresh token; the next connection after
		// an outage ends in auth_failed.
		s.fake.Set(func(f *oauthtest.Server) { f.FailRefresh = true })
		s.proxy.setUp(false)
		waitFor(t, 30*time.Second, "auth_failed", func() bool {
			s.proxy.setUp(true) // let the next attempt through
			return s.account(l, accID).State == "auth_failed"
		})
		a := s.account(l, accID)
		if a.Error == nil || *a.Error != oauth.ReauthMessage {
			t.Errorf("sync error = %v", a.Error)
		}
		if n := s.count(`SELECT count(*) FROM events WHERE type = 'account.error' AND payload->>'code' = 'auth_failed' AND payload->>'detail' = $1`, oauth.ReauthMessage); n == 0 {
			t.Error("no account.error auth_failed")
		}

		s.fake.Set(func(f *oauthtest.Server) { f.FailRefresh = false })
		loc := s.signIn(l, map[string]any{"accountId": accID})
		if loc.Query().Get("oauth") != "ok" || loc.Query().Get("accountId") != accID {
			t.Fatalf("re-authorization: %s", loc)
		}
		waitFor(t, 60*time.Second, "live again", func() bool { return s.account(l, accID).State == "live" })
	})
}

// tcpProxy forwards connections to target; setUp(false) drops them all and
// refuses new ones.
type tcpProxy struct {
	target string
	ln     net.Listener
	mu     sync.Mutex
	up     bool
	conns  []net.Conn
}

func newTCPProxy(t *testing.T, target string) *tcpProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &tcpProxy{target: target, ln: ln, up: true}
	t.Cleanup(func() { _ = ln.Close(); p.setUp(false) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			up := p.up
			p.mu.Unlock()
			if !up {
				_ = c.Close()
				continue
			}
			u, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, u)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(u, c); _ = u.Close() }()
			go func() { _, _ = io.Copy(c, u); _ = c.Close() }()
		}
	}()
	return p
}

func (p *tcpProxy) port() int { return p.ln.Addr().(*net.TCPAddr).Port }

func (p *tcpProxy) setUp(up bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.up = up
	if !up {
		for _, c := range p.conns {
			_ = c.Close()
		}
		p.conns = nil
	}
}
