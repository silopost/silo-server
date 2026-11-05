package sync_test

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/silopost/silo-server/internal/drafts"
	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/ops"
	silosync "github.com/silopost/silo-server/internal/sync"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

// labelPairs returns the set of (message_id_hdr, label) of the user labels
// of the account; byKeyword names the label by its IMAP keyword (labels
// without one are left out).
func (e *opsEnv) labelPairs(byKeyword bool) map[string]bool {
	e.t.Helper()
	rows, err := testPool.Query(context.Background(), `
SELECT m.message_id_hdr || ' | ' || CASE WHEN $2 THEN l.imap_keyword ELSE l.name END
FROM message_labels ml JOIN labels l ON l.id = ml.label_id JOIN messages m ON m.id = ml.message_id
WHERE l.account_id = $1 AND l.type = 'user' AND (NOT $2 OR l.imap_keyword IS NOT NULL)`, e.a.id, byKeyword)
	must(e.t, err)
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var s string
		must(e.t, rows.Scan(&s))
		out[s] = true
	}
	must(e.t, rows.Err())
	return out
}

// threadPartition maps every message_id_hdr of the account to the sorted
// message_id_hdrs of its thread.
func (e *opsEnv) threadPartition() map[string]string {
	e.t.Helper()
	rows, err := testPool.Query(context.Background(), `
SELECT m.message_id_hdr,
       (SELECT string_agg(m2.message_id_hdr, ',' ORDER BY m2.message_id_hdr) FROM messages m2
        WHERE m2.thread_id = m.thread_id AND m2.orphaned_at IS NULL)
FROM messages m WHERE m.account_id = $1 AND m.orphaned_at IS NULL`, e.a.id)
	must(e.t, err)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		must(e.t, rows.Scan(&k, &v))
		out[k] = v
	}
	must(e.t, rows.Err())
	return out
}

func diffSets(t *testing.T, what string, before, after map[string]bool) {
	t.Helper()
	for k := range before {
		if !after[k] {
			t.Errorf("%s: lost %s", what, k)
		}
	}
	for k := range after {
		if !before[k] {
			t.Errorf("%s: new %s", what, k)
		}
	}
}

// TestRebuildCache is DoD (b) of phase 4 (plan 3.3, 3.7): rebuilding the
// cache of an account from scratch (resync rebuild) gives back every user
// label from the IMAP keywords - the same (message_id_hdr, label) set - and the
// same split into threads. Snoozes, a label that is local only and a local
// draft survive. Variant "without metadata" first deletes the user labels
// (no export of metadata): they come back from the keywords, named after
// them.
func TestRebuildCache(t *testing.T) {
	for _, v := range []struct {
		name       string
		dropLabels bool
	}{{"keep labels", false}, {"without metadata", true}} {
		t.Run(v.name, func(t *testing.T) { testRebuildCache(t, v.dropLabels) })
	}
}

