package mail

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
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

// ErrNoDraft means the message is not a local draft (no drafts row).
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
	// MessageID is the Message-ID (without <>).
	MessageID   string
	From        Address
	To, Cc, Bcc []Address
}

// Recipients returns To, Cc and Bcc without duplicates: RCPT TO of a send.
func (d *DraftMIME) Recipients() []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range [][]Address{d.To, d.Cc, d.Bcc} {
		for _, a := range l {
			if e := strings.ToLower(a.Email); e != "" && !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	return out
}

// BuildDraftMIME builds the message of a local draft (plan 3.8/3.9):
// multipart/alternative text+html, in multipart/related with the inline
// pictures its HTML shows (cid:), in multipart/mixed with the other
// attachments pinned to the draft; From the account, the draft's Message-ID,
// In-Reply-To/References of a reply, X-Silo-Draft-Id. Bcc stays in the
// draft; BuildSendMIME removes it. ErrNoDraft when the message has no drafts row.
func BuildDraftMIME(ctx context.Context, db DB, store blob.BlobStore, messageID int64) (*DraftMIME, error) {
	return buildDraftMIME(ctx, db, store, messageID, time.Time{})
}

// BuildSendMIME builds the message that is sent (plan 3.9 step 1): the draft
// as BuildDraftMIME has it, dated date, without the Bcc header and without
// X-Silo-Draft-Id (Bcc recipients are only in the envelope: Recipients).
func BuildSendMIME(ctx context.Context, db DB, store blob.BlobStore, messageID int64, date time.Time) (*DraftMIME, error) {
	return buildDraftMIME(ctx, db, store, messageID, date)
}

// buildDraftMIME builds a draft; a non-zero sendDate builds the sent form.
func buildDraftMIME(ctx context.Context, db DB, store blob.BlobStore, messageID int64, sendDate time.Time) (*DraftMIME, error) {
	sending := !sendDate.IsZero()
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

	out.MessageID, out.From = msgIDHdr, Address{Name: fromName, Email: fromAddr}
	_ = json.Unmarshal(to, &out.To)
	_ = json.Unmarshal(cc, &out.Cc)
	_ = json.Unmarshal(bcc, &out.Bcc)

	var h gomail.Header
	if sending {
		h.SetDate(sendDate)
	} else {
		h.SetDate(updated)
	}
	h.SetAddressList("From", []*gomail.Address{{Name: fromName, Address: fromAddr}})
	for _, f := range []struct {
		key string
		l   []Address
	}{{"To", out.To}, {"Cc", out.Cc}, {"Bcc", out.Bcc}} {
		if len(f.l) == 0 || f.key == "Bcc" && sending {
			continue
		}
		list := make([]*gomail.Address, len(f.l))
		for i, a := range f.l {
			list[i] = &gomail.Address{Name: a.Name, Address: a.Email}
		}
		h.SetAddressList(f.key, list)
	}
	h.SetSubject(subject)
	h.SetMessageID(msgIDHdr)
	if inReplyTo != nil && *inReplyTo != "" {
		h.SetMsgIDList("In-Reply-To", []string{*inReplyTo})
	}
	if len(refs) > 0 {
		h.SetMsgIDList("References", refs)
	}
	if !sending {
		h.Set(HeaderSiloDraftID, out.SiloDraftID)
	}
	h.Set("MIME-Version", "1.0")

	text := HTMLToText(html)
	var related, mixed []att
	lowerHTML := strings.ToLower(html)
	for _, a := range atts {
		// An inline picture goes in multipart/related when the HTML refers
		// to it; one that is not used (removed from the text) is sent as a
		// plain attachment, as Gmail does.
		if a.inline && a.cid != nil && *a.cid != "" && strings.Contains(lowerHTML, "cid:"+strings.ToLower(*a.cid)) {
			related = append(related, a)
		} else {
			a.inline = false
			mixed = append(mixed, a)
		}
	}

	writeAlt := func(w *message.Writer) error {
		for _, p := range []struct{ ct, body string }{{"text/plain", text}, {"text/html", html}} {
			var ph message.Header
			ph.Set("Content-Type", p.ct+"; charset=utf-8")
			ph.Set("Content-Transfer-Encoding", "quoted-printable")
			pw, err := w.CreatePart(ph)
			if err != nil {
				return err
			}
			if _, err := io.WriteString(pw, p.body); err != nil {
				return err
			}
			if err := pw.Close(); err != nil {
				return err
			}
		}
		return w.Close()
	}
	writeAtt := func(w *message.Writer, a att) error {
		var ah message.Header
		params := map[string]string{}
		if a.filename != "" {
			params["name"] = a.filename
		}
		ah.SetContentType(a.contentType, params)
		ah.Set("Content-Transfer-Encoding", "base64")
		disp := "attachment"
		if a.inline {
			disp = "inline"
		}
		dparams := map[string]string{}
		if a.filename != "" {
			dparams["filename"] = a.filename
		}
		ah.SetContentDisposition(disp, dparams)
		if a.inline {
			ah.Set("Content-Id", "<"+*a.cid+">")
		}
		pw, err := w.CreatePart(ah)
		if err != nil {
			return err
		}
		f, err := store.Open(ctx, hex.EncodeToString(a.sha))
		if err != nil {
			return fmt.Errorf("mail: open draft attachment: %w", err)
		}
		_, err = io.Copy(pw, f)
		_ = f.Close()
		if err != nil {
			return err
		}
		return pw.Close()
	}
	altHeader := func() message.Header {
		var ah message.Header
		ah.SetContentType("multipart/alternative", nil)
		return ah
	}
	writeRelated := func(w *message.Writer) error {
		aw, err := w.CreatePart(altHeader())
		if err != nil {
			return err
		}
		if err := writeAlt(aw); err != nil {
			return err
		}
		for _, a := range related {
			if err := writeAtt(w, a); err != nil {
				return err
			}
		}
		return w.Close()
	}

	// Structure (plan 3.9 step 1):
	// mixed( related( alternative(text, html), inline... ), attachments... ),
	// leaving out the levels that have one part.
	var buf bytes.Buffer
	top := h.Header
	switch {
	case len(mixed) > 0:
		top.SetContentType("multipart/mixed", nil)
	case len(related) > 0:
		top.SetContentType("multipart/related", map[string]string{"type": "multipart/alternative"})
	default:
		top.SetContentType("multipart/alternative", nil)
	}
	w, err := message.CreateWriter(&buf, top)
	if err != nil {
		return nil, err
	}
	switch {
	case len(mixed) > 0:
		if len(related) > 0 {
			var rh message.Header
			rh.SetContentType("multipart/related", map[string]string{"type": "multipart/alternative"})
			rw, err := w.CreatePart(rh)
			if err != nil {
				return nil, err
			}
			if err := writeRelated(rw); err != nil {
				return nil, err
			}
		} else {
			aw, err := w.CreatePart(altHeader())
			if err != nil {
				return nil, err
			}
			if err := writeAlt(aw); err != nil {
				return nil, err
			}
		}
		for _, a := range mixed {
			if err := writeAtt(w, a); err != nil {
				return nil, err
			}
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	case len(related) > 0:
		if err := writeRelated(w); err != nil {
			return nil, err
		}
	default:
		if err := writeAlt(w); err != nil {
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

// Statuses of outbound (plan 3.9, 4.2). A message whose send is queued or
// sending is SCHEDULED; a sent one is SENT; cancelled and failed sends are
// drafts again.
const (
	OutboundQueued    = "queued"
	OutboundSending   = "sending"
	OutboundSent      = "sent"
	OutboundFailed    = "failed"
	OutboundCancelled = "cancelled"
)
