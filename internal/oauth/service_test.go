package oauth_test

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/oauth"
	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/store"
	"github.com/silopost/silo-server/internal/testutil/oauthtest"
	"github.com/silopost/silo-server/internal/testutil/pgtest"
)

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	ring *secrets.Keyring
	fake *oauthtest.Server
	svc  *oauth.Service
	user int64
}

func newEnv(t *testing.T) *env {
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
	ring, err := secrets.NewKeyring(bytes.Repeat([]byte{9}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	fake := oauthtest.Start(t)
	e := &env{t: t, pool: pool, ring: ring, fake: fake}
	e.svc = oauth.New(oauth.Options{Pool: pool, Keys: ring, PublicURL: "https://mail.silo.test/", Providers: oauth.NewProviders(config.OAuthConfig{
		GoogleClientID: oauthtest.ClientID, GoogleClientSecret: oauthtest.ClientSecret,
		GoogleAuthURL: fake.URL + "/authorize", GoogleTokenURL: fake.URL + "/token", GoogleUserinfoURL: fake.URL + "/userinfo",
	})})
	if err := pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ('u@silo.test') RETURNING id`).Scan(&e.user); err != nil {
		t.Fatal(err)
	}
	return e
}

// signIn runs start -> provider -> callback and returns the grant.
func (e *env) signIn(req oauth.StartRequest) *oauth.Grant {
	e.t.Helper()
	ctx := context.Background()
	authURL, state, err := e.svc.Start(ctx, "google", req)
	if err != nil {
		e.t.Fatal(err)
	}
	cb := e.fake.Authorize(e.t, authURL)
	if cb.Query().Get("state") != state {
		e.t.Fatalf("state %q, want %q", cb.Query().Get("state"), state)
	}
	g, err := e.svc.Complete(ctx, "google", cb.Query().Get("state"), cb.Query().Get("code"), cb.Query().Get("error"))
	if err != nil {
		e.t.Fatalf("complete: %v", err)
	}
	return g
}

// account stores an xoauth2 account with the refresh token of g.
func (e *env) account(g *oauth.Grant) int64 {
	e.t.Helper()
	ctx := context.Background()
	var id int64
	if err := e.pool.QueryRow(ctx, `
INSERT INTO accounts (user_id, email, provider, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, auth_type, secret_enc)
VALUES ($1, $2::text, 'gmail', 'imap.gmail.com', 993, 'implicit', 'smtp.gmail.com', 465, 'implicit', $2::text, 'xoauth2', '\x00') RETURNING id`,
		g.UserID, g.Email).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	sealed, err := e.ring.Encrypt([]byte(g.Token.RefreshToken), []byte(strconv.FormatInt(id, 10)))
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE accounts SET secret_enc = $2 WHERE id = $1`, id, sealed); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) refreshToken(id int64) string {
	e.t.Helper()
	var sealed []byte
	if err := e.pool.QueryRow(context.Background(), `SELECT secret_enc FROM accounts WHERE id = $1`, id).Scan(&sealed); err != nil {
		e.t.Fatal(err)
	}
	rt, err := e.ring.Decrypt(sealed, []byte(strconv.FormatInt(id, 10)))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(rt)
}

func TestFlow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	g := e.signIn(oauth.StartRequest{UserID: e.user, ReturnTo: "/settings/accounts"})
	if g.UserID != e.user || g.Email != "oauth@silo.test" || g.Name != "OAuth User" || g.ReturnTo != "/settings/accounts" ||
		g.AccountID != 0 || g.Token.RefreshToken == "" || g.Provider.AccountProvider != "gmail" {
		t.Fatalf("grant = %+v", g)
	}
	q := e.fake.LastAuthorize()
	if q.Get("redirect_uri") != "https://mail.silo.test/api/v1/oauth/google/callback" || q.Get("access_type") != "offline" ||
		q.Get("prompt") != "consent" || q.Get("scope") != "https://mail.google.com/ openid email" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize query = %v", q)
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM oauth_states`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("states left = %d, %v", n, err)
	}

	// The address comes from userinfo when the id_token has none.
	e.fake.Set(func(s *oauthtest.Server) { s.IDTokenEmail, s.Email = false, "Other@Silo.test" })
	if g := e.signIn(oauth.StartRequest{UserID: e.user, AccountID: 0}); g.Email != "other@silo.test" {
		t.Fatalf("email from userinfo = %q", g.Email)
	}

	// Re-authorization keeps the account and offers its address.
	acc := e.account(g)
	g2 := e.signIn(oauth.StartRequest{UserID: e.user, AccountID: acc, LoginHint: "other@silo.test"})
	if g2.AccountID != acc || e.fake.LastAuthorize().Get("login_hint") != "other@silo.test" {
		t.Fatalf("reauth grant account %d, hint %q", g2.AccountID, e.fake.LastAuthorize().Get("login_hint"))
	}
}

func TestFlowErrors(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	code := func(err error) string {
		var fe *oauth.FlowError
		if !errors.As(err, &fe) {
			t.Fatalf("not a flow error: %v", err)
		}
		return fe.Code
	}

	if _, _, err := e.svc.Start(ctx, "microsoft", oauth.StartRequest{UserID: e.user}); !errors.Is(err, oauth.ErrNotConfigured) {
		t.Fatalf("unconfigured provider: %v", err)
	}
	for _, bad := range []string{"//evil.example", "/\\evil.example", "https://evil.example/", "relative", "/a\nb"} {
		if _, _, err := e.svc.Start(ctx, "google", oauth.StartRequest{UserID: e.user, ReturnTo: bad}); !errors.Is(err, oauth.ErrInvalidReturnTo) {
			t.Fatalf("returnTo %q: %v", bad, err)
		}
	}

	// Unknown and reused states.
	if g, err := e.svc.Complete(ctx, "google", "nope", "code", ""); code(err) != oauth.CodeStateInvalid || g.ReturnTo != "/" {
		t.Fatalf("unknown state: %v", err)
	}
	authURL, state, err := e.svc.Start(ctx, "google", oauth.StartRequest{UserID: e.user, ReturnTo: "/inbox"})
	if err != nil {
		t.Fatal(err)
	}
	cb := e.fake.Authorize(t, authURL)
	if _, err := e.svc.Complete(ctx, "google", state, cb.Query().Get("code"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Complete(ctx, "google", state, cb.Query().Get("code"), ""); code(err) != oauth.CodeStateInvalid {
		t.Fatalf("reused state: %v", err)
	}

	// Expired state.
	_, state, _ = e.svc.Start(ctx, "google", oauth.StartRequest{UserID: e.user, ReturnTo: "/x"})
	if _, err := e.pool.Exec(ctx, `UPDATE oauth_states SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if g, err := e.svc.Complete(ctx, "google", state, "c", ""); code(err) != oauth.CodeStateInvalid || g.ReturnTo != "/x" {
		t.Fatalf("expired state: %v %+v", err, g)
	}
	_, _, _ = e.svc.Start(ctx, "google", oauth.StartRequest{UserID: e.user})
	if _, err := e.pool.Exec(ctx, `UPDATE oauth_states SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.PurgeStates(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM oauth_states`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("states after purge = %d, %v", n, err)
	}

	// Denied consent.
	e.fake.Set(func(s *oauthtest.Server) { s.Deny = true })
	authURL, _, _ = e.svc.Start(ctx, "google", oauth.StartRequest{UserID: e.user})
	cb = e.fake.Authorize(t, authURL)
	if _, err := e.svc.Complete(ctx, "google", cb.Query().Get("state"), cb.Query().Get("code"), cb.Query().Get("error")); code(err) != oauth.CodeDenied {
		t.Fatalf("denied: %v", err)
	}
	e.fake.Set(func(s *oauthtest.Server) { s.Deny = false })

	// PKCE: the exchange fails without the verifier of the challenge.
	authURL, state, _ = e.svc.Start(ctx, "google", oauth.StartRequest{UserID: e.user})
	cb = e.fake.Authorize(t, authURL)
	if _, err := e.pool.Exec(ctx, `UPDATE oauth_states SET code_verifier = 'wrong-verifier-wrong-verifier-wrong-verifier'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Complete(ctx, "google", state, cb.Query().Get("code"), ""); code(err) != oauth.CodeExchangeFailed {
		t.Fatalf("wrong verifier: %v", err)
	}
	// ... and the provider refuses an authorization request without PKCE.
	u, _ := url.Parse(authURL)
	q := u.Query()
	q.Del("code_challenge")
	q.Del("code_challenge_method")
	u.RawQuery = q.Encode()
	if cb := e.fake.Authorize(t, u.String()); cb.Query().Get("error") != "invalid_request" || cb.Query().Get("code") != "" {
		t.Fatalf("authorize without PKCE: %v", cb)
	}

	// No address at all.
	e.fake.Set(func(s *oauthtest.Server) { s.IDTokenEmail, s.Email = false, "" })
	authURL, state, _ = e.svc.Start(ctx, "google", oauth.StartRequest{UserID: e.user})
	cb = e.fake.Authorize(t, authURL)
	if _, err := e.svc.Complete(ctx, "google", state, cb.Query().Get("code"), ""); code(err) != oauth.CodeNoEmail {
		t.Fatalf("no email: %v", err)
	}
}

func TestTokenSource(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	acc := e.account(e.signIn(oauth.StartRequest{UserID: e.user}))

	// Two connections, one refresh: the access token is cached.
	t1, err := e.svc.Token(ctx, acc)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := e.svc.Token(ctx, acc)
	if err != nil || t2 != t1 || e.fake.Refreshes() != 1 || !e.fake.ValidAccessToken(t1) {
		t.Fatalf("second token %q (first %q), refreshes %d, err %v", t2, t1, e.fake.Refreshes(), err)
	}

	// A token that expires within the margin is renewed before use.
	e.fake.Set(func(s *oauthtest.Server) { s.ExpiresIn = 30 })
	other := oauth.New(oauth.Options{Pool: e.pool, Keys: e.ring, PublicURL: "https://mail.silo.test", Providers: oauth.NewProviders(config.OAuthConfig{
		GoogleClientID: oauthtest.ClientID, GoogleClientSecret: oauthtest.ClientSecret, GoogleTokenURL: e.fake.URL + "/token",
	})})
	a, _ := other.Token(ctx, acc)
	b, _ := other.Token(ctx, acc)
	if a == b || e.fake.Refreshes() != 3 {
		t.Fatalf("short-lived tokens: %q %q, refreshes %d", a, b, e.fake.Refreshes())
	}
	e.fake.Set(func(s *oauthtest.Server) { s.ExpiresIn = 3600 })

	// A rotated refresh token is stored (encrypted) and used from then on.
	old := e.refreshToken(acc)
	e.fake.Set(func(s *oauthtest.Server) { s.RotateRefresh = true })
	if _, err := other.Token(ctx, acc); err != nil {
		t.Fatal(err)
	}
	rotated := e.refreshToken(acc)
	if rotated == old || rotated == "" {
		t.Fatalf("refresh token not rotated: %q", rotated)
	}
	e.fake.Set(func(s *oauthtest.Server) { s.RotateRefresh = false })
	// The first service notices the new secret and builds a new source.
	if tok, err := e.svc.Token(ctx, acc); err != nil || !e.fake.ValidAccessToken(tok) {
		t.Fatalf("token after rotation: %v", err)
	}

	// invalid_grant: re-authorization required.
	e.fake.Set(func(s *oauthtest.Server) { s.FailRefresh = true })
	fresh := oauth.New(oauth.Options{Pool: e.pool, Keys: e.ring, Providers: oauth.NewProviders(config.OAuthConfig{
		GoogleClientID: oauthtest.ClientID, GoogleClientSecret: oauthtest.ClientSecret, GoogleTokenURL: e.fake.URL + "/token",
	})})
	if _, err := fresh.Token(ctx, acc); !errors.Is(err, oauth.ErrReauthRequired) {
		t.Fatalf("invalid_grant: %v", err)
	}

	// Not an OAuth account.
	var pw int64
	if err := e.pool.QueryRow(ctx, `
INSERT INTO accounts (user_id, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, secret_enc)
VALUES ($1, 'p@silo.test', 'h', 993, 'implicit', 'h', 465, 'implicit', 'p', '\x00') RETURNING id`, e.user).Scan(&pw); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Token(ctx, pw); err == nil {
		t.Fatal("password account got a token")
	}
}
