package mail_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	silomail "github.com/silopost/silo-server/internal/mail"
)

func (e *env) threadOf(hdr string) int64 {
	e.t.Helper()
	var id int64
	must(e.t, e.pool.QueryRow(e.ctx, `SELECT thread_id FROM messages WHERE account_id = $1 AND message_id_hdr = $2 LIMIT 1`, e.accountID, hdr).Scan(&id))
	return id
}

func TestRecomputeThreadAggregates(t *testing.T) {
	e := newEnv(t)
	orig := simple("Anna <anna@example.com>", "dev@silo.test", "Plan", "rc1@example.com", baseTime)
	mine := simple("Dev <dev@silo.test>", "anna@example.com", "Re: Plan", "rc2@silo.test", baseTime.Add(time.Hour), "In-Reply-To", "<rc1@example.com>")
	last := simple("Bartek <bartek@example.com>", "dev@silo.test", "Re: Plan", "rc3@example.com", baseTime.Add(2*time.Hour), "In-Reply-To", "<rc2@silo.test>", "References", "<rc1@example.com> <rc2@silo.test>")
	e.ingest("INBOX", e.fetched("INBOX", orig, `\Seen`), e.fetched("INBOX", last))
	e.ingest("Sent", e.fetched("Sent", mine, `\Seen`))
	tid := e.threadOf("rc1@example.com")
	var count, unread int
	var snippet, subject string
	var participants []byte
	var lastAt time.Time
	must(t, e.pool.QueryRow(e.ctx, `SELECT message_count, unread_count, snippet, subject, participants, last_message_at FROM threads WHERE id = $1`, tid).
		Scan(&count, &unread, &snippet, &subject, &participants, &lastAt))
	if count != 3 || unread != 1 || subject != "Plan" || !strings.Contains(snippet, "rc3@example.com") || !lastAt.Equal(baseTime.Add(2*time.Hour)) {
		t.Fatalf("thread = count %d unread %d subject %q snippet %q last %v", count, unread, subject, snippet, lastAt)
	}
	var parts []struct {
		Email  string `json:"email"`
		Name   string `json:"name"`
		Unread bool   `json:"unread"`
	}
	must(t, json.Unmarshal(participants, &parts))
	if len(parts) != 3 || parts[0].Email != "anna@example.com" || parts[0].Unread || !parts[2].Unread || parts[2].Name != "Bartek" {
		t.Fatalf("participants = %s", participants)
	}
	// thread_labels: INBOX has 2 messages (1 unread), SENT 1 (read).
	check := func(label string, wantCount, wantUnread int) {
		t.Helper()
		var c, u int
		must(t, e.pool.QueryRow(e.ctx, `SELECT message_count, unread_count FROM thread_labels WHERE thread_id = $1 AND label_id = $2`, tid, e.labelID(label)).Scan(&c, &u))
		if c != wantCount || u != wantUnread {
			t.Errorf("%s: count %d unread %d, want %d %d", label, c, u, wantCount, wantUnread)
		}
	}
	check("INBOX", 2, 1)
	check("SENT", 1, 0)
	e.reconcileClean()
}

func TestHiddenWhenEntirelyInTrash(t *testing.T) {
	e := newEnv(t)
	a := e.fetched("INBOX", simple("a@example.com", "dev@silo.test", "Kosz", "h1@example.com", baseTime), `\Seen`, "Praca")
	b := e.fetched("INBOX", simple("b@example.com", "dev@silo.test", "Re: Kosz", "h2@example.com", baseTime.Add(time.Hour), "In-Reply-To", "<h1@example.com>"), `\Seen`)
	e.ingest("INBOX", a, b)
	tid := e.threadOf("h1@example.com")
	hidden := func(label string) (bool, bool) {
		var h bool
		err := e.pool.QueryRow(e.ctx, `SELECT hidden FROM thread_labels tl JOIN labels l ON l.id = tl.label_id WHERE tl.thread_id = $1 AND l.name = $2`, tid, label).Scan(&h)
		return h, err == nil
	}
	if h, _ := hidden("INBOX"); h {
		t.Fatal("INBOX hidden before trashing")
	}
	// Both messages end up in Trash (moved by another client): vanish + appear.
	must(t, e.tx(func(tx pgx.Tx) error {
		_, err := silomail.LocationVanished(e.ctx, tx, e.folders["INBOX"], []uint32{a.UID, b.UID})
		return err
	}))
	ta := e.fetched("Trash", simple("a@example.com", "dev@silo.test", "Kosz", "h1@example.com", baseTime), `\Seen`, "Praca")
	e.ingest("Trash", ta)
	// Only one of two in Trash: the other is orphaned and ignored, so the thread
	// is entirely trashed already.
	if h, ok := hidden("Praca"); !ok || !h {
		t.Fatalf("user label of an entirely trashed thread must be hidden (present=%v hidden=%v)", ok, h)
	}
	if h, ok := hidden("TRASH"); !ok || h {
		t.Fatalf("TRASH itself must stay visible (present=%v hidden=%v)", ok, h)
	}
	if _, ok := hidden("INBOX"); ok {
		t.Fatal("INBOX row should be gone")
	}
	// Now the second message returns to INBOX: the thread is no longer entirely trashed.
	tb := e.fetched("INBOX", simple("b@example.com", "dev@silo.test", "Re: Kosz", "h2@example.com", baseTime.Add(time.Hour), "In-Reply-To", "<h1@example.com>"), `\Seen`)
	e.ingest("INBOX", tb)
	if h, ok := hidden("Praca"); !ok || h {
		t.Fatalf("label visible again expected (present=%v hidden=%v)", ok, h)
	}
	e.reconcileClean()
}

