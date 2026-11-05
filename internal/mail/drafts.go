package mail

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/blob"
)

// HeaderSiloDraftID names the copy of a draft in the server's Drafts folder
// (plan 3.8): its value is drafts.silo_draft_id, so that sync recognises the
// copy as the local draft instead of creating a duplicate.
const HeaderSiloDraftID = "X-Silo-Draft-Id"

// ErrNoDraft: the message is not a local draft (no drafts row).
var ErrNoDraft = errors.New("mail: not a draft")

// DraftFields are the parts of a draft mirrored on its local message, so that
// the draft shows in thread lists, search and the DRAFT label like any message.
type DraftFields struct {
	From           Address
	To, Cc, Bcc    []Address
	Subject        string
	BodyHTML       string
	HasAttachments bool
}

// NewDraft is a local draft message to create.
type NewDraft struct {
	AccountID, UserID int64
	// ThreadID is the thread of a reply; 0 starts a new thread.
	ThreadID     int64
	MessageIDHdr string // Message-ID without <>
	SiloDraftID  string
	InReplyTo    string   // Message-ID of the replied message (without <>), "" if none
	Refs         []string // References of the reply: the original's references and its id
	DraftFields
}

func draftText(f DraftFields) (text, snippet, subjNorm string, prefix bool) {
	text = HTMLToText(f.BodyHTML)
	subjNorm, prefix = NormalizeSubject(f.Subject, false)
	return text, MakeSnippet(text), subjNorm, prefix
}

// CreateDraftMessage inserts the local message of a new draft (body_state
// 'local', read, dated now) and, for a draft that is not a reply, its new
// thread. The caller inserts the drafts row next and then calls
// RefreshMessages, which derives the DRAFT label, recomputes the thread and
// writes the events.
func CreateDraftMessage(ctx context.Context, db DB, d NewDraft) (msgID, threadID int64, err error) {
	text, snippet, subjNorm, prefix := draftText(d.DraftFields)
	threadID = d.ThreadID
	if threadID == 0 {
		err = db.QueryRow(ctx, `
INSERT INTO threads (user_id, account_id, subject, subject_norm, last_message_at, sort_at)
VALUES ($1, $2, $3, $4, now(), now()) RETURNING id`, d.UserID, d.AccountID, d.Subject, subjNorm).Scan(&threadID)
		if err != nil {
			return 0, 0, fmt.Errorf("mail: insert draft thread: %w", err)
		}
	}
	err = db.QueryRow(ctx, `
INSERT INTO messages (user_id, account_id, thread_id, message_id_hdr, dedupe_key, in_reply_to_hdr, body_state,
  size_bytes, internal_date, from_addr, from_name, from_text, to_addrs, cc_addrs, bcc_addrs, recipients_text,
  subject, subject_norm, reply_prefix, snippet, body_text, has_attachments, has_html, is_unread)
VALUES ($1,$2,$3,$4,$5,$6,'local',$7,now(),$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,true,false)
RETURNING id`,
		d.UserID, d.AccountID, threadID, d.MessageIDHdr, "draft:"+d.SiloDraftID, nullStr(d.InReplyTo),
		int64(len(d.BodyHTML)), d.From.Email, d.From.Name, displayAddr(d.From), addrJSON(d.To), addrJSON(d.Cc), addrJSON(d.Bcc),
		joinAddrs(d.To, d.Cc, d.Bcc), d.Subject, subjNorm, prefix, snippet, text, d.HasAttachments).Scan(&msgID)
	if err != nil {
		return 0, 0, fmt.Errorf("mail: insert draft message: %w", err)
	}
	if len(d.Refs) > 0 {
		if _, err := db.Exec(ctx, `
INSERT INTO message_references (message_id, user_id, account_id, ref_message_id_hdr, position)
SELECT $1, $2, $3, r.ref, r.pos FROM unnest($4::text[]) WITH ORDINALITY AS r(ref, pos)`,
			msgID, d.UserID, d.AccountID, d.Refs); err != nil {
			return 0, 0, fmt.Errorf("mail: insert draft references: %w", err)
		}
	}
	if err := refreshSearch(ctx, db, []int64{msgID}); err != nil {
		return 0, 0, err
	}
	return msgID, threadID, nil
}

