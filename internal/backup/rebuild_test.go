package backup_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/drafts"
	"github.com/silopost/silo-server/internal/mail"
	silosync "github.com/silopost/silo-server/internal/sync"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

// TestRebuildCache is the "rebuild the cache from scratch" DoD (phase 4/5)
// over the phase 4 rebuild (account_rebuilds, wiped by the account worker):
// after a full sync with user labels (mirrored and local only), a snooze, a
// reply draft and a message scheduled for later, resync mode rebuild drops
// the mirror; after the new sync the (Message-ID, label) pairs, the split
// into threads (the draft in the conversation it answers) and the snooze are
// the same, the scheduled message is still queued, and the account is live
// again.
func TestRebuildCache(t *testing.T) {
	ctx := context.Background()
	addr, _, _ := imaptest.Start(t, imaptest.Full)
	c := imapUser(t, addr, imaptest.User, imaptest.Pass)
	appendMsg(t, c, "INBOX", "r1@test", "Umowa", "", 6*time.Hour)
	appendMsg(t, c, "INBOX", "r2@test", "Re: Umowa", "r1@test", 5*time.Hour)
	appendMsg(t, c, "INBOX", "r3@test", "Faktura", "", 4*time.Hour)
	appendMsg(t, c, "INBOX", "r4@test", "Na później", "", 3*time.Hour)
	appendMsg(t, c, "Sent", "r5@test", "Re: Faktura", "r3@test", 2*time.Hour)

	s := startStack(t, newDB(t), newBlobs(t))
	userID, accID := s.addAccount("anna@silo.test", addr, imaptest.User, imaptest.Pass)
	s.waitSettled("initial sync")
	s.labelMirrored(userID, accID, "Projekt", s.msgID(accID, "r1@test"), s.msgID(accID, "r2@test"))
	s.labelLocal(userID, accID, "Prywatne", s.msgID(accID, "r3@test"), s.msgID(accID, "r5@test"))
	s.snooze(userID, s.msgID(accID, "r4@test"), time.Now().Add(time.Hour).Truncate(time.Second))
	d, err := s.dr.Create(ctx, drafts.CreateParams{UserID: userID, ReplyTo: s.msgID(accID, "r2@test"), Mode: drafts.ModeReply})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.dr.Update(ctx, drafts.UpdateParams{UserID: userID, ID: d.ID, Version: d.Version, To: d.To,
		Subject: d.Subject, BodyHTML: "<p>Odpowiedź w toku</p>"}); err != nil {
		t.Fatal(err)
	}
	// A message scheduled in an hour: local until then, it does not block
	// the rebuild and stays queued.
	sd, err := s.dr.Create(ctx, drafts.CreateParams{UserID: userID, AccountID: accID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.dr.Update(ctx, drafts.UpdateParams{UserID: userID, ID: sd.ID, Version: sd.Version,
		To: []mail.Address{{Email: "jan@example.com"}}, Subject: "Zaplanowana", BodyHTML: "<p>Za godzinę</p>"}); err != nil {
		t.Fatal(err)
	}
	sendAt := time.Now().Add(time.Hour).Truncate(time.Second)
	if _, err := s.dr.Send(ctx, drafts.SendParams{UserID: userID, ID: sd.ID, SendAt: &sendAt}); err != nil {
		t.Fatal(err)
	}
	s.waitSettled("labels, snooze and draft copy written to the server")
	labels, threads, snoozes := s.userLabels(), s.threadPartition(accID), s.snoozes()
	messages := s.count(`SELECT count(*) FROM messages WHERE account_id = $1`, accID)
	t.Logf("before: labels %v threads %v snoozes %v", labels, threads, snoozes)
	if len(labels) != 4 || len(threads) != 4 || len(snoozes) != 1 {
		t.Fatalf("source state: labels %v threads %v snoozes %v", labels, threads, snoozes)
	}

	// Pending write-back blocks the rebuild; so does a send that is due.
	s.exec(`INSERT INTO jobs (kind, account_id, run_at) VALUES ('imap_op', $1, now() + interval '1 hour')`, accID)
	if err := s.sync.RequestResync(ctx, userID, accID, silosync.ResyncRebuild); !errors.Is(err, silosync.ErrRebuildBlocked) {
		t.Fatalf("rebuild with pending ops: %v", err)
	}
	s.exec(`DELETE FROM jobs WHERE kind = 'imap_op' AND account_id = $1`, accID)
	s.exec(`UPDATE outbound SET send_at = now() + interval '20 seconds' WHERE account_id = $1`, accID)
	if err := s.sync.RequestResync(ctx, userID, accID, silosync.ResyncRebuild); !errors.Is(err, silosync.ErrRebuildBlocked) {
		t.Fatalf("rebuild with a send due: %v", err)
	}
	s.exec(`UPDATE outbound SET send_at = $2 WHERE account_id = $1`, accID, sendAt)
	if err := s.sync.RequestResync(ctx, userID+1000, accID, silosync.ResyncRebuild); !errors.Is(err, silosync.ErrNotFound) {
		t.Fatalf("rebuild of another user's account: %v", err)
	}

	maxBefore := s.id(`SELECT max(id) FROM messages WHERE account_id = $1`, accID)
	draftMsg := s.id(`SELECT message_id FROM drafts WHERE account_id = $1 AND in_reply_to_message_id IS NOT NULL`, accID)
	scheduledMsg := s.id(`SELECT message_id FROM outbound WHERE account_id = $1`, accID)
	if err := s.sync.RequestResync(ctx, userID, accID, silosync.ResyncRebuild); err != nil {
		t.Fatal(err)
	}
	// The worker wipes the cache; the draft and the scheduled message stay.
	waitFor(t, time.Minute, "cache wiped", func() bool {
		return s.count(`SELECT count(*) FROM messages WHERE account_id = $1 AND id <= $2 AND id <> ALL($3)`,
			accID, maxBefore, []int64{draftMsg, scheduledMsg}) == 0
	})
	if n := s.count(`SELECT count(*) FROM drafts WHERE message_id = $1`, draftMsg); n != 1 {
		t.Fatal("draft dropped by the rebuild")
	}
	s.waitSettled("rebuild")
	waitFor(t, time.Minute, "restore finished", func() bool {
		return s.count(`SELECT count(*) FROM account_rebuilds WHERE account_id = $1`, accID) == 0
	})
	st, err := s.sync.Status(ctx, userID, accID)
	if err != nil || st.State != silosync.StateLive || st.InitialSyncDoneAt == nil {
		t.Fatalf("status after rebuild: %+v %v", st, err)
	}
	if n := s.count(`SELECT count(*) FROM messages WHERE account_id = $1 AND id > $2`, accID, maxBefore); n != messages-2 {
		t.Errorf("re-synced messages = %d, want %d", n, messages-2)
	}
	diff(t, "user labels", labels, s.userLabels())
	diff(t, "threads", threads, s.threadPartition(accID))
	diff(t, "snoozes", snoozes, s.snoozes())
	if n := s.count(`SELECT count(*) FROM messages WHERE account_id = $1`, accID); n != messages {
		t.Errorf("messages after rebuild = %d, want %d (no duplicate draft copy)", n, messages)
	}
	// The reply draft points at its original again.
	if n := s.count(`SELECT count(*) FROM drafts d JOIN messages m ON m.id = d.in_reply_to_message_id WHERE d.message_id = $1 AND m.message_id_hdr = 'r2@test'`, draftMsg); n != 1 {
		t.Error("draft not linked to its original after the rebuild")
	}
	if n := s.count(`SELECT count(*) FROM outbound WHERE message_id = $1 AND status = 'queued' AND send_at = $2`, scheduledMsg, sendAt); n != 1 {
		t.Error("scheduled message not queued after the rebuild")
	}
	if ds, err := mail.Reconcile(ctx, s.pool, accID); err != nil || len(ds) != 0 {
		t.Errorf("reconcile after rebuild: %v %v", ds, err)
	}
}
