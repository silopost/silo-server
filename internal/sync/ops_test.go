package sync_test

import (
	"context"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/ops"
	silosync "github.com/silopost/silo-server/internal/sync"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
	"github.com/silopost/silo-server/internal/worker"
)

// opsEnv is an account behind a recording proxy, synced by a Manager with
// the job queue (so imap_op jobs run), plus the ops service.
type opsEnv struct {
	t     *testing.T
	mb    *mailbox
	px    *proxy
	a     *account
	svc   *ops.Service
	stop  func()
	other *imapclient.Client
	mgr   *silosync.Manager
}

func newOpsEnv(t *testing.T, p imaptest.Profile, window time.Duration, specs ...spec) *opsEnv {
	mb := newMailbox(t, p)
	// The Manager starts every account that is not disabled: retire the others.
	_, err := testPool.Exec(context.Background(), `UPDATE accounts SET sync_state = 'disabled'`)
	must(t, err)
	mb.create("Faktury")
	mb.populate(specs...)
	px := newProxy(t, net.JoinHostPort(mb.host, strconv.Itoa(mb.port)))
	a := newAccount(t, "127.0.0.1", px.port(), mb.user, mb.pass)
	e := &opsEnv{t: t, mb: mb, px: px, a: a, svc: ops.NewService(testPool, window), other: mb.client()}
	e.start()
	a.waitInitialDone(time.Minute)
	return e
}

// start runs a Manager and job queue for the account (stop + start = restart
// of silo-server).
func (e *opsEnv) start() {
	ctx, cancel := context.WithCancel(context.Background())
	jobs := worker.New(worker.Options{Pool: testPool, PollInterval: 100 * time.Millisecond,
		Backoff: func(int) time.Duration { return 200 * time.Millisecond }})
	m := silosync.NewManager(*e.a.deps)
	m.RegisterJobs(jobs)
	jd, md := make(chan struct{}), make(chan struct{})
	go func() { defer close(jd); _ = jobs.Run(ctx) }()
	go func() { defer close(md); _ = m.Run(ctx) }()
	stopped := false
	e.stop = func() {
		if !stopped {
			stopped = true
			cancel()
			<-md
			<-jd
		}
	}
	e.t.Cleanup(e.stop)
	e.mgr = m
}

func (e *opsEnv) restart() { e.stop(); e.start() }

func (e *opsEnv) thread(hdr string) (msgID, threadID int64) {
	id, th, _, ok := e.a.msgByHdr(hdr)
	if !ok {
		e.t.Fatalf("%s not synced", hdr)
	}
	return id, th
}

func (e *opsEnv) modify(hdr string, add, remove []ops.LabelRef) *ops.Result {
	e.t.Helper()
	_, th := e.thread(hdr)
	r, err := e.svc.ModifyLabels(context.Background(), ops.Modify{UserID: e.a.userID, ThreadIDs: []int64{th}, Add: add, Remove: remove, Origin: "test"})
	must(e.t, err)
	return r
}

func sys(keys ...string) []ops.LabelRef {
	out := make([]ops.LabelRef, len(keys))
	for i, k := range keys {
		out[i] = ops.LabelRef{System: k}
	}
	return out
}

// serverFolders returns the folders holding a message on the server (copies
// flagged \Deleted do not count).
func (e *opsEnv) serverFolders(hdr string) []string {
	e.t.Helper()
	var out []string
	list, err := e.other.List("", "*", nil).Collect()
	must(e.t, err)
	for _, l := range list {
		if _, err := e.other.Select(l.Mailbox, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			continue
		}
		data, err := e.other.UIDSearch(&imap.SearchCriteria{
			Header:  []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: "<" + hdr + ">"}},
			NotFlag: []imap.Flag{imap.FlagDeleted},
		}, nil).Wait()
		must(e.t, err)
		for range data.AllUIDs() {
			out = append(out, l.Mailbox)
		}
	}
	slices.Sort(out)
	return out
}

func (e *opsEnv) serverFlags(hdr string) []string {
	e.t.Helper()
	fs := e.serverFolders(hdr)
	if len(fs) == 0 {
		return nil
	}
	_, err := e.other.Select(fs[0], &imap.SelectOptions{ReadOnly: true}).Wait()
	must(e.t, err)
	uid := uidOf(e.t, e.other, "<"+hdr+">")
	bufs, err := e.other.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{Flags: true, UID: true}).Collect()
	must(e.t, err)
	var out []string
	for _, f := range bufs[0].Flags {
		out = append(out, string(f))
	}
	return out
}

