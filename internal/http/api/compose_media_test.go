package api_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
	"github.com/silopost/silo-server/internal/testutil/smtptest"
)

// uploadPart posts one file with an explicit content type; inline adds the
// form field inline=true before the file.
func uploadPart(h http.Handler, l login, filename, contentType string, data []byte, inline bool) resp {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	if inline {
		_ = w.WriteField("inline", "true")
	}
	hd := make(map[string][]string)
	hd["Content-Disposition"] = []string{`form-data; name="file"; filename="` + filename + `"`}
	hd["Content-Type"] = []string{contentType}
	fw, _ := w.CreatePart(hd)
	_, _ = fw.Write(data)
	_ = w.Close()
	r := httptest.NewRequest("POST", "/api/v1/uploads", &b)
	r.Header.Set("Content-Type", w.FormDataContentType())
	r.AddCookie(l.cookie)
	r.Header.Set(api.CSRFHeader, l.csrf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return resp{rec}
}

func testImage(w, h int, alpha bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := uint8(255)
			if alpha && x < w/2 {
				a = 0
			}
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 90, A: a})
		}
	}
	return img
}

func encoded(t *testing.T, format string, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&b, img)
	case "jpeg":
		err = jpeg.Encode(&b, img, &jpeg.Options{Quality: 60})
	case "gif":
		err = gif.Encode(&b, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestComposeMedia covers plan 5.1/5.6 Faza 4: inline (cid:) uploads sent in
// multipart/related, cid: in received HTML, forwards carrying attachments,
// quotes of bodies that were not downloaded, and thumbnails.
func TestComposeMedia(t *testing.T) {
	s := newStack(t, stackOpts{heartbeat: time.Minute, buffer: 64})
	mb := &mailbox{t: t, host: s.cfg.DefaultIMAPHost, port: s.cfg.DefaultIMAPPort, raw: map[string][]byte{}, uids: map[string]imap.UID{}}
	c := mb.client()
	now := time.Now().Truncate(time.Second)
	mb.appendTo(c, "INBOX", 500, htmlMsg(500, "Media with PDF", "Look text", `<p>Look <img src="cid:`+logoCID+`"></p>`, now.Add(-3*time.Hour), true), true, now.Add(-3*time.Hour))
	mb.appendTo(c, "INBOX", 501, plainMsg(501, "Plain question", "Plain body line", now.Add(-2*time.Hour)), true, now.Add(-2*time.Hour))
	mb.appendTo(c, "INBOX", 502, htmlMsg(502, "Only inline", "x", `<p><img src="cid:`+logoCID+`"></p>`, now.Add(-time.Hour), false), true, now.Add(-time.Hour))

	s.l = s.login(s.h, imaptest.User, imaptest.Pass)
	l := s.l
	as := func(q req) req { q.cookie, q.csrf = l.cookie, l.csrf; return q }
	get := func(h http.Handler, path string) resp { return serve(h, as(req{method: "GET", path: path})) }
	accID := idOf("acc_", l.body.Accounts[0].ID)
	waitFor(t, 120*time.Second, "sync and bodies", func() bool {
		return s.count(`SELECT count(*) FROM messages WHERE account_id = $1 AND body_state = 'full'`, accID) == 3
	})
	smtpSrv := smtptest.StartServer(t, imaptest.User, imaptest.Pass, 0)
	if _, err := s.pool.Exec(context.Background(), `UPDATE accounts SET smtp_host = $2, smtp_port = $3, smtp_tls = 'none' WHERE id = $1`,
		accID, smtpSrv.Host, smtpSrv.Port); err != nil {
		t.Fatal(err)
	}
	msgOf := func(hdr string) int64 {
		var id int64
		if err := s.pool.QueryRow(context.Background(), `SELECT id FROM messages WHERE message_id_hdr = $1`, hdr).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	media, plain, onlyInline := msgOf("m500@silo.test"), msgOf("m501@silo.test"), msgOf("m502@silo.test")
	forget := func(mid int64) { // back to a message whose body was never downloaded
		t.Helper()
		if _, err := s.pool.Exec(context.Background(), `
UPDATE messages SET body_state = 'headers', raw_blob_sha256 = NULL, body_text = '' WHERE id = $1`, mid); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(context.Background(), `
UPDATE blobs SET ref_count = ref_count - 1 WHERE sha256 IN (SELECT blob_sha256 FROM attachments WHERE message_id = $1)`, mid); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(context.Background(), `UPDATE attachments SET blob_sha256 = NULL WHERE message_id = $1`, mid); err != nil {
			t.Fatal(err)
		}
	}
	createDraft := func(h http.Handler, replyTo int64, mode string) (api.Draft, resp) {
		t.Helper()
		var d api.Draft
		r := serve(h, as(req{method: "POST", path: "/drafts", body: map[string]any{"replyToMessageId": id36("m_", replyTo), "mode": mode}}))
		r.json(&d)
		if r.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", mode, r.Code, r.Body)
		}
		return d, r
	}
	sendNow := func(d api.Draft) []byte {
		t.Helper()
		var res api.SendResult
		r := serve(s.h, as(req{method: "POST", path: "/drafts/" + d.ID + "/send"}))
		r.json(&res)
		if r.Code != http.StatusOK && r.Code != http.StatusAccepted {
			t.Fatalf("send: %d %s", r.Code, r.Body)
		}
		before := len(smtpSrv.Messages())
		if err := s.compose.SendOutbound(context.Background(), idOf("ob_", res.OutboundID)); err != nil {
			t.Fatal(err)
		}
		msgs := smtpSrv.Messages()
		if len(msgs) != before+1 {
			t.Fatalf("delivered %d", len(msgs))
		}
		return msgs[len(msgs)-1].Data
	}
	save := func(d api.Draft, body map[string]any) api.Draft {
		t.Helper()
		body["version"] = d.Version
		var out api.Draft
		r := serve(s.h, as(req{method: "PUT", path: "/drafts/" + d.ID, body: body}))
		r.json(&out)
		if r.Code != http.StatusOK {
			t.Fatalf("save: %d %s", r.Code, r.Body)
		}
		return out
	}
	ids := func(d api.Draft) []string {
		out := []string{}
		for _, a := range d.Attachments {
			out = append(out, a.ID)
		}
		return out
	}

	t.Run("cid in received mail", func(t *testing.T) {
		r := get(s.h, "/messages/"+id36("m_", media)+"/html")
		if r.Code != http.StatusOK || strings.Contains(r.Body.String(), "cid:") || !strings.Contains(r.Body.String(), `src="/api/v1/attachments/a_`) {
			t.Errorf("html: %d %s", r.Code, r.Body)
		}
		var td api.ThreadDetail
		var tid int64
		_ = s.pool.QueryRow(context.Background(), `SELECT thread_id FROM messages WHERE id = $1`, media).Scan(&tid)
		get(s.h, "/threads/"+id36("t_", tid)).json(&td)
		if len(td.Messages) != 1 || len(td.Messages[0].Attachments) != 2 {
			t.Fatalf("attachments: %+v", td.Messages)
		}
		for _, a := range td.Messages[0].Attachments {
			switch a.Filename {
			case "logo.png":
				if !a.Inline || !a.Thumbnail {
					t.Errorf("logo: %+v", a)
				}
			case "oferta.pdf":
				if a.Inline || a.Thumbnail {
					t.Errorf("pdf: %+v", a)
				}
			}
		}
		// has:attachment does not count inline pictures.
		if n := s.count(`SELECT count(*) FROM messages WHERE id = ANY($1) AND has_attachments`, []int64{media, onlyInline}); n != 1 {
			t.Errorf("has_attachments: %d of 2", n)
		}
		if n := s.count(`SELECT count(*) FROM threads t JOIN messages m ON m.thread_id = t.id WHERE m.id = $1 AND NOT t.has_attachments`, onlyInline); n != 1 {
			t.Error("thread with only an inline picture has attachments")
		}
	})

	t.Run("forward carries attachments", func(t *testing.T) {
		forget(media)
		d, _ := createDraft(s.h, media, "forward")
		if len(d.Warnings) != 0 || d.Subject != "Fwd: Media with PDF" || !strings.Contains(d.BodyHTML, "Look") ||
			!strings.Contains(d.BodyHTML, `src="cid:`+logoCID+`"`) || len(d.Attachments) != 2 {
			t.Fatalf("forward: %+v", d)
		}
		var logo, pdf api.UploadInfo
		for _, a := range d.Attachments {
			if a.Filename == "logo.png" {
				logo = a
			} else {
				pdf = a
			}
		}
		if !logo.Inline || logo.CID == nil || *logo.CID != logoCID || pdf.Filename != "oferta.pdf" || pdf.Inline || pdf.CID != nil ||
			pdf.Size != int64(len(pdfBytes)) {
			t.Errorf("forward attachments: %+v", d.Attachments)
		}
		if n := s.count(`SELECT b.ref_count FROM attachments a JOIN blobs b ON b.sha256 = a.blob_sha256 WHERE a.id = $1`, idOf("u_", pdf.ID)); n != 2 {
			t.Errorf("pdf blob ref_count %d, want 2 (original + forward)", n)
		}
		if n := s.count(`SELECT count(*) FROM messages WHERE id = $1 AND has_attachments`, idOf("d_", d.ID)); n != 1 {
			t.Error("forward draft message without has_attachments")
		}
		// GET returns the same attachments; saving keeps them with attachmentIds.
		var got api.Draft
		get(s.h, "/drafts/"+d.ID).json(&got)
		if len(got.Attachments) != 2 || got.Warnings != nil {
			t.Errorf("get: %+v", got)
		}
		d = save(got, map[string]any{"to": []map[string]string{{"email": "ewa@firma.pl"}}, "subject": got.Subject,
			"bodyHtml": got.BodyHTML, "attachmentIds": ids(got)})
		raw := string(sendNow(d))
		for _, want := range []string{"multipart/mixed", "multipart/related", "multipart/alternative", "Content-Id: <" + logoCID + ">",
			"Content-Disposition: inline", `filename=oferta.pdf`, "Content-Disposition: attachment"} {
			if !strings.Contains(raw, want) {
				t.Errorf("sent MIME has no %q:\n%.3000s", want, raw)
			}
		}

		// Without the mail server the parts cannot be downloaded.
		forget(media)
		d, _ = createDraft(s.noFet, media, "forward")
		if len(d.Attachments) != 0 || !strings.Contains(strings.Join(d.Warnings, ","), "attachments_unavailable") {
			t.Errorf("forward without content: %+v", d)
		}
	})

	t.Run("reply quotes a body that was not downloaded", func(t *testing.T) {
		forget(plain)
		d, _ := createDraft(s.h, plain, "reply")
		if len(d.Warnings) != 0 || !strings.Contains(d.BodyHTML, "Plain body line") || !strings.Contains(d.BodyHTML, "<blockquote>") {
			t.Errorf("reply after download: %+v", d)
		}
		if n := s.count(`SELECT count(*) FROM messages WHERE id = $1 AND body_state = 'full'`, plain); n != 1 {
			t.Error("original not downloaded")
		}
		forget(plain)
		d, _ = createDraft(s.noFet, plain, "reply")
		if len(d.Warnings) != 1 || d.Warnings[0] != "quote_unavailable" || !strings.Contains(d.BodyHTML, "<blockquote>") {
			t.Errorf("reply without content: %+v", d)
		}
		// A reply to an HTML message quotes its sanitised HTML with the
		// inline picture copied (same cid).
		d, _ = createDraft(s.h, media, "reply")
		if !strings.Contains(d.BodyHTML, `src="cid:`+logoCID+`"`) || len(d.Attachments) != 1 || !d.Attachments[0].Inline {
			t.Errorf("reply to html: %+v", d)
		}
	})

	t.Run("inline upload sent in multipart/related", func(t *testing.T) {
		pic := encoded(t, "png", testImage(40, 30, false))
		var up api.UploadInfo
		r := uploadPart(s.h, l, "paste.png", "image/png", pic, true)
		r.json(&up)
		if r.Code != http.StatusCreated || !up.Inline || up.CID == nil || !strings.HasSuffix(*up.CID, "@silo") || !up.Thumbnail {
			t.Fatalf("inline upload: %d %s", r.Code, r.Body)
		}
		if r := uploadPart(s.h, l, "notes.txt", "text/plain", []byte("x"), true); r.Code != http.StatusUnprocessableEntity || r.code() != api.CodeInlineNotImage {
			t.Errorf("inline text: %d %s", r.Code, r.Body)
		}
		var plainUp api.UploadInfo
		uploadPart(s.h, l, "a.txt", "text/plain", []byte("plain"), false).json(&plainUp)
		if plainUp.Inline || plainUp.CID != nil || plainUp.Thumbnail {
			t.Errorf("plain upload: %+v", plainUp)
		}
		// An unused inline upload goes as a plain attachment.
		var unused api.UploadInfo
		uploadPart(s.h, l, "unused.png", "image/png", pic, true).json(&unused)
		if r := get(s.h, "/attachments/"+up.ID+"?inline=1"); r.Code != http.StatusOK || !bytes.Equal(r.Body.Bytes(), pic) {
			t.Errorf("upload bytes: %d", r.Code)
		}

		var d api.Draft
		serve(s.h, as(req{method: "POST", path: "/drafts", body: map[string]any{}})).json(&d)
		d = save(d, map[string]any{"to": []map[string]string{{"email": "jan@firma.pl"}}, "subject": "Picture",
			"bodyHtml": `<p>See <img src="cid:` + *up.CID + `"></p>`, "attachmentIds": []string{up.ID}})
		if len(d.Attachments) != 1 || !d.Attachments[0].Inline || d.Attachments[0].CID == nil || *d.Attachments[0].CID != *up.CID {
			t.Fatalf("draft attachments: %+v", d.Attachments)
		}
		if n := s.count(`SELECT count(*) FROM messages WHERE id = $1 AND NOT has_attachments`, idOf("d_", d.ID)); n != 1 {
			t.Error("inline picture counted as an attachment")
		}
		if r := get(s.h, "/messages/"+id36("m_", idOf("d_", d.ID))+"/html"); !strings.Contains(r.Body.String(), `/api/v1/attachments/a_`+strings.TrimPrefix(up.ID, "u_")+"?inline=1") {
			t.Errorf("draft html: %s", r.Body)
		}
		raw := string(sendNow(d))
		for _, want := range []string{"Content-Type: multipart/related", "Content-Id: <" + *up.CID + ">", "Content-Disposition: inline"} {
			if !strings.Contains(raw, want) {
				t.Errorf("sent MIME has no %q:\n%.2000s", want, raw)
			}
		}
		if strings.Contains(raw, "multipart/mixed") {
			t.Error("inline only: no multipart/mixed expected")
		}

		serve(s.h, as(req{method: "POST", path: "/drafts", body: map[string]any{}})).json(&d)
		d = save(d, map[string]any{"to": []map[string]string{{"email": "jan@firma.pl"}}, "subject": "Unused",
			"bodyHtml": `<p>No picture</p>`, "attachmentIds": []string{unused.ID}})
		raw = string(sendNow(d))
		if strings.Contains(raw, "multipart/related") || !strings.Contains(raw, "multipart/mixed") || !strings.Contains(raw, "Content-Disposition: attachment") {
			t.Errorf("unused inline upload not sent as an attachment:\n%.2000s", raw)
		}
	})

	t.Run("thumbnails", func(t *testing.T) {
		thumb := func(id, size string) (resp, image.Image, string) {
			t.Helper()
			r := get(s.h, "/attachments/"+id+"/thumbnail?size="+size)
			if r.Code != http.StatusOK {
				return r, nil, ""
			}
			img, format, err := image.Decode(bytes.NewReader(r.Body.Bytes()))
			if err != nil {
				t.Fatalf("decode thumbnail: %v", err)
			}
			return r, img, format
		}
		upl := func(name, ct string, data []byte) string {
			var up api.UploadInfo
			r := uploadPart(s.h, l, name, ct, data, false)
			r.json(&up)
			if r.Code != http.StatusCreated || !up.Thumbnail {
				t.Fatalf("upload %s: %d %s", name, r.Code, r.Body)
			}
			return up.ID
		}
		pngID := upl("alpha.png", "image/png", encoded(t, "png", testImage(600, 300, true)))
		jpgID := upl("tall.jpg", "image/jpeg", encoded(t, "jpeg", testImage(200, 400, false)))
		gifID := upl("small.gif", "image/gif", encoded(t, "gif", testImage(100, 50, false)))

		r, img, format := thumb(pngID, "m")
		if img == nil || format != "png" || img.Bounds().Dx() != 256 || img.Bounds().Dy() != 128 || r.Header().Get("Content-Type") != "image/png" ||
			!strings.Contains(r.Header().Get("Cache-Control"), "immutable") {
			t.Fatalf("png m: %d %v %s %v", r.Code, img, format, r.Header())
		}
		etag := r.Header().Get("ETag")
		gen := s.view.ThumbnailsGenerated()
		r, img, _ = thumb(pngID, "m")
		if img == nil || r.Header().Get("ETag") != etag || s.view.ThumbnailsGenerated() != gen {
			t.Errorf("second request not from the cache: generated %d -> %d", gen, s.view.ThumbnailsGenerated())
		}
		if n := s.count(`SELECT count(*) FROM thumbnails t JOIN attachments a ON a.blob_sha256 = t.source_sha256 WHERE a.id = $1`, idOf("u_", pngID)); n != 1 {
			t.Errorf("thumbnail rows %d", n)
		}
		if _, img, format := thumb(pngID, "s"); img == nil || format != "png" || img.Bounds().Dx() != 64 || img.Bounds().Dy() != 32 {
			t.Errorf("png s: %v", img)
		}
		if r, img, format := thumb(jpgID, "m"); img == nil || format != "jpeg" || img.Bounds().Dx() != 128 || img.Bounds().Dy() != 256 {
			t.Errorf("jpeg m: %d %v %s", r.Code, img, format)
		}
		if _, img, format := thumb(gifID, "s"); img == nil || format != "jpeg" || img.Bounds().Dx() != 64 || img.Bounds().Dy() != 32 {
			t.Errorf("gif s: %v %s", img, format)
		}
		// Not enlarged.
		if _, img, _ := thumb(gifID, "m"); img == nil || img.Bounds().Dx() != 100 || img.Bounds().Dy() != 50 {
			t.Errorf("gif m: %v", img)
		}

		var pdfID int64
		_ = s.pool.QueryRow(context.Background(), `SELECT id FROM attachments WHERE message_id = $1 AND filename = 'oferta.pdf'`, media).Scan(&pdfID)
		if r := get(s.h, "/attachments/"+id36("a_", pdfID)+"/thumbnail"); r.Code != http.StatusUnsupportedMediaType || r.code() != api.CodeThumbnailUnsupported {
			t.Errorf("pdf thumbnail: %d %s", r.Code, r.Body)
		}
		if r := get(s.h, "/attachments/"+pngID+"/thumbnail?size=xl"); r.Code == http.StatusOK {
			t.Errorf("size xl: %d", r.Code)
		}
		if r := get(s.h, "/attachments/a_zzzzzz/thumbnail"); r.Code != http.StatusNotFound {
			t.Errorf("unknown: %d", r.Code)
		}

		// Blob gc: uploads dropped after 24 h take their thumbnails along.
		if _, err := s.compose.BlobGC(context.Background(), time.Now().Add(72*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if n := s.count(`SELECT count(*) FROM thumbnails`); n != 0 {
			t.Errorf("thumbnails after gc: %d", n)
		}
	})
}
