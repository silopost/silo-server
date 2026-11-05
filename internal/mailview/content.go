package mailview

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"strconv"
	"strings"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // registers charset decoders
	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/blob"
	silosync "github.com/silopost/silo-server/internal/sync"
)

// SanitizerVersion is part of the ETag of sanitised HTML: bump it when the
// sanitiser changes so that caches drop old output.
const SanitizerVersion = "1"

// maxHTMLBytes bounds the HTML part that is sanitised.
const maxHTMLBytes = 8 << 20

type msgRef struct {
	accountID int64
	subject   string
	sha       []byte
}

func (v *View) message(ctx context.Context, userID, id int64) (*msgRef, error) {
	var m msgRef
	err := v.pool.QueryRow(ctx, `SELECT account_id, subject, raw_blob_sha256 FROM messages WHERE id = $1 AND user_id = $2`, id, userID).
		Scan(&m.accountID, &m.subject, &m.sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &m, err
}

// fetch downloads the body of a message from the mail server, waiting at
// most FetchWait.
func (v *View) fetch(ctx context.Context, accountID, messageID int64) error {
	if v.fetcher == nil {
		return ErrContentNotAvailable
	}
	ctx, cancel := context.WithTimeout(ctx, v.FetchWait)
	defer cancel()
	switch err := v.fetcher.FetchBody(ctx, accountID, messageID); {
	case err == nil:
		return nil
	case errors.Is(err, silosync.ErrNoContent):
		return ErrNotFound
	default:
		return fmt.Errorf("%w: %v", ErrContentNotAvailable, err)
	}
}

// Raw is the stored .eml of a message.
type Raw struct {
	MessageID int64
	Subject   string
	SHA       string
	Body      io.ReadSeekCloser
}

// OpenRaw opens the .eml of a message, downloading it first when the body is
// not in the mirror (body_state headers or deferred).
func (v *View) OpenRaw(ctx context.Context, userID, messageID int64) (*Raw, error) {
	m, err := v.message(ctx, userID, messageID)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		if m.sha == nil {
			if attempt > 0 {
				return nil, ErrContentNotAvailable
			}
			if err := v.fetch(ctx, m.accountID, messageID); err != nil {
				return nil, err
			}
			if m, err = v.message(ctx, userID, messageID); err != nil {
				return nil, err
			}
			continue
		}
		sum := hex.EncodeToString(m.sha)
		if v.blobs == nil {
			return nil, ErrContentNotAvailable
		}
		f, err := v.blobs.Open(ctx, sum)
		if errors.Is(err, blob.ErrNotFound) && attempt == 0 {
			m.sha = nil // evicted from the mirror: download again
			continue
		}
		if err != nil {
			return nil, err
		}
		return &Raw{MessageID: messageID, Subject: m.subject, SHA: sum, Body: f}, nil
	}
}

// HTML is the sanitised HTML body of a message.
type HTML struct {
	Body []byte
	ETag string
}

// HTML returns the sanitised HTML of a message: its text/html part, or the
// text/plain part wrapped in a preformatted block. cid: references become
// /api/v1/attachments/{id}?inline=1.
func (v *View) HTML(ctx context.Context, userID, messageID int64, attachmentURL func(id int64) string) (*HTML, error) {
	raw, err := v.OpenRaw(ctx, userID, messageID)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(raw.Body, 64<<20))
	_ = raw.Body.Close()
	if err != nil {
		return nil, err
	}
	htmlPart, text := extractBodies(data)
	cids := map[string]string{}
	rows, err := v.pool.Query(ctx, `SELECT lower(content_id), id FROM attachments WHERE message_id = $1 AND content_id IS NOT NULL`, messageID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var cid string
		var id int64
		if err := rows.Scan(&cid, &id); err != nil {
			rows.Close()
			return nil, err
		}
		cids[cid] = attachmentURL(id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out string
	if strings.TrimSpace(htmlPart) != "" {
		out = SanitizeHTML(htmlPart, func(cid string) string { return cids[strings.ToLower(cid)] })
	} else {
		out = `<pre style="white-space:pre-wrap;font-family:inherit;margin:0">` + html.EscapeString(text) + `</pre>`
	}
	return &HTML{Body: []byte(out), ETag: `"` + raw.SHA[:32] + "-" + SanitizerVersion + `"`}, nil
}

// extractBodies returns the concatenated text/html and text/plain parts of a
// message that are not attachments.
func extractBodies(raw []byte) (htmlPart, text string) {
	e, _ := message.Read(bytes.NewReader(raw))
	if e == nil {
		return "", ""
	}
	var hs, ts []string
	var walk func(e *message.Entity, depth int)
	walk = func(e *message.Entity, depth int) {
		mt, _, _ := e.Header.ContentType()
		if mt == "" {
			mt = "text/plain"
		}
		if strings.HasPrefix(mt, "multipart/") && depth < 20 {
			mr := e.MultipartReader()
			if mr == nil {
				return
			}
			for {
				p, err := mr.NextPart()
				if p == nil || err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
					return
				}
				walk(p, depth+1)
			}
		}
		if disp, _, _ := e.Header.ContentDisposition(); disp == "attachment" {
			return
		}
		switch mt {
		case "text/html", "text/plain":
			b, _ := io.ReadAll(io.LimitReader(e.Body, maxHTMLBytes))
			s := strings.ToValidUTF8(string(b), "")
			if mt == "text/html" {
				hs = append(hs, s)
			} else {
				ts = append(ts, s)
			}
		}
	}
	walk(e, 0)
	return strings.Join(hs, "\n"), strings.Join(ts, "\n\n")
}

// AttachmentContent is an attachment ready to be sent.
type AttachmentContent struct {
	Filename    string
	ContentType string
	Size        int64
	Body        io.ReadSeekCloser
	SHA         string
}

// OpenAttachment opens the bytes of an attachment. When they are not in the
// mirror yet the whole message is downloaded (the same path as IngestBody).
func (v *View) OpenAttachment(ctx context.Context, userID, id int64) (*AttachmentContent, error) {
	for attempt := 0; ; attempt++ {
		var a AttachmentContent
		var msgID, accountID int64
		var sha []byte
		err := v.pool.QueryRow(ctx, `
SELECT a.message_id, m.account_id, a.blob_sha256, a.filename, a.content_type, a.size_bytes
FROM attachments a JOIN messages m ON m.id = a.message_id
WHERE a.id = $1 AND a.user_id = $2`, id, userID).Scan(&msgID, &accountID, &sha, &a.Filename, &a.ContentType, &a.Size)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if sha != nil && v.blobs != nil {
			a.SHA = hex.EncodeToString(sha)
			f, err := v.blobs.Open(ctx, a.SHA)
			if err == nil {
				a.Body = f
				if sz, e := f.Seek(0, io.SeekEnd); e == nil {
					a.Size = sz
					_, _ = f.Seek(0, io.SeekStart)
				}
				return &a, nil
			}
			if !errors.Is(err, blob.ErrNotFound) {
				return nil, err
			}
		}
		if attempt > 0 {
			return nil, ErrNotFound // the part is not in the message
		}
		if err := v.fetch(ctx, accountID, msgID); err != nil {
			return nil, err
		}
	}
}

// AttachmentURL is the URL of an attachment in the API.
func AttachmentURL(prefix string, id int64) string {
	return prefix + "a_" + strconv.FormatInt(id, 36) + "?inline=1"
}