func TestReconcileDetectsDrift(t *testing.T) {
	e := newEnv(t)
	e.ingest("INBOX", e.fetched("INBOX", simple("a@example.com", "dev@silo.test", "Dryf", "dr1@example.com", baseTime)))
	e.reconcileClean()
	tid := e.threadOf("dr1@example.com")

	// 1. a stale thread aggregate
	_, err := e.pool.Exec(e.ctx, `UPDATE threads SET unread_count = 5 WHERE id = $1`, tid)
	must(t, err)
	if ds, err := silomail.Reconcile(e.ctx, e.pool, e.accountID); err != nil || len(ds) != 1 || ds[0].Kind != silomail.DiscThreadAggregate {
		t.Fatalf("aggregate drift: %v %v", ds, err)
	}
	must(t, e.tx(func(tx pgx.Tx) error { return silomail.RecomputeThread(e.ctx, tx, tid) }))
	e.reconcileClean()

	// 2. a stale thread_labels row
	_, err = e.pool.Exec(e.ctx, `UPDATE thread_labels SET message_count = 9 WHERE thread_id = $1`, tid)
	must(t, err)
	if ds, err := silomail.Reconcile(e.ctx, e.pool, e.accountID); err != nil || len(ds) != 1 || ds[0].Kind != silomail.DiscThreadLabel {
		t.Fatalf("thread_labels drift: %v %v", ds, err)
	}
	must(t, e.tx(func(tx pgx.Tx) error { return silomail.RecomputeThread(e.ctx, tx, tid) }))
	e.reconcileClean()

	// 3. a system label that disagrees with message_locations
	_, err = e.pool.Exec(e.ctx, `INSERT INTO message_labels (message_id, label_id, user_id)
SELECT m.id, $2, m.user_id FROM messages m WHERE m.account_id = $1`, e.accountID, e.labelID("TRASH"))
	must(t, err)
	ds, err := silomail.Reconcile(e.ctx, e.pool, e.accountID)
	if err != nil || len(ds) < 1 {
		t.Fatalf("label drift: %v %v", ds, err)
	}
	found := false
	for _, d := range ds {
		found = found || d.Kind == silomail.DiscMessageLabel
	}
	if !found {
		t.Fatalf("no message_label discrepancy in %v", ds)
	}
	// 4. wrong is_unread
	must(t, e.tx(func(tx pgx.Tx) error {
		var id int64
		if err := tx.QueryRow(e.ctx, `SELECT id FROM messages WHERE account_id = $1`, e.accountID).Scan(&id); err != nil {
			return err
		}
		return silomail.DeriveSystemLabels(e.ctx, tx, id)
	}))
	_, err = e.pool.Exec(e.ctx, `UPDATE messages SET is_unread = false WHERE account_id = $1`, e.accountID)
	must(t, err)
	ds, err = silomail.Reconcile(e.ctx, e.pool, e.accountID)
	must(t, err)
	if !slices.ContainsFunc(ds, func(d silomail.Discrepancy) bool { return d.Kind == silomail.DiscMessageUnread }) {
		t.Fatalf("no is_unread discrepancy in %v", ds)
	}
}

func TestEventsWritten(t *testing.T) {
	e := newEnv(t)
	e.ingest("INBOX", e.fetched("INBOX", simple("Anna <a@example.com>", "dev@silo.test", "Zdarzenia", "ev1@example.com", baseTime)))
	types := e.names(`SELECT type FROM events WHERE user_id = $1 ORDER BY seq`, e.userID)
	if !slices.Equal(types, []string{"message.new", "thread.updated", "label.counts"}) {
		t.Fatalf("events = %v", types)
	}
	var payload string
	must(t, e.pool.QueryRow(e.ctx, `SELECT payload::text FROM events WHERE user_id = $1 AND type = 'message.new'`, e.userID).Scan(&payload))
	var mn struct {
		AccountID int64   `json:"accountId"`
		LabelIDs  []int64 `json:"labelIds"`
		Subject   string  `json:"subject"`
		Snippet   string  `json:"snippet"`
	}
	must(t, json.Unmarshal([]byte(payload), &mn))
	if mn.AccountID != e.accountID || !slices.Contains(mn.LabelIDs, e.labelID("INBOX")) || mn.Subject != "Zdarzenia" || !strings.Contains(mn.Snippet, "ev1@example.com") {
		t.Fatalf("message.new = %s", payload)
	}
	must(t, e.pool.QueryRow(e.ctx, `SELECT payload::text FROM events WHERE user_id = $1 AND type = 'label.counts'`, e.userID).Scan(&payload))
	var lc struct {
		Labels []struct {
			ID            int64 `json:"id"`
			ThreadsUnread int   `json:"threadsUnread"`
			ThreadsTotal  int   `json:"threadsTotal"`
		} `json:"labels"`
	}
	must(t, json.Unmarshal([]byte(payload), &lc))
	if len(lc.Labels) != 1 || lc.Labels[0].ID != e.labelID("INBOX") || lc.Labels[0].ThreadsUnread != 1 || lc.Labels[0].ThreadsTotal != 1 {
		t.Fatalf("label.counts = %s", payload)
	}
	// Re-ingesting writes thread.updated only if something is recomputed; flag
	// updates emit thread.updated + label.counts.
	before := e.count(`SELECT count(*) FROM events WHERE user_id = $1`, e.userID)
	must(t, e.tx(func(tx pgx.Tx) error {
		return silomail.ApplyFlags(e.ctx, tx, e.folders["INBOX"], 1, []string{`\Seen`}, 2)
	}))
	if after := e.count(`SELECT count(*) FROM events WHERE user_id = $1`, e.userID); after != before+2 {
		t.Fatalf("flag change events: %d -> %d, want +2", before, after)
	}
}
