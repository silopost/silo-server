package sync_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	silosync "github.com/silopost/silo-server/internal/sync"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

// liveAccount is a mailbox with a few INBOX messages and the folder Faktury,
// fully synced by a running worker.
func liveAccount(t *testing.T, p imaptest.Profile) (*mailbox, *account, *running) {
	mb := newMailbox(t, p)
	mb.create("Faktury")
	mb.populate(spec{"INBOX", 6}, spec{"Faktury", 2})
	a := mb.account(mb.pass)
	r := a.start()
	a.waitInitialDone(time.Minute)
	return mb, a, r
}

// msgByHdr returns id, thread and body_state of a message by Message-ID.
func (a *account) msgByHdr(hdr string) (id, thread int64, body string, ok bool) {
	err := testPool.QueryRow(context.Background(), `SELECT id, thread_id, body_state FROM messages WHERE account_id = $1 AND message_id_hdr = $2`, a.id, hdr).
		Scan(&id, &thread, &body)
	return id, thread, body, err == nil
}

func (a *account) labelsOf(msgID int64) map[string]bool {
	rows, err := testPool.Query(context.Background(), `
SELECT COALESCE(l.system_key, l.name) FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE ml.message_id = $1`, msgID)
	must(a.t, err)
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		must(a.t, rows.Scan(&n))
		out[n] = true
	}
	return out
}

func selectInbox(t *testing.T, c *imapclient.Client) {
	t.Helper()
	_, err := c.Select("INBOX", nil).Wait()
	must(t, err)
}

func uidOf(t *testing.T, c *imapclient.Client, hdr string) imap.UID {
	t.Helper()
	data, err := c.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: hdr}}, NotFlag: []imap.Flag{imap.FlagDeleted}}, nil).Wait()
	must(t, err)
	uids := data.AllUIDs()
	if len(uids) != 1 {
		t.Fatalf("search %s: %v", hdr, uids)
	}
	return uids[0]
}

// TestIncremental runs the incremental scenarios on every Dovecot profile:
// full (CONDSTORE path), no-condstore (full flag comparison) and no-move
// (another client moves with COPY + STORE + EXPUNGE).
func TestIncremental(t *testing.T) {
	for _, p := range []imaptest.Profile{imaptest.Full, imaptest.NoCondstore, imaptest.NoMove} {
		t.Run(string(p), func(t *testing.T) { testIncremental(t, p) })
	}
}

