package sync_test

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/blob"
	"github.com/silopost/silo-server/internal/imapx"
	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/store"
	silosync "github.com/silopost/silo-server/internal/sync"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
	"github.com/silopost/silo-server/internal/testutil/pgtest"
)

// One Postgres container for the package; every test has its own Silo user
// and account, and (because a Dovecot container holds one mailbox) usually
// its own Dovecot.
var (
	testPool    *pgxpool.Pool
	dockerError error
	seq         atomic.Int64
	keyring     *secrets.Keyring
)

func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	var err error
	if keyring, err = secrets.NewKeyring(bytes.Repeat([]byte{9}, 32), nil); err != nil {
		panic(err)
	}
	if !testing.Short() {
		ctx := context.Background()
		url, terminate, err := pgtest.Run(ctx)
		if err != nil {
			dockerError = err
		} else {
			defer terminate()
			if err := store.MigrateUp(ctx, url); err != nil {
				fmt.Fprintln(os.Stderr, "migrate:", err)
				return 1
			}
			if testPool, err = store.Connect(ctx, url); err != nil {
				fmt.Fprintln(os.Stderr, "connect:", err)
				return 1
			}
			defer testPool.Close()
		}
	}
	return m.Run()
}

func needDB(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped with -short")
	}
	if testPool == nil {
		if os.Getenv("SILO_REQUIRE_DOCKER") == "true" {
			t.Fatalf("postgres container unavailable: %v", dockerError)
		}
		t.Skipf("docker unavailable: %v", dockerError)
	}
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// mailbox is a Dovecot container with the test account.
type mailbox struct {
	t          *testing.T
	host       string
	port       int
	user, pass string
}

func newMailbox(t *testing.T, p imaptest.Profile) *mailbox {
	t.Helper()
	needDB(t)
	addr, user, pass := imaptest.Start(t, p)
	host, ps, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(ps)
	return &mailbox{t: t, host: host, port: port, user: user, pass: pass}
}

// client opens an independent IMAP session ("another client", Thunderbird).
func (mb *mailbox) client() *imapclient.Client {
	mb.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cl, err := imapx.Dial(ctx, mb.host, mb.port, imapx.TLSNone, false)
	must(mb.t, err)
	mb.t.Cleanup(func() { cl.Close() })
	must(mb.t, cl.Login(ctx, mb.user, mb.pass))
	return cl.IMAP()
}

func (mb *mailbox) create(names ...string) {
	mb.t.Helper()
	c := mb.client()
	for _, n := range names {
		must(mb.t, c.Create(n, nil).Wait())
	}
	_ = c.Logout().Wait()
}

func appendMsg(t testing.TB, c *imapclient.Client, mailbox string, raw []byte, flags []imap.Flag, when time.Time) imap.UID {
	t.Helper()
	cmd := c.Append(mailbox, int64(len(raw)), &imap.AppendOptions{Flags: flags, Time: when})
	if _, err := cmd.Write(raw); err != nil {
		t.Fatal(err)
	}
	must(t, cmd.Close())
	data, err := cmd.Wait()
	if err != nil {
		t.Fatalf("APPEND %s: %v", mailbox, err)
	}
	return data.UID
}

// msg builds a message; reply > 0 makes it a reply to message reply.
func msg(id, reply int, subject string, date time.Time) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: Sender %d <s%d@silo.test>\r\nTo: silo@silo.test\r\n", id%17, id%17)
	fmt.Fprintf(&b, "Subject: %s\r\nMessage-ID: <m%d@silo.test>\r\nDate: %s\r\n", subject, id, date.Format(time.RFC1123Z))
	if reply > 0 {
		fmt.Fprintf(&b, "In-Reply-To: <m%d@silo.test>\r\nReferences: <m%d@silo.test>\r\n", reply, reply)
	}
	fmt.Fprintf(&b, "Content-Type: text/plain; charset=utf-8\r\n\r\nBody of message %d.\r\nLine two.\r\n", id)
	return b.Bytes()
}

// spec of a generated mailbox.
type spec struct {
	folder string
	count  int
}

