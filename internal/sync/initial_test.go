package sync_test

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"os"
	"runtime"
	"strconv"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

// initialSize is the mailbox of TestInitialSync: 2000 messages by default,
// SILO_SYNC_TEST_MESSAGES overrides it (e.g. 200000 for the DoD run of
// phase 5; see TestInitialSync for the memory report).
func initialSize() int {
	if n, err := strconv.Atoi(os.Getenv("SILO_SYNC_TEST_MESSAGES")); err == nil && n > 0 {
		return n
	}
	return 2000
}

func splitSpecs(n int) []spec {
	return []spec{{"INBOX", n * 60 / 100}, {"Sent", n * 25 / 100}, {"Projekty", n - n*60/100 - n*25/100}}
}

// TestInitialSync syncs a generated mailbox from scratch. It samples the
// memory of the test process every 2 s while the sync runs and logs the
// peaks; SILO_SYNC_TEST_REPORT=<file> also writes them as JSON (with the
// timings and the number of WARN/ERROR log records of the worker), and
// SILO_SYNC_TEST_PPROF=<file> keeps a heap profile taken at the HeapInuse
// peak. The RSS includes the test's own IMAP client and Postgres pool:
// an upper bound for silo-server, whose Dovecot and Postgres are elsewhere.
func TestInitialSync(t *testing.T) {
	mb := newMailbox(t, imaptest.Full)
	mb.create("Projekty")
	n := initialSize()
	rep := syncReport{Messages: n, GOOS: runtime.GOOS,
		Note: "RSS of the go test process (sync worker + test IMAP client + pgx pool); Dovecot and Postgres run in containers"}
	if path := os.Getenv("SILO_SYNC_TEST_REPORT"); path != "" {
		t.Cleanup(func() { rep.TestFailed = t.Failed(); writeSyncReport(t, path, rep) })
	}
	t0 := time.Now()
	recent := mb.populateAged(realisticAge, splitSpecs(n)...)
	rep.PopulateSeconds = time.Since(t0).Seconds()
	t.Logf("populated %d messages in %s", n, time.Since(t0).Round(time.Millisecond))

	a := mb.account(mb.pass)
	var warns, errs atomic.Int64
	a.deps.Logger = slog.New(countingHandler{a.deps.Logger.Handler(), &warns, &errs})
	var batches stdsync.Map
	a.deps.Hooks.AfterBatch = func(phase string, k int) {
		v, _ := batches.LoadOrStore(phase, new(int))
		*v.(*int) += k
	}
	// The default timeouts fit the 2000-message run; large runs scale them.
	liveTimeout := max(2*time.Minute, time.Duration(n)*3*time.Millisecond)
	doneTimeout := max(5*time.Minute, time.Duration(n)*30*time.Millisecond)
	runtime.GC()
	mem := startMemSampler(2*time.Second, os.Getenv("SILO_SYNC_TEST_PPROF"))
	defer mem.finish()
	t1 := time.Now()
	a.start()
	a.waitState("live", liveTimeout)
	live := time.Since(t1)
	rep.LiveSeconds = live.Seconds()
	// Phase A is the condition of live: the last 30 days of INBOX are there.
	inbox := a.folderID("INBOX")
	got := a.int(`SELECT count(*) FROM message_locations l JOIN messages m ON m.id = l.message_id
WHERE l.folder_id = $1 AND m.internal_date >= now() - interval '30 days' + interval '1 hour'`, inbox)
	if got < recent["INBOX"] {
		t.Errorf("at live: %d recent INBOX messages, want >= %d", got, recent["INBOX"])
	}
	if a.int(`SELECT count(*) FROM threads t JOIN thread_labels tl ON tl.thread_id = t.id JOIN labels l ON l.id = tl.label_id
WHERE t.account_id = $1 AND l.system_key = 'INBOX'`, a.id) == 0 {
		t.Error("INBOX thread list empty at live")
	}
	a.waitInitialDone(doneTimeout)
	complete := time.Since(t1)
	mem.finish()
	rep.InitialSeconds = complete.Seconds()
	rep.MaxRSSBytes, rep.PeakRSSBytes, rep.MaxSysBytes, rep.MaxHeapInuseBytes, rep.Samples = mem.maxRSS, mem.peakRSS, mem.maxSys, mem.maxHeapInuse, mem.samples
	rep.LogErrors, rep.LogWarnings = errs.Load(), warns.Load()
	t.Logf("initial sync of %d messages: live after %s, complete after %s", n, live.Round(time.Millisecond), complete.Round(time.Millisecond))
	t.Logf("memory during sync (%d samples): max RSS %d MiB (OS peak %d MiB, measured=%v), max Go Sys %d MiB, max HeapInuse %d MiB; worker log: %d errors, %d warnings",
		mem.samples, mem.maxRSS>>20, mem.peakRSS>>20, mem.rssOK, mem.maxSys>>20, mem.maxHeapInuse>>20, rep.LogErrors, rep.LogWarnings)

	// Every folder: locations = STATUS MESSAGES, cursors complete.
	c := mb.client()
	for _, name := range []string{"INBOX", "Sent", "Projekty", "Drafts", "Trash", "Junk"} {
		st, err := c.Status(name, &imap.StatusOptions{NumMessages: true}).Wait()
		must(t, err)
		id := a.folderID(name)
		if l := a.int(`SELECT count(*) FROM message_locations WHERE folder_id = $1`, id); l != int(*st.NumMessages) {
			t.Errorf("%s: %d locations, STATUS MESSAGES %d", name, l, *st.NumMessages)
		}
		if a.int(`SELECT count(*) FROM sync_cursors WHERE folder_id = $1 AND phase = 'done'`, id) != 1 {
			t.Errorf("%s: cursor not done", name)
		}
	}
	cnt := a.counts()
	if cnt.Messages != n || cnt.DistinctMsgID != n || cnt.Locations != n || cnt.Full != n {
		t.Errorf("counts %+v, want %d messages, locations and bodies", cnt, n)
	}
	if p := a.int(`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'account.sync_progress'`, a.userID); p == 0 {
		t.Error("no account.sync_progress events")
	}
	if a.int(`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'account.status' AND payload->>'state' = 'live'`, a.userID) == 0 {
		t.Error("no account.status live event")
	}
	if a.int(`SELECT count(*) FROM accounts WHERE id = $1 AND keywords_supported AND 'IDLE' = ANY(capabilities)`, a.id) != 1 {
		t.Error("capabilities / keywords_supported not recorded")
	}
	batches.Range(func(k, v any) bool { t.Logf("phase %s: %d messages", k, *v.(*int)); return true })
}

