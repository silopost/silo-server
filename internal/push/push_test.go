package push

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/store"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
	"github.com/silopost/silo-server/internal/testutil/pgtest"
	"github.com/silopost/silo-server/internal/worker"
)

// One Postgres container for the package; every test has its own user.
var (
	testPool    *pgxpool.Pool
	dockerError error
	seq         atomic.Int64
)

func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
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

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// fakeExpo is the Expo Push Service: it records the messages it gets and
// answers with the tickets of ticket (ok with a fresh id when nil), or with
// the HTTP status of fail while fail > 0 requests remain.
type fakeExpo struct {
	srv      *httptest.Server
	mu       sync.Mutex
	sends    [][]map[string]any // decoded messages per send request
	raw      []string           // raw send bodies
	receipts [][]string
	ticket   func(token string) map[string]any
	receipt  func(id string) map[string]any
	fail     int
	failCode int
	n        int
}

func newFakeExpo(t *testing.T) *fakeExpo {
	f := &fakeExpo{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/send":
			f.raw = append(f.raw, string(body))
			if f.fail > 0 {
				f.fail--
				w.WriteHeader(f.failCode)
				_, _ = w.Write([]byte(`{"errors":[{"code":"INTERNAL","message":"try later"}]}`))
				return
			}
			var msgs []map[string]any
			if err := json.Unmarshal(body, &msgs); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.sends = append(f.sends, msgs)
			data := make([]map[string]any, len(msgs))
			for i, m := range msgs {
				if f.ticket != nil {
					data[i] = f.ticket(m["to"].(string))
				} else {
					f.n++
					data[i] = map[string]any{"status": "ok", "id": fmt.Sprintf("r%d", f.n)}
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		case "/receipts":
			var req struct{ IDs []string }
			_ = json.Unmarshal(body, &req)
			f.receipts = append(f.receipts, req.IDs)
			out := map[string]any{}
			for _, id := range req.IDs {
				if f.receipt != nil {
					if r := f.receipt(id); r != nil {
						out[id] = r
					}
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeExpo) sent() [][]map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]map[string]any(nil), f.sends...)
}

func (f *fakeExpo) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.raw)
}

// env is a user with one account (INBOX, Archive), a bearer session with a
// registered device and a push service against a fake Expo.
type env struct {
	t        *testing.T
	ctx      context.Context
	expo     *fakeExpo
	svc      *Service
	userID   int64
	account  int64
	inbox    int64
	archive  int64
	session  sqlcgen.Session
	device   sqlcgen.PushDevice
	token    string
	nextUID  uint32
	msgCount int
}