// populate appends messages spread over the last 60 days (half within the
// phase A window), with one reply per five messages, one connection per
// folder in parallel. It returns the number of messages per folder younger
// than 30 days.
func (mb *mailbox) populate(specs ...spec) map[string]int {
	mb.t.Helper()
	now := time.Now()
	recent := map[string]int{}
	var mu stdsync.Mutex
	var wg stdsync.WaitGroup
	base := 0
	for _, s := range specs {
		first := base
		base += s.count
		wg.Add(1)
		go func(s spec, first int) {
			defer wg.Done()
			c := mb.client()
			for i := 0; i < s.count; i++ {
				id := first + i + 1
				age := time.Duration(float64(60*24*time.Hour) * float64(s.count-i) / float64(s.count+1))
				when := now.Add(-age).Truncate(time.Second)
				reply := 0
				if i%5 == 4 {
					reply = id - 1
				}
				var flags []imap.Flag
				if i%3 == 0 {
					flags = append(flags, imap.FlagSeen)
				}
				subject := fmt.Sprintf("Message %d", id)
				if reply > 0 {
					subject = fmt.Sprintf("Re: Message %d", reply)
				}
				appendMsg(mb.t, c, s.folder, msg(id, reply, subject, when), flags, when)
				if age < 30*24*time.Hour-time.Hour {
					mu.Lock()
					recent[s.folder]++
					mu.Unlock()
				}
			}
			_ = c.Logout().Wait()
		}(s, first)
	}
	wg.Wait()
	return recent
}

// account is a Silo user + account pointed at a mailbox.
type account struct {
	t      *testing.T
	id     int64
	userID int64
	deps   *silosync.Deps
}

func (mb *mailbox) account(pass string) *account {
	return newAccount(mb.t, mb.host, mb.port, mb.user, pass)
}

