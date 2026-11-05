package api_test

import (
	"bytes"
	"context"
	"encoding/base32"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
	"github.com/silopost/silo-server/internal/totp"
)

// localMinLogin is MinLoginDuration of the mode local API under test.
const localMinLogin = 300 * time.Millisecond

type localEnv struct {
	*env
	h    http.Handler
	svc  *auth.Service
	cfg  config.Config
	host string // Dovecot / SMTP test host, reachable only through the allowlist
}

// newLocalEnv serves the API in SILO_AUTH_MODE=local with no default mail
// server; the test servers are reachable through the custom server allowlist.
func newLocalEnv(t *testing.T) *localEnv {
	e := newEnv(t)
	cfg := e.cfg
	cfg.AuthMode = config.AuthModeLocal
	cfg.CustomServerAllowlist = []string{cfg.DefaultIMAPHost, cfg.DefaultSMTPHost}
	host := cfg.DefaultIMAPHost
	cfg.DefaultIMAPHost, cfg.DefaultSMTPHost = "", ""
	ao := auth.Options{MinLoginDuration: localMinLogin, LoginLimit: auth.Limit{Burst: 1000, Every: time.Second}}
	return &localEnv{
		env: e, h: e.router(cfg, ao), cfg: cfg, host: host,
		svc: auth.New(auth.Options{Pool: e.pool, Keys: e.ring, Config: cfg}),
	}
}

func (l *localEnv) addUser(t *testing.T, email, password string, admin bool) int64 {
	t.Helper()
	u, err := l.svc.CreateUser(context.Background(), auth.NewUser{Email: email, Password: password, IsAdmin: admin})
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// tryLogin posts credentials and reports how long the answer took.
func (l *localEnv) tryLogin(body map[string]any) (resp, time.Duration) {
	start := time.Now()
	r := serve(l.h, req{method: "POST", path: "/session", body: body})
	return r, time.Since(start)
}

func (l *localEnv) mustLogin(t *testing.T, body map[string]any) login {
	t.Helper()
	r, _ := l.tryLogin(body)
	if r.Code != http.StatusOK {
		t.Fatalf("login %v: %d %s", body["email"], r.Code, r.Body)
	}
	var lg login
	r.json(&lg.body)
	lg.cookie, lg.csrf = r.cookie(), lg.body.CSRFToken
	return lg
}

func creds(email, password string) map[string]any {
	return map[string]any{"email": email, "password": password}
}

func TestLocalModeLogin(t *testing.T) {
	l := newLocalEnv(t)
	ctx := context.Background()
	const pw = "correct horse battery"
	aliceID := l.addUser(t, "alice@local.test", pw, false)
	bobID := l.addUser(t, "bob@local.test", pw, false)
	if _, err := l.pool.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, bobID); err != nil {
		t.Fatal(err)
	}

	t.Run("failures look alike", func(t *testing.T) {
		for name, body := range map[string]map[string]any{
			"wrong password": creds("alice@local.test", "wrong password!"),
			"unknown user":   creds("nobody@local.test", pw),
			"disabled user":  creds("bob@local.test", pw),
		} {
			r, took := l.tryLogin(body)
			if r.Code != http.StatusUnauthorized || r.code() != api.CodeInvalidCredentials {
				t.Errorf("%s: %d %q", name, r.Code, r.code())
			}
			if took < localMinLogin {
				t.Errorf("%s answered in %s, before MinLoginDuration", name, took)
			}
			if r.cookie() != nil {
				t.Errorf("%s: cookie set", name)
			}
		}
	})

	t.Run("success creates nothing", func(t *testing.T) {
		r, took := l.tryLogin(creds("ALICE@local.test", pw))
		if r.Code != http.StatusOK || r.cookie() == nil {
			t.Fatalf("login: %d %s", r.Code, r.Body)
		}
		if took < localMinLogin {
			t.Errorf("success answered in %s", took)
		}
		var body api.SessionBody
		r.json(&body)
		if body.User.ID != "u_"+strconv.FormatInt(aliceID, 36) || len(body.Accounts) != 0 || body.User.TOTPEnabled {
			t.Errorf("session body %+v", body)
		}
		if n := l.count(`SELECT count(*) FROM users`); n != 2 {
			t.Errorf("users = %d", n)
		}
	})

	t.Run("disabled user's session stops working", func(t *testing.T) {
		lg := l.mustLogin(t, creds("alice@local.test", pw))
		if _, err := l.pool.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, aliceID); err != nil {
			t.Fatal(err)
		}
		if r := serve(l.h, req{method: "GET", path: "/session", cookie: lg.cookie}); r.Code != http.StatusUnauthorized {
			t.Errorf("GET /session of a disabled user: %d", r.Code)
		}
		if _, err := l.pool.Exec(ctx, `UPDATE users SET disabled_at = NULL WHERE id = $1`, aliceID); err != nil {
			t.Fatal(err)
		}
		if r := serve(l.h, req{method: "GET", path: "/session", cookie: lg.cookie}); r.Code != http.StatusOK {
			t.Errorf("GET /session after re-enable: %d", r.Code)
		}
	})

	t.Run("accounts need an explicit host", func(t *testing.T) {
		lg := l.mustLogin(t, creds("alice@local.test", pw))
		r := serve(l.h, as(lg, req{method: "POST", path: "/accounts", body: map[string]any{
			"email": "silo@silo.test", "username": imaptest.User, "password": imaptest.Pass}}))
		if r.Code != http.StatusUnprocessableEntity || r.code() != api.CodeValidation {
			t.Errorf("no host: %d %s", r.Code, r.Body)
		}
		// A host outside the allowlist is checked against SSRF rules.
		r = serve(l.h, as(lg, req{method: "POST", path: "/accounts", body: map[string]any{
			"email": "silo@silo.test", "username": imaptest.User, "password": imaptest.Pass, "imapHost": "127.0.0.2",
			"imapPort": l.env.cfg.DefaultIMAPPort, "imapTls": "none"}}))
		if r.Code != http.StatusUnprocessableEntity || r.code() != api.CodeHostNotAllowed {
			t.Errorf("private host: %d %s", r.Code, r.Body)
		}
		r = serve(l.h, as(lg, req{method: "POST", path: "/accounts", body: map[string]any{
			"email": "silo@silo.test", "username": imaptest.User, "password": imaptest.Pass,
			"imapHost": l.host, "imapPort": l.env.cfg.DefaultIMAPPort, "imapTls": "none",
			"smtpHost": l.env.cfg.DefaultSMTPHost, "smtpPort": l.env.cfg.DefaultSMTPPort, "smtpTls": "none"}}))
		if r.Code != http.StatusCreated {
			t.Fatalf("add account: %d %s", r.Code, r.Body)
		}
	})
}

