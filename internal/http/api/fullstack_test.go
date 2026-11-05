package api_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
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
	"github.com/silopost/silo-server/internal/ops"
	silosync "github.com/silopost/silo-server/internal/sync"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
	"github.com/silopost/silo-server/internal/testutil/smtptest"
	"github.com/silopost/silo-server/internal/worker"
)

// fullStack is the whole server as cmd/silo-server wires it (sync engine,
// job worker with the sync, write-back and send jobs, event hub, drafts)
// over one Postgres and the Dovecot of newEnv, behind a real HTTP server
// (SSE). The default SMTP server records what it accepts for imaptest.User.
type fullStack struct {
	*env
	h    http.Handler
	srv  *httptest.Server
	hub  *events.Hub
	smtp *smtptest.Server
}

func newFullStack(t *testing.T, tweak func(*config.Config)) *fullStack {
	t.Helper()
	e := newEnv(t)
	smtp := smtptest.StartServer(t, imaptest.User, imaptest.Pass, 0)
	e.cfg.DefaultSMTPHost, e.cfg.DefaultSMTPPort = smtp.Host, smtp.Port
	if tweak != nil {
		tweak(&e.cfg)
	}
	blobs, err := blob.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	jobs := worker.New(worker.Options{Pool: e.pool, Logger: logger, Concurrency: 4, PollInterval: 100 * time.Millisecond})
	mgr := silosync.NewManager(silosync.Deps{
		Pool: e.pool, Keys: e.ring, Blobs: blobs, Logger: logger,
		Settings: silosync.Settings{MaxConnections: 3, InitialConcurrency: 2, PollInterval: time.Minute, BackfillRate: 50 << 20,
			MaxMessageBytes: 50 << 20, InsecureTLS: true},
		Backoff: []time.Duration{100 * time.Millisecond, 200 * time.Millisecond},
	})
	mgr.RegisterJobs(jobs)
	view := mailview.New(e.pool, blobs, mgr)
	dr := drafts.New(drafts.Options{Pool: e.pool, Blobs: blobs, MaxUploadBytes: testMaxUpload, SyncDelay: 300 * time.Millisecond,
		Keys: e.ring, InsecureTransport: true, SendRetry: []time.Duration{time.Second}, Logger: logger,
		OpenRaw: func(ctx context.Context, userID, messageID int64) (io.ReadCloser, error) {
			r, err := view.OpenRaw(ctx, userID, messageID)
			if err != nil {
				return nil, err
			}
			return r.Body, nil
		}})
	dr.RegisterJobs(jobs)
	hub := events.New(events.Options{Pool: e.pool, Logger: logger, PollInterval: time.Minute})
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
	r := chi.NewRouter()
	api.Mount(r, api.Options{
		Auth: auth.New(auth.Options{Pool: e.pool, Keys: e.ring, Config: e.cfg, MinLoginDuration: 20 * time.Millisecond,
			LoginLimit: auth.Limit{Burst: 1000, Every: time.Second}}),
		Accounts: accounts.New(accounts.Options{Pool: e.pool, Keys: e.ring, Config: e.cfg}),
		Sync:     silosync.NewService(e.pool), Ops: ops.NewService(e.pool, 30*time.Second),
		View: view, Drafts: dr, Events: hub, PublicURL: publicURL,
	})
	s := &fullStack{env: e, h: r, hub: hub, smtp: smtp}
	s.srv = httptest.NewServer(r)
	t.Cleanup(s.srv.Close)
	return s
}

// as sends a request with the session of l.
func as(l login, q req) req {
	if l.bearer != "" {
		q.headers = bearerHdr(l.bearer)
	} else {
		q.cookie, q.csrf = l.cookie, l.csrf
	}
	return q
}

// waitLive waits until every account of l has finished its initial sync.
func (s *fullStack) waitLive(t *testing.T, l login, n int) []api.Account {
	t.Helper()
	var list struct {
		Accounts []api.Account `json:"accounts"`
	}
	waitFor(t, 90*time.Second, "accounts live", func() bool {
		serve(s.h, as(l, req{method: "GET", path: "/accounts"})).json(&list)
		if len(list.Accounts) != n {
			return false
		}
		for _, a := range list.Accounts {
			id := idOf("acc_", a.ID)
			if a.State != "live" || s.count(`SELECT count(*) FROM accounts WHERE id = $1 AND initial_sync_done_at IS NOT NULL`, id) != 1 {
				return false
			}
		}
		return true
	})
	return list.Accounts
}

// imapUser is a direct IMAP session to a test mailbox (to seed or inspect it).
func imapUser(t *testing.T, addr, user, pass string) *imapclient.Client {
	t.Helper()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cl, err := imapx.Dial(ctx, host, port, imapx.TLSNone, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	if err := cl.Login(ctx, user, pass); err != nil {
		t.Fatal(err)
	}
	return cl.IMAP()
}

// appendMsg APPENDs a small plain message with the given Message-ID.
func appendMsg(t *testing.T, c *imapclient.Client, folder, msgID, from, to, subject string) {
	t.Helper()
	appendRaw(t, c, folder, []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: <%s>\r\nDate: %s\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n\r\nBody of %s\r\n", from, to, subject, msgID,
		time.Now().Add(-time.Hour).Format(time.RFC1123Z), subject)))
}

// appendRaw APPENDs a message to a folder.
func appendRaw(t *testing.T, c *imapclient.Client, folder string, raw []byte) {
	t.Helper()
	cmd := c.Append(folder, int64(len(raw)), &imap.AppendOptions{Time: time.Now().Add(-time.Hour)})
	if _, err := cmd.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.Wait(); err != nil {
		t.Fatalf("APPEND %s: %v", folder, err)
	}
}

// folderCount is the number of messages in a folder, as the server sees it.
func folderCount(t *testing.T, c *imapclient.Client, folder string) int {
	t.Helper()
	st, err := c.Status(folder, &imap.StatusOptions{NumMessages: true}).Wait()
	if err != nil {
		t.Fatalf("STATUS %s: %v", folder, err)
	}
	return int(*st.NumMessages)
}
