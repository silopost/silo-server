package sync_test

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/filters"
	"github.com/silopost/silo-server/internal/ops"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
	"github.com/silopost/silo-server/internal/worker"
)

// mailFrom is a message from the given sender with a body.
func mailFrom(id int, from, subject, body string, date time.Time) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\nTo: silo@silo.test\r\nSubject: %s\r\nMessage-ID: <f%d@silo.test>\r\nDate: %s\r\n", from, subject, id, date.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Content-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", body)
	return b.Bytes()
}

// envBodies fetches bodies through the env's current Manager.
type envBodies struct {
	e     *opsEnv
	calls atomic.Int32
}

func (b *envBodies) FetchBody(ctx context.Context, accountID, messageID int64) error {
	b.calls.Add(1)
	return b.e.mgr.FetchBody(ctx, accountID, messageID)
}

// TestFilters is the DoD of phase 4 for filters (plan 3.11) against Dovecot:
// "from:x -> skip the inbox + label Y" works for new mail (on the server:
// Archive + keyword) and backwards after apply; mail older than
// filters_since is left alone by the automatic run; a filter on a body word
// waits for the body (the timeout itself: api TestFilters).
func TestFilters(t *testing.T) {
	ctx := context.Background()
	e := newOpsEnv(t, imaptest.Full, time.Second, spec{"INBOX", 2})
	bodies := &envBodies{e: e}
	const bodyWait = 3 * time.Second
	fsvc := filters.New(filters.Options{Pool: testPool, Ops: e.svc, Bodies: bodies, BodyWait: bodyWait})
	e.extra = func(w *worker.Worker) { fsvc.RegisterJobs(w) }
	e.restart()
	e.a.waitState("live", 30*time.Second)
	waitFor(t, 10*time.Second, "account worker", func() bool { return e.mgr.Worker(e.a.id) != nil })
	if n := e.a.int(`SELECT count(*) FROM accounts WHERE id = $1 AND filters_since IS NOT NULL`, e.a.id); n != 1 {
		t.Fatal("filters_since not set after going live")
	}
	// Pin filters_since an hour back: Postgres (container) and the INTERNALDATE
	// given by the test (host clock) may drift by seconds.
	_, err := testPool.Exec(ctx, `UPDATE accounts SET filters_since = now() - interval '1 hour' WHERE id = $1`, e.a.id)
	must(t, err)
	if n := e.a.int(`SELECT count(*) FROM accounts WHERE id = $1 AND keywords_supported`, e.a.id); n != 1 {
		t.Fatal("Dovecot full profile should keep keywords")
	}

	y, err := e.svc.CreateLabel(ctx, ops.CreateLabel{UserID: e.a.userID, AccountID: e.a.id, Name: "Y", Type: "user"})
	must(t, err)
	var kw string
	must(t, testPool.QueryRow(ctx, `SELECT imap_keyword FROM labels WHERE id = $1`, y.ID).Scan(&kw))
	f, err := fsvc.Create(ctx, e.a.userID, filters.Input{AccountID: e.a.id, Criteria: "from:x@news.test", Enabled: true,
		Actions: filters.Actions{Archive: true, AddLabelIDs: []int64{y.ID}}})
	must(t, err)

	sync := func() { e.mgr.Worker(e.a.id).RequestResync("incremental") }
	synced := func(hdr string) int64 {
		t.Helper()
		var id int64
		waitFor(t, 30*time.Second, hdr+" synced", func() bool {
			var ok bool
			id, _, _, ok = e.a.msgByHdr(hdr)
			return ok
		})
		return id
	}
	filterJobsDone := func() {
		t.Helper()
		waitFor(t, 30*time.Second, "filter jobs done", func() bool {
			return e.a.int(`SELECT count(*) FROM jobs WHERE kind IN ('filter_eval', 'filter_apply') AND status = 'pending'`) == 0
		})
	}
	// archiveName is the folder of role archive ("Archive", created by Silo on
	// the first archive), "?" before it exists.
	archiveName := func() string {
		name := "?"
		_ = testPool.QueryRow(ctx, `SELECT name FROM mailbox_folders WHERE account_id = $1 AND role = 'archive'`, e.a.id).Scan(&name)
		return name
	}

	// Mail from before filters_since (INTERNALDATE two days ago).
	appendMsg(t, e.other, "INBOX", mailFrom(1, "News <x@news.test>", "Old news", "old", time.Now().Add(-48*time.Hour)), nil, time.Now().Add(-48*time.Hour))
	sync()
	oldID := synced("f1@silo.test")

	t.Run("new mail: Archive and keyword on the server", func(t *testing.T) {
		appendMsg(t, e.other, "INBOX", mailFrom(2, "News <x@news.test>", "Fresh news", "new", time.Now()), nil, time.Now())
		appendMsg(t, e.other, "INBOX", mailFrom(3, "Jan <jan@firma.pl>", "Hello", "hi", time.Now()), nil, time.Now())
		sync()
		id := synced("f2@silo.test")
		other := synced("f3@silo.test")
		waitFor(t, 30*time.Second, "f2 in Archive on the server", func() bool {
			return slices.Equal(e.serverFolders("f2@silo.test"), []string{archiveName()})
		})
		e.waitNoJobs(15 * time.Second)
		waitFor(t, 15*time.Second, "keyword of Y on the server", func() bool { return slices.Contains(e.serverFlags("f2@silo.test"), kw) })
		if l := e.a.labelsOf(id); l["INBOX"] || !l["ARCHIVE"] || !l["Y"] {
			t.Errorf("labels of the filtered message: %v", l)
		}
		if l := e.a.labelsOf(other); !l["INBOX"] || l["Y"] {
			t.Errorf("labels of an unmatched message: %v", l)
		}
		if f := e.serverFolders("f3@silo.test"); !slices.Equal(f, []string{"INBOX"}) {
			t.Errorf("unmatched message moved: %v", f)
		}
		if n := e.a.int(`SELECT count(*) FROM operations WHERE user_id = $1`, e.a.userID); n != 0 {
			t.Errorf("filter actions recorded as undoable operations: %d", n)
		}
	})

	t.Run("mail before filters_since is left alone", func(t *testing.T) {
		filterJobsDone()
		if l := e.a.labelsOf(oldID); !l["INBOX"] || l["Y"] {
			t.Errorf("labels of old mail: %v", l)
		}
		if f := e.serverFolders("f1@silo.test"); !slices.Equal(f, []string{"INBOX"}) {
			t.Errorf("old mail moved: %v", f)
		}
	})

	t.Run("apply moves existing mail", func(t *testing.T) {
		r, err := fsvc.Apply(ctx, e.a.userID, f.ID, "test")
		must(t, err)
		if r.MatchedEstimate == nil || *r.MatchedEstimate != 2 { // f1 and f2
			t.Errorf("matchedEstimate = %v", r.MatchedEstimate)
		}
		filterJobsDone()
		waitFor(t, 30*time.Second, "f1 in Archive on the server", func() bool {
			return slices.Equal(e.serverFolders("f1@silo.test"), []string{archiveName()})
		})
		e.waitNoJobs(15 * time.Second)
		if !slices.Contains(e.serverFlags("f1@silo.test"), kw) {
			t.Errorf("no keyword on f1: %v", e.serverFlags("f1@silo.test"))
		}
		if l := e.a.labelsOf(oldID); l["INBOX"] || !l["ARCHIVE"] || !l["Y"] {
			t.Errorf("labels of old mail after apply: %v", l)
		}
		if n := e.a.int(`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'filter.applied' AND payload->>'messages' = '2'`, e.a.userID); n != 1 {
			t.Error("no filter.applied")
		}
		must(t, fsvc.Delete(ctx, e.a.userID, f.ID))
	})

	t.Run("a body word waits for the body", func(t *testing.T) {
		bf, err := fsvc.Create(ctx, e.a.userID, filters.Input{AccountID: e.a.id, Criteria: "zebrakod", Enabled: true,
			Actions: filters.Actions{Star: true}})
		must(t, err)
		appendMsg(t, e.other, "INBOX", mailFrom(4, "Ala <ala@firma.pl>", "Plain subject", "Kod: zebrakod 42", time.Now()), nil, time.Now())
		sync()
		id := synced("f4@silo.test")
		filterJobsDone()
		if l := e.a.labelsOf(id); !l["STARRED"] {
			_, _, state, _ := e.a.msgByHdr("f4@silo.test")
			t.Errorf("body word not matched: labels %v, body_state %s, fetches %d", l, state, bodies.calls.Load())
		}
		waitFor(t, 15*time.Second, "\\Flagged on the server", func() bool { return slices.Contains(e.serverFlags("f4@silo.test"), `\Flagged`) })

		must(t, fsvc.Delete(ctx, e.a.userID, bf.ID))
	})
}
