package sync_test

import (
	"slices"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

// TestGmailGenericMode covers Gmail without X-GM-EXT-1 (plan 3.4, "Gmail
// IMAP before phase 5"): a message labelled Inbox is in INBOX and in
// [Gmail]/All Mail with the same bytes. Silo keeps one message with two
// locations (dedup) and one thread; archiving drops the INBOX copy only
// (\Deleted + UID EXPUNGE, no Archive folder), the All Mail copy stays. A
// message only in All Mail (archived in Gmail) has no INBOX label.
func TestGmailGenericMode(t *testing.T) {
	e := newOpsEnv(t, imaptest.GmailLike, time.Second)
	const all = "[Gmail]/All Mail"
	if e.a.folderID(all) == 0 || e.a.int(`SELECT count(*) FROM mailbox_folders WHERE account_id = $1 AND name = $2 AND role = 'all'`, e.a.id, all) != 1 {
		t.Fatal("All Mail not discovered with role all")
	}
	now := time.Now().Add(-time.Hour)
	inbox := msg(900, 0, "Gmail inbox", now)
	appendMsg(t, e.other, "INBOX", inbox, nil, now)
	appendMsg(t, e.other, all, inbox, nil, now)
	appendMsg(t, e.other, all, msg(901, 0, "Gmail archived", now), nil, now)
	e.mgr.Worker(e.a.id).RequestResync("")
	waitFor(t, 30*time.Second, "both messages and both locations synced", func() bool {
		_, _, _, ok := e.a.msgByHdr("m901@silo.test")
		return ok && e.a.int(`SELECT count(*) FROM message_locations l JOIN messages m ON m.id = l.message_id
WHERE m.account_id = $1 AND m.message_id_hdr = 'm900@silo.test'`, e.a.id) == 2
	})

	id, th := e.thread("m900@silo.test")
	if n := e.a.int(`SELECT count(*) FROM messages WHERE account_id = $1 AND message_id_hdr = 'm900@silo.test'`, e.a.id); n != 1 {
		t.Fatalf("copies of m900 in the mirror: %d", n)
	}
	if n := e.a.int(`SELECT count(*) FROM threads WHERE account_id = $1`, e.a.id); n != 2 {
		t.Errorf("threads: %d, want 2", n)
	}
	if l := e.a.labelsOf(id); !l["INBOX"] || l["ARCHIVE"] {
		t.Errorf("labels of m900: %v", l)
	}
	inboxThreads := func() int {
		return e.a.int(`SELECT count(*) FROM thread_labels tl JOIN labels l ON l.id = tl.label_id
WHERE l.account_id = $1 AND l.system_key = 'INBOX' AND NOT tl.hidden`, e.a.id)
	}
	if n := inboxThreads(); n != 1 {
		t.Errorf("threads labelled INBOX: %d, want 1 (no duplicate)", n)
	}
	archivedID, _ := e.thread("m901@silo.test")
	if l := e.a.labelsOf(archivedID); l["INBOX"] {
		t.Errorf("message only in All Mail labelled INBOX: %v", l)
	}

	// Archive: out of INBOX on the server, the All Mail copy stays.
	e.modify("m900@silo.test", nil, sys("INBOX"))
	waitFor(t, 15*time.Second, "INBOX copy removed on the server", func() bool {
		return slices.Equal(e.serverFolders("m900@silo.test"), []string{all})
	})
	e.waitNoJobs(5 * time.Second)
	if n := e.a.int(`SELECT count(*) FROM mailbox_folders WHERE account_id = $1 AND (role = 'archive' OR name = 'Archive')`, e.a.id); n != 0 {
		t.Errorf("an Archive folder was created (%d)", n)
	}
	waitFor(t, 5*time.Second, "mirror updated", func() bool {
		return e.a.int(`SELECT count(*) FROM message_locations WHERE message_id = $1`, id) == 1 &&
			e.a.int(`SELECT count(*) FROM threads WHERE id = $1 AND pending_ops = 0`, th) == 1
	})
	if l := e.a.labelsOf(id); l["INBOX"] {
		t.Errorf("labels after archiving: %v", l)
	}
	if n := e.a.int(`SELECT count(*) FROM messages WHERE account_id = $1 AND message_id_hdr = 'm900@silo.test'`, e.a.id); n != 1 {
		t.Errorf("copies of m900 after archiving: %d", n)
	}
	if n := e.a.int(`SELECT count(*) FROM message_locations WHERE message_id = $1 AND folder_id = $2`, id, e.a.folderID(all)); n != 1 {
		t.Error("All Mail location lost")
	}
	if n := inboxThreads(); n != 0 {
		t.Errorf("threads labelled INBOX after archiving: %d", n)
	}
	// Still a thread of the account ("All mail" view: no label filter).
	if n := e.a.int(`SELECT count(*) FROM threads WHERE id = $1 AND account_id = $2`, th, e.a.id); n != 1 {
		t.Error("thread gone after archiving")
	}

	// A later sync pass changes nothing.
	e.mgr.Worker(e.a.id).RequestResync("full")
	time.Sleep(2 * time.Second)
	e.waitNoJobs(5 * time.Second)
	if n := e.a.int(`SELECT count(*) FROM message_locations WHERE message_id = $1`, id); n != 1 || inboxThreads() != 0 {
		t.Errorf("after resync: %d locations, %d INBOX threads", n, inboxThreads())
	}
	e.reconcile()
}