func TestLocalModePasswordChange(t *testing.T) {
	l := newLocalEnv(t)
	const pw, next = "first password", "second password"
	l.addUser(t, "carol@local.test", pw, false)
	a := l.mustLogin(t, creds("carol@local.test", pw))
	b := l.mustLogin(t, creds("carol@local.test", pw))

	change := func(cur, nw string) resp {
		return serve(l.h, as(a, req{method: "POST", path: "/session/password",
			body: map[string]any{"currentPassword": cur, "newPassword": nw}}))
	}
	if r := change("wrong one!", next); r.Code != http.StatusForbidden || r.code() != api.CodeInvalidCredentials {
		t.Errorf("wrong current: %d %s", r.Code, r.Body)
	}
	if r := change(pw, "short"); r.Code != http.StatusBadRequest || r.code() != api.CodeValidation {
		t.Errorf("short new: %d %s", r.Code, r.Body)
	}
	if r := change(pw, next); r.Code != http.StatusNoContent {
		t.Fatalf("change: %d %s", r.Code, r.Body)
	}
	if r := serve(l.h, req{method: "GET", path: "/session", cookie: b.cookie}); r.Code != http.StatusUnauthorized {
		t.Errorf("other session still valid: %d", r.Code)
	}
	if r := serve(l.h, req{method: "GET", path: "/session", cookie: a.cookie}); r.Code != http.StatusOK {
		t.Errorf("current session dropped: %d", r.Code)
	}
	if r, _ := l.tryLogin(creds("carol@local.test", pw)); r.Code != http.StatusUnauthorized {
		t.Errorf("old password still works: %d", r.Code)
	}
	l.mustLogin(t, creds("carol@local.test", next))
	if n := l.count(`SELECT count(*) FROM users WHERE password_changed_at IS NOT NULL`); n != 1 {
		t.Errorf("password_changed_at not stamped")
	}
}