func testIncremental(t *testing.T, p imaptest.Profile) {
	mb, a, _ := liveAccount(t, p)
	other := mb.client()

	t.Run("new message", func(t *testing.T) {
		before := a.int(`SELECT COALESCE(max(seq), 0)::int FROM events WHERE user_id = $1`, a.userID)
		start := time.Now()
		appendMsg(t, other, "INBOX", msg(9001, 0, "Fresh one", time.Now()), nil, time.Now())
		lat := waitFor(t, 2*time.Second, "message.new event", func() bool {
			return a.int(`SELECT count(*) FROM events WHERE user_id = $1 AND seq > $2 AND type = 'message.new'`, a.userID, before) > 0
		})
		t.Logf("%s: APPEND -> message.new in %s", p, lat.Round(time.Millisecond))
		waitFor(t, 2*time.Second-time.Since(start)+time.Second, "body downloaded", func() bool {
			_, _, body, ok := a.msgByHdr("m9001@silo.test")
			return ok && body == "full"
		})
	})

	t.Run("flags", func(t *testing.T) {
		selectInbox(t, other)
		uid := uidOf(t, other, "<m2@silo.test>")
		_, err := other.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true,
			Flags: []imap.Flag{imap.FlagSeen, imap.FlagFlagged, "Projekt2"}}, nil).Collect()
		must(t, err)
		id, _, _, _ := a.msgByHdr("m2@silo.test")
		lat := waitFor(t, 5*time.Second, "flags applied", func() bool {
			l := a.labelsOf(id)
			return l["STARRED"] && l["Projekt2"] && a.int(`SELECT count(*) FROM messages WHERE id = $1 AND NOT is_unread`, id) == 1
		})
		t.Logf("%s: STORE -> labels in %s", p, lat.Round(time.Millisecond))
		_, err = other.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: imap.StoreFlagsDel, Silent: true,
			Flags: []imap.Flag{imap.FlagSeen, imap.FlagFlagged}}, nil).Collect()
		must(t, err)
		waitFor(t, 5*time.Second, "flags removed", func() bool {
			l := a.labelsOf(id)
			return !l["STARRED"] && l["Projekt2"] && a.int(`SELECT count(*) FROM messages WHERE id = $1 AND is_unread`, id) == 1
		})
	})

	t.Run("move by another client", func(t *testing.T) {
		id, thread, _, ok := a.msgByHdr("m5@silo.test")
		if !ok {
			t.Fatal("m5 not synced")
		}
		var labelID int64
		must(t, testPool.QueryRow(context.Background(), `
INSERT INTO labels (user_id, account_id, name, type) VALUES ($1, $2, 'Projekt', 'user') RETURNING id`, a.userID, a.id).Scan(&labelID))
		_, err := testPool.Exec(context.Background(), `INSERT INTO message_labels (message_id, label_id, user_id) VALUES ($1, $2, $3)`, id, labelID, a.userID)
		must(t, err)

		selectInbox(t, other)
		uid := uidOf(t, other, "<m5@silo.test>")
		start := time.Now()
		_, err = other.Move(imap.UIDSetNum(uid), "Faktury").Wait()
		must(t, err)
		faktury := a.folderID("Faktury")
		lat := waitFor(t, 5*time.Second, "move visible", func() bool {
			return a.int(`SELECT count(*) FROM message_locations WHERE message_id = $1 AND folder_id = $2`, id, faktury) == 1 &&
				a.int(`SELECT count(*) FROM message_locations WHERE message_id = $1`, id) == 1
		})
		t.Logf("%s: MOVE INBOX -> Faktury visible in %s", p, time.Since(start).Round(time.Millisecond))
		_ = lat
		id2, thread2, _, _ := a.msgByHdr("m5@silo.test")
		l := a.labelsOf(id)
		if id2 != id || thread2 != thread || !l["Faktury"] || !l["Projekt"] || l["INBOX"] {
			t.Errorf("after move: id %d->%d thread %d->%d labels %v", id, id2, thread, thread2, l)
		}
		if a.int(`SELECT count(*) FROM messages WHERE id = $1 AND orphaned_at IS NULL`, id) != 1 {
			t.Error("moved message still orphaned")
		}
	})

	t.Run("expunge and orphan gc", func(t *testing.T) {
		id, _, _, _ := a.msgByHdr("m3@silo.test")
		selectInbox(t, other)
		uid := uidOf(t, other, "<m3@silo.test>")
		_, err := other.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Collect()
		must(t, err)
		must(t, other.Expunge().Close())
		waitFor(t, 5*time.Second, "location removed", func() bool {
			return a.int(`SELECT count(*) FROM message_locations WHERE message_id = $1`, id) == 0 &&
				a.int(`SELECT count(*) FROM messages WHERE id = $1 AND orphaned_at IS NOT NULL`, id) == 1
		})
		if l := a.labelsOf(id); l["INBOX"] {
			t.Errorf("orphan keeps INBOX: %v", l)
		}
		// Not yet: orphaned for less than 24 h.
		res, err := silosync.OrphanGC(context.Background(), a.deps, a.id, time.Now())
		must(t, err)
		if res.Messages != 0 {
			t.Fatalf("purged %d fresh orphans", res.Messages)
		}
		// 25 hours later, after a full sync cycle of every folder.
		_, err = testPool.Exec(context.Background(), `UPDATE messages SET orphaned_at = now() - interval '25 hours' WHERE id = $1`, id)
		must(t, err)
		_, err = testPool.Exec(context.Background(), `UPDATE mailbox_folders SET last_sync_at = now() - interval '26 hours' WHERE account_id = $1 AND name = 'Junk'`, a.id)
		must(t, err)
		res, err = silosync.OrphanGC(context.Background(), a.deps, a.id, time.Now())
		must(t, err)
		if res.Messages != 0 {
			t.Fatalf("purged although Junk was not synced since the orphaning")
		}
		_, err = testPool.Exec(context.Background(), `UPDATE mailbox_folders SET last_sync_at = now() WHERE account_id = $1`, a.id)
		must(t, err)
		res, err = silosync.OrphanGC(context.Background(), a.deps, a.id, time.Now())
		must(t, err)
		if res.Messages != 1 || a.int(`SELECT count(*) FROM messages WHERE id = $1`, id) != 0 {
			t.Fatalf("orphan gc: %+v", res)
		}
	})
	_ = fmt.Sprint
}

