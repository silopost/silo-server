package mail_test

import (
	"bytes"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-message"
	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/blob"
	silomail "github.com/silopost/silo-server/internal/mail"
)

// TestBuildSendMIMERelated checks the structure of a sent draft (plan 3.9
// step 1): mixed( related( alternative(text, html), inline... ), attachments... ).
// An inline picture the HTML does not show is sent as a plain attachment.
func TestBuildSendMIMERelated(t *testing.T) {
	e := newEnv(t)
	store, err := blob.NewFSStore(t.TempDir())
	must(t, err)
	put := func(data string) []byte {
		sum, n, err := store.Put(e.ctx, strings.NewReader(data))
		must(t, err)
		sha, _ := hex.DecodeString(sum)
		_, err = e.pool.Exec(e.ctx, `INSERT INTO blobs (sha256, size_bytes, kind, ref_count) VALUES ($1, $2, 'local', 1) ON CONFLICT DO NOTHING`, sha, n)
		must(t, err)
		return sha
	}
	build := func(html string, atts ...[4]string) string { // filename, content type, content id, data
		t.Helper()
		var raw []byte
		must(t, e.tx(func(tx pgx.Tx) error {
			id := strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
			msgID, _, err := silomail.CreateDraftMessage(e.ctx, tx, silomail.NewDraft{AccountID: e.accountID, UserID: e.userID,
				MessageIDHdr: id + "@silo.test", SiloDraftID: "d_" + id, DraftFields: silomail.DraftFields{
					From: silomail.Address{Email: e.email}, To: []silomail.Address{{Email: "jan@firma.pl"}}, Subject: "Pic", BodyHTML: html}})
			if err != nil {
				return err
			}
			if _, err := tx.Exec(e.ctx, `INSERT INTO drafts (message_id, user_id, account_id, to_addrs, subject, body_html, silo_draft_id)
VALUES ($1, $2, $3, '[{"email":"jan@firma.pl"}]', 'Pic', $4, $5)`, msgID, e.userID, e.accountID, html, "d_"+id); err != nil {
				return err
			}
			for _, a := range atts {
				var cid *string
				if a[2] != "" {
					cid = &a[2]
				}
				if _, err := tx.Exec(e.ctx, `INSERT INTO attachments (message_id, user_id, blob_sha256, filename, content_type, size_bytes, part_path, is_inline, content_id)
VALUES ($1, $2, $3, $4, $5, $6, '', $7, $8)`, msgID, e.userID, put(a[3]), a[0], a[1], len(a[3]), cid != nil, cid); err != nil {
					return err
				}
			}
			d, err := silomail.BuildSendMIME(e.ctx, tx, store, msgID, time.Now())
			if err != nil {
				return err
			}
			raw = d.Raw
			return nil
		}))
		return string(raw)
	}
	// structure lists the media types of a message, depth first, with the
	// Content-ID and disposition of leaves.
	structure := func(raw string) string {
		t.Helper()
		ent, err := message.Read(strings.NewReader(raw))
		must(t, err)
		var out []string
		var walk func(e *message.Entity, depth int)
		walk = func(e *message.Entity, depth int) {
			mt, _, _ := e.Header.ContentType()
			s := strings.Repeat(" ", depth) + mt
			if mr := e.MultipartReader(); mr != nil {
				out = append(out, s)
				for {
					p, err := mr.NextPart()
					if err == io.EOF {
						break
					}
					must(t, err)
					walk(p, depth+1)
				}
				return
			}
			if disp, _, _ := e.Header.ContentDisposition(); disp != "" {
				s += " " + disp
			}
			if id := e.Header.Get("Content-Id"); id != "" {
				s += " " + id
			}
			body, _ := io.ReadAll(e.Body)
			if mt == "image/png" && !bytes.Equal(body, []byte("PNGDATA")) {
				s += " BADBODY"
			}
			out = append(out, s)
		}
		walk(ent, 0)
		return strings.Join(out, "\n")
	}

	png := [4]string{"pic.png", "image/png", "pic1@silo", "PNGDATA"}
	pdf := [4]string{"umowa.pdf", "application/pdf", "", "%PDF"}
	cases := []struct {
		name, html string
		atts       [][4]string
		want       string
	}{
		{"text only", "<p>Hi</p>", nil, "multipart/alternative\n text/plain\n text/html"},
		{"inline only", `<p>Hi <img src="cid:pic1@silo"></p>`, [][4]string{png},
			"multipart/related\n multipart/alternative\n  text/plain\n  text/html\n image/png inline <pic1@silo>"},
		{"inline and attachment", `<p>Hi <img src="CID:pic1@silo"></p>`, [][4]string{png, pdf},
			"multipart/mixed\n multipart/related\n  multipart/alternative\n   text/plain\n   text/html\n  image/png inline <pic1@silo>\n application/pdf attachment"},
		{"unused inline", "<p>Hi</p>", [][4]string{png},
			"multipart/mixed\n multipart/alternative\n  text/plain\n  text/html\n image/png attachment"},
	}
	for _, c := range cases {
		atts := c.atts
		got := structure(build(c.html, atts...))
		if got != c.want {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, got, c.want)
		}
	}
	raw := build(`<img src="cid:pic1@silo">`, png, pdf)
	for _, want := range []string{`type="multipart/alternative"`, "filename=umowa.pdf", "Content-Id: <pic1@silo>"} {
		if !strings.Contains(raw, want) {
			t.Errorf("no %q in\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "Bcc:") || strings.Contains(raw, silomail.HeaderSiloDraftID) {
		t.Error("sent form carries Bcc or X-Silo-Draft-Id")
	}
}
