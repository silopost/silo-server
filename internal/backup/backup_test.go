package backup_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/backup"
	"github.com/silopost/silo-server/internal/drafts"
	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

// snapshot is what a backup must preserve (compared before and after).
type snapshot struct {
	labels      []string
	inboxA      []string
	inboxB      []string
	snoozes     []string
	filters     []string
	contacts    []string
	history     []string
	drafts      []string
	outbound    []string
	attachments []string
}

func (s *stack) snapshot(userA, accA, userB, accB int64) snapshot {
	s.t.Helper()
	return snapshot{
		labels:  s.userLabels(),
		inboxA:  s.inbox(userA, accA),
		inboxB:  s.inbox(userB, accB),
		snoozes: s.snoozes(),
		filters: s.strings(`SELECT u.email::text || '|' || f.criteria || '|' || f.actions::text FROM filters f JOIN users u ON u.id = f.user_id ORDER BY 1`),
		contacts: s.strings(`SELECT u.email::text || '|' || c.email::text || '|' || c.name FROM contacts c JOIN users u ON u.id = c.user_id
WHERE c.is_manual ORDER BY 1`),
		history: s.strings(`SELECT a.username || '|' || h.query FROM search_history h JOIN accounts a ON a.id = h.account_id ORDER BY 1`),
		drafts: s.strings(`
SELECT d.silo_draft_id || '|' || m.message_id_hdr || '|' || d.subject || '|' || d.to_addrs::text || '|' || d.version || '|' ||
       cardinality(d.attachment_ids) || '|' || m.body_state || '|' || to_char(m.internal_date AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS')
FROM drafts d JOIN messages m ON m.id = d.message_id ORDER BY 1`),
		outbound: s.strings(`
SELECT d.silo_draft_id || '|' || o.status || '|' || to_char(o.send_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS') || '|' ||
       encode(m.raw_blob_sha256, 'hex') || '|' ||
       (SELECT count(*) FROM jobs j WHERE j.kind = 'send' AND j.status = 'pending' AND (j.payload->>'outboundId')::bigint = o.id
          AND j.run_at = o.send_at)
FROM outbound o JOIN drafts d ON d.message_id = o.message_id JOIN messages m ON m.id = o.message_id ORDER BY 1`),
		attachments: s.strings(`
SELECT d.silo_draft_id || '|' || a.filename || '|' || encode(a.blob_sha256, 'hex') || '|' || b.kind || '|' || b.ref_count
FROM attachments a JOIN drafts d ON d.message_id = a.message_id JOIN blobs b ON b.sha256 = a.blob_sha256 ORDER BY 1`),
	}
}

func diff(t *testing.T, what string, before, after []string) {
	t.Helper()
	if !slices.Equal(before, after) {
		t.Errorf("%s differ:\nbefore %q\nafter  %q", what, before, after)
	}
}

func (a snapshot) compare(t *testing.T, b snapshot) {
	t.Helper()
	diff(t, "user labels", a.labels, b.labels)
	diff(t, "inbox A", a.inboxA, b.inboxA)
	diff(t, "inbox B", a.inboxB, b.inboxB)
	diff(t, "snoozes", a.snoozes, b.snoozes)
	diff(t, "filters", a.filters, b.filters)
	diff(t, "contacts", a.contacts, b.contacts)
	diff(t, "search history", a.history, b.history)
	diff(t, "drafts", a.drafts, b.drafts)
	diff(t, "outbound", a.outbound, b.outbound)
	diff(t, "attachments", a.attachments, b.attachments)
}