// TestRestartDuringInitialSync is the DoD of phase 2: stopping the worker at
// a random point of the initial sync and starting a new one on the same
// database ends with exactly the same mirror as an uninterrupted run.
func TestRestartDuringInitialSync(t *testing.T) {
	mb := newMailbox(t, imaptest.Full)
	mb.create("Projekty")
	mb.populate(spec{"INBOX", 500}, spec{"Sent", 150}, spec{"Projekty", 150})

	base := mb.account(mb.pass)
	br := base.start()
	base.waitInitialDone(3 * time.Minute)
	want := base.counts()
	br.stop() // one Dovecot user: stay below mail_max_userip_connections
	t.Logf("uninterrupted: %+v", want)

	for rep := range 5 {
		a := mb.account(mb.pass)
		stopAfter := 1 + rand.IntN(12)
		ctx, cancel := context.WithCancel(context.Background())
		var mu stdsync.Mutex
		seen := 0
		a.deps.Hooks.AfterBatch = func(phase string, k int) {
			mu.Lock()
			defer mu.Unlock()
			seen++
			if seen == stopAfter {
				cancel()
			}
		}
		r := a.start()
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Minute):
			t.Fatalf("rep %d: never reached batch %d", rep, stopAfter)
		}
		r.stop()
		mid := a.counts()
		a.deps.Hooks.AfterBatch = nil
		r2 := a.start()
		a.waitInitialDone(3 * time.Minute)
		got := a.counts()
		r2.stop()
		t.Logf("rep %d: stopped after batch %d at %+v, final %+v", rep, stopAfter, mid, got)
		if got != want {
			t.Errorf("rep %d: final %+v, uninterrupted %+v", rep, got, want)
		}
		if d := a.int(`SELECT count(*) FROM (SELECT dedupe_key FROM messages WHERE account_id = $1 GROUP BY 1 HAVING count(*) > 1) x`, a.id); d != 0 {
			t.Errorf("rep %d: %d duplicated messages", rep, d)
		}
	}
}

// TestLabelCountsAtLive: going live and completing the initial sync each
// write one label.counts event with every label of the account (the
// counters of a newly added account reach clients without a refetch).
func TestLabelCountsAtLive(t *testing.T) {
	mb := newMailbox(t, imaptest.Full)
	mb.populate(spec{"INBOX", 40}, spec{"Sent", 10})
	a := mb.account(mb.pass)
	a.start()
	a.waitInitialDone(2 * time.Minute)

	labels := a.int(`SELECT count(*) FROM labels WHERE account_id = $1`, a.id)
	inboxTotal := a.int(`SELECT count(*) FROM thread_labels tl JOIN labels l ON l.id = tl.label_id
WHERE l.account_id = $1 AND l.system_key = 'INBOX' AND NOT tl.hidden`, a.id)
	live := a.int(`SELECT max(seq) FROM events WHERE user_id = $1 AND type = 'account.status' AND payload->>'state' = 'live'`, a.userID)
	full := `FROM events WHERE user_id = $1 AND type = 'label.counts' AND (payload->>'accountId')::bigint = $2
AND jsonb_array_length(payload->'labels') = $3`
	// The first full event after live (the live one) and the last full event
	// (the completion one): each with every label.
	if n := a.int(`SELECT count(*) `+full+` AND seq > $4`, a.userID, a.id, labels, live); n < 2 {
		t.Fatalf("%d label.counts events with all %d labels after live, want >= 2", n, labels)
	}
	var total int
	must(t, testPool.QueryRow(context.Background(), `
SELECT (x.v->>'threadsTotal')::int FROM (SELECT payload `+full+` ORDER BY seq DESC LIMIT 1) e,
     jsonb_array_elements(e.payload->'labels') x(v)
WHERE (x.v->>'id')::bigint = (SELECT id FROM labels WHERE account_id = $2 AND system_key = 'INBOX')`,
		a.userID, a.id, labels).Scan(&total))
	if total != inboxTotal || total == 0 {
		t.Errorf("last label.counts: INBOX threadsTotal %d, want %d", total, inboxTotal)
	}
}