func (e *opsEnv) reconcile() {
	e.t.Helper()
	d, err := mail.Reconcile(context.Background(), testPool, e.a.id)
	must(e.t, err)
	for _, x := range d {
		e.t.Errorf("reconcile: %s", x)
	}
}

func (e *opsEnv) waitNoJobs(timeout time.Duration) {
	e.t.Helper()
	waitFor(e.t, timeout, "imap_op jobs done", func() bool {
		return e.a.int(`SELECT count(*) FROM jobs WHERE account_id = $1 AND kind = 'imap_op' AND status = 'pending'`, e.a.id) == 0
	})
}

// TestWriteBack runs the write-back scenarios on the full and no-move
// profiles (no MOVE and no UIDPLUS: COPY + \Deleted without EXPUNGE).
func TestWriteBack(t *testing.T) {
	for _, p := range []imaptest.Profile{imaptest.Full, imaptest.NoMove} {
		t.Run(string(p), func(t *testing.T) { testWriteBack(t, p) })
	}
}

func testWriteBack(t *testing.T, p imaptest.Profile) {
	const window = 1500 * time.Millisecond
	e := newOpsEnv(t, p, window, spec{"INBOX", 12})

	t.Run("archive creates Archive and moves", func(t *testing.T) {
		id, th := e.thread("m1@silo.test")
		r := e.modify("m1@silo.test", nil, sys("INBOX"))
		l := e.a.labelsOf(id)
		if l["INBOX"] || !l["ARCHIVE"] || r.OperationID == "" || r.UndoUntil == nil || len(r.Threads) != 1 || !r.Threads[0].PendingSync {
			t.Fatalf("after modify: labels %v result %+v", l, r)
		}
		if f := e.serverFolders("m1@silo.test"); !slices.Equal(f, []string{"INBOX"}) {
			t.Errorf("server changed inside the undo window: %v", f)
		}
		waitFor(t, 10*time.Second, "archived on the server", func() bool {
			return slices.Equal(e.serverFolders("m1@silo.test"), []string{"Archive"})
		})
		e.waitNoJobs(5 * time.Second)
		waitFor(t, 5*time.Second, "pending cleared", func() bool {
			return e.a.int(`SELECT count(*) FROM threads WHERE id = $1 AND pending_ops = 0`, th) == 1
		})
		if e.a.int(`SELECT count(*) FROM mailbox_folders WHERE account_id = $1 AND name = 'Archive' AND role = 'archive'`, e.a.id) != 1 {
			t.Error("Archive not recorded with its role")
		}
		if l := e.a.labelsOf(id); l["INBOX"] || !l["ARCHIVE"] {
			t.Errorf("labels after write-back: %v", l)
		}
		if e.a.int(`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'thread.updated' AND payload->>'origin' = 'test' AND (payload->>'pendingSync')::bool`, e.a.userID) == 0 {
			t.Error("no thread.updated with pendingSync and origin")
		}
	})

	t.Run("star", func(t *testing.T) {
		start := time.Now()
		e.modify("m2@silo.test", sys("STARRED"), nil)
		waitFor(t, 5*time.Second, `\Flagged on the server`, func() bool { return slices.Contains(e.serverFlags("m2@silo.test"), `\Flagged`) })
		t.Logf("%s: STARRED -> \\Flagged on the server in %s", p, time.Since(start).Round(time.Millisecond))
	})

	t.Run("user label as keyword", func(t *testing.T) {
		var lid int64
		must(t, testPool.QueryRow(context.Background(), `INSERT INTO labels (user_id, account_id, name, type) VALUES ($1, $2, 'Projekt X', 'user') RETURNING id`, e.a.userID, e.a.id).Scan(&lid))
		e.modify("m3@silo.test", []ops.LabelRef{{ID: lid}}, nil)
		id, _ := e.thread("m3@silo.test")
		if !e.a.labelsOf(id)["Projekt X"] {
			t.Fatal("label not applied locally")
		}
		waitFor(t, 5*time.Second, "keyword on the server", func() bool { return slices.Contains(e.serverFlags("m3@silo.test"), "Projekt_X") })
		e.waitNoJobs(5 * time.Second)
		if !e.a.labelsOf(id)["Projekt X"] {
			t.Error("label lost after the write-back")
		}
	})

	t.Run("undo inside the window", func(t *testing.T) {
		id, _ := e.thread("m4@silo.test")
		moves := e.px.count("MOVE") + e.px.count("COPY")
		r := e.modify("m4@silo.test", nil, sys("INBOX"))
		if _, err := e.svc.Undo(context.Background(), e.a.userID, r.OperationID, "test"); err != nil {
			t.Fatal(err)
		}
		if l := e.a.labelsOf(id); !l["INBOX"] || l["ARCHIVE"] {
			t.Errorf("labels after undo: %v", l)
		}
		time.Sleep(window + time.Second)
		if f := e.serverFolders("m4@silo.test"); !slices.Equal(f, []string{"INBOX"}) || e.px.count("MOVE")+e.px.count("COPY") != moves {
			t.Errorf("server saw an undone operation: %v", f)
		}
		if _, err := e.svc.Undo(context.Background(), e.a.userID, r.OperationID, "test"); err != ops.ErrNotFound {
			t.Errorf("second undo: %v", err)
		}
	})

	t.Run("sync during the window keeps the change", func(t *testing.T) {
		id, _ := e.thread("m5@silo.test")
		e.modify("m5@silo.test", nil, sys("INBOX"))
		_, err := testPool.Exec(context.Background(), `UPDATE mailbox_folders SET last_sync_at = NULL WHERE account_id = $1 AND role = 'inbox'`, e.a.id)
		must(t, err)
		e.mgr.Worker(e.a.id).RequestResync("full")
		waitFor(t, 5*time.Second, "resync pass", func() bool {
			return e.a.int(`SELECT count(*) FROM mailbox_folders WHERE account_id = $1 AND role = 'inbox' AND last_sync_at IS NOT NULL`, e.a.id) == 1
		})
		if l := e.a.labelsOf(id); l["INBOX"] {
			t.Errorf("sync undid the pending archive: %v", l)
		}
		waitFor(t, 10*time.Second, "archived", func() bool { return slices.Equal(e.serverFolders("m5@silo.test"), []string{"Archive"}) })
	})

	t.Run("message gone before the job runs", func(t *testing.T) {
		before := e.a.int(`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'operation.failed' AND payload->>'reason' = 'message_gone'`, e.a.userID)
		e.modify("m6@silo.test", nil, sys("INBOX"))
		selectInbox(t, e.other)
		uid := uidOf(t, e.other, "<m6@silo.test>")
		_, err := e.other.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Collect()
		must(t, err)
		must(t, e.other.Expunge().Close())
		waitFor(t, 10*time.Second, "operation.failed message_gone", func() bool {
			return e.a.int(`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'operation.failed' AND payload->>'reason' = 'message_gone'`, e.a.userID) > before
		})
		e.waitNoJobs(5 * time.Second)
	})

	t.Run("moved elsewhere before the job runs", func(t *testing.T) {
		e.modify("m7@silo.test", nil, sys("INBOX"))
		selectInbox(t, e.other)
		uid := uidOf(t, e.other, "<m7@silo.test>")
		_, err := e.other.Move(imap.UIDSetNum(uid), "Faktury").Wait()
		must(t, err)
		waitFor(t, 10*time.Second, "archived from Faktury", func() bool { return slices.Equal(e.serverFolders("m7@silo.test"), []string{"Archive"}) })
	})

	t.Run("trash, untrash, delete", func(t *testing.T) {
		id, th := e.thread("m8@silo.test")
		if _, err := e.svc.Trash(context.Background(), e.a.userID, []int64{th}, ""); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 10*time.Second, "in Trash", func() bool { return slices.Equal(e.serverFolders("m8@silo.test"), []string{"Trash"}) })
		e.waitNoJobs(5 * time.Second)
		if _, err := e.svc.Untrash(context.Background(), e.a.userID, []int64{th}, ""); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 10*time.Second, "back in INBOX", func() bool { return slices.Equal(e.serverFolders("m8@silo.test"), []string{"INBOX"}) })
		e.waitNoJobs(5 * time.Second)
		if _, err := e.svc.Delete(context.Background(), e.a.userID, []int64{th}, ""); err == nil {
			t.Error("delete outside Trash accepted")
		}
		if _, err := e.svc.Trash(context.Background(), e.a.userID, []int64{th}, ""); err != nil {
			t.Fatal(err)
		}
		e.waitNoJobs(10 * time.Second)
		waitFor(t, 10*time.Second, "location in Trash", func() bool {
			return e.a.int(`SELECT count(*) FROM message_locations l JOIN mailbox_folders f ON f.id = l.folder_id WHERE l.message_id = $1 AND f.role = 'trash'`, id) == 1
		})
		if _, err := e.svc.Delete(context.Background(), e.a.userID, []int64{th}, ""); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 10*time.Second, "deleted on the server", func() bool { return len(e.serverFolders("m8@silo.test")) == 0 })
	})

	t.Run("not modifiable", func(t *testing.T) {
		_, th := e.thread("m9@silo.test")
		for _, k := range []string{"SENT", "DRAFT", "SNOOZED", "SCHEDULED", "ARCHIVE"} {
			if _, err := e.svc.ModifyLabels(context.Background(), ops.Modify{UserID: e.a.userID, ThreadIDs: []int64{th}, Add: sys(k)}); err == nil {
				t.Errorf("+%s accepted", k)
			}
		}
	})

	t.Run("restart keeps the job and moves once", func(t *testing.T) {
		e.stop()
		// A pending archive survives the restart ...
		e.modify("m10@silo.test", nil, sys("INBOX"))
		// ... and one whose MOVE already happened (another client, or a crash
		// after the server answered) is not moved twice.
		e.modify("m11@silo.test", nil, sys("INBOX"))
		selectInbox(t, e.other)
		uid := uidOf(t, e.other, "<m11@silo.test>")
		_, err := e.other.Move(imap.UIDSetNum(uid), "Archive").Wait()
		must(t, err)
		e.start()
		waitFor(t, 15*time.Second, "both archived once", func() bool {
			return slices.Equal(e.serverFolders("m10@silo.test"), []string{"Archive"}) &&
				slices.Equal(e.serverFolders("m11@silo.test"), []string{"Archive"})
		})
		e.waitNoJobs(10 * time.Second)
		time.Sleep(time.Second)
		if f := e.serverFolders("m11@silo.test"); !slices.Equal(f, []string{"Archive"}) {
			t.Errorf("m11 on the server: %v", f)
		}
	})

	t.Run("undo after the window moves back", func(t *testing.T) {
		id, _ := e.thread("m12@silo.test")
		r := e.modify("m12@silo.test", nil, sys("INBOX"))
		waitFor(t, 10*time.Second, "archived on the server", func() bool {
			return slices.Equal(e.serverFolders("m12@silo.test"), []string{"Archive"})
		})
		e.waitNoJobs(5 * time.Second)
		u, err := e.svc.Undo(context.Background(), e.a.userID, r.OperationID, "test")
		must(t, err)
		if u.OperationID == "" || u.OperationID == r.OperationID || u.UndoUntil == nil {
			t.Fatalf("late undo: %+v", u)
		}
		if l := e.a.labelsOf(id); !l["INBOX"] || l["ARCHIVE"] {
			t.Errorf("labels right after the late undo: %v", l)
		}
		waitFor(t, 15*time.Second, "back in INBOX on the server", func() bool {
			return slices.Equal(e.serverFolders("m12@silo.test"), []string{"INBOX"})
		})
		e.waitNoJobs(5 * time.Second)
		if l := e.a.labelsOf(id); !l["INBOX"] || l["ARCHIVE"] {
			t.Errorf("labels after the inverse ran: %v", l)
		}
	})

	e.waitNoJobs(10 * time.Second)
	e.mgr.Worker(e.a.id).RequestResync("full")
	time.Sleep(2 * time.Second)
	e.reconcile()
}

