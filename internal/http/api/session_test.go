package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/accounts"
	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/ops"
	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/store"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
	silosync "github.com/silopost/silo-server/internal/sync"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
	"github.com/silopost/silo-server/internal/testutil/pgtest"
	"github.com/silopost/silo-server/internal/testutil/smtptest"
)

const publicURL = "http://example.com"

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	ring *secrets.Keyring
	cfg  config.Config
	web  http.Handler // normal service: Dovecot reachable
}

type resp struct {
	*httptest.ResponseRecorder
}

func (r resp) json(v any) {
	r.ResponseRecorder.Result() // flush headers
	if err := json.Unmarshal(r.Body.Bytes(), v); err != nil {
		panic("decode " + r.Body.String() + ": " + err.Error())
	}
}

func (r resp) code() string {
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &p)
	return p.Code
}

func (r resp) cookie() *http.Cookie {
	for _, c := range r.Result().Cookies() {
		if c.Name == api.SessionCookie {
			return c
		}
	}
	return nil
}

func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	url := pgtest.New(t)
	if err := store.MigrateUp(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	addr, _, _ := imaptest.Start(t, imaptest.Full)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	smtpHost, smtpPort := smtptest.Start(t, imaptest.User, imaptest.Pass)

	key := bytes.Repeat([]byte{7}, 32)
	ring, err := secrets.NewKeyring(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, pool: pool, ring: ring, cfg: config.Config{
		DefaultIMAPHost: host, DefaultIMAPPort: port, DefaultIMAPTLS: "none",
		DefaultSMTPHost: smtpHost, DefaultSMTPPort: smtpPort, DefaultSMTPTLS: "none",
		DefaultLoginFormat: config.LoginFormatLocalpart, DefaultDomain: "silo.test",
		AllowInsecureTransport: true, MaxAccountsPerUser: 5,
	}}
	e.web = e.router(e.cfg, auth.Options{MinLoginDuration: 20 * time.Millisecond, LoginLimit: auth.Limit{Burst: 1000, Every: time.Second}})
	return e
}

// router builds the API over cfg with the given auth options.
func (e *env) router(cfg config.Config, ao auth.Options) http.Handler {
	ao.Pool, ao.Keys, ao.Config = e.pool, e.ring, cfg
	r := chi.NewRouter()
	api.Mount(r, api.Options{
		Auth:      auth.New(ao),
		Accounts:  accounts.New(accounts.Options{Pool: e.pool, Keys: e.ring, Config: cfg}),
		Sync:      silosync.NewService(e.pool),
		Ops:       ops.NewService(e.pool, 10*time.Second),
		PublicURL: publicURL,
	})
	return r
}

type req struct {
	method, path string
	body         any
	cookie       *http.Cookie
	csrf         string
	headers      map[string]string
}