// UpdateDraftMessage copies a saved draft to its local message (recipients,
// subject, text, snippet, attachments flag) and dates it now. The caller then
// calls RefreshMessages for the thread aggregates and events.
func UpdateDraftMessage(ctx context.Context, db DB, msgID int64, f DraftFields) error {
	text, snippet, subjNorm, prefix := draftText(f)
	_, err := db.Exec(ctx, `
UPDATE messages SET to_addrs = $2, cc_addrs = $3, bcc_addrs = $4, recipients_text = $5, subject = $6,
  subject_norm = $7, reply_prefix = $8, snippet = $9, body_text = $10, has_attachments = $11,
  size_bytes = $12, internal_date = now()
WHERE id = $1`, msgID, addrJSON(f.To), addrJSON(f.Cc), addrJSON(f.Bcc), joinAddrs(f.To, f.Cc, f.Bcc), f.Subject,
		subjNorm, prefix, snippet, text, f.HasAttachments, int64(len(f.BodyHTML)))
	if err != nil {
		return fmt.Errorf("mail: update draft message: %w", err)
	}
	return refreshSearch(ctx, db, []int64{msgID})
}

// DeleteMessages deletes messages of one account for good, in the caller's
// transaction: blob references are released, empty threads deleted and the
// others recomputed, with thread.updated / thread.deleted / label.counts
// carrying origin. A discarded draft goes this way.
func DeleteMessages(ctx context.Context, db DB, accountID int64, msgIDs []int64, origin string) error {
	msgIDs = uniqueSorted(msgIDs)
	if len(msgIDs) == 0 {
		return nil
	}
	var userID int64
	if err := db.QueryRow(ctx, `SELECT user_id FROM accounts WHERE id = $1`, accountID).Scan(&userID); err != nil {
		return fmt.Errorf("mail: load account %d: %w", accountID, err)
	}
	threads, err := threadsOfMessages(ctx, db, msgIDs)
	if err != nil {
		return err
	}
	ev := &eventBuf{userID: userID, origin: origin}
	if err := deleteMessages(ctx, db, ev, accountID, msgIDs, threads, nil); err != nil {
		return err
	}
	return ev.flush(ctx, db)
}

// DraftMIME is the RFC 5322 form of the current version of a draft.
type DraftMIME struct {
	Raw         []byte
	Version     int32
	SiloDraftID string
}

