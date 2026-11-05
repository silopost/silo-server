package sync_test

import (
	"context"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/drafts"
	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
	"github.com/silopost/silo-server/internal/testutil/smtptest"
	"github.com/silopost/silo-server/internal/worker"
)

// TestScheduledSend checks plan 3.9 step 3 and the Faza 4 DoD with short
// times: a scheduled message is on the server in no folder before its time
// (its copy in Drafts goes when it is scheduled), it is delivered by the send
// job within 30 s of sendAt and then saved in Sent; a cancelled schedule puts
// the draft back in Drafts.
func TestScheduledSend(t *testing.T) {
	ctx := context.Background()
	e := newOpsEnv(t, imaptest.Full, time.Second, spec{"INBOX", 2})
	srv := smtptest.StartServer(t, e.mb.user, e.mb.pass, 0)
	_, err := testPool.Exec(ctx, `UPDATE accounts SET smtp_host = $2, smtp_port = $3, smtp_tls = 'none', sent_append = true WHERE id = $1`,
		e.a.id, srv.Host, srv.Port)
	must(t, err)
	svc := drafts.New(drafts.Options{Pool: testPool, Blobs: e.a.deps.Blobs, SyncDelay: 300 * time.Millisecond, MaxUploadBytes: 1 << 20,
		Keys: keyring, InsecureTransport: true})
	e.extra = func(w *worker.Worker) { svc.RegisterJobs(w) }
	e.restart()

	compose := func(subject string) (int64, string) {
		t.Helper()
		d, err := svc.Create(ctx, drafts.CreateParams{UserID: e.a.userID, AccountID: e.a.id, Origin: "test"})
		must(t, err)
		_, err = svc.Update(ctx, drafts.UpdateParams{UserID: e.a.userID, ID: d.ID, Version: d.Version, Subject: subject,
			BodyHTML: "<p>Later</p>", To: []mail.Address{{Email: "jan@firma.pl"}}})
		must(t, err)
		var hdr string
		must(t, testPool.QueryRow(ctx, `SELECT message_id_hdr FROM messages WHERE id = $1`, d.ID).Scan(&hdr))
		waitFor(t, 20*time.Second, "draft copy in Drafts", func() bool {
			f := e.serverFolders(hdr)
			return len(f) == 1 && f[0] == "Drafts"
		})
		e.waitNoJobs(10 * time.Second)
		return d.ID, hdr
	}

	// ---- scheduled 8 s ahead (beyond the 5 s undo-send window)
	id, hdr := compose("Scheduled")
	sendAt := time.Now().Add(8 * time.Second)
	res, err := svc.Send(ctx, drafts.SendParams{UserID: e.a.userID, ID: id, SendAt: &sendAt, Origin: "test"})
	must(t, err)
	if !res.UndoUntil.Equal(sendAt) {
		t.Errorf("send_at = %s, want %s", res.UndoUntil, sendAt)
	}
	if l := e.a.labelsOf(id); !l["SCHEDULED"] || len(l) != 1 {
		t.Errorf("labels of the scheduled message: %v", l)
	}
	waitFor(t, 5*time.Second, "draft copy removed from Drafts", func() bool { return len(e.serverFolders(hdr)) == 0 })
	for time.Until(sendAt) > 500*time.Millisecond {
		if f := e.serverFolders(hdr); len(f) != 0 {
			t.Fatalf("scheduled message on the server before its time: %v", f)
		}
		if n := len(srv.Messages()); n != 0 {
			t.Fatalf("delivered before its time: %d", n)
		}
		time.Sleep(300 * time.Millisecond)
	}
	waitFor(t, 40*time.Second, "sent", func() bool {
		return e.a.int(`SELECT count(*) FROM outbound WHERE id = $1 AND status = 'sent'`, res.OutboundID) == 1
	})
	var sentAt time.Time
	must(t, testPool.QueryRow(ctx, `SELECT updated_at FROM outbound WHERE id = $1`, res.OutboundID).Scan(&sentAt))
	if d := sentAt.Sub(sendAt); d < 0 || d > 30*time.Second {
		t.Errorf("sent %s after sendAt, want within 30 s", d)
	}
	t.Logf("delivered %s after sendAt", sentAt.Sub(sendAt).Round(time.Millisecond))
	if n := len(srv.Messages()); n != 1 {
		t.Errorf("delivered %d messages", n)
	}
	waitFor(t, 20*time.Second, "copy in Sent only", func() bool {
		f := e.serverFolders(hdr)
		return len(f) == 1 && f[0] == "Sent"
	})

	// ---- cancelled schedule: the draft goes back to Drafts
	id2, hdr2 := compose("Cancelled")
	later := time.Now().Add(time.Hour)
	res2, err := svc.Send(ctx, drafts.SendParams{UserID: e.a.userID, ID: id2, SendAt: &later, Origin: "test"})
	must(t, err)
	waitFor(t, 5*time.Second, "draft copy removed from Drafts", func() bool { return len(e.serverFolders(hdr2)) == 0 })
	_, err = svc.Cancel(ctx, e.a.userID, res2.OutboundID, "test")
	must(t, err)
	waitFor(t, 20*time.Second, "draft saved in Drafts again", func() bool {
		f := e.serverFolders(hdr2)
		return len(f) == 1 && f[0] == "Drafts"
	})
	if l := e.a.labelsOf(id2); !l["DRAFT"] || l["SCHEDULED"] {
		t.Errorf("labels after cancel: %v", l)
	}
	if n := len(srv.Messages()); n != 1 {
		t.Errorf("delivered %d messages, want 1", n)
	}
	e.waitNoJobs(10 * time.Second)
	e.reconcile()
}