func serve(h http.Handler, q req) resp {
	var rd *bytes.Reader
	if q.body != nil {
		b, _ := json.Marshal(q.body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(q.method, "/api/v1"+q.path, rd)
	if q.body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if q.cookie != nil {
		r.AddCookie(q.cookie)
	}
	if q.csrf != "" {
		r.Header.Set(api.CSRFHeader, q.csrf)
	}
	for k, v := range q.headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return resp{rec}
}

type login struct {
	cookie *http.Cookie
	csrf   string
	body   api.SessionBody
}

func (e *env) login(h http.Handler, user, pass string) login {
	e.t.Helper()
	r := serve(h, req{method: "POST", path: "/session", body: map[string]string{"email": user, "password": pass}})
	if r.Code != http.StatusOK {
		e.t.Fatalf("login %s: status %d: %s", user, r.Code, r.Body)
	}
	var l login
	r.json(&l.body)
	l.cookie, l.csrf = r.cookie(), l.body.CSRFToken
	return l
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestSessionAndAccounts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.web
	user, pass := imaptest.User, imaptest.Pass

	t.Run("validation", func(t *testing.T) {
		r := serve(h, req{method: "POST", path: "/session", body: map[string]string{}})
		if r.Code != http.StatusBadRequest || r.code() != api.CodeValidation {
			t.Fatalf("status %d code %q: %s", r.Code, r.code(), r.Body)
		}
		if ct := r.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
			t.Errorf("content-type = %q", ct)
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		r := serve(h, req{method: "POST", path: "/session", body: map[string]string{"email": user, "password": "nope"}})
		if r.Code != http.StatusUnauthorized || r.code() != api.CodeInvalidCredentials {
			t.Fatalf("status %d code %q", r.Code, r.code())
		}
		if r.cookie() != nil {
			t.Error("cookie set on failed login")
		}
		if n := e.count("SELECT count(*) FROM users"); n != 0 {
			t.Errorf("users = %d, want 0", n)
		}
	})

	var first login
	t.Run("first login creates user and account", func(t *testing.T) {
		first = e.login(h, user, pass)
		c := first.cookie
		if c == nil || !c.HttpOnly || c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || len(c.Value) < 40 {
			t.Fatalf("cookie = %+v (Secure must be off for an http PublicURL)", c)
		}
		if first.body.User.Email != "silo@silo.test" || len(first.body.Accounts) != 1 || first.body.CSRFToken == "" {
			t.Fatalf("body = %+v", first.body)
		}
		if first.body.Settings.UndoSendSeconds != 5 || first.body.Settings.ActiveAccountID == nil {
			t.Errorf("settings = %+v", first.body.Settings)
		}
		a, err := sqlcgen.New(e.pool).ListAccountsByUser(ctx, 1)
		if err != nil || len(a) != 1 {
			t.Fatalf("accounts = %v, %v", a, err)
		}
		plain, err := e.ring.Decrypt(a[0].SecretEnc, []byte(strconv.FormatInt(a[0].ID, 10)))
		if err != nil || string(plain) != pass {
			t.Fatalf("stored secret does not decrypt to the password: %v", err)
		}
		if a[0].SyncState != "connecting" || a[0].Username != user || a[0].Email != "silo@silo.test" {
			t.Errorf("account = %+v", a[0])
		}
		if bytes.Contains(a[0].SecretEnc, []byte(pass)) {
			t.Error("password stored in the clear")
		}
		u, _ := sqlcgen.New(e.pool).GetUserByID(ctx, 1)
		if !strings.HasPrefix(u.PasswordHash.String, "$argon2id$") {
			t.Errorf("password_hash = %q", u.PasswordHash.String)
		}
		if n := e.count("SELECT count(*) FROM jobs WHERE kind = 'account.initial_sync' AND account_id = $1", a[0].ID); n != 1 {
			t.Errorf("initial sync jobs = %d, want 1", n)
		}
		// Only the digest of the token is stored.
		if n := e.count("SELECT count(*) FROM sessions WHERE token_hash = $1", tokenHash(c.Value)); n != 1 {
			t.Errorf("sessions stored under sha256(token) = %d, want 1", n)
		}
	})

	t.Run("second login does not duplicate", func(t *testing.T) {
		l := e.login(h, "silo@silo.test", pass)
		if l.cookie.Value == first.cookie.Value || l.csrf == first.csrf {
			t.Error("tokens reused across sessions")
		}
		if n := e.count("SELECT count(*) FROM users"); n != 1 {
			t.Errorf("users = %d", n)
		}
		if n := e.count("SELECT count(*) FROM accounts"); n != 1 {
			t.Errorf("accounts = %d", n)
		}
		if n := e.count("SELECT count(*) FROM jobs WHERE kind = 'account.initial_sync'"); n != 1 {
			t.Errorf("initial sync jobs = %d, want 1", n)
		}
	})

	t.Run("get session", func(t *testing.T) {
		r := serve(h, req{method: "GET", path: "/session"})
		if r.Code != http.StatusUnauthorized || r.code() != api.CodeUnauthenticated {
			t.Fatalf("no cookie: status %d code %q", r.Code, r.code())
		}
		r = serve(h, req{method: "GET", path: "/session", cookie: &http.Cookie{Name: api.SessionCookie, Value: "forged"}})
		if r.Code != http.StatusUnauthorized {
			t.Fatalf("forged cookie: status %d", r.Code)
		}
		r = serve(h, req{method: "GET", path: "/session", cookie: first.cookie})
		var b api.SessionBody
		r.json(&b)
		if r.Code != http.StatusOK || b.CSRFToken != first.csrf || b.User.Email != "silo@silo.test" {
			t.Fatalf("status %d body %s", r.Code, r.Body)
		}
	})

	t.Run("csrf and origin", func(t *testing.T) {
		body := map[string]string{"email": "silo@silo.test", "username": user, "password": pass}
		try := func(csrf string, hdr map[string]string) resp {
			return serve(h, req{method: "POST", path: "/accounts/test", body: body, cookie: first.cookie, csrf: csrf, headers: hdr})
		}
		for name, r := range map[string]resp{
			"missing token":        try("", nil),
			"wrong token":          try("not-the-token", nil),
			"foreign origin":       try(first.csrf, map[string]string{"Origin": "https://evil.example"}),
			"cross-site fetch":     try(first.csrf, map[string]string{"Sec-Fetch-Site": "cross-site"}),
			"same-site fetch":      try(first.csrf, map[string]string{"Sec-Fetch-Site": "same-site"}),
			"origin of other port": try(first.csrf, map[string]string{"Origin": "http://example.com:81"}),
		} {
			if r.Code != http.StatusForbidden || r.code() != api.CodeCSRF {
				t.Errorf("%s: status %d code %q", name, r.Code, r.code())
			}
		}
		// Safe methods need no token.
		if r := serve(h, req{method: "GET", path: "/accounts", cookie: first.cookie}); r.Code != http.StatusOK {
			t.Errorf("GET without csrf: %d", r.Code)
		}
		for name, hdr := range map[string]map[string]string{
			"no origin":     nil,
			"public origin": {"Origin": publicURL},
			"same-origin":   {"Sec-Fetch-Site": "same-origin", "Origin": publicURL},
			"request host":  {"Origin": "http://example.com"},
		} {
			r := try(first.csrf, hdr)
			if r.Code != http.StatusOK {
				t.Fatalf("%s: status %d: %s", name, r.Code, r.Body)
			}
		}
		// A foreign Origin cannot sign in either.
		r := serve(h, req{method: "POST", path: "/session", headers: map[string]string{"Origin": "https://evil.example"},
			body: map[string]string{"email": user, "password": pass}})
		if r.Code != http.StatusForbidden || r.code() != api.CodeCSRF {
			t.Errorf("login from foreign origin: %d %q", r.Code, r.code())
		}
	})

	t.Run("account test steps", func(t *testing.T) {
		post := func(pw string) accounts.TestResult {
			r := serve(h, req{method: "POST", path: "/accounts/test", cookie: first.cookie, csrf: first.csrf,
				body: map[string]string{"email": "silo@silo.test", "username": user, "password": pw}})
			if r.Code != http.StatusOK {
				t.Fatalf("status %d: %s", r.Code, r.Body)
			}
			if strings.Contains(r.Body.String(), pw) && pw != "" {
				t.Error("password echoed in the response")
			}
			var res accounts.TestResult
			r.json(&res)
			return res
		}
		ok := post(pass)
		if !ok.IMAP.OK || !ok.SMTP.OK || len(ok.IMAP.Steps) != 5 || len(ok.SMTP.Steps) != 3 {
			t.Fatalf("good credentials: %+v", ok)
		}
		bad := post("wrong")
		if bad.IMAP.OK || bad.SMTP.OK || bad.IMAP.Steps[len(bad.IMAP.Steps)-1].Step != "login" {
			t.Fatalf("bad credentials: %+v", bad)
		}
	})

	t.Run("sessions list and revoke", func(t *testing.T) {
		other := e.login(h, user, pass)
		r := serve(h, req{method: "GET", path: "/sessions", cookie: first.cookie})
		var list struct{ Sessions []api.SessionInfo }
		r.json(&list)
		if r.Code != http.StatusOK || len(list.Sessions) < 2 {
			t.Fatalf("status %d: %s", r.Code, r.Body)
		}
		cur, otherID := 0, ""
		for _, s := range list.Sessions {
			if s.Current {
				cur++
			}
		}
		if cur != 1 {
			t.Errorf("current sessions = %d", cur)
		}
		r = serve(h, req{method: "GET", path: "/sessions", cookie: other.cookie})
		r.json(&list)
		for _, s := range list.Sessions {
			if s.Current {
				otherID = s.ID
			}
		}
		if r := serve(h, req{method: "DELETE", path: "/sessions/" + otherID, cookie: first.cookie}); r.Code != http.StatusForbidden {
			t.Errorf("revoke without csrf: %d", r.Code)
		}
		if r := serve(h, req{method: "DELETE", path: "/sessions/" + otherID, cookie: first.cookie, csrf: first.csrf}); r.Code != http.StatusNoContent {
			t.Fatalf("revoke: %d %s", r.Code, r.Body)
		}
		if r := serve(h, req{method: "GET", path: "/session", cookie: other.cookie}); r.Code != http.StatusUnauthorized {
			t.Errorf("revoked session still valid: %d", r.Code)
		}
		for _, id := range []string{otherID, "s_zzzzzz", "garbage"} {
			if r := serve(h, req{method: "DELETE", path: "/sessions/" + id, cookie: first.cookie, csrf: first.csrf}); r.Code != http.StatusNotFound {
				t.Errorf("revoke %q: %d, want 404", id, r.Code)
			}
		}
	})

	t.Run("expiry", func(t *testing.T) {
		for name, sql := range map[string]string{
			"idle":     "UPDATE sessions SET expires_at = now() - interval '1 minute' WHERE token_hash = $1",
			"absolute": "UPDATE sessions SET created_at = now() - interval '91 days' WHERE token_hash = $1",
		} {
			l := e.login(h, user, pass)
			if r := serve(h, req{method: "GET", path: "/session", cookie: l.cookie}); r.Code != http.StatusOK {
				t.Fatalf("%s: fresh session rejected: %d", name, r.Code)
			}
			if _, err := e.pool.Exec(ctx, sql, tokenHash(l.cookie.Value)); err != nil {
				t.Fatal(err)
			}
			if r := serve(h, req{method: "GET", path: "/session", cookie: l.cookie}); r.Code != http.StatusUnauthorized {
				t.Errorf("%s: expired session accepted: %d", name, r.Code)
			}
		}
	})

	t.Run("tenant isolation", func(t *testing.T) {
		q := sqlcgen.New(e.pool)
		u2, err := q.InsertUser(ctx, sqlcgen.InsertUserParams{Email: "other@silo.test"})
		if err != nil {
			t.Fatal(err)
		}
		a2, err := q.InsertAccount(ctx, sqlcgen.InsertAccountParams{
			UserID: u2.ID, Email: "other@silo.test", Provider: "generic", ImapHost: "h", ImapPort: 993, ImapTls: "implicit",
			SmtpHost: "h", SmtpPort: 587, SmtpTls: "starttls", Username: "other", AuthType: "password", SecretEnc: []byte("x"),
		})
		if err != nil {
			t.Fatal(err)
		}
		id := "acc_" + strconv.FormatInt(a2.ID, 36)
		for _, c := range []req{
			{method: "GET", path: "/accounts/" + id + "/sync-status"},
			{method: "PATCH", path: "/accounts/" + id, body: map[string]any{"displayName": "mine now"}},
			{method: "DELETE", path: "/accounts/" + id},
			{method: "DELETE", path: "/accounts/acc_nonexistent"},
			{method: "GET", path: "/accounts/garbage/sync-status"},
		} {
			c.cookie, c.csrf = first.cookie, first.csrf
			if r := serve(h, c); r.Code != http.StatusNotFound || r.code() != api.CodeNotFound {
				t.Errorf("%s %s: status %d code %q, want 404", c.method, c.path, r.Code, r.code())
			}
		}
		if n := e.count("SELECT count(*) FROM accounts WHERE id = $1 AND display_name = ''", a2.ID); n != 1 {
			t.Error("foreign account was modified or deleted")
		}
		var list struct{ Accounts []api.Account }
		serve(h, req{method: "GET", path: "/accounts", cookie: first.cookie}).json(&list)
		if len(list.Accounts) != 1 {
			t.Errorf("accounts listed = %d, want only own", len(list.Accounts))
		}
	})

	t.Run("update account", func(t *testing.T) {
		id := first.body.Accounts[0].ID
		r := serve(h, req{method: "PATCH", path: "/accounts/" + id, cookie: first.cookie, csrf: first.csrf,
			body: map[string]any{"displayName": "Work", "sentAppend": true, "imapPort": 143}})
		var a api.Account
		r.json(&a)
		if r.Code != http.StatusOK || a.DisplayName != "Work" || !a.SentAppend || a.IMAPPort != 143 {
			t.Fatalf("status %d: %s", r.Code, r.Body)
		}
		// Back to the real port for the following steps.
		serve(h, req{method: "PATCH", path: "/accounts/" + id, cookie: first.cookie, csrf: first.csrf,
			body: map[string]any{"imapPort": e.cfg.DefaultIMAPPort}})
		r = serve(h, req{method: "PATCH", path: "/accounts/" + id, cookie: first.cookie, csrf: first.csrf,
			body: map[string]any{"imapTls": "none-such"}})
		if r.Code != http.StatusBadRequest {
			t.Errorf("bad tls value: %d", r.Code)
		}
		// A new password is stored encrypted and clears auth_failed.
		accID, _ := strconv.ParseInt(strings.TrimPrefix(id, "acc_"), 36, 64)
		if _, err := e.pool.Exec(ctx, "UPDATE accounts SET sync_state = 'auth_failed' WHERE id = $1", accID); err != nil {
			t.Fatal(err)
		}
		r = serve(h, req{method: "PATCH", path: "/accounts/" + id, cookie: first.cookie, csrf: first.csrf,
			body: map[string]any{"password": "new-secret"}})
		r.json(&a)
		if r.Code != http.StatusOK || a.State != "connecting" {
			t.Fatalf("status %d state %q", r.Code, a.State)
		}
		acc, _ := sqlcgen.New(e.pool).GetAccount(ctx, sqlcgen.GetAccountParams{ID: accID, UserID: 1})
		if p, err := e.ring.Decrypt(acc.SecretEnc, []byte(strconv.FormatInt(accID, 10))); err != nil || string(p) != "new-secret" {
			t.Errorf("secret after PATCH: %q, %v", p, err)
		}
		// Restore the working password (an online login does it).
		e.login(h, user, pass)
	})

	t.Run("offline verifier", func(t *testing.T) {
		offCfg := e.cfg
		offCfg.DefaultIMAPPort = closedPort(t)
		off := e.router(offCfg, auth.Options{MinLoginDuration: 20 * time.Millisecond, IMAPTimeout: 3 * time.Second})

		// Known user, right password: let in, account backs off.
		r := serve(off, req{method: "POST", path: "/session", body: map[string]string{"email": user, "password": pass}})
		if r.Code != http.StatusOK || r.cookie() == nil {
			t.Fatalf("offline login: %d %s", r.Code, r.Body)
		}
		var b api.SessionBody
		r.json(&b)
		if len(b.Accounts) != 1 || b.Accounts[0].State != "backoff" {
			t.Fatalf("session accounts = %+v", b.Accounts)
		}
		var list struct{ Accounts []api.Account }
		serve(h, req{method: "GET", path: "/accounts", cookie: r.cookie()}).json(&list)
		if len(list.Accounts) != 1 || list.Accounts[0].State != "backoff" || list.Accounts[0].Error == nil {
			t.Fatalf("GET /accounts = %+v", list.Accounts)
		}
		var sess api.SessionBody
		serve(h, req{method: "GET", path: "/session", cookie: r.cookie()}).json(&sess)
		if sess.Accounts[0].State != "backoff" {
			t.Errorf("GET /session state = %q", sess.Accounts[0].State)
		}

		// Wrong password while the server is down: no way in.
		r = serve(off, req{method: "POST", path: "/session", body: map[string]string{"email": user, "password": "wrong"}})
		if r.Code != http.StatusUnauthorized || r.code() != api.CodeInvalidCredentials {
			t.Errorf("wrong password offline: %d %q", r.Code, r.code())
		}
		// Unknown user while the server is down: cannot be verified.
		r = serve(off, req{method: "POST", path: "/session", body: map[string]string{"email": "ghost", "password": pass}})
		if r.Code != http.StatusBadGateway || r.code() != api.CodeUpstreamIMAP {
			t.Errorf("unknown user offline: %d %q", r.Code, r.code())
		}
		if n := e.count("SELECT count(*) FROM users WHERE email = 'ghost@silo.test'"); n != 0 {
			t.Error("user created without a successful IMAP login")
		}

		// Server back: the next online login recovers the account.
		e.login(h, user, pass)
		serve(h, req{method: "GET", path: "/accounts", cookie: r.cookie()}) // old cookie is irrelevant
		fresh := e.login(h, user, pass)
		if fresh.body.Accounts[0].State != "connecting" {
			t.Errorf("state after recovery = %q", fresh.body.Accounts[0].State)
		}
		if n := e.count("SELECT count(*) FROM jobs WHERE kind = 'account.reconnect'"); n != 2 { // one from the earlier password PATCH, one from this recovery
			t.Errorf("reconnect jobs = %d, want 2", n)
		}
	})

	t.Run("rate limit", func(t *testing.T) {
		offCfg := e.cfg
		offCfg.DefaultIMAPPort = closedPort(t)
		try := func(h http.Handler, login string) resp {
			return serve(h, req{method: "POST", path: "/session", body: map[string]string{"email": login, "password": "x"}})
		}

		perIP := e.router(offCfg, auth.Options{MinLoginDuration: time.Millisecond, IMAPTimeout: time.Second,
			IPLimit: auth.Limit{Burst: 2, Every: time.Hour}})
		for i := 0; i < 2; i++ {
			if r := try(perIP, "a"+strconv.Itoa(i)); r.Code == http.StatusTooManyRequests {
				t.Fatalf("attempt %d throttled within burst", i+1)
			}
		}
		r := try(perIP, "a9")
		ra, err := strconv.Atoi(r.Header().Get("Retry-After"))
		if r.Code != http.StatusTooManyRequests || r.code() != api.CodeRateLimited || err != nil || ra < 1 {
			t.Fatalf("per-IP: status %d code %q Retry-After %q", r.Code, r.code(), r.Header().Get("Retry-After"))
		}

		perLogin := e.router(offCfg, auth.Options{MinLoginDuration: time.Millisecond, IMAPTimeout: time.Second,
			LoginLimit: auth.Limit{Burst: 2, Every: time.Hour}})
		try(perLogin, "victim")
		try(perLogin, "VICTIM") // case-insensitive key
		if r := try(perLogin, "victim"); r.Code != http.StatusTooManyRequests || r.Header().Get("Retry-After") == "" {
			t.Fatalf("per-login: status %d", r.Code)
		}
		if r := try(perLogin, "someone-else"); r.Code == http.StatusTooManyRequests {
			t.Error("other logins must not be throttled")
		}
	})

	t.Run("constant time", func(t *testing.T) {
		const min = 400 * time.Millisecond
		slow := e.router(e.cfg, auth.Options{MinLoginDuration: min})
		for name, body := range map[string]map[string]string{
			"wrong password":  {"email": user, "password": "wrong"},
			"malformed login": {"email": "a b", "password": "x"},
			"foreign domain":  {"email": "x@other.example", "password": "x"},
		} {
			start := time.Now()
			r := serve(slow, req{method: "POST", path: "/session", body: body})
			if r.Code != http.StatusUnauthorized {
				t.Fatalf("%s: status %d", name, r.Code)
			}
			if d := time.Since(start); d < min {
				t.Errorf("%s answered in %v, want >= %v", name, d, min)
			}
		}
	})

	t.Run("logout", func(t *testing.T) {
		l := e.login(h, user, pass)
		if r := serve(h, req{method: "DELETE", path: "/session", cookie: l.cookie}); r.Code != http.StatusForbidden {
			t.Fatalf("logout without csrf: %d", r.Code)
		}
		r := serve(h, req{method: "DELETE", path: "/session", cookie: l.cookie, csrf: l.csrf})
		if r.Code != http.StatusNoContent {
			t.Fatalf("logout: %d %s", r.Code, r.Body)
		}
		if c := r.cookie(); c == nil || c.MaxAge >= 0 || c.Value != "" {
			t.Errorf("cookie not cleared: %+v", c)
		}
		if r := serve(h, req{method: "GET", path: "/session", cookie: l.cookie}); r.Code != http.StatusUnauthorized {
			t.Errorf("session valid after logout: %d", r.Code)
		}
	})

	t.Run("account lifecycle", func(t *testing.T) {
		l := e.login(h, user, pass)
		as := func(c req) req { c.cookie, c.csrf = l.cookie, l.csrf; return c }
		accID := l.body.Accounts[0].ID

		// Custom hosts are refused while SILO_ALLOW_CUSTOM_SERVERS is off.
		r := serve(h, as(req{method: "POST", path: "/accounts", body: map[string]any{
			"email": "x@y.z", "password": "p", "imapHost": "imap.elsewhere.example"}}))
		if r.Code != http.StatusForbidden || r.code() != api.CodeCustomServers {
			t.Errorf("custom host: %d %q", r.Code, r.code())
		}
		// Same login on the same server already exists.
		r = serve(h, as(req{method: "POST", path: "/accounts", body: map[string]any{
			"email": "silo@silo.test", "username": user, "password": pass}}))
		if r.Code != http.StatusConflict || r.code() != api.CodeAccountExists {
			t.Errorf("duplicate: %d %q: %s", r.Code, r.code(), r.Body)
		}

		if r := serve(h, as(req{method: "DELETE", path: "/accounts/" + accID})); r.Code != http.StatusNoContent {
			t.Fatalf("delete: %d %s", r.Code, r.Body)
		}
		if n := e.count("SELECT count(*) FROM accounts WHERE user_id = 1"); n != 0 {
			t.Fatalf("accounts left = %d", n)
		}

		// A failing test produces 422 with the steps and stores nothing.
		r = serve(h, as(req{method: "POST", path: "/accounts", body: map[string]any{
			"email": "silo@silo.test", "username": user, "password": "wrong"}}))
		var p api.Problem
		r.json(&p)
		if r.Code != http.StatusUnprocessableEntity || p.Code != api.CodeAccountTestFailed || p.Test == nil || p.Test.IMAP.OK {
			t.Fatalf("failed test: %d %s", r.Code, r.Body)
		}
		if n := e.count("SELECT count(*) FROM accounts WHERE user_id = 1"); n != 0 {
			t.Fatalf("account stored despite failed test")
		}

		r = serve(h, as(req{method: "POST", path: "/accounts", body: map[string]any{
			"email": "silo@silo.test", "username": user, "password": pass, "displayName": "Silo"}}))
		var a api.Account
		r.json(&a)
		if r.Code != http.StatusCreated || a.State != "connecting" || a.DisplayName != "Silo" || a.IMAPHost != e.cfg.DefaultIMAPHost {
			t.Fatalf("create: %d %s", r.Code, r.Body)
		}
		if strings.Contains(r.Body.String(), pass) {
			t.Error("password in the create response")
		}
		id, _ := strconv.ParseInt(strings.TrimPrefix(a.ID, "acc_"), 36, 64)
		acc, err := sqlcgen.New(e.pool).GetAccount(ctx, sqlcgen.GetAccountParams{ID: id, UserID: 1})
		if err != nil {
			t.Fatal(err)
		}
		if p, err := e.ring.Decrypt(acc.SecretEnc, []byte(strconv.FormatInt(id, 10))); err != nil || string(p) != pass {
			t.Errorf("stored secret: %q, %v", p, err)
		}
		if n := e.count("SELECT count(*) FROM jobs WHERE kind = 'account.initial_sync' AND account_id = $1", id); n != 1 {
			t.Errorf("initial sync jobs for the new account = %d", n)
		}

		var st api.SyncStatus
		r = serve(h, as(req{method: "GET", path: "/accounts/" + a.ID + "/sync-status"}))
		r.json(&st)
		if r.Code != http.StatusOK || st.State != "connecting" || st.ID != a.ID {
			t.Errorf("sync-status: %d %s", r.Code, r.Body)
		}
	})
}

func tokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