func TestLocalModeTOTP(t *testing.T) {
	l := newLocalEnv(t)
	ctx := context.Background()
	const email, pw = "dave@local.test", "dave's password"
	uid := l.addUser(t, email, pw, false)
	lg := l.mustLogin(t, creds(email, pw))
	call := func(method, path string, body any) resp {
		return serve(l.h, as(lg, req{method: method, path: path, body: body}))
	}

	if r := call("POST", "/totp/enable", map[string]any{"code": "123456"}); r.Code != http.StatusConflict || r.code() != api.CodeTOTPNotSetUp {
		t.Errorf("enable before setup: %d %s", r.Code, r.Body)
	}
	// A second setup replaces the first secret.
	call("POST", "/totp/setup", nil)
	r := call("POST", "/totp/setup", nil)
	var setup api.TOTPSetupBody
	r.json(&setup)
	if r.Code != http.StatusOK || len(setup.Secret) != 32 ||
		setup.OTPAuthURL != "otpauth://totp/Silo:"+email+"?secret="+setup.Secret+"&issuer=Silo&algorithm=SHA1&digits=6&period=30" {
		t.Fatalf("setup: %d %s", r.Code, r.Body)
	}
	secret, err := b32decode(setup.Secret)
	if err != nil {
		t.Fatal(err)
	}
	// Stored sealed, never in the clear.
	var stored []byte
	if err := l.pool.QueryRow(ctx, `SELECT totp_secret_enc FROM users WHERE id = $1`, uid).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, secret) || bytes.Contains(stored, []byte(setup.Secret)) {
		t.Fatal("TOTP secret stored in the clear")
	}
	// Not enforced until enabled.
	l.mustLogin(t, creds(email, pw))

	step := func() int64 { return totp.Step(time.Now()) }
	if r := call("POST", "/totp/enable", map[string]any{"code": wrongCode(secret, step())}); r.Code != http.StatusUnprocessableEntity || r.code() != api.CodeInvalidTOTPCode {
		t.Errorf("enable with a wrong code: %d %s", r.Code, r.Body)
	}
	r = call("POST", "/totp/enable", map[string]any{"code": totp.Code(secret, step())})
	var codes api.BackupCodesBody
	r.json(&codes)
	if r.Code != http.StatusOK || len(codes.BackupCodes) != 10 {
		t.Fatalf("enable: %d %s", r.Code, r.Body)
	}
	if r := call("POST", "/totp/setup", nil); r.Code != http.StatusConflict || r.code() != api.CodeTOTPEnabled {
		t.Errorf("setup while enabled: %d %s", r.Code, r.Body)
	}
	var sess api.SessionBody
	call("GET", "/session", nil).json(&sess)
	if !sess.User.TOTPEnabled {
		t.Error("session.user.totpEnabled = false")
	}

	withCode := func(password, code string) map[string]any {
		b := creds(email, password)
		b["totpCode"] = code
		return b
	}
	// Right password, no code: totp_required, still after the minimum time.
	r, took := l.tryLogin(creds(email, pw))
	if r.Code != http.StatusUnauthorized || r.code() != api.CodeTOTPRequired || took < localMinLogin || r.cookie() != nil {
		t.Errorf("no code: %d %q in %s", r.Code, r.code(), took)
	}
	// Wrong password never reveals that TOTP is on.
	if r, _ := l.tryLogin(creds(email, "not the password")); r.code() != api.CodeInvalidCredentials {
		t.Errorf("wrong password: %q", r.code())
	}
	if r, _ := l.tryLogin(withCode(pw, wrongCode(secret, step()))); r.Code != http.StatusUnauthorized || r.code() != api.CodeInvalidCredentials {
		t.Errorf("wrong code: %d %q", r.Code, r.code())
	}
	// The next step's code is accepted once (the current one was used by enable).
	code := totp.Code(secret, step()+1)
	l.mustLogin(t, withCode(pw, code))
	if r, _ := l.tryLogin(withCode(pw, code)); r.Code != http.StatusUnauthorized || r.code() != api.CodeInvalidCredentials {
		t.Errorf("replayed code: %d %q", r.Code, r.code())
	}
	// A backup code works exactly once.
	l.mustLogin(t, withCode(pw, codes.BackupCodes[0]))
	if r, _ := l.tryLogin(withCode(pw, codes.BackupCodes[0])); r.Code != http.StatusUnauthorized {
		t.Errorf("backup code reused: %d", r.Code)
	}
	if n := l.count(`SELECT cardinality(totp_backup_codes) FROM users WHERE id = $1`, uid); n != 9 {
		t.Errorf("backup codes left = %d", n)
	}

	// New backup codes need the password and a code; the old set stops working.
	if r := call("POST", "/totp/backup-codes", map[string]any{"password": "wrong password", "code": codes.BackupCodes[1]}); r.Code != http.StatusForbidden || r.code() != api.CodeInvalidCredentials {
		t.Errorf("backup-codes with a wrong password: %d %s", r.Code, r.Body)
	}
	r = call("POST", "/totp/backup-codes", map[string]any{"password": pw, "code": codes.BackupCodes[1]})
	var fresh api.BackupCodesBody
	r.json(&fresh)
	if r.Code != http.StatusOK || len(fresh.BackupCodes) != 10 {
		t.Fatalf("backup-codes: %d %s", r.Code, r.Body)
	}
	if r, _ := l.tryLogin(withCode(pw, codes.BackupCodes[2])); r.Code != http.StatusUnauthorized {
		t.Errorf("old backup code still works: %d", r.Code)
	}

	// Disable with the password and a code; sign-in no longer asks for one.
	if r := call("DELETE", "/totp", map[string]any{"password": pw, "code": wrongCode(secret, step())}); r.Code != http.StatusForbidden {
		t.Errorf("disable with a wrong code: %d %s", r.Code, r.Body)
	}
	if r := call("DELETE", "/totp", map[string]any{"password": pw, "code": fresh.BackupCodes[0]}); r.Code != http.StatusNoContent {
		t.Fatalf("disable: %d %s", r.Code, r.Body)
	}
	l.mustLogin(t, creds(email, pw))
	if n := l.count(`SELECT count(*) FROM users WHERE totp_secret_enc IS NULL AND totp_enabled_at IS NULL AND cardinality(totp_backup_codes) = 0`); n != 1 {
		t.Error("TOTP state not cleared")
	}
	if r := call("DELETE", "/totp", map[string]any{"password": pw, "code": "000000"}); r.Code != http.StatusConflict || r.code() != api.CodeTOTPNotEnabled {
		t.Errorf("disable twice: %d %s", r.Code, r.Body)
	}
}