func testRebuildCache(t *testing.T, dropLabels bool) {
	ctx := context.Background()
	// 32 messages in 3 folders; every fifth one answers the one before (5
	// threads of two), plus a reply in Faktury to m1 in INBOX.
	e := newOpsEnv(t, imaptest.Full, 300*time.Millisecond, spec{"INBOX", 14}, spec{"Faktury", 10}, spec{"Sent", 8})
	appendMsg(t, e.other, "Faktury", msg(100, 1, "Re: Message 1", time.Now().Add(-time.Hour)), nil, time.Now().Add(-time.Hour))
	e.mgr.Worker(e.a.id).RequestResync(silosync.ResyncIncremental)
	waitFor(t, 70*time.Second, "33 messages", func() bool {
		return e.a.int(`SELECT count(*) FROM messages WHERE account_id = $1`, e.a.id) == 33
	})
	_, th1 := e.thread("m1@silo.test")
	if e.a.int(`SELECT count(*) FROM messages WHERE thread_id = $1`, th1) != 2 {
		t.Fatal("cross-folder reply not threaded")
	}

	// Labels set in Silo: one with a colour, one nested, one with a slug.
	blue := "#4986e7"
	projekty := e.createLabel("Projekty", nil, &blue)
	alfa := e.createLabel("Projekty/Alfa", &projekty, nil)
	zazolc := e.createLabel("Zażółć", nil, nil)
	e.modify("m1@silo.test", []ops.LabelRef{{ID: projekty}}, nil)
	e.modify("m6@silo.test", []ops.LabelRef{{ID: alfa}, {ID: projekty}}, nil)
	e.modify("m12@silo.test", []ops.LabelRef{{ID: zazolc}}, nil)
	e.modify("m18@silo.test", []ops.LabelRef{{ID: zazolc}}, nil)
	e.modify("m27@silo.test", []ops.LabelRef{{ID: alfa}}, nil)
	// A tag of another client, in two folders.
	e.storeKeyword("INBOX", "m3@silo.test", imap.StoreFlagsAdd, "Klient_ACME")
	e.storeKeyword("Faktury", "m20@silo.test", imap.StoreFlagsAdd, "Klient_ACME")
	e.waitNoJobs(15 * time.Second)
	waitFor(t, 70*time.Second, "keywords on the server and tags imported", func() bool {
		return slices.Contains(e.flagsIn("Sent", "m27@silo.test"), "Projekty_Alfa") &&
			e.a.int(`SELECT count(*) FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE l.account_id = $1 AND l.name = 'Klient_ACME'`, e.a.id) == 2
	})

	// Local state the server does not keep: a label that is local only, a
	// snooze and a draft.
	var prywatne int64
	if !dropLabels {
		prywatne = e.createLabel("Prywatne", nil, nil)
		_, err := testPool.Exec(ctx, `UPDATE labels SET keyword_state = 'local_only' WHERE id = $1`, prywatne)
		must(t, err)
		e.modify("m7@silo.test", []ops.LabelRef{{ID: prywatne}}, nil)
	}
	_, th2 := e.thread("m2@silo.test")
	wake := time.Now().Add(3 * time.Hour).Truncate(time.Millisecond)
	_, err := e.svc.Snooze(ctx, e.a.userID, th2, wake, "test")
	must(t, err)
	waitFor(t, 15*time.Second, "m2 in Snoozed", func() bool { return slices.Equal(e.serverFolders("m2@silo.test"), []string{"Snoozed"}) })
	dsvc := drafts.New(drafts.Options{Pool: testPool, Blobs: e.a.deps.Blobs, SyncDelay: 200 * time.Millisecond, MaxUploadBytes: 1 << 20})
	d, err := dsvc.Create(ctx, drafts.CreateParams{UserID: e.a.userID, AccountID: e.a.id, Origin: "test"})
	must(t, err)
	_, err = dsvc.Update(ctx, drafts.UpdateParams{UserID: e.a.userID, ID: d.ID, Version: 1, Subject: "Szkic przed odbudową",
		BodyHTML: "<p>Treść</p>", To: []mail.Address{{Email: "jan@firma.pl"}}})
	must(t, err)
	waitFor(t, 20*time.Second, "draft in Drafts", func() bool {
		return e.a.int(`SELECT count(*) FROM message_locations l JOIN mailbox_folders f ON f.id = l.folder_id WHERE l.message_id = $1 AND f.role = 'drafts'`, d.ID) == 1
	})
	e.waitNoJobs(15 * time.Second)
	e.mgr.Worker(e.a.id).RequestResync(silosync.ResyncIncremental)
	time.Sleep(time.Second)

	labelsBefore := e.labelPairs(dropLabels)
	threadsBefore := e.threadPartition()
	messagesBefore := e.a.int(`SELECT count(*) FROM messages WHERE account_id = $1`, e.a.id)
	if len(labelsBefore) < 8 || len(threadsBefore) != messagesBefore {
		t.Fatalf("seed: %d label pairs, %d messages in threads of %d", len(labelsBefore), len(threadsBefore), messagesBefore)
	}
	var draftHdr string
	must(t, testPool.QueryRow(ctx, `SELECT message_id_hdr FROM messages WHERE id = $1`, d.ID).Scan(&draftHdr))

	if dropLabels {
		_, err := testPool.Exec(ctx, `DELETE FROM labels WHERE account_id = $1 AND type = 'user'`, e.a.id)
		must(t, err)
	}
	maxMsg := e.a.int(`SELECT max(id)::int FROM messages WHERE account_id = $1`, e.a.id)
	must(t, silosync.NewService(testPool).RequestResync(ctx, e.a.userID, e.a.id, silosync.ResyncRebuild))
	start := time.Now()
	waitFor(t, 120*time.Second, "rebuild finished", func() bool {
		return e.a.int(`SELECT count(*) FROM account_rebuilds WHERE account_id = $1`, e.a.id) == 0 &&
			e.a.int(`SELECT count(*) FROM messages WHERE account_id = $1 AND id > $2`, e.a.id, maxMsg) == messagesBefore-1 &&
			e.a.state() == silosync.StateLive &&
			e.a.int(`SELECT count(*) FROM accounts WHERE id = $1 AND initial_sync_done_at IS NOT NULL`, e.a.id) == 1
	})
	t.Logf("rebuild of %d messages done in %s", messagesBefore, time.Since(start).Round(time.Millisecond))
	e.waitNoJobs(10 * time.Second)

	if n := e.a.int(`SELECT count(*) FROM messages WHERE account_id = $1`, e.a.id); n != messagesBefore {
		t.Errorf("messages after rebuild = %d, want %d", n, messagesBefore)
	}
	diffSets(t, "user labels", labelsBefore, e.labelPairs(dropLabels))
	if threadsAfter := e.threadPartition(); !maps.Equal(threadsBefore, threadsAfter) {
		for k, v := range threadsBefore {
			if threadsAfter[k] != v {
				t.Errorf("thread of %s: %q, before %q", k, threadsAfter[k], v)
			}
		}
	}
	if dropLabels {
		for _, kw := range []string{"Projekty", "Projekty_Alfa", "Zazolc", "Klient_ACME"} {
			if _, k, state, ok := e.userLabel(kw); !ok || k != kw || state != "mirrored" {
				t.Errorf("label %s from the keyword: %q %q %v", kw, k, state, ok)
			}
		}
	} else {
		var color *string
		var parent *int64
		must(t, testPool.QueryRow(ctx, `SELECT (SELECT color_bg FROM labels WHERE id = $1), (SELECT parent_id FROM labels WHERE id = $2)`, projekty, alfa).Scan(&color, &parent))
		if color == nil || *color != blue || parent == nil || *parent != projekty {
			t.Errorf("label metadata: colour %v parent %v", color, parent)
		}
		id7, _ := e.thread("m7@silo.test")
		if !e.a.labelsOf(id7)["Prywatne"] {
			t.Error("local-only label not restored")
		}
	}
	// The snooze is back on the new thread of m2, with its wake time.
	id2, nth2 := e.thread("m2@silo.test")
	var at time.Time
	if err := testPool.QueryRow(ctx, `SELECT wake_at FROM snoozes WHERE thread_id = $1`, nth2).Scan(&at); err != nil || !at.Equal(wake) {
		t.Errorf("snooze after rebuild: %v %v (want %v)", at, err, wake)
	}
	if l := e.a.labelsOf(id2); !l["SNOOZED"] || l["INBOX"] {
		t.Errorf("labels of the snoozed message: %v", l)
	}
	// The local draft is kept and attached to its server copy (no duplicate).
	if e.a.int(`SELECT count(*) FROM drafts WHERE message_id = $1`, d.ID) != 1 ||
		e.a.int(`SELECT count(*) FROM messages WHERE account_id = $1 AND message_id_hdr = $2`, e.a.id, draftHdr) != 1 ||
		e.a.int(`SELECT count(*) FROM message_locations l JOIN mailbox_folders f ON f.id = l.folder_id WHERE l.message_id = $1 AND f.role = 'drafts'`, d.ID) != 1 {
		t.Error("local draft not kept or not re-attached")
	}
	if e.a.int(`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'resync'`, e.a.userID) == 0 {
		t.Error("no resync event")
	}
	e.reconcile()
}
