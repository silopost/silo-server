package sync_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/filters"
	"github.com/silopost/silo-server/internal/ops"
	"github.com/silopost/silo-server/internal/push"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
	"github.com/silopost/silo-server/internal/worker"
)

// TestPushNewMail checks where push.notify comes from against Dovecot: not
// from the initial sync, once for new INBOX mail of the incremental sync,
// not for new mail a filter moves out of INBOX, and once for new mail the
// filters leave in INBOX. The Expo Push Service is a fake.
func TestPushNewMail(t *testing.T) {
	ctx := context.Background()
	mb := newMailbox(t, imaptest.Full)
	_, err := testPool.Exec(ctx, `UPDATE accounts SET sync_state = 'disabled'`)
	must(t, err)
	mb.populate(spec{"INBOX", 2})
	px := newProxy(t, net.JoinHostPort(mb.host, strconv.Itoa(mb.port)))
	a := newAccount(t, "127.0.0.1", px.port(), mb.user, mb.pass)
	a.deps.Settings.Push = true

	var mu gosync.Mutex
	var sends []string
	expo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msgs []map[string]any
		_ = json.Unmarshal(body, &msgs)
		mu.Lock()
		sends = append(sends, string(body))
		mu.Unlock()
		data := make([]map[string]any, len(msgs))
		for i := range msgs {
			data[i] = map[string]any{"status": "ok", "id": fmt.Sprintf("r%d", i)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(expo.Close)
	sent := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), sends...) }

	psvc := push.New(push.Options{Pool: testPool, Config: config.PushConfig{
		Enabled: true, ExpoSendURL: expo.URL + "/send", ExpoReceiptsURL: expo.URL + "/receipts",
	}})
	sum := sha256.Sum256([]byte(fmt.Sprintf("push-%d", a.id)))
	sess := sqlcgen.Session{UserID: a.userID, Kind: "bearer"}
	must(t, testPool.QueryRow(ctx, `INSERT INTO sessions (user_id, token_hash, csrf_token, expires_at, kind)
VALUES ($1, $2, 'x', now() + interval '1 hour', 'bearer') RETURNING id`, a.userID, sum[:]).Scan(&sess.ID))
	_, err = psvc.Register(ctx, sess, push.Registration{Provider: "expo", Token: "ExponentPushToken[sync]"})
	must(t, err)

	e := &opsEnv{t: t, mb: mb, px: px, a: a, svc: ops.NewService(testPool, time.Second), other: mb.client()}
	fsvc := filters.New(filters.Options{Pool: testPool, Ops: e.svc})
	e.extra = func(w *worker.Worker) { psvc.RegisterJobs(w); fsvc.RegisterJobs(w) }
	e.start()
	a.waitInitialDone(time.Minute)
	a.waitState("live", 30*time.Second)
	waitFor(t, 10*time.Second, "account worker", func() bool { return e.mgr.Worker(a.id) != nil })
	_, err = testPool.Exec(ctx, `UPDATE accounts SET filters_since = now() - interval '1 hour' WHERE id = $1`, a.id)
	must(t, err)

	pushJobs := func() int {
		return a.int(`SELECT count(*) FROM jobs WHERE kind = 'push.notify' AND status = 'pending'`)
	}
	quiet := func(what string, want int) {
		t.Helper()
		waitFor(t, 30*time.Second, "push jobs done", func() bool {
			return pushJobs() == 0 && a.int(`SELECT count(*) FROM jobs WHERE kind = 'filter_eval' AND status = 'pending'`) == 0
		})
		time.Sleep(3 * time.Second) // longer than mail.PushDelay
		if n := len(sent()); n != want {
			t.Fatalf("%s: %d push requests, want %d: %v", what, n, want, sent())
		}
	}
	sync := func() { e.mgr.Worker(a.id).RequestResync("incremental") }
	synced := func(hdr string) (id, thread int64) {
		t.Helper()
		waitFor(t, 30*time.Second, hdr+" synced", func() bool {
			var ok bool
			id, thread, _, ok = a.msgByHdr(hdr)
			return ok
		})
		return id, thread
	}

	// The initial sync pushes nothing.
	quiet("initial sync", 0)

	// New INBOX mail: one push with the ids of the thread.
	appendMsg(t, e.other, "INBOX", mailFrom(101, "Jan <jan@firma.pl>", "Private plans", "secret body", time.Now()), nil, time.Now())
	sync()
	_, thread := synced("f101@silo.test")
	waitFor(t, 30*time.Second, "push of new mail", func() bool { return len(sent()) == 1 })
	var msgs []map[string]any
	must(t, json.Unmarshal([]byte(sent()[0]), &msgs))
	data, _ := msgs[0]["data"].(map[string]any)
	if len(msgs) != 1 || msgs[0]["to"] != "ExponentPushToken[sync]" || data["threadId"] != "t_"+strconv.FormatInt(thread, 36) ||
		data["accountId"] != "acc_"+strconv.FormatInt(a.id, 36) || data["count"] != float64(1) {
		t.Fatalf("push = %s", sent()[0])
	}
	for _, s := range []string{"Jan", "jan@firma.pl", "Private plans", "secret body", "silo@silo.test"} {
		if strings.Contains(sent()[0], s) {
			t.Errorf("push contains %q", s)
		}
	}
	quiet("one new message", 1)

	// A filter moves new mail out of INBOX: no push. Mail it leaves: push.
	_, err = fsvc.Create(ctx, a.userID, filters.Input{AccountID: a.id, Criteria: "from:x@news.test", Enabled: true,
		Actions: filters.Actions{Archive: true}})
	must(t, err)
	appendMsg(t, e.other, "INBOX", mailFrom(102, "News <x@news.test>", "Fresh news", "news", time.Now()), nil, time.Now())
	sync()
	id, _ := synced("f102@silo.test")
	waitFor(t, 30*time.Second, "filter archived f102", func() bool { return !a.labelsOf(id)["INBOX"] })
	quiet("filtered out of INBOX", 1)

	appendMsg(t, e.other, "INBOX", mailFrom(103, "Ola <ola@firma.pl>", "Hi", "hi", time.Now()), nil, time.Now())
	sync()
	synced("f103@silo.test")
	waitFor(t, 30*time.Second, "push of mail the filters left in INBOX", func() bool { return len(sent()) == 2 })
	quiet("left in INBOX by filters", 2)
}