// wrongCode returns a 6-digit code valid for none of the steps around step.
func wrongCode(secret []byte, step int64) string {
	for i := 0; ; i++ {
		c := strconv.Itoa(100000 + i)
		ok := true
		for s := step - 2; s <= step+2; s++ {
			if totp.Code(secret, s) == c {
				ok = false
			}
		}
		if ok {
			return c
		}
	}
}

func b32decode(s string) ([]byte, error) {
	return base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
}

// TestImapModeLocalOnlyEndpoints: the mode local endpoints answer 409
// not_available on an imap instance.
func TestImapModeLocalOnlyEndpoints(t *testing.T) {
	e := newEnv(t)
	lg := e.login(e.web, imaptest.User, imaptest.Pass)
	if lg.body.User.TOTPEnabled {
		t.Error("totpEnabled in imap mode")
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/totp/setup", nil},
		{"POST", "/totp/enable", map[string]any{"code": "123456"}},
		{"DELETE", "/totp", map[string]any{"password": "x", "code": "123456"}},
		{"POST", "/totp/backup-codes", map[string]any{"password": "x", "code": "123456"}},
		{"POST", "/session/password", map[string]any{"currentPassword": imaptest.Pass, "newPassword": "long enough password"}},
	} {
		r := serve(e.web, as(lg, req{method: c.method, path: c.path, body: c.body}))
		if r.Code != http.StatusConflict || r.code() != api.CodeNotAvailable {
			t.Errorf("%s %s: %d %s", c.method, c.path, r.Code, r.Body)
		}
	}
}