// TestCoalesceMarkRead: ten "mark read" requests queued before the worker
// runs become one UID STORE.
func TestCoalesceMarkRead(t *testing.T) {
	e := newOpsEnv(t, imaptest.Full, time.Second, spec{"INBOX", 10})
	e.stop()
	for i := 1; i <= 10; i++ {
		e.modify("m"+strconv.Itoa(i)+"@silo.test", nil, sys("UNREAD"))
	}
	if n := e.a.int(`SELECT count(*) FROM jobs WHERE account_id = $1 AND kind = 'imap_op'`, e.a.id); n != 1 {
		t.Fatalf("%d jobs after 10 mark-read, want 1", n)
	}
	if n := e.a.int(`SELECT count(*) FROM messages WHERE account_id = $1 AND is_unread`, e.a.id); n != 0 {
		t.Errorf("%d unread locally", n)
	}
	before := e.px.count("UID STORE", `\Seen`)
	e.start()
	e.waitNoJobs(15 * time.Second)
	if n := e.px.count("UID STORE", `\Seen`) - before; n != 1 {
		t.Errorf("UID STORE \\Seen commands = %d, want 1", n)
	}
	t.Logf("10 mark-read -> %d UID STORE", e.px.count("UID STORE", `\Seen`)-before)
	for i := 1; i <= 10; i++ {
		if !slices.Contains(e.serverFlags("m"+strconv.Itoa(i)+"@silo.test"), `\Seen`) {
			t.Errorf("m%d not \\Seen on the server", i)
		}
	}
	// An inverse change cancels a queued one: +UNREAD then -UNREAD = nothing.
	e.stop()
	e.modify("m1@silo.test", sys("UNREAD"), nil)
	e.modify("m1@silo.test", nil, sys("UNREAD"))
	if n := e.a.int(`SELECT count(*) FROM jobs WHERE account_id = $1 AND kind = 'imap_op'`, e.a.id); n != 0 {
		t.Errorf("%d jobs after +UNREAD -UNREAD", n)
	}
	e.start()
}

