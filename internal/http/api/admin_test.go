package api_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

func TestAdminPanel(t *testing.T) {
	l := newLocalEnv(t)
	ctx := context.Background()
	const pw = "admin password"
	rootID := l.addUser(t, "root@local.test", pw, true)
	eveID := l.addUser(t, "eve@local.test", pw, false)
	root := l.mustLogin(t, creds("root@local.test", pw))
	eve := l.mustLogin(t, creds("eve@local.test", pw))
	uid := func(id int64) string { return "u_" + strconv.FormatInt(id, 36) }

	// eve has one mail account with a dead and a pending write-back job.
	var accID int64
	if err := l.pool.QueryRow(ctx, `INSERT INTO accounts (user_id, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, secret_enc, sync_state)
		VALUES ($1, 'eve@mail.test', 'imap.mail.test', 993, 'implicit', 'smtp.mail.test', 587, 'starttls', 'eve', '\x00', 'live') RETURNING id`, eveID).Scan(&accID); err != nil {
		t.Fatal(err)
	}
	var deadJob int64
	if err := l.pool.QueryRow(ctx, `INSERT INTO jobs (kind, account_id, payload, status, attempts, last_error)
		VALUES ('imap_op', $1, '{"secret":"do-not-show"}', 'dead', 5, 'NO [TRYCREATE] gone') RETURNING id`, accID).Scan(&deadJob); err != nil {
		t.Fatal(err)
	}
	if _, err := l.pool.Exec(ctx, `INSERT INTO jobs (kind, account_id, run_at) VALUES ('imap_op', $1, now() + interval '1 hour')`, accID); err != nil {
		t.Fatal(err)
	}
	accStr := "acc_" + strconv.FormatInt(accID, 36)
	jobStr := "job_" + strconv.FormatInt(deadJob, 36)

	adm := func(method, path string, body any) resp {
		return serve(l.h, as(root, req{method: method, path: path, body: body}))
	}

	t.Run("admins only", func(t *testing.T) {
		for _, p := range []string{"/admin/users", "/admin/accounts", "/admin/jobs"} {
			if r := serve(l.h, req{method: "GET", path: p, cookie: eve.cookie}); r.Code != http.StatusForbidden || r.code() != api.CodeForbidden {
				t.Errorf("GET %s as a user: %d %s", p, r.Code, r.Body)
			}
			if r := serve(l.h, req{method: "GET", path: p}); r.Code != http.StatusUnauthorized {
				t.Errorf("GET %s without a session: %d", p, r.Code)
			}
		}
		if r := serve(l.h, as(eve, req{method: "PATCH", path: "/admin/users/" + uid(eveID), body: map[string]any{"isAdmin": true}})); r.Code != http.StatusForbidden {
			t.Errorf("self-promotion: %d", r.Code)
		}
	})

	t.Run("create users", func(t *testing.T) {
		r := adm("POST", "/admin/users", map[string]any{"email": "frank@local.test", "displayName": "Frank"})
		var created api.AdminCreatedUser
		r.json(&created)
		if r.Code != http.StatusCreated || len(created.TemporaryPassword) != 24 || created.DisplayName != "Frank" || created.IsAdmin || len(created.Accounts) != 0 {
			t.Fatalf("create: %d %s", r.Code, r.Body)
		}
		l.mustLogin(t, creds("frank@local.test", created.TemporaryPassword))

		r = adm("POST", "/admin/users", map[string]any{"email": "gina@local.test", "password": "gina's own password", "isAdmin": true})
		var gina api.AdminCreatedUser
		r.json(&gina)
		if r.Code != http.StatusCreated || gina.TemporaryPassword != "" || !gina.IsAdmin {
			t.Fatalf("create with password: %d %s", r.Code, r.Body)
		}
		if r := adm("POST", "/admin/users", map[string]any{"email": "GINA@local.test"}); r.Code != http.StatusConflict || r.code() != api.CodeUserExists {
			t.Errorf("duplicate: %d %s", r.Code, r.Body)
		}
		if r := adm("POST", "/admin/users", map[string]any{"email": "h@local.test", "password": "short"}); r.Code != http.StatusBadRequest {
			t.Errorf("short password: %d %s", r.Code, r.Body)
		}
	})

	t.Run("list users and accounts", func(t *testing.T) {
		var users struct{ Users []api.AdminUser }
		adm("GET", "/admin/users", nil).json(&users)
		if len(users.Users) != 4 {
			t.Fatalf("users: %+v", users.Users)
		}
		var e api.AdminUser
		for _, u := range users.Users {
			if u.ID == uid(eveID) {
				e = u
			}
		}
		if e.Email != "eve@local.test" || e.IsAdmin || e.Disabled || e.TOTPEnabled || e.CreatedAt.IsZero() ||
			len(e.Accounts) != 1 || e.Accounts[0].ID != accStr || e.Accounts[0].State != "live" {
			t.Errorf("eve: %+v", e)
		}

		var accs struct{ Accounts []api.AdminAccount }
		adm("GET", "/admin/accounts", nil).json(&accs)
		if len(accs.Accounts) != 1 {
			t.Fatalf("accounts: %+v", accs.Accounts)
		}
		a := accs.Accounts[0]
		if a.ID != accStr || a.UserID != uid(eveID) || a.UserEmail != "eve@local.test" || a.State != "live" || a.PendingOps != 1 || a.DeadOps != 1 {
			t.Errorf("account: %+v", a)
		}
	})

	t.Run("jobs", func(t *testing.T) {
		r := adm("GET", "/admin/jobs?status=dead", nil)
		var raw struct{ Jobs []map[string]any }
		r.json(&raw)
		if r.Code != http.StatusOK || len(raw.Jobs) != 1 {
			t.Fatalf("dead jobs: %d %s", r.Code, r.Body)
		}
		if _, ok := raw.Jobs[0]["payload"]; ok {
			t.Error("job payload exposed")
		}
		var jobs struct{ Jobs []api.AdminJob }
		r.json(&jobs)
		j := jobs.Jobs[0]
		if j.ID != jobStr || j.Kind != "imap_op" || j.Status != "dead" || j.Attempts != 5 || j.AccountID == nil || *j.AccountID != accStr ||
			j.UserID == nil || *j.UserID != uid(eveID) || j.LastError == nil {
			t.Errorf("dead job: %+v", j)
		}
		adm("GET", "/admin/jobs?accountId="+accStr, nil).json(&jobs)
		if len(jobs.Jobs) != 2 {
			t.Errorf("jobs of the account: %d", len(jobs.Jobs))
		}
		if r := adm("GET", "/admin/jobs?status=running", nil); r.Code != http.StatusBadRequest {
			t.Errorf("bad status filter: %d", r.Code)
		}
		if r := adm("GET", "/admin/jobs?accountId=nope", nil); r.Code != http.StatusBadRequest {
			t.Errorf("bad account filter: %d", r.Code)
		}

		r = adm("POST", "/admin/jobs/"+jobStr+"/retry", nil)
		var retried api.AdminJob
		r.json(&retried)
		if r.Code != http.StatusOK || retried.Status != "pending" || retried.Attempts != 0 || retried.UserID == nil {
			t.Fatalf("retry: %d %s", r.Code, r.Body)
		}
		if n := l.count(`SELECT count(*) FROM jobs WHERE id = $1 AND status = 'pending' AND run_at <= now()`, deadJob); n != 1 {
			t.Error("retried job is not due")
		}
		adm("GET", "/admin/jobs?status=dead", nil).json(&jobs)
		if len(jobs.Jobs) != 0 {
			t.Errorf("dead jobs after retry: %d", len(jobs.Jobs))
		}
		if r := adm("DELETE", "/admin/jobs/"+jobStr, nil); r.Code != http.StatusNoContent {
			t.Errorf("delete job: %d %s", r.Code, r.Body)
		}
		if r := adm("DELETE", "/admin/jobs/"+jobStr, nil); r.Code != http.StatusNotFound {
			t.Errorf("delete job twice: %d", r.Code)
		}
		if r := adm("POST", "/admin/jobs/"+jobStr+"/retry", nil); r.Code != http.StatusNotFound {
			t.Errorf("retry deleted job: %d", r.Code)
		}
	})

	t.Run("no self lock-out", func(t *testing.T) {
		for _, body := range []map[string]any{{"disabled": true}, {"isAdmin": false}} {
			if r := adm("PATCH", "/admin/users/"+uid(rootID), body); r.Code != http.StatusConflict || r.code() != api.CodeSelfChange {
				t.Errorf("self %v: %d %s", body, r.Code, r.Body)
			}
		}
		if r := adm("DELETE", "/admin/users/"+uid(rootID), nil); r.Code != http.StatusConflict || r.code() != api.CodeSelfChange {
			t.Errorf("self delete: %d %s", r.Code, r.Body)
		}
		// Renaming oneself is fine.
		var me api.AdminUser
		if r := adm("PATCH", "/admin/users/"+uid(rootID), map[string]any{"displayName": "Root"}); r.Code != http.StatusOK {
			t.Errorf("self rename: %d %s", r.Code, r.Body)
		} else if r.json(&me); me.DisplayName != "Root" || !me.IsAdmin {
			t.Errorf("self rename: %+v", me)
		}
	})

	t.Run("disable and enable", func(t *testing.T) {
		r := adm("PATCH", "/admin/users/"+uid(eveID), map[string]any{"disabled": true})
		var u api.AdminUser
		r.json(&u)
		if r.Code != http.StatusOK || !u.Disabled || u.Accounts[0].State != "disabled" {
			t.Fatalf("disable: %d %s", r.Code, r.Body)
		}
		if r := serve(l.h, req{method: "GET", path: "/session", cookie: eve.cookie}); r.Code != http.StatusUnauthorized {
			t.Errorf("disabled user's session: %d", r.Code)
		}
		if n := l.count(`SELECT count(*) FROM sessions WHERE user_id = $1`, eveID); n != 0 {
			t.Errorf("sessions left: %d", n)
		}
		if r, _ := l.tryLogin(creds("eve@local.test", pw)); r.code() != api.CodeInvalidCredentials {
			t.Errorf("disabled login: %d %q", r.Code, r.code())
		}

		r = adm("PATCH", "/admin/users/"+uid(eveID), map[string]any{"disabled": false})
		r.json(&u)
		if r.Code != http.StatusOK || u.Disabled || u.Accounts[0].State != "connecting" {
			t.Fatalf("enable: %d %s", r.Code, r.Body)
		}
		if n := l.count(`SELECT count(*) FROM jobs WHERE account_id = $1 AND kind = 'account.reconnect'`, accID); n != 1 {
			t.Errorf("reconnect jobs: %d", n)
		}
		eve = l.mustLogin(t, creds("eve@local.test", pw))
	})

	t.Run("password and totp reset", func(t *testing.T) {
		const next = "eve's new password"
		if r := adm("PATCH", "/admin/users/"+uid(eveID), map[string]any{"password": next, "isAdmin": true}); r.Code != http.StatusOK {
			t.Fatalf("set password: %d %s", r.Code, r.Body)
		}
		if r := serve(l.h, req{method: "GET", path: "/session", cookie: eve.cookie}); r.Code != http.StatusUnauthorized {
			t.Errorf("session after password reset: %d", r.Code)
		}
		eve = l.mustLogin(t, creds("eve@local.test", next))
		if !eve.body.User.IsAdmin {
			t.Error("isAdmin not granted")
		}

		if _, err := l.pool.Exec(ctx, `UPDATE users SET totp_secret_enc = '\x01', totp_enabled_at = now(), totp_backup_codes = '{a}' WHERE id = $1`, eveID); err != nil {
			t.Fatal(err)
		}
		if r, _ := l.tryLogin(creds("eve@local.test", next)); r.code() != api.CodeTOTPRequired {
			t.Fatalf("TOTP not enforced: %q", r.code())
		}
		if r := adm("POST", "/admin/users/"+uid(eveID)+"/totp/reset", nil); r.Code != http.StatusNoContent {
			t.Fatalf("totp reset: %d %s", r.Code, r.Body)
		}
		l.mustLogin(t, creds("eve@local.test", next))
		if r := adm("POST", "/admin/users/u_zzzz/totp/reset", nil); r.Code != http.StatusNotFound {
			t.Errorf("reset unknown: %d", r.Code)
		}
		if r := adm("PATCH", "/admin/users/u_zzzz", map[string]any{"displayName": "x"}); r.Code != http.StatusNotFound {
			t.Errorf("patch unknown: %d", r.Code)
		}
	})

	t.Run("delete user", func(t *testing.T) {
		if r := adm("DELETE", "/admin/users/"+uid(eveID), nil); r.Code != http.StatusNoContent {
			t.Fatalf("delete: %d %s", r.Code, r.Body)
		}
		if n := l.count(`SELECT count(*) FROM accounts WHERE id = $1`, accID); n != 0 {
			t.Error("account survived its user")
		}
		if r := adm("DELETE", "/admin/users/"+uid(eveID), nil); r.Code != http.StatusNotFound {
			t.Errorf("delete twice: %d", r.Code)
		}
	})
}

