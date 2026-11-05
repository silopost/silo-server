package mail

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // registers charset decoders

	"github.com/silopost/silo-server/internal/blob"
)

// ParsedAttachment is an attachment found in a full message, with its bytes.
type ParsedAttachment struct {
	AttachmentInfo
	Data []byte
}

// ParsedBody is the result of ParseBody.
type ParsedBody struct {
	Text        string // text/plain, or HTML converted to text; at most 512 KB
	Snippet     string
	Attachments []ParsedAttachment
	ParseError  string // non-empty if the MIME structure could not be fully read
}

// ParseBody parses a complete RFC 822 message: it extracts the body text
// (text/plain preferred, HTML converted by HTMLToText otherwise), the snippet
// and the attachments with their decoded bytes. Part paths follow the IMAP
// numbering so they match attachments created from BODYSTRUCTURE. HTML is not
// returned; it is served from the raw blob when needed.
func ParseBody(raw []byte) *ParsedBody {
	out := &ParsedBody{}
	e, err := message.Read(bytes.NewReader(raw))
	if e == nil {
		out.ParseError = "mime: " + errString(err)
		return out
	}
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		out.ParseError = "mime: " + err.Error()
	}
	var plain, htmlText []string
	w := &bodyWalker{out: out, plain: &plain, html: &htmlText}
	w.walk(e, nil, 0)
	text := strings.Join(plain, "\n\n")
	if strings.TrimSpace(text) == "" && len(htmlText) > 0 {
		text = HTMLToText(strings.Join(htmlText, "\n"))
	}
	text = strings.ToValidUTF8(strings.TrimSpace(text), "")
	if len(text) > maxBodyTextBytes {
		text = strings.ToValidUTF8(text[:maxBodyTextBytes], "")
	}
	out.Text = text
	out.Snippet = MakeSnippet(text)
	return out
}

func errString(err error) string {
	if err == nil {
		return "unreadable message"
	}
	return err.Error()
}

type bodyWalker struct {
	out         *ParsedBody
	plain, html *[]string
}

func (w *bodyWalker) walk(e *message.Entity, path []int, depth int) {
	mediaType, params, _ := e.Header.ContentType()
	if mediaType == "" {
		mediaType = "text/plain"
	}
	if strings.HasPrefix(mediaType, "multipart/") && depth < 20 {
		mr := e.MultipartReader()
		if mr == nil {
			return
		}
		for i := 1; ; i++ {
			p, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				return
			}
			if p == nil {
				w.out.ParseError = joinErr(w.out.ParseError, "multipart: "+errString(err))
				return
			}
			w.walk(p, append(append([]int{}, path...), i), depth+1)
		}
	}
	disp, dparams, _ := e.Header.ContentDisposition()
	filename := decodeWords(dparams["filename"])
	if filename == "" {
		filename = decodeWords(params["name"])
	}
	info := AttachmentInfo{
		PartPath:    joinPath(path),
		Filename:    filename,
		ContentType: mediaType,
		ContentID:   strings.Trim(e.Header.Get("Content-Id"), "<> "),
	}
	info.Inline = disp != "attachment" && info.ContentID != ""
	data, err := io.ReadAll(e.Body)
	if err != nil && len(data) == 0 {
		w.out.ParseError = joinErr(w.out.ParseError, "part "+info.PartPath+": "+err.Error())
	}
	info.Size = int64(len(data))
	if isAttachmentPart(mediaType, disp, filename) {
		w.out.Attachments = append(w.out.Attachments, ParsedAttachment{AttachmentInfo: info, Data: data})
		return
	}
	switch mediaType {
	case "text/plain":
		*w.plain = append(*w.plain, string(data))
	case "text/html":
		*w.html = append(*w.html, string(data))
	}
}

