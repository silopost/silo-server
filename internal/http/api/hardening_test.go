package api_test

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/accounts"
	"github.com/silopost/silo-server/internal/admin"
	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/events"
	siloHTTP "github.com/silopost/silo-server/internal/http"
	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/mailview"
	"github.com/silopost/silo-server/internal/ops"
	silosync "github.com/silopost/silo-server/internal/sync"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

// syncBuf is a log sink safe for concurrent writers.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// hardEnv is the API behind the full router (RealIP, security headers, rate
// limits), with an events hub, a short SSE heartbeat and a captured log.
type hardEnv struct {
	*env
	h    http.Handler
	srv  *httptest.Server
	logs *syncBuf
}

func newHardEnv(t *testing.T, maxStreams int) *hardEnv {
	t.Helper()
	e := newEnv(t)
	logs := &syncBuf{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	hub := events.New(events.Options{Pool: e.pool, Logger: logger, PollInterval: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = hub.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	authSvc := auth.New(auth.Options{Pool: e.pool, Keys: e.ring, Config: e.cfg, Logger: logger,
		MinLoginDuration: 20 * time.Millisecond, LoginLimit: auth.Limit{Burst: 1000, Every: time.Second}})
	h := siloHTTP.NewRouter(siloHTTP.Deps{
		Logger: logger,
		// serve() requests come from 192.0.2.1 (httptest), which may then
		// name the client in X-Forwarded-For.
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")},
		API: api.Options{
			Auth: authSvc, Admin: admin.New(e.pool, authSvc),
			Accounts: accounts.New(accounts.Options{Pool: e.pool, Keys: e.ring, Config: e.cfg}),
			Sync:     silosync.NewService(e.pool), Ops: ops.NewService(e.pool, 10*time.Second),
			View: mailview.New(e.pool, nil, nil), Drafts: e.drafts, Events: hub,
			SSEHeartbeat: 200 * time.Millisecond, PublicURL: publicURL, Logger: logger, MaxStreamsPerUser: maxStreams,
		},
	})
	he := &hardEnv{env: e, h: h, logs: logs}
	he.srv = httptest.NewServer(h)
	t.Cleanup(he.srv.Close)
	return he
}

func TestSecurityLogAndHeaders(t *testing.T) {
	he := newHardEnv(t, 0)
	xff := map[string]string{"X-Forwarded-For": "203.0.113.7"}

	r := serve(he.h, req{method: "POST", path: "/session", body: map[string]string{"email": imaptest.User, "password": "wrong-password-123"}, headers: xff})
	if r.Code != http.StatusUnauthorized || r.code() != "invalid_credentials" {
		t.Fatalf("wrong password: %d %s", r.Code, r.Body)
	}
	if want := "event=auth_failed ip=203.0.113.7 path=/api/v1/session login=s***"; !strings.Contains(he.logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, he.logs)
	}

	l := he.login(he.h, imaptest.User, imaptest.Pass)

	// JSON answers with user data (and their problems): not cacheable, not
	// frameable. (Before the first sync INBOX is still unknown: 404.)
	for _, path := range []string{"/session", "/threads?labelId=INBOX"} {
		r = serve(he.h, req{method: "GET", path: path, cookie: l.cookie})
		checkHeaders(t, path, r.Header())
	}

	// A cookie request without the CSRF token.
	r = serve(he.h, req{method: "DELETE", path: "/session", cookie: l.cookie})
	if r.Code != http.StatusForbidden || r.code() != "csrf_failed" {
		t.Fatalf("no csrf: %d %s", r.Code, r.Body)
	}
	if want := "event=csrf_rejected ip=192.0.2.1 path=/api/v1/session login=s***@silo.test"; !strings.Contains(he.logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, he.logs)
	}

	// A non-admin on the admin API.
	if _, err := he.pool.Exec(context.Background(), `UPDATE users SET is_admin = false`); err != nil {
		t.Fatal(err)
	}
	r = serve(he.h, req{method: "GET", path: "/admin/users", cookie: l.cookie})
	if r.Code != http.StatusForbidden {
		t.Errorf("admin API as a user: %d", r.Code)
	} else if !strings.Contains(he.logs.String(), "event=forbidden ip=192.0.2.1 path=/api/v1/admin/users") {
		t.Errorf("forbidden not logged:\n%s", he.logs)
	}

	// Secrets never reach the log.
	logs := he.logs.String()
	for _, secret := range []string{imaptest.Pass, "wrong-password-123", l.cookie.Value, l.csrf} {
		if strings.Contains(logs, secret) {
			t.Errorf("log contains a secret %q", secret)
		}
	}
}

func TestEventStreamLimit(t *testing.T) {
	he := newHardEnv(t, 2)
	l := he.login(he.h, imaptest.User, imaptest.Pass)
	s1 := openSSE(t, he.srv.URL, l, "")
	openSSE(t, he.srv.URL, l, "")

	third := func() *http.Response {
		rq, _ := http.NewRequest(http.MethodGet, he.srv.URL+"/api/v1/events", nil)
		rq.AddCookie(l.cookie)
		res, err := http.DefaultClient.Do(rq)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// Over the cap: 200 text/event-stream with one stream.limit event, then
	// the end of the stream (an EventSource cannot see a 429).
	res := third()
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := "retry: 30000\nevent: stream.limit\ndata: {\"retryAfter\":30}\n\n"
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") || string(body) != want {
		t.Fatalf("third stream: %d %v %q", res.StatusCode, res.Header, body)
	}
	if !strings.Contains(he.logs.String(), "event=rate_limited ip=127.0.0.1 path=/api/v1/events login=s***@silo.test") {
		t.Errorf("refused stream not logged:\n%s", he.logs)
	}
	// Closing a stream frees its slot: the next one is a real stream.
	s1.cancel()
	waitFor(t, 3*time.Second, "a free stream slot", func() bool {
		res := third()
		defer res.Body.Close()
		line, _ := bufio.NewReader(res.Body).ReadString('\n')
		return res.StatusCode == http.StatusOK && line == ": connected\n"
	})
}

func TestEventStreamEndsWithSession(t *testing.T) {
	he := newHardEnv(t, 0)
	expired := func(t *testing.T, s *sseStream, start time.Time) {
		t.Helper()
		e := s.wait(t, 2*time.Second, "session.expired", ofType("session.expired"))
		if e.data != "{}" {
			t.Errorf("data = %q", e.data)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("session.expired after %v", d)
		}
		select {
		case _, ok := <-s.events:
			if ok {
				t.Error("event after session.expired")
			}
		case <-time.After(time.Second):
			t.Error("stream not closed after session.expired")
		}
	}

	t.Run("logout", func(t *testing.T) {
		l := he.login(he.h, imaptest.User, imaptest.Pass)
		s := openSSE(t, he.srv.URL, l, "")
		start := time.Now()
		if r := serve(he.h, req{method: "DELETE", path: "/session", cookie: l.cookie, csrf: l.csrf}); r.Code != http.StatusNoContent {
			t.Fatalf("logout: %d %s", r.Code, r.Body)
		}
		expired(t, s, start)
	})

	t.Run("revoked from another session", func(t *testing.T) {
		la := he.login(he.h, imaptest.User, imaptest.Pass)
		lb := he.login(he.h, imaptest.User, imaptest.Pass)
		s := openSSE(t, he.srv.URL, lb, "")
		var list struct {
			Sessions []api.SessionInfo `json:"sessions"`
		}
		serve(he.h, req{method: "GET", path: "/sessions", cookie: lb.cookie}).json(&list)
		var id string
		for _, si := range list.Sessions {
			if si.Current {
				id = si.ID
			}
		}
		start := time.Now()
		if r := serve(he.h, req{method: "DELETE", path: "/sessions/" + id, cookie: la.cookie, csrf: la.csrf}); r.Code != http.StatusNoContent {
			t.Fatalf("revoke: %d %s", r.Code, r.Body)
		}
		expired(t, s, start)
	})

	t.Run("user disabled", func(t *testing.T) {
		l := he.login(he.h, imaptest.User, imaptest.Pass)
		s := openSSE(t, he.srv.URL, l, "")
		// A live session keeps its stream across heartbeats.
		time.Sleep(500 * time.Millisecond)
		select {
		case e, ok := <-s.events:
			t.Fatalf("unexpected event %+v (open %v)", e, ok)
		default:
		}
		start := time.Now()
		if _, err := he.pool.Exec(context.Background(), `UPDATE users SET disabled_at = now() WHERE email = 'silo@silo.test'`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = he.pool.Exec(context.Background(), `UPDATE users SET disabled_at = NULL WHERE email = 'silo@silo.test'`)
		})
		expired(t, s, start)
	})
}

// checkHeaders asserts the security headers of a JSON (or problem) answer.
func checkHeaders(t *testing.T, what string, h http.Header) {
	t.Helper()
	for k, want := range map[string]string{
		"Cache-Control": "no-store", "Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
		"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer",
		"Cross-Origin-Resource-Policy": "same-origin", "Permissions-Policy": "camera=(), microphone=(), geolocation=()",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s: %s = %q, want %q", what, k, got, want)
		}
	}
}
