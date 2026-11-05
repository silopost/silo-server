package mail_test

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/blob"
	silomail "github.com/silopost/silo-server/internal/mail"
)

func mimeMessage() []byte {
	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 fake pdf content"))
	var b strings.Builder
	w := func(s string) { b.WriteString(strings.ReplaceAll(s, "\n", "\r\n")) }
	w("From: =?UTF-8?Q?Za=C5=BC=C3=B3=C5=82=C4=87?= <anna@example.com>\n")
	w("To: dev@silo.test\nSubject: Faktura i zalacznik\nDate: Mon, 02 Nov 2026 09:00:00 +0000\nMessage-ID: <mime1@example.com>\n")
	w("MIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=\"outer\"\n\n")
	w("--outer\nContent-Type: multipart/alternative; boundary=\"alt\"\n\n")
	w("--alt\nContent-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: quoted-printable\n\n")
	w("Dzie=C5=84 dobry,\n\nw za=C5=82=C4=85czniku faktura.\n\n> cytat poprzedniej wiadomosci\n\n-- \nAnna Podpis\n\n")
	w("--alt\nContent-Type: text/html; charset=utf-8\n\n<html><body><p>Dzień dobry</p><script>evil()</script></body></html>\n\n")
	w("--alt--\n\n")
	w("--outer\nContent-Type: application/pdf; name=\"faktura.pdf\"\nContent-Disposition: attachment; filename=\"faktura.pdf\"\nContent-Transfer-Encoding: base64\n\n")
	w(pdf + "\n\n--outer--\n")
	return []byte(b.String())
}

func mimeStructure() imap.BodyStructure {
	return &imap.BodyStructureMultiPart{
		Subtype: "mixed",
		Children: []imap.BodyStructure{
			&imap.BodyStructureMultiPart{Subtype: "alternative", Children: []imap.BodyStructure{
				&imap.BodyStructureSinglePart{Type: "text", Subtype: "plain", Params: map[string]string{"charset": "utf-8"}, Encoding: "quoted-printable", Size: 120},
				&imap.BodyStructureSinglePart{Type: "text", Subtype: "html", Params: map[string]string{"charset": "utf-8"}, Encoding: "7bit", Size: 90},
			}},
			&imap.BodyStructureSinglePart{Type: "application", Subtype: "pdf", Params: map[string]string{"name": "faktura.pdf"}, Encoding: "base64", Size: 36,
				Extended: &imap.BodyStructureSinglePartExt{Disposition: &imap.BodyStructureDisposition{Value: "attachment", Params: map[string]string{"filename": "faktura.pdf"}}}},
		},
	}
}

func TestParseBody(t *testing.T) {
	p := silomail.ParseBody(mimeMessage())
	if p.ParseError != "" {
		t.Fatalf("parse error: %s", p.ParseError)
	}
	if !strings.Contains(p.Text, "Dzień dobry") || !strings.Contains(p.Text, "w załączniku faktura") {
		t.Fatalf("text = %q", p.Text)
	}
	if p.Snippet != "Dzień dobry, w załączniku faktura." {
		t.Fatalf("snippet = %q (quotes and signature must be skipped)", p.Snippet)
	}
	if len(p.Attachments) != 1 || p.Attachments[0].PartPath != "2" || p.Attachments[0].Filename != "faktura.pdf" ||
		!bytes.Equal(p.Attachments[0].Data, []byte("%PDF-1.4 fake pdf content")) {
		t.Fatalf("attachments = %+v", p.Attachments)
	}
	// HTML only.
	html := []byte("From: a@example.com\r\nSubject: h\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<style>p{}</style><p>Jeden</p><p>Dwa &amp; trzy</p>")
	if p := silomail.ParseBody(html); p.Text != "Jeden\n\nDwa & trzy" && p.Text != "Jeden\nDwa & trzy" {
		t.Fatalf("html text = %q", p.Text)
	}
	// Garbage never panics and reports nothing fatal.
	_ = silomail.ParseBody([]byte("\x00\x01 not a message"))
	_ = silomail.ParseBody(nil)
}

