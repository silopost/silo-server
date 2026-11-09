package sync_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/silopost/silo-server/internal/drafts"
	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/mailview"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

// TestDraftSync checks plan 3.8 on the full and no-move (no UIDPLUS)
// profiles: a saved draft reaches the Drafts folder (created when missing)
// after the throttling delay as exactly one message carrying its
// X-Silo-Draft-Id, a second save replaces it (still one copy), sync adopts
// the copy as the local draft instead of creating a duplicate, and discarding
// the draft removes it from the server.
func TestDraftSync(t *testing.T) {
	for _, p := range []imaptest.Profile{imaptest.Full, imaptest.NoMove} {
		t.Run(string(p), func(t *testing.T) { testDraftSync(t, p) })
	}
}

func testDraftSync(t *testing.T, p imaptest.Profile) {
	ctx := context.Background()
	e := newOpsEnv(t, p, time.Second, spec{"INBOX", 3})
	const delay = 300 * time.Millisecond
	svc := drafts.New(drafts.Options{Pool: testPool, Blobs: e.a.deps.Blobs, SyncDelay: delay, MaxUploadBytes: 1 << 20})

	d, err := svc.Create(ctx, drafts.CreateParams{UserID: e.a.userID, AccountID: e.a.id, Origin: "test"})
	must(t, err)
	up, err := svc.Upload(ctx, e.a.userID, "notes.txt", "text/plain", false, strings.NewReader("attached notes"))
	must(t, err)
	save := func(version int32, subject string) *drafts.Draft {
		t.Helper()
		x, err := svc.Update(ctx, drafts.UpdateParams{
			UserID: e.a.userID, ID: d.ID, Version: version, Subject: subject, BodyHTML: "<p>Body of " + subject + "</p>",
			To: []mail.Address{{Name: "Jan", Email: "jan@firma.pl"}}, AttachmentIDs: []int64{up.ID}, Origin: "test",
		})
		must(t, err)
		return x
	}
	var draftID, msgIDHdr string
	must(t, testPool.QueryRow(ctx, `SELECT d.silo_draft_id, m.message_id_hdr FROM drafts d JOIN messages m ON m.id = d.message_id WHERE d.message_id = $1`, d.ID).Scan(&draftID, &msgIDHdr))

	// serverCopies lists the copies of the draft in Drafts (not \Deleted) with their raw bytes.
	serverCopies := func() [][]byte {
		t.Helper()
		if _, err := e.other.Select("Drafts", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			return nil // not created yet
		}
		data, err := e.other.UIDSearch(&imap.SearchCriteria{
			Header:  []imap.SearchCriteriaHeaderField{{Key: mail.HeaderSiloDraftID, Value: draftID}},
			NotFlag: []imap.Flag{imap.FlagDeleted},
		}, nil).Wait()
		must(t, err)
		var out [][]byte
		for _, uid := range data.AllUIDs() {
			bufs, err := e.other.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{UID: true, Flags: true,
				BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
			must(t, err)
			for _, b := range bufs {
				if !slicesHasFlag(b.Flags, imap.FlagDraft) {
					t.Errorf("copy without \\Draft: %v", b.Flags)
				}
				for _, s := range b.BodySection {
					out = append(out, s.Bytes)
				}
			}
		}
		return out
	}
	dupes := func() int {
		return e.a.int(`SELECT count(*) FROM messages WHERE account_id = $1 AND message_id_hdr = $2`, e.a.id, msgIDHdr)
	}

	start := time.Now()
	x := save(1, "First version")
	if x.Version != 2 || x.SyncedVersion != 0 {
		t.Fatalf("after save: %+v", x)
	}
	if c := serverCopies(); len(c) != 0 {
		t.Fatalf("copy on the server before the throttling delay: %d", len(c))
	}
	waitFor(t, 20*time.Second, "first copy in Drafts", func() bool { return len(serverCopies()) == 1 })
	if time.Since(start) < delay {
		t.Error("appended before the throttling delay")
	}
	raw := serverCopies()[0]
	for _, want := range []string{"Subject: First version", "multipart/mixed", "multipart/alternative", "notes.txt", "Message-Id: <" + msgIDHdr + ">"} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("server copy lacks %q:\n%s", want, raw)
		}
	}
	waitFor(t, 20*time.Second, "draft synced and located in Drafts", func() bool {
		return e.a.int(`SELECT count(*) FROM drafts WHERE message_id = $1 AND server_synced_version = 2`, d.ID) == 1 &&
			e.a.int(`SELECT count(*) FROM message_locations l JOIN mailbox_folders f ON f.id = l.folder_id
			         WHERE l.message_id = $1 AND f.role = 'drafts'`, d.ID) == 1
	})
	e.waitNoJobs(10 * time.Second)
	// Let the incremental sync of Drafts run: it must adopt the copy, not duplicate it.
	e.mgr.Worker(e.a.id).RequestResync("incremental")
	time.Sleep(time.Second)
	if n := dupes(); n != 1 {
		t.Errorf("messages with the draft's Message-ID after sync = %d, want 1", n)
	}
	if l := e.a.labelsOf(d.ID); !l["DRAFT"] || len(l) != 1 {
		t.Errorf("draft labels = %v", l)
	}

	save(2, "Second version")
	waitFor(t, 20*time.Second, "second version in Drafts", func() bool {
		c := serverCopies()
		return len(c) == 1 && bytes.Contains(c[0], []byte("Subject: Second version"))
	})
	waitFor(t, 20*time.Second, "second version synced", func() bool {
		return e.a.int(`SELECT count(*) FROM drafts WHERE message_id = $1 AND server_synced_version = 3`, d.ID) == 1
	})
	e.waitNoJobs(10 * time.Second)
	e.mgr.Worker(e.a.id).RequestResync("incremental")
	time.Sleep(time.Second)
	if c := serverCopies(); len(c) != 1 {
		t.Errorf("copies after the second save = %d", len(c))
	}
	if n := dupes(); n != 1 {
		t.Errorf("messages with the draft's Message-ID after the second save = %d", n)
	}
	got, err := svc.Get(ctx, e.a.userID, d.ID)
	must(t, err)
	if got.ServerChanged || got.SyncedVersion != got.Version {
		t.Errorf("draft after sync: %+v", got)
	}

	must(t, svc.Delete(ctx, e.a.userID, d.ID, "test"))
	waitFor(t, 20*time.Second, "draft removed from Drafts", func() bool { return len(serverCopies()) == 0 })
	e.waitNoJobs(10 * time.Second)
	time.Sleep(time.Second)
	if n := dupes(); n != 0 {
		t.Errorf("messages of the discarded draft = %d", n)
	}

	// A draft written by another client: a plain message with the DRAFT
	// label; opening it creates the draft from its MIME, and the next save
	// replaces it on the server.
	view := mailview.New(testPool, e.a.deps.Blobs, e.mgr)
	svc = drafts.New(drafts.Options{Pool: testPool, Blobs: e.a.deps.Blobs, SyncDelay: delay, MaxUploadBytes: 1 << 20,
		OpenRaw: func(ctx context.Context, userID, messageID int64) (io.ReadCloser, error) {
			r, err := view.OpenRaw(ctx, userID, messageID)
			if err != nil {
				return nil, err
			}
			return r.Body, nil
		}})
	tb := "From: silo@silo.test\r\nTo: Ola <ola@firma.pl>\r\nSubject: From Thunderbird\r\nMessage-ID: <tb1@silo.test>\r\n" +
		"Date: " + time.Now().Format(time.RFC1123Z) + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nWritten elsewhere.\r\n"
	appendMsg(t, e.other, "Drafts", []byte(tb), []imap.Flag{imap.FlagDraft, imap.FlagSeen}, time.Now())
	e.mgr.Worker(e.a.id).RequestResync("incremental")
	var tbID int64
	waitFor(t, 20*time.Second, "foreign draft synced", func() bool {
		id, _, _, ok := e.a.msgByHdr("tb1@silo.test")
		tbID = id
		return ok
	})
	if l := e.a.labelsOf(tbID); !l["DRAFT"] {
		t.Errorf("foreign draft labels = %v", l)
	}
	fd, err := svc.Get(ctx, e.a.userID, tbID)
	must(t, err)
	if fd.Subject != "From Thunderbird" || len(fd.To) != 1 || fd.To[0].Email != "ola@firma.pl" || !strings.Contains(fd.BodyHTML, "Written elsewhere.") || fd.Version != 1 {
		t.Fatalf("opened foreign draft: %+v", fd)
	}
	_, err = svc.Update(ctx, drafts.UpdateParams{UserID: e.a.userID, ID: tbID, Version: 1, Subject: "From Thunderbird, edited in Silo",
		BodyHTML: fd.BodyHTML, To: fd.To})
	must(t, err)
	var tbDraftID string
	must(t, testPool.QueryRow(ctx, `SELECT silo_draft_id FROM drafts WHERE message_id = $1`, tbID).Scan(&tbDraftID))
	draftID = tbDraftID
	waitFor(t, 20*time.Second, "edited foreign draft replaces the original", func() bool {
		c := serverCopies()
		if len(c) != 1 || !bytes.Contains(c[0], []byte("edited in Silo")) {
			return false
		}
		data, err := e.other.UIDSearch(&imap.SearchCriteria{
			Header:  []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: "<tb1@silo.test>"}},
			NotFlag: []imap.Flag{imap.FlagDeleted},
		}, nil).Wait()
		must(t, err)
		return len(data.AllUIDs()) == 1 // only Silo's copy (same Message-ID) is left
	})
	e.waitNoJobs(10 * time.Second)
	e.reconcile()
}

func slicesHasFlag(fs []imap.Flag, f imap.Flag) bool {
	for _, x := range fs {
		if strings.EqualFold(string(x), string(f)) {
			return true
		}
	}
	return false
}
