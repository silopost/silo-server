package sync_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

// TestSnooze runs the snooze scenarios of plan 3.10 against Dovecot on the
// full and no-move profiles.
func TestSnooze(t *testing.T) {
	for _, p := range []imaptest.Profile{imaptest.Full, imaptest.NoMove} {
		t.Run(string(p), func(t *testing.T) { testSnooze(t, p) })
	}
}

func testSnooze(t *testing.T, p imaptest.Profile) {
	const window = 1500 * time.Millisecond
	ctx := context.Background()
	// m1, m2 and m3 are threads of their own; m1 is read.
	e := newOpsEnv(t, p, window, spec{"INBOX", 4})
	keywords := e.a.int(`SELECT count(*) FROM accounts WHERE id = $1 AND keywords_supported`, e.a.id) == 1
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := testPool.Exec(ctx, q, args...)
		must(t, err)
	}
	snooze := func(hdr string) (int64, int64) {
		t.Helper()
		id, th := e.thread(hdr)
		r, err := e.svc.Snooze(ctx, e.a.userID, th, time.Now().Add(time.Hour), "test")
		must(t, err)
		if l := e.a.labelsOf(id); !l["SNOOZED"] || l["INBOX"] || r.OperationID == "" || len(r.Threads) != 1 || r.Threads[0].SnoozedUntil == nil {
			t.Fatalf("after snooze of %s: labels %v result %+v", hdr, l, r)
		}
		waitFor(t, 15*time.Second, hdr+" in Snoozed on the server", func() bool {
			return slices.Equal(e.serverFolders(hdr), []string{"Snoozed"})
		})
		e.waitNoJobs(10 * time.Second)
		return id, th
	}
	wakeNow := func(th int64) {
		t.Helper()
		exec(`UPDATE snoozes SET wake_at = now() - interval '1 second' WHERE thread_id = $1`, th)
		_, err := e.svc.WakeDue(ctx) // the 30 s tick
		must(t, err)
		if n := e.a.int(`SELECT count(*) FROM snoozes WHERE thread_id = $1`, th); n != 0 {
			t.Fatalf("snooze left after the tick: %d", n)
		}
	}
	hasFlag := func(hdr, flag string) bool { return slices.Contains(e.serverFlags(hdr), flag) }

	t.Run("snooze moves to a Snoozed folder created by Silo", func(t *testing.T) {
		id, th := e.thread("m1@silo.test")
		_, err := e.svc.Snooze(ctx, e.a.userID, th, time.Now().Add(time.Hour), "test")
		must(t, err)
		if l := e.a.labelsOf(id); !l["SNOOZED"] || l["INBOX"] {
			t.Fatalf("labels after snooze: %v", l)
		}
		if f := e.serverFolders("m1@silo.test"); !slices.Equal(f, []string{"INBOX"}) {
			t.Errorf("server changed inside the undo window: %v", f)
		}
		waitFor(t, 15*time.Second, "m1 in Snoozed", func() bool { return slices.Equal(e.serverFolders("m1@silo.test"), []string{"Snoozed"}) })
		e.waitNoJobs(10 * time.Second)
		if n := e.a.int(`SELECT count(*) FROM mailbox_folders WHERE account_id = $1 AND name = 'Snoozed' AND role = 'snoozed'`, e.a.id); n != 1 {
			t.Error("Snoozed not recorded with its role")
		}
		if n := e.a.int(`SELECT count(*) FROM accounts WHERE id = $1 AND folder_mapping->>'snoozed' = 'Snoozed'`, e.a.id); n != 1 {
			t.Error("Snoozed not stored in folder_mapping")
		}
		if keywords && !hasFlag("m1@silo.test", mail.FlagSnoozed) {
			t.Errorf("no %s on the server: %v", mail.FlagSnoozed, e.serverFlags("m1@silo.test"))
		}
		if !hasFlag("m1@silo.test", `\Seen`) {
			t.Fatal("m1 should still be read")
		}
		if l := e.a.labelsOf(id); !l["SNOOZED"] || l["INBOX"] {
			t.Errorf("labels after the write-back: %v", l)
		}
	})

	t.Run("wake at the time: INBOX, unread, on top", func(t *testing.T) {
		id, th := e.thread("m1@silo.test")
		wakeNow(th)
		if l := e.a.labelsOf(id); !l["INBOX"] || l["SNOOZED"] {
			t.Errorf("labels after wake: %v", l)
		}
		if e.a.int(`SELECT count(*) FROM threads WHERE id = $1 AND unread_count > 0 AND snoozed_until IS NULL`, th) != 1 {
			t.Error("woken thread is not unread")
		}
		top := e.a.int(`SELECT tl.thread_id FROM thread_labels tl JOIN labels l ON l.id = tl.label_id
			WHERE l.account_id = $1 AND l.system_key = 'INBOX' ORDER BY tl.sort_at DESC, tl.thread_id DESC LIMIT 1`, e.a.id)
		if int64(top) != th {
			t.Errorf("top of INBOX is thread %d, want %d", top, th)
		}
		waitFor(t, 15*time.Second, "m1 back in INBOX", func() bool { return slices.Equal(e.serverFolders("m1@silo.test"), []string{"INBOX"}) })
		e.waitNoJobs(10 * time.Second)
		if hasFlag("m1@silo.test", `\Seen`) || hasFlag("m1@silo.test", mail.FlagSnoozed) {
			t.Errorf("flags after wake: %v", e.serverFlags("m1@silo.test"))
		}
		if l := e.a.labelsOf(id); !l["INBOX"] || l["SNOOZED"] {
			t.Errorf("labels after the write-back: %v", l)
		}
		e.reconcile()
	})

	t.Run("a new message wakes the thread", func(t *testing.T) {
		_, th := snooze("m2@silo.test")
		appendMsg(t, e.other, "INBOX", msg(100, 2, "Re: Message 2", time.Now()), nil, time.Now())
		e.mgr.Worker(e.a.id).RequestResync("incremental")
		waitFor(t, 30*time.Second, "thread woken by the reply", func() bool {
			return e.a.int(`SELECT count(*) FROM snoozes WHERE thread_id = $1`, th) == 0
		})
		waitFor(t, 15*time.Second, "m2 back in INBOX", func() bool { return slices.Equal(e.serverFolders("m2@silo.test"), []string{"INBOX"}) })
		id, th2 := e.thread("m100@silo.test")
		if th2 != th || !e.a.labelsOf(id)["INBOX"] {
			t.Errorf("reply thread %d (want %d), labels %v", th2, th, e.a.labelsOf(id))
		}
	})

	t.Run("taken out of Snoozed by another client", func(t *testing.T) {
		id, th := snooze("m3@silo.test")
		_, err := e.other.Select("Snoozed", nil).Wait()
		must(t, err)
		uid := uidOf(t, e.other, "<m3@silo.test>")
		if e.other.Caps().Has(imap.CapMove) {
			_, err = e.other.Move(imap.UIDSetNum(uid), "INBOX").Wait()
			must(t, err)
		} else {
			_, err = e.other.Copy(imap.UIDSetNum(uid), "INBOX").Wait()
			must(t, err)
			_, err = e.other.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Collect()
			must(t, err)
			must(t, e.other.Expunge().Close())
		}
		e.mgr.Worker(e.a.id).RequestResync("incremental")
		waitFor(t, 30*time.Second, "move seen by the sync", func() bool {
			return e.a.int(`SELECT count(*) FROM message_locations l JOIN mailbox_folders f ON f.id = l.folder_id
				WHERE l.message_id = $1 AND f.role = 'snoozed' AND NOT ('\Deleted' = ANY(l.flags))`, id) == 0 &&
				e.a.int(`SELECT count(*) FROM message_locations l JOIN mailbox_folders f ON f.id = l.folder_id
				WHERE l.message_id = $1 AND f.role = 'inbox' AND NOT ('\Deleted' = ANY(l.flags))`, id) == 1
		})
		_, err = e.svc.WakeDue(ctx) // the tick drops the abandoned snooze
		must(t, err)
		if n := e.a.int(`SELECT count(*) FROM snoozes WHERE thread_id = $1`, th); n != 0 {
			t.Errorf("snooze kept after the message left Snoozed")
		}
		if l := e.a.labelsOf(id); !l["INBOX"] || l["SNOOZED"] || e.a.int(`SELECT count(*) FROM threads WHERE id = $1 AND snoozed_until IS NULL`, th) != 1 {
			t.Errorf("labels %v", l)
		}
	})

	t.Run("snooze survives a UIDVALIDITY change of Snoozed", func(t *testing.T) {
		id, th := snooze("m1@silo.test")
		var uv1 int64
		must(t, testPool.QueryRow(ctx, `SELECT uidvalidity FROM mailbox_folders WHERE account_id = $1 AND role = 'snoozed'`, e.a.id).Scan(&uv1))
		e.stop()
		// Re-create Snoozed with the same message: a new UIDVALIDITY.
		c := e.mb.client()
		_, err := c.Select("Snoozed", nil).Wait()
		must(t, err)
		bufs, err := c.Fetch(imap.UIDSet{imap.UIDRange{Start: 1, Stop: 0}}, &imap.FetchOptions{UID: true, Flags: true, InternalDate: true,
			BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
		must(t, err)
		must(t, c.Unselect().Wait())
		must(t, c.Delete("Snoozed").Wait())
		time.Sleep(1100 * time.Millisecond) // Dovecot derives UIDVALIDITY from the clock
		must(t, c.Create("Snoozed", nil).Wait())
		appendMsg(t, c, "Snoozed", msg(999, 0, "Unrelated", time.Now()), nil, time.Now()) // the old UIDs now point elsewhere
		for _, b := range bufs {
			if slices.Contains(b.Flags, imap.FlagDeleted) {
				continue // a copy left by a move without UIDPLUS (no-move)
			}
			var flags []imap.Flag
			for _, f := range b.Flags {
				if f != `\Recent` {
					flags = append(flags, f)
				}
			}
			appendMsg(t, c, "Snoozed", b.FindBodySection(&imap.FetchItemBodySection{}), flags, b.InternalDate)
		}
		_ = c.Logout().Wait()
		e.start()
		waitFor(t, 10*time.Second, "account worker", func() bool { return e.mgr.Worker(e.a.id) != nil })
		e.mgr.Worker(e.a.id).RequestResync("full")
		waitFor(t, 30*time.Second, "new UIDVALIDITY of Snoozed synced", func() bool {
			return e.a.int(`SELECT count(*) FROM mailbox_folders f JOIN sync_cursors c ON c.folder_id = f.id
				WHERE f.account_id = $1 AND f.role = 'snoozed' AND f.uidvalidity <> $2 AND c.phase IN ('bodies','done')`, e.a.id, uv1) == 1 &&
				e.a.int(`SELECT count(*) FROM message_locations WHERE account_id = $1 AND stale`, e.a.id) == 0 &&
				e.a.int(`SELECT count(*) FROM message_locations l JOIN mailbox_folders f ON f.id = l.folder_id
					WHERE l.message_id = $1 AND f.role = 'snoozed'`, id) == 1
		})
		id2, th2 := e.thread("m1@silo.test")
		if id2 != id || th2 != th {
			t.Fatalf("m1 is a new message after the UIDVALIDITY change: %d/%d -> %d/%d", id, th, id2, th2)
		}
		_, err = e.svc.WakeDue(ctx)
		must(t, err)
		if n := e.a.int(`SELECT count(*) FROM snoozes s JOIN snooze_items si ON si.snooze_id = s.id WHERE s.thread_id = $1 AND si.message_id = $2`, th, id); n != 1 {
			t.Fatalf("snooze lost after the UIDVALIDITY change")
		}
		if l := e.a.labelsOf(id); !l["SNOOZED"] || l["INBOX"] {
			t.Errorf("labels after the UIDVALIDITY change: %v", l)
		}
		wakeNow(th)
		waitFor(t, 15*time.Second, "m1 back in INBOX", func() bool { return slices.Equal(e.serverFolders("m1@silo.test"), []string{"INBOX"}) })
	})

	t.Run("undo after the window brings the thread back", func(t *testing.T) {
		id, th := e.thread("m4@silo.test")
		r, err := e.svc.Snooze(ctx, e.a.userID, th, time.Now().Add(time.Hour), "test")
		must(t, err)
		waitFor(t, 15*time.Second, "m4 in Snoozed", func() bool { return slices.Equal(e.serverFolders("m4@silo.test"), []string{"Snoozed"}) })
		e.waitNoJobs(10 * time.Second)
		u, err := e.svc.Undo(ctx, e.a.userID, r.OperationID, "test")
		must(t, err)
		if l := e.a.labelsOf(id); !l["INBOX"] || l["SNOOZED"] || len(u.Threads) == 0 || u.Threads[0].SnoozedUntil != nil {
			t.Errorf("after undo: labels %v threads %+v", l, u.Threads)
		}
		if n := e.a.int(`SELECT count(*) FROM snoozes WHERE thread_id = $1`, th); n != 0 {
			t.Errorf("snoozes after undo = %d", n)
		}
		waitFor(t, 15*time.Second, "m4 back in INBOX", func() bool { return slices.Equal(e.serverFolders("m4@silo.test"), []string{"INBOX"}) })
		e.waitNoJobs(10 * time.Second)
		if keywords && hasFlag("m4@silo.test", mail.FlagSnoozed) {
			t.Errorf("flags after undo: %v", e.serverFlags("m4@silo.test"))
		}
		e.reconcile()
	})
}