func TestIngestBodyAndPurge(t *testing.T) {
	e := newEnv(t)
	store, err := blob.NewFSStore(t.TempDir())
	must(t, err)
	raw := mimeMessage()
	m := e.fetched("INBOX", raw)
	m.BodyStructure = mimeStructure()
	m.Header = raw[:bytes.Index(raw, []byte("\r\n\r\n"))+4]
	m.TextSnippet = nil
	e.ingest("INBOX", m)
	mid := e.messageIDByHdr("mime1@example.com")

	// Header-only state: attachment row from BODYSTRUCTURE, no blob yet.
	var hasAtt bool
	var state string
	must(t, e.pool.QueryRow(e.ctx, `SELECT has_attachments, body_state FROM messages WHERE id = $1`, mid).Scan(&hasAtt, &state))
	if !hasAtt || state != "headers" {
		t.Fatalf("after ingest: has_attachments=%v body_state=%q", hasAtt, state)
	}
	if n := e.count(`SELECT count(*) FROM attachments WHERE message_id = $1 AND part_path = '2' AND blob_sha256 IS NULL AND filename = 'faktura.pdf'`, mid); n != 1 {
		t.Fatalf("attachment rows from BODYSTRUCTURE = %d", n)
	}
	var from string
	must(t, e.pool.QueryRow(e.ctx, `SELECT from_name FROM messages WHERE id = $1`, mid).Scan(&from))
	if from != "Zażółć" {
		t.Fatalf("from_name = %q", from)
	}

	ingestBody := func() {
		must(t, e.tx(func(tx pgx.Tx) error {
			_, err := silomail.IngestBody(e.ctx, tx, store, mid, raw)
			return err
		}))
	}
	ingestBody()
	ingestBody() // idempotent
	must(t, e.pool.QueryRow(e.ctx, `SELECT body_state FROM messages WHERE id = $1`, mid).Scan(&state))
	if state != "full" {
		t.Fatalf("body_state = %q", state)
	}
	if n := e.count(`SELECT count(*) FROM attachments WHERE message_id = $1`, mid); n != 1 {
		t.Fatalf("attachments = %d, want the BODYSTRUCTURE row updated in place", n)
	}
	if n := e.count(`SELECT count(*) FROM attachments WHERE message_id = $1 AND blob_sha256 IS NOT NULL`, mid); n != 1 {
		t.Fatal("attachment blob not linked")
	}
	if n := e.count(`SELECT count(*) FROM blobs WHERE ref_count = 1`); n < 2 {
		t.Fatalf("blobs with ref_count 1 = %d, want raw + attachment", n)
	}
	if n := e.count(`SELECT count(*) FROM blobs WHERE ref_count > 1`); n != 0 {
		t.Fatalf("re-running IngestBody leaked references (%d blobs)", n)
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE id = $1 AND search_tsv @@ plainto_tsquery('simple', unaccent('zalaczniku')) AND search_tsv @@ plainto_tsquery('simple', 'faktura.pdf')`, mid); n != 1 {
		t.Fatal("search_tsv does not contain body and attachment name")
	}
	var body, snippet string
	must(t, e.pool.QueryRow(e.ctx, `SELECT body_text, snippet FROM messages WHERE id = $1`, mid).Scan(&body, &snippet))
	if !strings.Contains(body, "faktura") || strings.Contains(snippet, "cytat") {
		t.Fatalf("body %q snippet %q", body, snippet)
	}
	var sha []byte
	must(t, e.pool.QueryRow(e.ctx, `SELECT raw_blob_sha256 FROM messages WHERE id = $1`, mid).Scan(&sha))
	rc, err := store.Open(e.ctx, bytesHex(sha))
	must(t, err)
	_ = rc.Close()
	e.reconcileClean()

	// The message disappears from the server; PurgeOrphans releases the blobs.
	must(t, e.tx(func(tx pgx.Tx) error {
		_, err := silomail.LocationVanished(e.ctx, tx, e.folders["INBOX"], []uint32{m.UID})
		return err
	}))
	var pr *silomail.PurgeResult
	purge := func(older time.Time) {
		must(t, e.tx(func(tx pgx.Tx) (err error) {
			pr, err = silomail.PurgeOrphans(e.ctx, tx, e.accountID, older)
			return err
		}))
	}
	purge(time.Now().Add(-time.Hour)) // too recent: nothing happens
	if pr.Messages != 0 {
		t.Fatalf("purged too early: %+v", pr)
	}
	purge(time.Now().Add(time.Minute))
	if pr.Messages != 1 || pr.Threads != 1 || pr.BlobsReleased != 2 {
		t.Fatalf("purge = %+v", pr)
	}
	if n := e.count(`SELECT count(*) FROM blobs WHERE ref_count <> 0`); n != 0 {
		t.Fatalf("blobs still referenced: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM threads WHERE account_id = $1`, e.accountID); n != 0 {
		t.Fatalf("threads left: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'thread.deleted'`, e.userID); n != 1 {
		t.Fatalf("thread.deleted events = %d", n)
	}
	e.reconcileClean()
}

func bytesHex(b []byte) string {
	const hexd = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hexd[c>>4], hexd[c&15])
	}
	return string(out)
}

func TestParseErrorDoesNotBlock(t *testing.T) {
	e := newEnv(t)
	bad := silomail.FetchedMessage{UID: 1, Size: 42, InternalDate: baseTime, Header: []byte("Date: not a date\r\nFrom: <<<broken\r\nSubject: ok\r\n\r\n")}
	e.ingest("INBOX", bad)
	empty := silomail.FetchedMessage{UID: 2, Size: 10, InternalDate: baseTime.Add(time.Hour)}
	res := e.ingest("INBOX", empty)
	if len(res.Messages) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE account_id = $1 AND parse_error IS NOT NULL`, e.accountID); n != 2 {
		t.Fatalf("messages with parse_error = %d, want 2", n)
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE account_id = $1 AND message_id_hdr LIKE '%@silo.invalid'`, e.accountID); n != 2 {
		t.Fatalf("synthetic ids = %d", n)
	}
	e.reconcileClean()
}