// TestUIDValidityChange: a folder deleted and re-created with the same
// messages gets a new UIDVALIDITY; user labels, threads and the number of
// messages survive.
func TestUIDValidityChange(t *testing.T) {
	mb := newMailbox(t, imaptest.Full)
	mb.create("Projekty")
	mb.populate(spec{"INBOX", 4}, spec{"Projekty", 12})
	a := mb.account(mb.pass)
	r := a.start()
	a.waitInitialDone(time.Minute)

	id, thread, _, _ := a.msgByHdr("m7@silo.test")
	var labelID int64
	must(t, testPool.QueryRow(context.Background(), `INSERT INTO labels (user_id, account_id, name, type) VALUES ($1, $2, 'Projekt', 'user') RETURNING id`, a.userID, a.id).Scan(&labelID))
	_, err := testPool.Exec(context.Background(), `INSERT INTO message_labels (message_id, label_id, user_id) VALUES ($1, $2, $3)`, id, labelID, a.userID)
	must(t, err)
	before := a.counts()
	var uv1 int64
	must(t, testPool.QueryRow(context.Background(), `SELECT uidvalidity FROM mailbox_folders WHERE account_id = $1 AND name = 'Projekty'`, a.id).Scan(&uv1))
	r.stop()

	// Copy the raw messages out, drop the folder, re-create it.
	c := mb.client()
	_, err = c.Select("Projekty", nil).Wait()
	must(t, err)
	bufs, err := c.Fetch(imap.UIDSet{imap.UIDRange{Start: 1, Stop: 0}}, &imap.FetchOptions{UID: true, Flags: true, InternalDate: true,
		BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
	must(t, err)
	must(t, c.Unselect().Wait())
	must(t, c.Delete("Projekty").Wait())
	time.Sleep(1100 * time.Millisecond) // Dovecot derives UIDVALIDITY from the clock
	must(t, c.Create("Projekty", nil).Wait())
	for i := len(bufs) - 1; i >= 0; i-- { // reversed: the UIDs now point to other messages
		b := bufs[i]
		appendMsg(t, c, "Projekty", b.FindBodySection(&imap.FetchItemBodySection{}), b.Flags, b.InternalDate)
	}

	a.start()
	waitFor(t, 30*time.Second, "new uidvalidity synced", func() bool {
		return a.int(`SELECT count(*) FROM mailbox_folders f JOIN sync_cursors c ON c.folder_id = f.id
WHERE f.account_id = $1 AND f.name = 'Projekty' AND f.uidvalidity <> $2 AND c.phase IN ('bodies','done')`, a.id, uv1) == 1 &&
			a.int(`SELECT count(*) FROM message_locations WHERE account_id = $1 AND stale`, a.id) == 0
	})
	after := a.counts()
	id2, thread2, _, _ := a.msgByHdr("m7@silo.test")
	if after.Messages != before.Messages || after.Locations != before.Locations || after.UserLabels != before.UserLabels {
		t.Errorf("counts before %+v after %+v", before, after)
	}
	if id2 != id || thread2 != thread || !a.labelsOf(id)["Projekt"] || !a.labelsOf(id)["Projekty"] {
		t.Errorf("m7: id %d->%d thread %d->%d labels %v", id, id2, thread, thread2, a.labelsOf(id))
	}
	if n := a.int(`SELECT count(*) FROM messages WHERE account_id = $1 AND orphaned_at IS NOT NULL`, a.id); n != 0 {
		t.Errorf("%d orphans after UIDVALIDITY change", n)
	}
}

// TestFolderRenameAndDelete: a folder renamed in another client keeps its
// row and label (same UIDVALIDITY and count); a deleted folder goes through
// the disappearance procedure.
func TestFolderRenameAndDelete(t *testing.T) {
	mb := newMailbox(t, imaptest.Full)
	mb.create("Stare", "Tmp")
	mb.populate(spec{"INBOX", 2}, spec{"Stare", 3}, spec{"Tmp", 2})
	a := mb.account(mb.pass)
	r := a.start()
	a.waitInitialDone(time.Minute)
	oldID := a.folderID("Stare")
	tmp := a.folderID("Tmp")
	id, _, _, _ := a.msgByHdr("m3@silo.test")

	c := mb.client()
	must(t, c.Rename("Stare", "Nowe", nil).Wait())
	must(t, c.Delete("Tmp").Wait())
	r.w.RequestResync("incremental")
	waitFor(t, 10*time.Second, "rename and delete seen", func() bool {
		return a.int(`SELECT count(*) FROM mailbox_folders WHERE id = $1 AND name = 'Nowe'`, oldID) == 1 &&
			a.int(`SELECT count(*) FROM mailbox_folders WHERE id = $1`, tmp) == 0
	})
	if l := a.labelsOf(id); !l["Nowe"] || l["Stare"] {
		t.Errorf("labels after rename: %v", l)
	}
	if n := a.int(`SELECT count(*) FROM messages WHERE account_id = $1 AND orphaned_at IS NOT NULL`, a.id); n != 2 {
		t.Errorf("orphans after folder delete = %d, want 2", n)
	}
}
