package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/push"
	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/store"
	"github.com/silopost/silo-server/internal/testutil/pgtest"
)

// deviceEnv is the API over Postgres only: sessions are inserted directly.
type deviceEnv struct {
	t    *testing.T
	pool *pgxpool.Pool
	ring *secrets.Keyring
	seq  int
}

func newDeviceEnv(t *testing.T) *deviceEnv {
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
	ring, err := secrets.NewKeyring(bytes.Repeat([]byte{7}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	return &deviceEnv{t: t, pool: pool, ring: ring}
}

// router mounts the API with push on or off (the expo URLs are never called:
// no job runs here).
func (e *deviceEnv) router(enabled bool) http.Handler {
	cfg := config.Config{}
	r := chi.NewRouter()
	api.Mount(r, api.Options{
		Auth:      auth.New(auth.Options{Pool: e.pool, Keys: e.ring, Config: cfg}),
		Push:      push.New(push.Options{Pool: e.pool, Config: config.PushConfig{Enabled: enabled, ExpoSendURL: "http://127.0.0.1:1/send"}}),
		PublicURL: publicURL,
	})
	return r
}

type deviceUser struct {
	id       int64
	accounts []int64
}

func (e *deviceEnv) user(accounts int) deviceUser {
	e.t.Helper()
	ctx := context.Background()
	e.seq++
	var u deviceUser
	if err := e.pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`, fmt.Sprintf("dev%d@silo.test", e.seq)).Scan(&u.id); err != nil {
		e.t.Fatal(err)
	}
	for i := range accounts {
		var id int64
		email := fmt.Sprintf("dev%d-%d@silo.test", e.seq, i)
		if err := e.pool.QueryRow(ctx, `
INSERT INTO accounts (user_id, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, secret_enc)
VALUES ($1, $2, 'imap.test', 993, 'implicit', 'smtp.test', 465, 'implicit', $3, '\x00') RETURNING id`, u.id, email, email).Scan(&id); err != nil {
			e.t.Fatal(err)
		}
		u.accounts = append(u.accounts, id)
	}
	return u
}

// session inserts a session of kind for the user.
func (e *deviceEnv) session(u deviceUser, kind string) login {
	e.t.Helper()
	e.seq++
	token := fmt.Sprintf("token-%d", e.seq)
	sum := sha256.Sum256([]byte(token))
	csrf := fmt.Sprintf("csrf-%d", e.seq)
	if _, err := e.pool.Exec(context.Background(), `
INSERT INTO sessions (user_id, token_hash, csrf_token, expires_at, kind) VALUES ($1, $2, $3, now() + interval '1 hour', $4)`,
		u.id, sum[:], csrf, kind); err != nil {
		e.t.Fatal(err)
	}
	if kind == "bearer" {
		return login{bearer: token}
	}
	return login{cookie: &http.Cookie{Name: api.SessionCookie, Value: token}, csrf: csrf}
}

func (e *deviceEnv) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// call serves q as l (bearer header or cookie + CSRF).
func call(h http.Handler, l login, q req) resp {
	if l.bearer != "" {
		q.headers = bearerHdr(l.bearer)
	} else {
		q.cookie, q.csrf = l.cookie, l.csrf
	}
	return serve(h, q)
}

func accID(id int64) string { return "acc_" + strconv.FormatInt(id, 36) }

func TestDevices(t *testing.T) {
	e := newDeviceEnv(t)
	h := e.router(true)
	alice := e.user(2)
	phone := e.session(alice, "bearer")
	const token = "ExponentPushToken[abcDEF_123-x]"

	var dev api.Device
	t.Run("register from a bearer session", func(t *testing.T) {
		r := call(h, phone, req{method: "POST", path: "/devices", body: map[string]any{"provider": "expo", "pushToken": token}})
		if r.Code != http.StatusOK {
			t.Fatalf("status %d: %s", r.Code, r.Body)
		}
		r.json(&dev)
		if dev.Provider != "expo" || dev.PushToken != token || dev.AccountIDs != nil || dev.LastError != nil || dev.ID == "" {
			t.Fatalf("device = %+v", dev)
		}
		r = call(h, phone, req{method: "GET", path: "/devices/current"})
		var cur api.Device
		if r.Code != http.StatusOK {
			t.Fatalf("GET current: %d %s", r.Code, r.Body)
		}
		r.json(&cur)
		if cur.ID != dev.ID {
			t.Fatalf("current = %+v, want %s", cur, dev.ID)
		}
		// Limited to one account.
		r = call(h, phone, req{method: "POST", path: "/devices", body: map[string]any{
			"provider": "expo", "pushToken": token, "accountIds": []string{accID(alice.accounts[1])},
		}})
		var lim api.Device
		r.json(&lim)
		if r.Code != http.StatusOK || lim.ID != dev.ID || len(lim.AccountIDs) != 1 || lim.AccountIDs[0] != accID(alice.accounts[1]) {
			t.Fatalf("limited: %d %+v", r.Code, lim)
		}
	})

	t.Run("cookie session refused", func(t *testing.T) {
		web := e.session(alice, "cookie")
		r := call(h, web, req{method: "POST", path: "/devices", body: map[string]any{"provider": "expo", "pushToken": "ExpoPushToken[web]"}})
		if r.Code != http.StatusForbidden || r.code() != api.CodeBearerSessionRequired {
			t.Fatalf("status %d code %q: %s", r.Code, r.code(), r.Body)
		}
		if r := call(h, web, req{method: "GET", path: "/devices/current"}); r.Code != http.StatusForbidden || r.code() != api.CodeBearerSessionRequired {
			t.Fatalf("GET current from cookie: %d %s", r.Code, r.Body)
		}
	})

	t.Run("validation", func(t *testing.T) {
		for name, body := range map[string]map[string]any{
			"provider":   {"provider": "fcm", "pushToken": token},
			"token":      {"provider": "expo", "pushToken": "not-a-token"},
			"empty":      {"provider": "expo", "pushToken": "ExponentPushToken[]"},
			"too long":   {"provider": "expo", "pushToken": "ExponentPushToken[" + string(bytes.Repeat([]byte("a"), 300)) + "]"},
			"no token":   {"provider": "expo"},
			"bad acc id": {"provider": "expo", "pushToken": token, "accountIds": []string{"t_1"}},
		} {
			r := call(h, phone, req{method: "POST", path: "/devices", body: body})
			want := http.StatusBadRequest
			if name == "bad acc id" {
				want = http.StatusUnprocessableEntity
			}
			if r.Code != want {
				t.Errorf("%s: status %d: %s", name, r.Code, r.Body)
			}
		}
		bob := e.user(1)
		r := call(h, phone, req{method: "POST", path: "/devices", body: map[string]any{
			"provider": "expo", "pushToken": token, "accountIds": []string{accID(alice.accounts[0]), accID(bob.accounts[0])},
		}})
		if r.Code != http.StatusUnprocessableEntity || r.code() != api.CodeInvalidAccount {
			t.Fatalf("foreign account: %d %q %s", r.Code, r.code(), r.Body)
		}
	})

	t.Run("same token from a new session moves the row", func(t *testing.T) {
		again := e.session(alice, "bearer")
		r := call(h, again, req{method: "POST", path: "/devices", body: map[string]any{"provider": "expo", "pushToken": token}})
		var d api.Device
		r.json(&d)
		if r.Code != http.StatusOK || d.ID != dev.ID || d.AccountIDs != nil {
			t.Fatalf("re-register: %d %+v", r.Code, d)
		}
		if n := e.count(`SELECT count(*) FROM push_devices WHERE push_token = $1`, token); n != 1 {
			t.Fatalf("rows for the token = %d", n)
		}
		if r := call(h, phone, req{method: "GET", path: "/devices/current"}); r.Code != http.StatusNotFound {
			t.Fatalf("old session still has the device: %d %s", r.Code, r.Body)
		}
		phone = again
	})

	t.Run("delete only own devices", func(t *testing.T) {
		bob := e.user(1)
		bobPhone := e.session(bob, "bearer")
		r := call(h, bobPhone, req{method: "POST", path: "/devices", body: map[string]any{"provider": "expo", "pushToken": "ExpoPushToken[bob]"}})
		var bd api.Device
		r.json(&bd)
		if r := call(h, phone, req{method: "DELETE", path: "/devices/" + bd.ID}); r.Code != http.StatusNotFound {
			t.Fatalf("deleted another user's device: %d", r.Code)
		}
		if r := call(h, bobPhone, req{method: "DELETE", path: "/devices/" + bd.ID}); r.Code != http.StatusNoContent {
			t.Fatalf("delete own: %d %s", r.Code, r.Body)
		}
		if r := call(h, bobPhone, req{method: "GET", path: "/devices/current"}); r.Code != http.StatusNotFound {
			t.Fatalf("after delete: %d", r.Code)
		}
	})

	t.Run("sign-out removes the device", func(t *testing.T) {
		if r := call(h, phone, req{method: "DELETE", path: "/session"}); r.Code != http.StatusNoContent {
			t.Fatalf("sign out: %d %s", r.Code, r.Body)
		}
		if n := e.count(`SELECT count(*) FROM push_devices WHERE push_token = $1`, token); n != 0 {
			t.Fatalf("device survived sign-out: %d", n)
		}
	})

	t.Run("push switched off", func(t *testing.T) {
		off := e.router(false)
		r := serve(off, req{method: "GET", path: "/meta"})
		var meta api.Meta
		r.json(&meta)
		if meta.Push.Providers == nil || len(meta.Push.Providers) != 0 {
			t.Fatalf("meta push with push off = %+v", meta.Push)
		}
		r = serve(h, req{method: "GET", path: "/meta"})
		r.json(&meta)
		if len(meta.Push.Providers) != 1 || meta.Push.Providers[0] != "expo" {
			t.Fatalf("meta push = %+v", meta.Push)
		}
		s := e.session(alice, "bearer")
		r = call(off, s, req{method: "POST", path: "/devices", body: map[string]any{"provider": "expo", "pushToken": "ExpoPushToken[off]"}})
		if r.Code != http.StatusForbidden || r.code() != api.CodePushDisabled {
			t.Fatalf("register with push off: %d %q %s", r.Code, r.code(), r.Body)
		}
	})
}
