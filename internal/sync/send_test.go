package sync_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/silopost/silo-server/internal/drafts"
	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
	"github.com/silopost/silo-server/internal/testutil/smtptest"
)

// TestSendSync checks plan 3.9 step 5 against Dovecot (the SMTP server is
// in-process): with sent_append the reply is in Sent exactly once, its draft
// copy is gone from Drafts, the original has \Answered and Silo holds one
// message; without sent_append a copy saved in Sent by the server itself is
// attached to the sent message instead of becoming a duplicate.
func TestSendSync(t *testing.T) {
	ctx := context.Background()
	e := newOpsEnv(t, imaptest.Full, time.Second, spec{"INBOX", 3})
	srv := smtptest.StartServer(t, e.mb.user, e.mb.pass, 0)
	_, err := testPool.Exec(ctx, `UPDATE accounts SET smtp_host = $2, smtp_port = $3, smtp_tls = 'none', sent_append = true WHERE id = $1`,
		e.a.id, srv.Host, srv.Port)
	must(t, err)
	svc := drafts.New(drafts.Options{Pool: testPool, Blobs: e.a.deps.Blobs, SyncDelay: 300 * time.Millisecond, MaxUploadBytes: 1 << 20,
		Keys: keyring, InsecureTransport: true})

	compose := func(replyTo int64, to string) (int64, string) {
		t.Helper()
		p := drafts.CreateParams{UserID: e.a.userID, AccountID: e.a.id, Origin: "test"}
		if replyTo != 0 {
			p.ReplyTo, p.Mode = replyTo, drafts.ModeReply
		}
		d, err := svc.Create(ctx, p)
		must(t, err)
		_, err = svc.Update(ctx, drafts.UpdateParams{UserID: e.a.userID, ID: d.ID, Version: d.Version, Subject: "Re: Message 1",
			BodyHTML: "<p>Reply</p>", To: []mail.Address{{Email: to}}, Bcc: []mail.Address{{Email: "hidden@firma.pl"}}})
		must(t, err)
		var hdr string
		must(t, testPool.QueryRow(ctx, `SELECT message_id_hdr FROM messages WHERE id = $1`, d.ID).Scan(&hdr))
		return d.ID, hdr
	}
	send := func(id int64) {
		t.Helper()
		r, err := svc.Send(ctx, drafts.SendParams{UserID: e.a.userID, ID: id, Origin: "test"})
		must(t, err)
		must(t, svc.SendOutbound(ctx, r.OutboundID))
		if n := e.a.int(`SELECT count(*) FROM outbound WHERE id = $1 AND status = 'sent'`, r.OutboundID); n != 1 {
			t.Fatal("not sent")
		}
	}
	silo := func(hdr string) int {
		return e.a.int(`SELECT count(*) FROM messages WHERE account_id = $1 AND message_id_hdr = $2`, e.a.id, hdr)
	}
	inSent := func(msgID int64) bool {
		return e.a.int(`SELECT count(*) FROM message_locations l JOIN mailbox_folders f ON f.id = l.folder_id
		                WHERE l.message_id = $1 AND f.role = 'sent'`, msgID) == 1
	}

	// ---- sent_append: Silo saves the copy in Sent
	origID, _ := e.thread("m1@silo.test")
	id, hdr := compose(origID, "jan@firma.pl")
	waitFor(t, 20*time.Second, "draft copy in Drafts", func() bool { return slices.Equal(e.serverFolders(hdr), []string{"Drafts"}) })
	e.waitNoJobs(10 * time.Second)
	send(id)
	waitFor(t, 20*time.Second, "one copy in Sent, none in Drafts", func() bool { return slices.Equal(e.serverFolders(hdr), []string{"Sent"}) })
	waitFor(t, 20*time.Second, "\\Answered on the original", func() bool {
		return slices.ContainsFunc(e.serverFlags("m1@silo.test"), func(f string) bool { return f == string(imap.FlagAnswered) })
	})
	e.waitNoJobs(10 * time.Second)
	e.mgr.Worker(e.a.id).RequestResync("incremental")
	waitFor(t, 20*time.Second, "sent message located in Sent", func() bool { return inSent(id) })
	time.Sleep(time.Second)
	if n := silo(hdr); n != 1 {
		t.Errorf("messages with the reply's Message-ID = %d, want 1", n)
	}
	if l := e.a.labelsOf(id); !l["SENT"] || l["DRAFT"] || l["SCHEDULED"] {
		t.Errorf("reply labels = %v", l)
	}
	if msgs := srv.Messages(); len(msgs) != 1 || !slices.Contains(msgs[0].To, "hidden@firma.pl") {
		t.Errorf("delivered: %+v", msgs)
	}
	e.reconcile()

	// ---- no sent_append: the server saves Sent itself (simulated by APPEND)
	_, err = testPool.Exec(ctx, `UPDATE accounts SET sent_append = false WHERE id = $1`, e.a.id)
	must(t, err)
	id2, hdr2 := compose(0, "ola@firma.pl")
	send(id2)
	if n := e.a.int(`SELECT count(*) FROM jobs j JOIN op_messages om ON om.job_id = j.id WHERE om.message_id = $1 AND j.payload->>'toRole' = 'sent'`, id2); n != 0 {
		t.Errorf("append to Sent without sent_append: %d jobs", n)
	}
	msgs := srv.Messages()
	appendMsg(t, e.other, "Sent", msgs[len(msgs)-1].Data, []imap.Flag{imap.FlagSeen}, time.Now())
	e.mgr.Worker(e.a.id).RequestResync("incremental")
	waitFor(t, 20*time.Second, "server copy attached to the sent message", func() bool { return inSent(id2) })
	e.waitNoJobs(10 * time.Second)
	time.Sleep(time.Second)
	if n := silo(hdr2); n != 1 {
		t.Errorf("messages with the second Message-ID = %d, want 1", n)
	}
	if l := e.a.labelsOf(id2); !l["SENT"] || len(l) != 1 {
		t.Errorf("second message labels = %v", l)
	}
	e.reconcile()
}