// TestAdminPanelImapMode: admin endpoints work in mode imap, except creating
// users and setting Silo passwords.
func TestAdminPanelImapMode(t *testing.T) {
	e := newEnv(t)
	lg := e.login(e.web, imaptest.User, imaptest.Pass)
	if r := serve(e.web, req{method: "GET", path: "/admin/users", cookie: lg.cookie}); r.Code != http.StatusForbidden {
		t.Errorf("non-admin: %d", r.Code)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET is_admin = true`); err != nil {
		t.Fatal(err)
	}
	r := serve(e.web, req{method: "GET", path: "/admin/users", cookie: lg.cookie})
	var users struct{ Users []api.AdminUser }
	r.json(&users)
	if r.Code != http.StatusOK || len(users.Users) != 1 || len(users.Users[0].Accounts) != 1 {
		t.Fatalf("list: %d %s", r.Code, r.Body)
	}
	if r := serve(e.web, as(lg, req{method: "POST", path: "/admin/users", body: map[string]any{"email": "x@silo.test"}})); r.Code != http.StatusConflict || r.code() != api.CodeNotAvailable {
		t.Errorf("create in imap mode: %d %s", r.Code, r.Body)
	}
	newUserSession(t, e, "other@silo.test")
	var otherID int64
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email = 'other@silo.test'`).Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	path := "/admin/users/u_" + strconv.FormatInt(otherID, 36)
	if r := serve(e.web, as(lg, req{method: "PATCH", path: path, body: map[string]any{"password": "long enough password"}})); r.Code != http.StatusConflict || r.code() != api.CodeNotAvailable {
		t.Errorf("password in imap mode: %d %s", r.Code, r.Body)
	}
	r = serve(e.web, as(lg, req{method: "PATCH", path: path, body: map[string]any{"isAdmin": true}}))
	var u api.AdminUser
	r.json(&u)
	if r.Code != http.StatusOK || !u.IsAdmin {
		t.Errorf("grant admin: %d %s", r.Code, r.Body)
	}
	// A disabled imap user cannot sign in with the mailbox password either.
	var selfID int64
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email <> 'other@silo.test'`).Scan(&selfID); err != nil {
		t.Fatal(err)
	}
	if r := serve(e.web, as(lg, req{method: "PATCH", path: path, body: map[string]any{"disabled": true}})); r.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", r.Code, r.Body)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET disabled_at = now() WHERE id = $1`, selfID); err != nil {
		t.Fatal(err)
	}
	if r := serve(e.web, req{method: "POST", path: "/session", body: map[string]string{"email": imaptest.User, "password": imaptest.Pass}}); r.Code != http.StatusUnauthorized || r.code() != api.CodeInvalidCredentials {
		t.Errorf("disabled imap login: %d %s", r.Code, r.Body)
	}
}