// IngestBody stores the full message for a message whose body was not
// downloaded yet (phase C of the initial sync, or on demand): the raw .eml
// goes to the blob store (SHA-256, referenced through blobs.ref_count), the
// text, snippet and search_tsv are filled, attachments get their blobs (parts
// already created from BODYSTRUCTURE are updated in place, matched by
// part_path), and body_state becomes 'full'. Running it again for the same
// message replaces the previous references. The thread aggregates are
// recomputed and thread.updated is written. A MIME error is recorded in
// parse_error and never fails the call; blob store and database errors do.
func IngestBody(ctx context.Context, db DB, store blob.BlobStore, messageID int64, raw []byte) (*ParsedBody, error) {
	var accountID, userID, threadID int64
	err := db.QueryRow(ctx, `SELECT account_id, user_id, thread_id FROM messages WHERE id = $1 FOR UPDATE`, messageID).Scan(&accountID, &userID, &threadID)
	if err != nil {
		return nil, fmt.Errorf("mail: load message %d: %w", messageID, err)
	}
	parsed := ParseBody(raw)

	// Blob store writes are idempotent and happen before the references.
	emlSum, emlSize, err := store.Put(ctx, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("mail: store raw message: %w", err)
	}
	type putAtt struct {
		a    ParsedAttachment
		sha  []byte
		size int64
	}
	atts := make([]putAtt, 0, len(parsed.Attachments))
	for _, a := range parsed.Attachments {
		sum, size, err := store.Put(ctx, bytes.NewReader(a.Data))
		if err != nil {
			return nil, fmt.Errorf("mail: store attachment %s: %w", a.PartPath, err)
		}
		b, _ := hex.DecodeString(sum)
		atts = append(atts, putAtt{a: a, sha: b, size: size})
	}
	emlSha, _ := hex.DecodeString(emlSum)

	// Release the references of an earlier run, then take the new ones.
	if _, err := db.Exec(ctx, `
WITH refs AS (
  SELECT raw_blob_sha256 AS sha FROM messages WHERE id = $1 AND raw_blob_sha256 IS NOT NULL
  UNION ALL
  SELECT blob_sha256 FROM attachments WHERE message_id = $1 AND blob_sha256 IS NOT NULL
), cnt AS (SELECT sha, count(*)::int AS n FROM refs GROUP BY sha)
UPDATE blobs b SET ref_count = GREATEST(b.ref_count - cnt.n, 0) FROM cnt WHERE b.sha256 = cnt.sha`, messageID); err != nil {
		return nil, fmt.Errorf("mail: release old blob refs: %w", err)
	}
	if _, err := db.Exec(ctx, `UPDATE attachments SET blob_sha256 = NULL WHERE message_id = $1`, messageID); err != nil {
		return nil, fmt.Errorf("mail: clear attachment blobs: %w", err)
	}
	addRef := func(sha []byte, size int64) error {
		_, err := db.Exec(ctx, `
INSERT INTO blobs (sha256, size_bytes, kind, ref_count) VALUES ($1, $2, 'mirror', 1)
ON CONFLICT (sha256) DO UPDATE SET ref_count = blobs.ref_count + 1`, sha, size)
		return err
	}
	if err := addRef(emlSha, emlSize); err != nil {
		return nil, fmt.Errorf("mail: reference raw blob: %w", err)
	}
	for _, p := range atts {
		if err := addRef(p.sha, p.size); err != nil {
			return nil, fmt.Errorf("mail: reference attachment blob: %w", err)
		}
		tag, err := db.Exec(ctx, `
UPDATE attachments SET blob_sha256 = $3, size_bytes = $4, content_type = $5,
  filename = CASE WHEN filename = '' THEN $6 ELSE filename END,
  content_id = COALESCE(content_id, $7), is_inline = $8
WHERE message_id = $1 AND part_path = $2`,
			messageID, p.a.PartPath, p.sha, p.size, p.a.ContentType, p.a.Filename, nullStr(p.a.ContentID), p.a.Inline)
		if err != nil {
			return nil, fmt.Errorf("mail: update attachment: %w", err)
		}
		if tag.RowsAffected() == 0 {
			if _, err := db.Exec(ctx, `
INSERT INTO attachments (message_id, user_id, blob_sha256, filename, content_type, size_bytes, content_id, is_inline, part_path)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				messageID, userID, p.sha, p.a.Filename, p.a.ContentType, p.size, nullStr(p.a.ContentID), p.a.Inline, p.a.PartPath); err != nil {
				return nil, fmt.Errorf("mail: insert attachment: %w", err)
			}
		}
	}
	hasAtt := false
	for _, p := range atts {
		hasAtt = hasAtt || !p.a.Inline
	}
	_, err = db.Exec(ctx, `
UPDATE messages SET raw_blob_sha256 = $2, body_state = 'full', body_text = $3,
  snippet = CASE WHEN $4 <> '' THEN $4 ELSE snippet END,
  has_attachments = CASE WHEN $5::text <> '' THEN has_attachments ELSE $6 END,
  parse_error = COALESCE(NULLIF($5, ''), CASE WHEN parse_error LIKE 'mime:%' OR parse_error LIKE 'multipart:%' THEN NULL ELSE parse_error END)
WHERE id = $1`, messageID, emlSha, parsed.Text, parsed.Snippet, parsed.ParseError, hasAtt)
	if err != nil {
		return nil, fmt.Errorf("mail: update message body: %w", err)
	}
	if err := refreshSearch(ctx, db, []int64{messageID}); err != nil {
		return nil, err
	}
	if err := RecomputeThread(ctx, db, threadID); err != nil {
		return nil, err
	}
	ev := &eventBuf{userID: userID}
	if err := ev.emitThreadEvents(ctx, db, accountID, []int64{threadID}, nil); err != nil {
		return nil, err
	}
	return parsed, ev.flush(ctx, db)
}