func newAccount(t *testing.T, host string, port int, user, pass string) *account {
	t.Helper()
	ctx := context.Background()
	n := seq.Add(1)
	a := &account{t: t}
	must(t, testPool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`, fmt.Sprintf("u%d-%d@silo.test", n, time.Now().UnixNano())).Scan(&a.userID))
	must(t, testPool.QueryRow(ctx, `
INSERT INTO accounts (user_id, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, secret_enc)
VALUES ($1, 'silo@silo.test', $2, $3, 'none', $2, 25, 'none', $4, '\x00') RETURNING id`, a.userID, host, port, user).Scan(&a.id))
	a.setPassword(pass)
	blobs, err := blob.NewFSStore(t.TempDir())
	must(t, err)
	a.deps = &silosync.Deps{
		Pool: testPool, Keys: keyring, Blobs: blobs,
		Logger:   slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: testLogLevel()})),
		Settings: silosync.Settings{MaxConnections: 3, InitialConcurrency: 2, PollInterval: time.Minute, BackfillRate: 50 << 20, MaxMessageBytes: 50 << 20},
		Backoff:  []time.Duration{100 * time.Millisecond, 200 * time.Millisecond},
	}
	return a
}

func (a *account) setPassword(pass string) {
	sealed, err := keyring.Encrypt([]byte(pass), []byte(strconv.FormatInt(a.id, 10)))
	must(a.t, err)
	_, err = testPool.Exec(context.Background(), `UPDATE accounts SET secret_enc = $2 WHERE id = $1`, a.id, sealed)
	must(a.t, err)
}

// testWriter sends worker logs to t.Log (visible with -v or on failure).
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	defer func() { _ = recover() }() // a log line after the test ended
	w.t.Log(string(bytes.TrimRight(p, "\n")))
	return len(p), nil
}

// running is a started AccountWorker.
type running struct {
	w      *silosync.AccountWorker
	cancel context.CancelFunc
	done   chan struct{}
}

func (a *account) start() *running {
	a.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{w: silosync.NewAccountWorker(a.deps, a.id, nil), cancel: cancel, done: make(chan struct{})}
	go func() { defer close(r.done); _ = r.w.Run(ctx) }()
	a.t.Cleanup(r.stop)
	return r
}

func (r *running) stop() {
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(30 * time.Second):
		panic("account worker did not stop within 30s")
	}
}

func (a *account) int(q string, args ...any) int {
	a.t.Helper()
	var n int
	must(a.t, testPool.QueryRow(context.Background(), q, args...).Scan(&n))
	return n
}

func (a *account) state() string {
	var s string
	must(a.t, testPool.QueryRow(context.Background(), `SELECT sync_state FROM accounts WHERE id = $1`, a.id).Scan(&s))
	return s
}

func (a *account) folderID(name string) int64 {
	a.t.Helper()
	var id int64
	must(a.t, testPool.QueryRow(context.Background(), `SELECT id FROM mailbox_folders WHERE account_id = $1 AND name = $2`, a.id, name).Scan(&id))
	return id
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t testing.TB, timeout time.Duration, what string, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < timeout {
		if cond() {
			return time.Since(start)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timeout after %s waiting for %s", timeout, what)
	return 0
}

func (a *account) waitState(state string, timeout time.Duration) {
	a.t.Helper()
	waitFor(a.t, timeout, "state "+state, func() bool { return a.state() == state })
}

func (a *account) waitInitialDone(timeout time.Duration) time.Duration {
	a.t.Helper()
	return waitFor(a.t, timeout, "initial sync done", func() bool {
		return a.int(`SELECT count(*) FROM accounts WHERE id = $1 AND initial_sync_done_at IS NOT NULL`, a.id) == 1
	})
}

// counts summarises the mirror of an account for comparisons.
type counts struct {
	Messages, Locations, Labels, UserLabels, Threads, Full, DistinctMsgID int
}

func (a *account) counts() counts {
	var c counts
	must(a.t, testPool.QueryRow(context.Background(), `
SELECT (SELECT count(*) FROM messages WHERE account_id = $1),
       (SELECT count(*) FROM message_locations WHERE account_id = $1),
       (SELECT count(*) FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE l.account_id = $1),
       (SELECT count(*) FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE l.account_id = $1 AND l.type = 'user'),
       (SELECT count(*) FROM threads WHERE account_id = $1),
       (SELECT count(*) FROM messages WHERE account_id = $1 AND body_state = 'full'),
       (SELECT count(DISTINCT message_id_hdr) FROM messages WHERE account_id = $1)`, a.id).
		Scan(&c.Messages, &c.Locations, &c.Labels, &c.UserLabels, &c.Threads, &c.Full, &c.DistinctMsgID))
	return c
}

// proxy is a TCP forwarder that can refuse connections ("server down") and
// counts the connections it accepted.
type proxy struct {
	t       *testing.T
	target  string
	ln      net.Listener
	addr    string
	mu      stdsync.Mutex
	up      bool
	conns   []net.Conn
	accepts atomic.Int64
}

func newProxy(t *testing.T, target string) *proxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	p := &proxy{t: t, target: target, ln: ln, addr: ln.Addr().String(), up: true}
	t.Cleanup(func() { ln.Close(); p.setUp(false) })
	go p.serve()
	return p
}

func (p *proxy) port() int { return p.ln.Addr().(*net.TCPAddr).Port }

func (p *proxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		up := p.up
		if up {
			p.conns = append(p.conns, c)
		}
		p.mu.Unlock()
		if !up {
			c.Close()
			continue
		}
		p.accepts.Add(1)
		go func() {
			b, err := net.DialTimeout("tcp", p.target, 5*time.Second)
			if err != nil {
				c.Close()
				return
			}
			p.mu.Lock()
			p.conns = append(p.conns, b)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(b, c); b.Close() }()
			_, _ = io.Copy(c, b)
			c.Close()
		}()
	}
}

// setUp switches the proxy; going down drops every open connection.
func (p *proxy) setUp(up bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.up = up
	if !up {
		for _, c := range p.conns {
			c.Close()
		}
		p.conns = nil
	}
}

func testLogLevel() slog.Level {
	if os.Getenv("SILO_SYNC_TEST_DEBUG") != "" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}
