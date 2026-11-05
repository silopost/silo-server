package sync_test

import (
	"context"
	"math/rand/v2"
	"os"
	"strconv"
	stdsync "sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

// initialSize is the mailbox of TestInitialSync: 2000 messages by default,
// SILO_SYNC_TEST_MESSAGES overrides it (e.g. 10000 for the large run).
func initialSize() int {
	if n, err := strconv.Atoi(os.Getenv("SILO_SYNC_TEST_MESSAGES")); err == nil && n > 0 {
		return n
	}
	return 2000
}

func splitSpecs(n int) []spec {
	return []spec{{"INBOX", n * 60 / 100}, {"Sent", n * 25 / 100}, {"Projekty", n - n*60/100 - n*25/100}}
}

func TestInitialSync(t *testing.T) {
	mb := newMailbox(t, imaptest.Full)
	mb.create("Projekty")
	n := initialSize()
	t0 := time.Now()
	recent := mb.populate(splitSpecs(n)...)
	t.Logf("populated %d messages in %s", n, time.Since(t0).Round(time.Millisecond))

	a := mb.account(mb.pass)
	var batches stdsync.Map
	a.deps.Hooks.AfterBatch = func(phase string, k int) {
		v, _ := batches.LoadOrStore(phase, new(int))
		*v.(*int) += k
	}
	t1 := time.Now()
	a.start()
	a.waitState("live", 2*time.Minute)
	live := time.Since(t1)
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
	done := a.waitInitialDone(5 * time.Minute)
	t.Logf("initial sync of %d messages: live after %s, complete after %s", n, live.Round(time.Millisecond), (time.Since(t1)).Round(time.Millisecond))
	_ = done

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