// BuildDraftMIME builds the message of a local draft (plan 3.8/3.9):
// multipart/alternative text+html, wrapped in multipart/mixed with the
// attachments pinned to the draft; From the account, the draft's Message-ID,
// In-Reply-To/References of a reply, X-Silo-Draft-Id. Bcc stays in the
// draft; sending removes it. ErrNoDraft when the message has no drafts row.
func BuildDraftMIME(ctx context.Context, db DB, store blob.BlobStore, messageID int64) (*DraftMIME, error) {
	var (
		out                DraftMIME
		to, cc, bcc        []byte
		subject, html      string
		updated            time.Time
		msgIDHdr, fromAddr string
		fromName           string
		inReplyTo          *string
		refs               []string
	)
	err := db.QueryRow(ctx, `
SELECT d.silo_draft_id, d.version, d.to_addrs, d.cc_addrs, d.bcc_addrs, d.subject, d.body_html, d.updated_at,
       m.message_id_hdr, m.in_reply_to_hdr, a.email::text, a.display_name,
       COALESCE((SELECT array_agg(r.ref_message_id_hdr ORDER BY r.position) FROM message_references r WHERE r.message_id = m.id), '{}')
FROM drafts d JOIN messages m ON m.id = d.message_id JOIN accounts a ON a.id = d.account_id
WHERE d.message_id = $1`, messageID).Scan(&out.SiloDraftID, &out.Version, &to, &cc, &bcc, &subject, &html, &updated,
		&msgIDHdr, &inReplyTo, &fromAddr, &fromName, &refs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoDraft
	}
	if err != nil {
		return nil, fmt.Errorf("mail: load draft %d: %w", messageID, err)
	}
	type att struct {
		filename, contentType string
		sha                   []byte
		inline                bool
		cid                   *string
	}
	rows, err := db.Query(ctx, `
SELECT filename, content_type, blob_sha256, is_inline, content_id FROM attachments
WHERE message_id = $1 AND blob_sha256 IS NOT NULL ORDER BY id`, messageID)
	if err != nil {
		return nil, fmt.Errorf("mail: load draft attachments: %w", err)
	}
	var atts []att
	for rows.Next() {
		var a att
		if err := rows.Scan(&a.filename, &a.contentType, &a.sha, &a.inline, &a.cid); err != nil {
			rows.Close()
			return nil, err
		}
		atts = append(atts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var h gomail.Header
	h.SetDate(updated)
	h.SetAddressList("From", []*gomail.Address{{Name: fromName, Address: fromAddr}})
	for key, raw := range map[string][]byte{"To": to, "Cc": cc, "Bcc": bcc} {
		var l []Address
		_ = json.Unmarshal(raw, &l)
		if len(l) == 0 {
			continue
		}
		list := make([]*gomail.Address, len(l))
		for i, a := range l {
			list[i] = &gomail.Address{Name: a.Name, Address: a.Email}
		}
		h.SetAddressList(key, list)
	}
	h.SetSubject(subject)
	h.SetMessageID(msgIDHdr)
	if inReplyTo != nil && *inReplyTo != "" {
		h.SetMsgIDList("In-Reply-To", []string{*inReplyTo})
	}
	if len(refs) > 0 {
		h.SetMsgIDList("References", refs)
	}
	h.Set(HeaderSiloDraftID, out.SiloDraftID)
	h.Set("MIME-Version", "1.0")

	text := HTMLToText(html)
	writeAlt := func(iw *gomail.InlineWriter) error {
		for _, p := range []struct{ ct, body string }{{"text/plain", text}, {"text/html", html}} {
			var ph gomail.InlineHeader
			ph.Set("Content-Type", p.ct+"; charset=utf-8")
			ph.Set("Content-Transfer-Encoding", "quoted-printable")
			w, err := iw.CreatePart(ph)
			if err != nil {
				return err
			}
			if _, err := io.WriteString(w, p.body); err != nil {
				return err
			}
			if err := w.Close(); err != nil {
				return err
			}
		}
		return iw.Close()
	}

	var buf bytes.Buffer
	if len(atts) == 0 {
		iw, err := gomail.CreateInlineWriter(&buf, h)
		if err != nil {
			return nil, err
		}
		if err := writeAlt(iw); err != nil {
			return nil, err
		}
	} else {
		mw, err := gomail.CreateWriter(&buf, h)
		if err != nil {
			return nil, err
		}
		iw, err := mw.CreateInline()
		if err != nil {
			return nil, err
		}
		if err := writeAlt(iw); err != nil {
			return nil, err
		}
		for _, a := range atts {
			var ah gomail.AttachmentHeader
			ah.Set("Content-Type", a.contentType)
			ah.Set("Content-Transfer-Encoding", "base64")
			if a.inline {
				ah.Set("Content-Disposition", "inline")
			}
			if a.filename != "" {
				ah.SetFilename(a.filename)
			}
			if a.cid != nil && *a.cid != "" {
				ah.Set("Content-Id", "<"+*a.cid+">")
			}
			w, err := mw.CreateAttachment(ah)
			if err != nil {
				return nil, err
			}
			f, err := store.Open(ctx, hex.EncodeToString(a.sha))
			if err != nil {
				return nil, fmt.Errorf("mail: open draft attachment: %w", err)
			}
			_, err = io.Copy(w, f)
			_ = f.Close()
			if err != nil {
				return nil, err
			}
			if err := w.Close(); err != nil {
				return nil, err
			}
		}
		if err := mw.Close(); err != nil {
			return nil, err
		}
	}
	out.Raw = buf.Bytes()
	return &out, nil
}

// ParsedDraft is a draft read from a message written by another client.
type ParsedDraft struct {
	To, Cc, Bcc []Address
	Subject     string
	BodyHTML    string
}

// ParseDraft reads recipients, subject and body of a raw message (a draft
// written by another client, plan 3.8 step 3): the text/html part, or the
// text/plain part escaped into paragraphs.
func ParseDraft(raw []byte) *ParsedDraft {
	ph := ParseHeader(headerBlock(raw), int64(len(raw)))
	out := &ParsedDraft{To: ph.To, Cc: ph.Cc, Subject: ph.Subject}
	if e, err := message.Read(bytes.NewReader(raw)); e != nil {
		if l, err := parseAddressList(e.Header.Get("Bcc")); err == nil {
			out.Bcc = l
		}
		var htmlPart, plain string
		_ = e.Walk(func(_ []int, p *message.Entity, _ error) error {
			ct, params, _ := p.Header.ContentType()
			disp, _, _ := p.Header.ContentDisposition()
			if disp == "attachment" || params["name"] != "" && disp != "inline" {
				return nil
			}
			switch {
			case ct == "text/html" && htmlPart == "":
				b, _ := io.ReadAll(io.LimitReader(p.Body, maxBodyTextBytes))
				htmlPart = string(b)
			case ct == "text/plain" && plain == "":
				b, _ := io.ReadAll(io.LimitReader(p.Body, maxBodyTextBytes))
				plain = string(b)
			}
			return nil
		})
		_ = err
		out.BodyHTML = htmlPart
		if out.BodyHTML == "" && plain != "" {
			out.BodyHTML = TextToHTML(plain)
		}
	}
	return out
}

// headerBlock returns the header part of a raw message.
func headerBlock(raw []byte) []byte {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return raw[:i+4]
	}
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		return raw[:i+2]
	}
	return raw
}