// TestExportImportRoundTrip is the backup DoD of phase 5 at package level:
// two users with an account each, user labels (mirrored as keywords and
// local only), a draft with an attachment, a scheduled message, a snooze, a
// filter, a manual contact and search history; export, a fresh database
// and blob store, import, sync: everything is back, the Inbox lists are
// the same.
func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	addr, _, _ := imaptest.Start(t, imaptest.Full)
	ca := imapUser(t, addr, imaptest.User, imaptest.Pass)
	appendMsg(t, ca, "INBOX", "a1@test", "Plan projektu", "", 5*time.Hour)
	appendMsg(t, ca, "INBOX", "a2@test", "Re: Plan projektu", "a1@test", 4*time.Hour)
	appendMsg(t, ca, "INBOX", "a3@test", "Faktura", "", 3*time.Hour)
	appendMsg(t, ca, "INBOX", "a4@test", "Na później", "", 2*time.Hour)
	cb := imapUser(t, addr, imaptest.User2, imaptest.Pass2)
	appendMsg(t, cb, "INBOX", "b1@test", "Spotkanie", "", 3*time.Hour)
	appendMsg(t, cb, "INBOX", "b2@test", "Raport", "", 2*time.Hour)

	src := startStack(t, newDB(t), newBlobs(t))
	userA, accA := src.addAccount("anna@silo.test", addr, imaptest.User, imaptest.Pass)
	userB, accB := src.addAccount("bartek@silo.test", addr, imaptest.User2, imaptest.Pass2)
	src.waitSettled("source initial sync")
	if n := src.count(`SELECT count(*) FROM accounts WHERE keywords_supported`); n != 2 {
		t.Fatalf("keywords_supported on %d accounts", n)
	}
	src.exec(`UPDATE users SET settings = jsonb_build_object('active_account_id', $2::bigint, 'density', 'compact') WHERE id = $1`, userA, accA)

	// Labels: mirrored (keyword on the server), local only on a server with
	// keywords, local only on an account marked without keywords.
	src.labelMirrored(userA, accA, "Projekt", src.msgID(accA, "a1@test"), src.msgID(accA, "a2@test"))
	src.labelLocal(userA, accA, "Prywatne", src.msgID(accA, "a3@test"))
	src.exec(`UPDATE accounts SET keywords_supported = false WHERE id = $1`, accB)
	src.labelLocal(userB, accB, "Lokalna", src.msgID(accB, "b1@test"), src.msgID(accB, "b2@test"))
	src.snooze(userA, src.msgID(accA, "a4@test"), time.Now().Add(2*time.Hour).Truncate(time.Second))

	// A draft with an attachment and a message scheduled in an hour.
	pdf := bytes.Repeat([]byte("%PDF-1.4 plan "), 4000)
	up, err := src.dr.Upload(ctx, userA, "plan.pdf", "application/pdf", false, bytes.NewReader(pdf))
	if err != nil {
		t.Fatal(err)
	}
	d1, err := src.dr.Create(ctx, drafts.CreateParams{UserID: userA, AccountID: accA})
	if err != nil {
		t.Fatal(err)
	}
	to := []mail.Address{{Email: "jan@example.com", Name: "Jan"}}
	if _, err := src.dr.Update(ctx, drafts.UpdateParams{UserID: userA, ID: d1.ID, Version: d1.Version, To: to,
		Subject: "Szkic z załącznikiem", BodyHTML: "<p>Plan w załączniku</p>", AttachmentIDs: []int64{up.ID}}); err != nil {
		t.Fatal(err)
	}
	d2, err := src.dr.Create(ctx, drafts.CreateParams{UserID: userA, AccountID: accA})
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.dr.Update(ctx, drafts.UpdateParams{UserID: userA, ID: d2.ID, Version: d2.Version, To: to,
		Subject: "Zaplanowana", BodyHTML: "<p>Za godzinę</p>"})
	if err != nil {
		t.Fatal(err)
	}
	sendAt := time.Now().Add(time.Hour).Truncate(time.Second)
	if _, err := src.dr.Send(ctx, drafts.SendParams{UserID: userA, ID: d2.ID, SendAt: &sendAt}); err != nil {
		t.Fatal(err)
	}

	src.exec(`INSERT INTO filters (user_id, account_id, criteria, actions, position) VALUES ($1, $2, 'from:szef@example.com', '{"skipInbox": true}', 0)`, userA, accA)
	src.exec(`INSERT INTO contacts (user_id, email, name, is_manual) VALUES ($1, 'kontakt@example.com', 'Kontakt Ręczny', true)`, userB)
	src.exec(`INSERT INTO contacts (user_id, email, name, received_count) VALUES ($1, 'auto@example.com', 'Automatyczny', 3)`, userB)
	src.exec(`INSERT INTO search_history (user_id, account_id, query) VALUES ($1, $2, 'faktura')`, userA, accA)
	src.waitSettled("labels and drafts written to the server")
	before := src.snapshot(userA, accA, userB, accB)
	if len(before.labels) != 5 || len(before.inboxA) != 2 || // the snoozed thread left INBOX
		len(before.snoozes) != 1 || len(before.outbound) != 1 || len(before.attachments) != 1 {
		t.Fatalf("source state incomplete: %+v", before)
	}
	t.Logf("source state: %+v", before)
	pdfSum := sha256.Sum256(pdf)
	if !strings.Contains(before.attachments[0], hex.EncodeToString(pdfSum[:])) {
		t.Fatalf("attachment blob: %v", before.attachments)
	}

	var buf bytes.Buffer
	st, err := backup.Export(ctx, src.pool, &buf, backup.ExportOptions{Keys: src.ring, Blobs: src.blobs})
	if err != nil {
		t.Fatal(err)
	}
	if st.Users != 2 || st.Accounts != 2 || st.Labels != 3 || st.LabelAssignments != 5 || st.Snoozes != 1 || st.Drafts != 2 ||
		st.Outbound != 1 || st.Contacts != 1 || st.Blobs != 2 || len(st.Warnings) != 0 {
		t.Fatalf("export stats: %+v", st)
	}
	exported := buf.String()
	for _, secret := range []string{imaptest.Pass, imaptest.Pass2, `"id":`, "active_account_id"} {
		if strings.Contains(exported, secret) {
			t.Errorf("export contains %q", secret)
		}
	}
	src.stop()

	// A section from a newer server is skipped with a warning.
	doc := strings.Replace(exported, `"sections":{`, `"sections":{"futureSection":[{"x":1}],`, 1)

	dst := newDB(t)
	dstBlobs := newBlobs(t)
	ist, err := backup.Import(ctx, dst, strings.NewReader(doc), backup.ImportOptions{Keys: keyring(t), Blobs: dstBlobs})
	if err != nil {
		t.Fatal(err)
	}
	if len(ist.Warnings) != 1 || !strings.Contains(ist.Warnings[0], "futureSection") {
		t.Errorf("import warnings: %v", ist.Warnings)
	}
	if ist.Users != 2 || ist.Accounts != 2 || ist.Labels != 3 || ist.Drafts != 2 || ist.Outbound != 1 || ist.Blobs != 2 {
		t.Errorf("import stats: %+v", ist)
	}
	var staged int
	if err := dst.QueryRow(ctx, `SELECT count(*) FROM metadata_restores`).Scan(&staged); err != nil || staged != 4 {
		t.Errorf("staged rows = %d (%v), want 4: Projekt, Prywatne and the snooze of A, Lokalna of B", staged, err)
	}
	tgt := startStack(t, dst, dstBlobs)
	tgt.waitSettled("target sync and restore")

	newA := tgt.id(`SELECT a.id FROM accounts a JOIN users u ON u.id = a.user_id WHERE u.email = 'anna@silo.test'`)
	newB := tgt.id(`SELECT a.id FROM accounts a JOIN users u ON u.id = a.user_id WHERE u.email = 'bartek@silo.test'`)
	newUA := tgt.id(`SELECT id FROM users WHERE email = 'anna@silo.test'`)
	newUB := tgt.id(`SELECT id FROM users WHERE email = 'bartek@silo.test'`)
	after := tgt.snapshot(newUA, newA, newUB, newB)
	before.compare(t, after)

	// The account secret decrypts with the same key and the new id.
	var sealed []byte
	if err := tgt.pool.QueryRow(ctx, `SELECT secret_enc FROM accounts WHERE id = $1`, newA).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if plain, err := tgt.ring.Decrypt(sealed, []byte(strconv.FormatInt(newA, 10))); err != nil || string(plain) != imaptest.Pass {
		t.Errorf("secret after import: %q %v", plain, err)
	}
	if n := tgt.count(`SELECT count(*) FROM users WHERE id = $1 AND (settings->>'active_account_id')::bigint = $2 AND settings->>'density' = 'compact'`, newUA, newA); n != 1 {
		t.Error("settings.active_account_id not translated")
	}
	// The draft and its attachment are readable; the blob is the same.
	var draftID int64
	if err := tgt.pool.QueryRow(ctx, `SELECT message_id FROM drafts WHERE subject = 'Szkic z załącznikiem'`).Scan(&draftID); err != nil {
		t.Fatal(err)
	}
	d, err := tgt.dr.Get(ctx, newUA, draftID)
	if err != nil || len(d.Attachments) != 1 || d.Attachments[0].Filename != "plan.pdf" || len(d.To) != 1 {
		t.Fatalf("draft after import: %+v %v", d, err)
	}
	rc, err := dstBlobs.Open(ctx, hex.EncodeToString(pdfSum[:]))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, pdf) {
		t.Error("attachment content differs")
	}
	// The server copies of the drafts were recognised, not duplicated.
	if n := tgt.count(`SELECT count(*) FROM messages WHERE account_id = $1 AND subject IN ('Szkic z załącznikiem', 'Zaplanowana')`, newA); n != 2 {
		t.Errorf("draft messages after sync = %d, want 2", n)
	}

	// --merge again changes nothing; without --merge the users exist.
	counts := func() []string {
		return tgt.strings(`
SELECT t || '=' || n FROM (
  SELECT 'users' t, count(*) n FROM users UNION ALL SELECT 'accounts', count(*) FROM accounts
  UNION ALL SELECT 'labels', count(*) FROM labels WHERE type = 'user' UNION ALL SELECT 'message_labels', count(*) FROM message_labels
  UNION ALL SELECT 'drafts', count(*) FROM drafts UNION ALL SELECT 'outbound', count(*) FROM outbound
  UNION ALL SELECT 'snoozes', count(*) FROM snoozes UNION ALL SELECT 'snooze_items', count(*) FROM snooze_items
  UNION ALL SELECT 'filters', count(*) FROM filters UNION ALL SELECT 'contacts', count(*) FROM contacts
  UNION ALL SELECT 'history', count(*) FROM search_history UNION ALL SELECT 'messages', count(*) FROM messages
) x ORDER BY 1`)
	}
	c1 := counts()
	if _, err := backup.Import(ctx, dst, strings.NewReader(exported), backup.ImportOptions{Keys: tgt.ring, Blobs: dstBlobs, Merge: true}); err != nil {
		t.Fatal(err)
	}
	tgt.waitSettled("merge import restore")
	diff(t, "row counts after --merge", c1, counts())
	tgt.snapshot(newUA, newA, newUB, newB).compare(t, after)
	if _, err := backup.Import(ctx, dst, strings.NewReader(exported), backup.ImportOptions{Keys: tgt.ring, Blobs: dstBlobs}); !errors.Is(err, backup.ErrUserExists) {
		t.Errorf("import into existing users without --merge: %v", err)
	}
	// Another key cannot open the secrets.
	other, err := secrets.NewKeyring(bytes.Repeat([]byte("o"), 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Import(ctx, newDB(t), strings.NewReader(exported), backup.ImportOptions{Keys: other, Blobs: newBlobs(t)}); err == nil ||
		!strings.Contains(err.Error(), "SILO_SECRET_KEY") {
		t.Errorf("import with another key: %v", err)
	}
}

// TestExportNoLocalBlobs: --no-local-blobs leaves the bytes out and keeps
// the sha256; a user filter exports one user.
func TestExportNoLocalBlobs(t *testing.T) {
	ctx := context.Background()
	s := startStack(t, newDB(t), newBlobs(t))
	s.exec(`INSERT INTO users (email) VALUES ('a@x.test'), ('b@x.test')`)
	var buf bytes.Buffer
	st, err := backup.Export(ctx, s.pool, &buf, backup.ExportOptions{Keys: s.ring, Blobs: s.blobs, User: "B@x.test", NoLocalBlobs: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.Users != 1 || !strings.Contains(buf.String(), `"localBlobs":false`) || !strings.Contains(buf.String(), "b@x.test") ||
		strings.Contains(buf.String(), "a@x.test") {
		t.Errorf("export: %+v %s", st, buf.String())
	}
	if _, err := backup.Export(ctx, s.pool, io.Discard, backup.ExportOptions{Keys: s.ring, User: "nobody@x.test"}); err == nil {
		t.Error("unknown user exported")
	}
	if _, err := backup.Import(ctx, s.pool, strings.NewReader(`{"format":"other","version":1,"sections":{}}`), backup.ImportOptions{Keys: s.ring}); !errors.Is(err, backup.ErrFormat) {
		t.Errorf("foreign document: %v", err)
	}
	if _, err := backup.Import(ctx, s.pool, strings.NewReader(`{"format":"silo-metadata","version":99,"sections":{}}`), backup.ImportOptions{Keys: s.ring}); !errors.Is(err, backup.ErrFormat) {
		t.Errorf("newer version: %v", err)
	}
}