// TestConcurrentSyncAndOps interleaves local operations with changes by
// another client on the same messages; in the end the mirror equals the
// server and the derived data reconciles.
func TestConcurrentSyncAndOps(t *testing.T) {
	for _, p := range []imaptest.Profile{imaptest.Full, imaptest.NoMove} {
		t.Run(string(p), func(t *testing.T) { testConcurrent(t, p) })
	}
}

func testConcurrent(t *testing.T, p imaptest.Profile) {
	const n = 16
	e := newOpsEnv(t, p, 300*time.Millisecond, spec{"INBOX", n})
	rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 7))
	hdr := func() string { return "m" + strconv.Itoa(1+rng.IntN(n)) + "@silo.test" }
	changes := [][2][]ops.LabelRef{
		{nil, sys("INBOX")}, {sys("INBOX"), nil}, {sys("STARRED"), nil}, {nil, sys("STARRED")},
		{sys("UNREAD"), nil}, {nil, sys("UNREAD")}, {sys("TRASH"), nil}, {nil, sys("TRASH")},
	}
	mustCreate := func(name string) { _ = e.other.Create(name, nil).Wait() }
	mustCreate("Archive")
	for i := 0; i < 60; i++ {
		if rng.IntN(2) == 0 {
			c := changes[rng.IntN(len(changes))]
			_, th := e.thread(hdr())
			_, err := e.svc.ModifyLabels(context.Background(), ops.Modify{UserID: e.a.userID, ThreadIDs: []int64{th}, Add: c[0], Remove: c[1]})
			if err != nil && err != ops.ErrNotFound { // not found: orphaned while a move is in flight
				t.Fatal(err)
			}
		} else {
			h := hdr()
			fs := e.serverFolders(h)
			if len(fs) == 0 {
				continue
			}
			if _, err := e.other.Select(fs[0], nil).Wait(); err != nil {
				t.Fatal(err)
			}
			uid := uidOf(t, e.other, "<"+h+">")
			switch rng.IntN(3) {
			case 0:
				_, err := e.other.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Collect()
				must(t, err)
			case 1:
				_, err := e.other.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: imap.StoreFlagsDel, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}, nil).Collect()
				must(t, err)
			default:
				dst := []string{"INBOX", "Archive", "Faktury"}[rng.IntN(3)]
				if dst != fs[0] {
					_, err := e.other.Move(imap.UIDSetNum(uid), dst).Wait()
					must(t, err)
				}
			}
		}
		time.Sleep(time.Duration(rng.IntN(150)) * time.Millisecond)
	}
	e.waitNoJobs(30 * time.Second)
	_, err := testPool.Exec(context.Background(), `UPDATE mailbox_folders SET last_sync_at = NULL WHERE account_id = $1`, e.a.id)
	must(t, err)
	e.mgr.Worker(e.a.id).RequestResync("full")
	waitFor(t, 15*time.Second, "final full sync", func() bool {
		return e.a.int(`SELECT count(*) FROM mailbox_folders WHERE account_id = $1 AND sync_mode = 'sync' AND last_sync_at IS NULL`, e.a.id) == 0 &&
			e.a.int(`SELECT count(*) FROM jobs WHERE account_id = $1 AND kind = 'imap_op' AND status = 'pending'`, e.a.id) == 0
	})
	time.Sleep(500 * time.Millisecond)
	e.reconcile()
	for i := 1; i <= n; i++ {
		h := "m" + strconv.Itoa(i) + "@silo.test"
		server := e.serverFolders(h)
		rows, err := testPool.Query(context.Background(), `
SELECT f.name FROM message_locations l JOIN mailbox_folders f ON f.id = l.folder_id JOIN messages m ON m.id = l.message_id
WHERE m.account_id = $1 AND m.message_id_hdr = $2 AND NOT ('\Deleted' = ANY(l.flags)) ORDER BY f.name`, e.a.id, h)
		must(t, err)
		var local []string
		for rows.Next() {
			var s string
			must(t, rows.Scan(&s))
			local = append(local, s)
		}
		rows.Close()
		if !slices.Equal(local, server) {
			t.Errorf("%s: local %v, server %v", h, local, server)
			continue
		}
		if len(server) == 0 {
			continue
		}
		id, _ := e.thread(h)
		flags := e.serverFlags(h)
		l := e.a.labelsOf(id)
		unread := e.a.int(`SELECT count(*) FROM messages WHERE id = $1 AND is_unread`, id) == 1
		if l["STARRED"] != slices.Contains(flags, `\Flagged`) || unread == slices.Contains(flags, `\Seen`) {
			t.Errorf("%s: labels %v unread %v, server flags %v", h, l, unread, flags)
		}
	}
	_ = strings.ToLower
}