func newEnv(t *testing.T) *env {
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
	n := seq.Add(1)
	e := &env{t: t, ctx: context.Background(), expo: newFakeExpo(t)}
	e.svc = New(Options{Pool: testPool, Config: config.PushConfig{
		Enabled: true, ExpoSendURL: e.expo.srv.URL + "/send", ExpoReceiptsURL: e.expo.srv.URL + "/receipts",
	}})
	must(t, testPool.QueryRow(e.ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`, fmt.Sprintf("push%d@silo.test", n)).Scan(&e.userID))
	e.account = e.newAccount(fmt.Sprintf("me%d@silo.test", n))
	e.session = e.newSession()
	e.token = fmt.Sprintf("ExponentPushToken[tok%d]", n)
	e.device = e.register(e.session, e.token, nil)
	return e
}

func (e *env) newAccount(email string) int64 {
	e.t.Helper()
	var id int64
	must(e.t, testPool.QueryRow(e.ctx, `
INSERT INTO accounts (user_id, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, secret_enc)
VALUES ($1, $2, 'imap.test', 993, 'implicit', 'smtp.test', 465, 'implicit', $3, '\x00') RETURNING id`, e.userID, email, email).Scan(&id))
	for _, f := range []struct{ name, role string }{{"INBOX", "inbox"}, {"Archive", "archive"}} {
		var fid int64
		must(e.t, testPool.QueryRow(e.ctx, `
INSERT INTO mailbox_folders (account_id, user_id, name, name_raw, role, uidvalidity) VALUES ($1, $2, $3, $3, $4, 1) RETURNING id`,
			id, e.userID, f.name, f.role).Scan(&fid))
		if e.inbox == 0 && f.role == "inbox" {
			e.inbox = fid
		} else if e.archive == 0 && f.role == "archive" {
			e.archive = fid
		}
	}
	must(e.t, pgx.BeginFunc(e.ctx, testPool, func(tx pgx.Tx) error { return mail.EnsureSystemLabels(e.ctx, tx, id) }))
	return id
}

func (e *env) newSession() sqlcgen.Session {
	e.t.Helper()
	sum := sha256.Sum256([]byte(fmt.Sprintf("tok-%d-%d", e.userID, seq.Add(1))))
	var s sqlcgen.Session
	must(e.t, testPool.QueryRow(e.ctx, `
INSERT INTO sessions (user_id, token_hash, csrf_token, expires_at, kind) VALUES ($1, $2, 'x', now() + interval '1 hour', 'bearer')
RETURNING id, user_id, kind`, e.userID, sum[:]).Scan(&s.ID, &s.UserID, &s.Kind))
	return s
}

func (e *env) register(s sqlcgen.Session, token string, accounts []int64) sqlcgen.PushDevice {
	e.t.Helper()
	d, err := e.svc.Register(e.ctx, s, Registration{Provider: "expo", Token: token, AccountIDs: accounts})
	must(e.t, err)
	return d
}

// deliver ingests one new INBOX message of the account, as the incremental
// sync (notify) or the initial sync does.
func (e *env) deliver(account, folder int64, from string, notify bool, flags ...string) int64 {
	e.t.Helper()
	e.nextUID++
	e.msgCount++
	hdr := fmt.Sprintf("From: Alice Sender <%s>\r\nTo: me@silo.test\r\nSubject: Secret subject %d\r\nMessage-ID: <m%d-%d@silo.test>\r\nDate: %s\r\n\r\n",
		from, e.msgCount, e.userID, e.msgCount, time.Now().Format(time.RFC1123Z))
	var id int64
	must(e.t, pgx.BeginFunc(e.ctx, testPool, func(tx pgx.Tx) error {
		res, err := mail.IngestBatchWith(e.ctx, tx, account, folder, []mail.FetchedMessage{{
			UID: e.nextUID, Size: 200, InternalDate: time.Now(), Header: []byte(hdr), Flags: flags,
			TextSnippet: []byte("Confidential snippet text"),
		}}, mail.IngestOptions{Notify: notify})
		if err == nil {
			id = res.Messages[0].MessageID
		}
		return err
	}))
	return id
}

// jobs returns the pending jobs of a kind.
func (e *env) jobs(kind string) []worker.Job {
	e.t.Helper()
	rows, err := testPool.Query(e.ctx, `SELECT id, kind, payload, run_at FROM jobs WHERE kind = $1 AND status = 'pending' ORDER BY id`, kind)
	must(e.t, err)
	defer rows.Close()
	var out []worker.Job
	for rows.Next() {
		var j worker.Job
		must(e.t, rows.Scan(&j.ID, &j.Kind, &j.Payload, &j.RunAt))
		out = append(out, j)
	}
	must(e.t, rows.Err())
	return out
}

// notifyJobs returns this user's pending push.notify jobs.
func (e *env) notifyJobs() []worker.Job {
	var out []worker.Job
	for _, j := range e.jobs(mail.JobPushNotify) {
		var p mail.PushNotifyPayload
		must(e.t, json.Unmarshal(j.Payload, &p))
		var owner int64
		must(e.t, testPool.QueryRow(e.ctx, `SELECT user_id FROM accounts WHERE id = $1`, p.AccountID).Scan(&owner))
		if owner == e.userID {
			out = append(out, j)
		}
	}
	return out
}

// runNotify runs this user's push.notify jobs as the worker would (and
// deletes them on success).
func (e *env) runNotify() error {
	e.t.Helper()
	for _, j := range e.notifyJobs() {
		if err := e.svc.runNotify(e.ctx, j); err != nil {
			return err
		}
		_, err := testPool.Exec(e.ctx, `DELETE FROM jobs WHERE id = $1`, j.ID)
		must(e.t, err)
	}
	return nil
}

func (e *env) deviceExists(id int64) bool {
	var n int
	must(e.t, testPool.QueryRow(e.ctx, `SELECT count(*) FROM push_devices WHERE id = $1`, id).Scan(&n))
	return n == 1
}

func (e *env) lastError(id int64) string {
	var s *string
	must(e.t, testPool.QueryRow(e.ctx, `SELECT last_error FROM push_devices WHERE id = $1`, id).Scan(&s))
	if s == nil {
		return ""
	}
	return *s
}

func TestNotifyNewInboxMessage(t *testing.T) {
	e := newEnv(t)
	before := time.Now()
	msg := e.deliver(e.account, e.inbox, "alice@example.org", true)
	after := time.Now()
	jobs := e.notifyJobs()
	if len(jobs) != 1 {
		t.Fatalf("push.notify jobs = %d, want 1", len(jobs))
	}
	// run_at is PushDelay after the delivery, whenever this check runs (a slow
	// runner must not fail it); one second of slack for the database clock.
	if at := jobs[0].RunAt.Time; at.Before(before.Add(mail.PushDelay-time.Second)) || at.After(after.Add(mail.PushDelay+time.Second)) {
		t.Errorf("job runs %v after the delivery started, want ~%v", at.Sub(before), mail.PushDelay)
	}
	must(t, e.runNotify())
	sends := e.expo.sent()
	if len(sends) != 1 || len(sends[0]) != 1 {
		t.Fatalf("send requests = %v, want one with one message", sends)
	}
	var thread int64
	must(t, testPool.QueryRow(e.ctx, `SELECT thread_id FROM messages WHERE id = $1`, msg).Scan(&thread))
	got := sends[0][0]
	acc := apiID("acc_", e.account)
	want := map[string]any{
		"to": e.token, "title": "New message", "channelId": "mail", "priority": "high", "sound": "default",
		"tag": "mail-" + acc, "collapseId": "mail-" + acc,
		"data": map[string]any{"v": float64(1), "type": "mail", "accountId": acc, "threadId": apiID("t_", thread), "count": float64(1)},
	}
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("message\n got %s\nwant %s", gotJSON, wantJSON)
	}
	// No content leaves the server: sender, subject, snippet, addresses.
	raw := e.expo.raw[0]
	for _, secret := range []string{"Alice", "alice@example.org", "Secret subject", "Confidential", "@silo.test"} {
		if strings.Contains(raw, secret) {
			t.Errorf("push request contains %q: %s", secret, raw)
		}
	}
	// The ticket is checked by push.receipts later; the device is fine.
	rj := e.jobs(JobReceipts)
	var rp receiptsPayload
	if len(rj) > 0 {
		must(t, json.Unmarshal(rj[len(rj)-1].Payload, &rp))
	}
	if len(rp.Tickets) != 1 || rp.Tickets[0].DeviceID != e.device.ID || rp.Tickets[0].ID == "" {
		t.Errorf("push.receipts not queued for the ticket: %+v", rp)
	} else if d := time.Until(rj[len(rj)-1].RunAt.Time); d < 14*time.Minute {
		t.Errorf("push.receipts runs in %v, want ~15m", d)
	}
	if le := e.lastError(e.device.ID); le != "" {
		t.Errorf("last_error = %q", le)
	}
}

func TestNotifyNothingToSend(t *testing.T) {
	t.Run("initial sync", func(t *testing.T) {
		e := newEnv(t)
		e.deliver(e.account, e.inbox, "alice@example.org", false)
		if n := len(e.notifyJobs()); n != 0 {
			t.Fatalf("push.notify after an initial sync batch: %d jobs", n)
		}
	})
	t.Run("not INBOX", func(t *testing.T) {
		e := newEnv(t)
		e.deliver(e.account, e.archive, "alice@example.org", true)
		if n := len(e.notifyJobs()); n != 0 {
			t.Fatalf("push.notify for new mail outside INBOX: %d jobs", n)
		}
	})
	t.Run("no device", func(t *testing.T) {
		e := newEnv(t)
		must(t, e.svc.Delete(e.ctx, e.userID, e.device.ID))
		e.deliver(e.account, e.inbox, "alice@example.org", true)
		if n := len(e.notifyJobs()); n != 0 {
			t.Fatalf("push.notify without devices: %d jobs", n)
		}
	})
	t.Run("account not in accountIds", func(t *testing.T) {
		e := newEnv(t)
		other := e.newAccount(fmt.Sprintf("other%d@silo.test", seq.Add(1)))
		e.register(e.session, e.token, []int64{other})
		e.deliver(e.account, e.inbox, "alice@example.org", true)
		if n := len(e.notifyJobs()); n != 0 {
			t.Fatalf("push.notify for an account the device does not want: %d jobs", n)
		}
		// Changed meanwhile (job queued while the account was wanted).
		e.register(e.session, e.token, nil)
		e.deliver(e.account, e.inbox, "alice@example.org", true)
		e.register(e.session, e.token, []int64{other})
		must(t, e.runNotify())
		if n := e.expo.requests(); n != 0 {
			t.Fatalf("pushed to a device without the account: %d requests", n)
		}
	})
	t.Run("read meanwhile", func(t *testing.T) {
		e := newEnv(t)
		e.deliver(e.account, e.inbox, "alice@example.org", true, `\Seen`)
		id := e.deliver(e.account, e.inbox, "alice@example.org", true)
		_, err := testPool.Exec(e.ctx, `UPDATE messages SET is_unread = false WHERE id = $1`, id)
		must(t, err)
		must(t, e.runNotify())
		if n := e.expo.requests(); n != 0 {
			t.Fatalf("pushed read mail: %d requests", n)
		}
	})
	t.Run("left INBOX meanwhile", func(t *testing.T) {
		e := newEnv(t)
		id := e.deliver(e.account, e.inbox, "alice@example.org", true)
		_, err := testPool.Exec(e.ctx, `DELETE FROM message_labels ml USING labels l WHERE ml.message_id = $1 AND l.id = ml.label_id AND l.system_key = 'INBOX'`, id)
		must(t, err)
		must(t, e.runNotify())
		if n := e.expo.requests(); n != 0 {
			t.Fatalf("pushed mail no longer in INBOX: %d requests", n)
		}
	})
	t.Run("own mail", func(t *testing.T) {
		e := newEnv(t)
		var self string
		must(t, testPool.QueryRow(e.ctx, `SELECT email FROM accounts WHERE id = $1`, e.account).Scan(&self))
		e.deliver(e.account, e.inbox, strings.ToUpper(self), true)
		must(t, e.runNotify())
		if n := e.expo.requests(); n != 0 {
			t.Fatalf("pushed the user's own mail: %d requests", n)
		}
	})
	t.Run("signed out", func(t *testing.T) {
		e := newEnv(t)
		e.deliver(e.account, e.inbox, "alice@example.org", true)
		_, err := testPool.Exec(e.ctx, `DELETE FROM sessions WHERE id = $1`, e.session.ID)
		must(t, err)
		if e.deviceExists(e.device.ID) {
			t.Fatal("device survived its session")
		}
		must(t, e.runNotify())
		if n := e.expo.requests(); n != 0 {
			t.Fatalf("pushed after sign-out: %d requests", n)
		}
	})
	t.Run("user disabled", func(t *testing.T) {
		e := newEnv(t)
		e.deliver(e.account, e.inbox, "alice@example.org", true)
		_, err := testPool.Exec(e.ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, e.userID)
		must(t, err)
		must(t, e.runNotify())
		if n := e.expo.requests(); n != 0 {
			t.Fatalf("pushed to a disabled user: %d requests", n)
		}
	})
	t.Run("push switched off", func(t *testing.T) {
		e := newEnv(t)
		e.deliver(e.account, e.inbox, "alice@example.org", true)
		e.svc.o.Config.Enabled = false
		must(t, e.runNotify())
		if n := e.expo.requests(); n != 0 {
			t.Fatalf("pushed with push off: %d requests", n)
		}
	})
}

func TestNotifyBurst(t *testing.T) {
	e := newEnv(t)
	_, err := testPool.Exec(e.ctx, `UPDATE users SET settings = settings || '{"language":"pl"}' WHERE id = $1`, e.userID)
	must(t, err)
	// A second device of the user (another phone) gets its own message.
	other := e.register(e.newSession(), "ExpoPushToken[second]", nil)
	for range 3 {
		e.deliver(e.account, e.inbox, "alice@example.org", true) // one transaction each
	}
	if n := len(e.notifyJobs()); n != 1 {
		t.Fatalf("push.notify jobs = %d, want 1 for the burst", n)
	}
	must(t, e.runNotify())
	sends := e.expo.sent()
	if len(sends) != 1 || len(sends[0]) != 2 {
		t.Fatalf("send requests = %v, want one with a message per device", sends)
	}
	for _, m := range sends[0] {
		if m["title"] != "3 nowe wiadomości" || m["data"].(map[string]any)["count"] != float64(3) {
			t.Errorf("message = %v, want 3 new messages in Polish", m)
		}
	}
	// A job that already started does not take new mail: a new job.
	e.deliver(e.account, e.inbox, "alice@example.org", true)
	_, err = testPool.Exec(e.ctx, `UPDATE jobs SET attempts = 1 WHERE kind = $1`, mail.JobPushNotify)
	must(t, err)
	e.deliver(e.account, e.inbox, "alice@example.org", true)
	if n := len(e.notifyJobs()); n != 2 {
		t.Fatalf("push.notify jobs = %d, want 2", n)
	}
	_ = other
}

func TestTitle(t *testing.T) {
	for _, c := range []struct {
		lang string
		n    int
		want string
	}{
		{"", 1, "New message"}, {"en", 2, "2 new messages"}, {"de", 1, "New message"},
		{"pl", 1, "Nowa wiadomość"}, {"pl", 2, "2 nowe wiadomości"}, {"pl", 5, "5 nowych wiadomości"},
		{"pl", 12, "12 nowych wiadomości"}, {"pl", 22, "22 nowe wiadomości"}, {"pl", 25, "25 nowych wiadomości"},
	} {
		if got := Title(c.lang, c.n); got != c.want {
			t.Errorf("Title(%q, %d) = %q, want %q", c.lang, c.n, got, c.want)
		}
	}
}

func TestTickets(t *testing.T) {
	t.Run("DeviceNotRegistered", func(t *testing.T) {
		e := newEnv(t)
		e.expo.ticket = func(string) map[string]any {
			return map[string]any{"status": "error", "message": "not registered", "details": map[string]any{"error": "DeviceNotRegistered"}}
		}
		e.deliver(e.account, e.inbox, "alice@example.org", true)
		must(t, e.runNotify())
		if e.deviceExists(e.device.ID) {
			t.Fatal("device kept after DeviceNotRegistered")
		}
	})
	t.Run("other error", func(t *testing.T) {
		e := newEnv(t)
		e.expo.ticket = func(string) map[string]any {
			return map[string]any{"status": "error", "message": "slow down", "details": map[string]any{"error": "MessageRateExceeded"}}
		}
		e.deliver(e.account, e.inbox, "alice@example.org", true)
		must(t, e.runNotify()) // no retry
		if le := e.lastError(e.device.ID); le != "MessageRateExceeded: slow down" {
			t.Fatalf("last_error = %q", le)
		}
		dev, err := e.svc.Current(e.ctx, e.session)
		must(t, err)
		if !dev.LastError.Valid {
			t.Fatal("Current without lastError")
		}
	})
	t.Run("request refused", func(t *testing.T) {
		e := newEnv(t)
		e.expo.fail, e.expo.failCode = 1, http.StatusBadRequest
		e.deliver(e.account, e.inbox, "alice@example.org", true)
		must(t, e.runNotify())
		if le := e.lastError(e.device.ID); !strings.Contains(le, "INTERNAL") {
			t.Fatalf("last_error = %q", le)
		}
	})
}

func TestReceipts(t *testing.T) {
	e := newEnv(t)
	other := e.register(e.newSession(), "ExpoPushToken[other]", nil)
	e.expo.receipt = func(id string) map[string]any {
		switch id {
		case "gone":
			return map[string]any{"status": "error", "message": "x", "details": map[string]any{"error": "DeviceNotRegistered"}}
		case "big":
			return map[string]any{"status": "error", "message": "too big", "details": map[string]any{"error": "MessageTooBig"}}
		}
		return nil // not ready
	}
	payload, _ := json.Marshal(receiptsPayload{Tickets: []ticketRef{{ID: "gone", DeviceID: e.device.ID}, {ID: "big", DeviceID: other.ID}, {ID: "later", DeviceID: other.ID}}})
	must(t, e.svc.runReceipts(e.ctx, worker.Job{Kind: JobReceipts, Payload: payload}))
	if e.deviceExists(e.device.ID) {
		t.Error("device kept after a DeviceNotRegistered receipt")
	}
	if le := e.lastError(other.ID); le != "MessageTooBig: too big" {
		t.Errorf("last_error = %q", le)
	}
	if len(e.expo.receipts) != 1 || len(e.expo.receipts[0]) != 3 {
		t.Errorf("receipt requests = %v", e.expo.receipts)
	}
}

// TestRetry runs push.notify on the job queue: a 5xx answer retries the job
// with the queue's backoff.
func TestRetry(t *testing.T) {
	e := newEnv(t)
	e.expo.fail, e.expo.failCode = 2, http.StatusServiceUnavailable
	// The queue is shared by the package: leave only this test's job.
	_, err := testPool.Exec(e.ctx, `DELETE FROM jobs`)
	must(t, err)
	ctx, cancel := context.WithCancel(e.ctx)
	w := worker.New(worker.Options{Pool: testPool, PollInterval: 100 * time.Millisecond, Backoff: func(int) time.Duration { return 200 * time.Millisecond }})
	e.svc.RegisterJobs(w)
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	defer func() { cancel(); <-done }()
	e.deliver(e.account, e.inbox, "alice@example.org", true)
	deadline := time.Now().Add(20 * time.Second)
	for len(e.expo.sent()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("not sent after retries; requests = %d", e.expo.requests())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := e.expo.requests(); n != 3 {
		t.Errorf("requests = %d, want 2 failed + 1 sent", n)
	}
	for time.Now().Before(deadline) && len(e.notifyJobs()) > 0 {
		time.Sleep(100 * time.Millisecond)
	}
	if n := len(e.notifyJobs()); n != 0 {
		t.Errorf("push.notify left after success: %d", n)
	}
}

func TestRegister(t *testing.T) {
	e := newEnv(t)
	// Same token from a new session (sign-in again on the phone): the row moves.
	s2 := e.newSession()
	d2 := e.register(s2, e.token, nil)
	if d2.ID != e.device.ID || d2.SessionID != s2.ID {
		t.Fatalf("re-registration: %+v, want device %d on session %d", d2, e.device.ID, s2.ID)
	}
	if _, err := e.svc.Current(e.ctx, e.session); err != ErrNotFound {
		t.Fatalf("old session still has the device: %v", err)
	}
	// Another token on the same session replaces its device.
	d3 := e.register(s2, "ExpoPushToken[new]", nil)
	var n int
	must(t, testPool.QueryRow(e.ctx, `SELECT count(*) FROM push_devices WHERE session_id = $1`, s2.ID).Scan(&n))
	if n != 1 || d3.PushToken != "ExpoPushToken[new]" {
		t.Fatalf("devices of the session = %d (%+v)", n, d3)
	}
	for _, bad := range []string{"", "ExponentPushToken[]", "FcmToken[abc]", "ExponentPushToken[a b]", "ExponentPushToken[" + strings.Repeat("a", 300) + "]"} {
		if _, err := e.svc.Register(e.ctx, s2, Registration{Provider: "expo", Token: bad}); err != ErrInvalid {
			t.Errorf("token %q: %v", bad, err)
		}
	}
	if _, err := e.svc.Register(e.ctx, sqlcgen.Session{ID: s2.ID, UserID: e.userID, Kind: "cookie"}, Registration{Provider: "expo", Token: e.token}); err != ErrNotBearer {
		t.Errorf("cookie session: %v", err)
	}
	if got := RedactToken("ExponentPushToken[abcdefghijkl]"); strings.Contains(got, "abcdefghijkl") {
		t.Errorf("RedactToken = %q", got)
	}
}