// TestKeywordRejected: a maildir folder takes at most 26 keywords (plan
// 3.7); a rejected keyword turns the label local-only, the label stays on the
// message and account.error keywords_unsupported is written.
func TestKeywordRejected(t *testing.T) {
	e := newOpsEnv(t, imaptest.Full, time.Second, spec{"INBOX", 2})
	selectInbox(t, e.other)
	uid := uidOf(t, e.other, "<m1@silo.test>")
	var kws []imap.Flag
	for i := 0; i < 26; i++ {
		kws = append(kws, imap.Flag("kw"+strconv.Itoa(i)))
	}
	if _, err := e.other.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: kws}, nil).Collect(); err != nil {
		t.Skipf("server refused 26 keywords: %v", err)
	}
	if _, err := e.other.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{"probe27"}}, nil).Collect(); err == nil {
		t.Skip("this Dovecot accepts more than 26 keywords per folder")
	}
	var lid int64
	must(t, testPool.QueryRow(context.Background(), `INSERT INTO labels (user_id, account_id, name, type) VALUES ($1, $2, 'Nadmiar', 'user') RETURNING id`, e.a.userID, e.a.id).Scan(&lid))
	e.modify("m2@silo.test", []ops.LabelRef{{ID: lid}}, sys("UNREAD"))
	e.waitNoJobs(10 * time.Second)
	id, _ := e.thread("m2@silo.test")
	if !e.a.labelsOf(id)["Nadmiar"] || e.a.int(`SELECT count(*) FROM labels WHERE id = $1 AND keyword_state = 'local_only'`, lid) != 1 {
		t.Errorf("label after rejection: %v", e.a.labelsOf(id))
	}
	if !slices.Contains(e.serverFlags("m2@silo.test"), `\Seen`) {
		t.Error("system flag not stored after the keyword was dropped")
	}
	if e.a.int(`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'account.error' AND payload->>'code' = 'keywords_unsupported'`, e.a.userID) == 0 {
		t.Error("no account.error keywords_unsupported")
	}
}
