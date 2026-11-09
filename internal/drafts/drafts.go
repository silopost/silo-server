// Package drafts is the server side of composing (plan 3.8, 5.6): draft
// attachments uploaded to the blob store, drafts saved locally with optimistic
// concurrency and mirrored on a local message (DRAFT label, thread lists),
// and the throttled write-back of the current version to the server's Drafts
// folder (an imap_op append job executed by the sync engine). Sending (SMTP,
// outbound) is in send.go (API side) and smtp.go (the send job).
package drafts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime"
	"net"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/blob"
	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/ops"
	"github.com/silopost/silo-server/internal/secrets"
)

// Errors of the package.
var (
	ErrNotFound = errors.New("drafts: not found")
	ErrInvalid  = errors.New("drafts: invalid request")
	ErrConflict = errors.New("drafts: version conflict")
	ErrTooLarge = errors.New("drafts: upload too large")
	// ErrInlineNotImage: only image/* uploads may be inline.
	ErrInlineNotImage = errors.New("drafts: an inline upload must be an image")
)

// Warnings of a new reply or forward (Draft.Warnings).
const (
	// WarnQuoteUnavailable: the body of the original could not be downloaded;
	// the quote holds its snippet only.
	WarnQuoteUnavailable = "quote_unavailable"
	// WarnAttachmentsUnavailable: some attachments of a forwarded message
	// could not be downloaded and were left out.
	WarnAttachmentsUnavailable = "attachments_unavailable"
	// WarnAttachmentsTooLarge: attachments of a forwarded message over the
	// attachment limit (together) were left out.
	WarnAttachmentsTooLarge = "attachments_too_large"
)

// PrefillWait bounds the download of the original's content for a reply or
// forward whose body is not in the mirror.
const PrefillWait = 10 * time.Second

// Reply modes of a new draft.
const (
	ModeReply    = "reply"
	ModeReplyAll = "replyAll"
	ModeForward  = "forward"
)

// DefaultSyncDelay is the throttling of the Drafts copy: the append job runs
// this long after the last change (plan 3.8: at least 30 s).
const DefaultSyncDelay = 30 * time.Second

// UploadMaxAge is the age after which an upload not pinned to a draft is deleted (plan 5.6).
const UploadMaxAge = 24 * time.Hour

// Options configure a Service.
type Options struct {
	Pool  *pgxpool.Pool
	Blobs blob.BlobStore
	// MaxUploadBytes is SILO_MAX_ATTACHMENT_BYTES, the limit of one upload.
	MaxUploadBytes int64
	// SyncDelay is the throttling of the Drafts copy [DefaultSyncDelay].
	SyncDelay time.Duration
	// OpenRaw returns the .eml of a message of the user, downloading it from
	// the mail server when needed (mailview.View.OpenRaw). It is used to
	// open a draft written by another client.
	OpenRaw func(ctx context.Context, userID, messageID int64) (io.ReadCloser, error)
	// Keys decrypt the SMTP password of an account (the send job).
	Keys *secrets.Keyring
	// InsecureTransport is SILO_ALLOW_INSECURE_TRANSPORT: SMTP without TLS
	// (smtp_tls none, or a server without STARTTLS) and unverified certificates.
	InsecureTransport bool
	// SendRetry are the delays after transient SMTP failures; one more failure
	// fails the send [DefaultSendRetry].
	SendRetry []time.Duration
	// SMTPTimeout bounds one delivery attempt [2m].
	SMTPTimeout time.Duration
	Logger      *slog.Logger
	// DialContext opens the SMTP connections (the anti-SSRF dialer of netx);
	// net.Dialer when nil.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// OAuth issues the access tokens of xoauth2 accounts (oauth.go).
	OAuth OAuthTokens
}

// Service implements the drafts and uploads endpoints.
type Service struct {
	o   Options
	log *slog.Logger
}

// New returns a Service.
func New(o Options) *Service {
	if o.SyncDelay <= 0 {
		o.SyncDelay = DefaultSyncDelay
	}
	if o.SendRetry == nil {
		o.SendRetry = DefaultSendRetry
	}
	if o.SMTPTimeout <= 0 {
		o.SMTPTimeout = 2 * time.Minute
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{o: o, log: log.With("component", "drafts")}
}

// MaxUploadBytes is the limit of one upload.
func (s *Service) MaxUploadBytes() int64 { return s.o.MaxUploadBytes }

// Upload is a draft attachment (an attachments row; message_id NULL until it
// is pinned to a draft).
type Upload struct {
	ID          int64
	Filename    string
	ContentType string
	Size        int64
	// Inline: a picture shown in the HTML of the draft as cid:CID (plan 5.6).
	Inline bool
	// CID is the Content-ID (without <>) of an inline upload, "" otherwise.
	CID string
}

// Draft is a local draft. ID is the id of its local message.
type Draft struct {
	ID, AccountID, ThreadID int64
	To, Cc, Bcc             []mail.Address
	Subject                 string
	BodyHTML                string
	BodyJSON                json.RawMessage
	InReplyTo               *int64
	Attachments             []Upload
	// Warnings of a new reply or forward (only from Create): WarnQuoteUnavailable,
	// WarnAttachmentsUnavailable, WarnAttachmentsTooLarge.
	Warnings  []string
	Version   int32
	UpdatedAt time.Time
	// SyncedVersion is the version saved in the server's Drafts folder (0: none).
	SyncedVersion int32
	// ServerChanged: the copy in the server's Drafts folder was changed or
	// removed by another program since Silo saved it (plan 3.8 step 3).
	ServerChanged bool
	// LastSend is the last send of the draft when it failed (nil otherwise).
	LastSend *LastSend
}

// LastSend is a failed send of a draft (its outbound row).
type LastSend struct {
	OutboundID       int64
	Status           string
	Error            string
	FailedRecipients []string
	UpdatedAt        time.Time
}

func (s *Service) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.o.Pool, fn)
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// ---- uploads -----------------------------------------------------------------

type limitReader struct {
	r        io.Reader
	n        int64
	exceeded bool
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.n < 0 {
		l.exceeded = true
		return 0, ErrTooLarge
	}
	if int64(len(p)) > l.n+1 {
		p = p[:l.n+1]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	if l.n < 0 {
		l.exceeded = true
		return n, ErrTooLarge
	}
	return n, err
}

func cleanFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.TrimSpace(path.Base(name))
	if name == "." || name == "/" {
		name = ""
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if len(name) > 255 {
		name = strings.ToValidUTF8(name[:255], "")
	}
	return name
}

func cleanContentType(ct string) string {
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil || !strings.Contains(mt, "/") {
		return "application/octet-stream"
	}
	if cs := params["charset"]; cs != "" && strings.HasPrefix(mt, "text/") {
		return mime.FormatMediaType(mt, map[string]string{"charset": cs})
	}
	return mt
}

// Upload streams one file into the blob store (kind local) and records it as
// an upload of the user. A file larger than MaxUploadBytes fails with
// ErrTooLarge and leaves nothing behind. An inline upload (a picture pasted
// into the editor, plan 5.6) must be image/* (ErrInlineNotImage); it gets a
// Content-ID, which the client puts in the HTML as cid:CID.
func (s *Service) Upload(ctx context.Context, userID int64, filename, contentType string, inline bool, r io.Reader) (*Upload, error) {
	ct := cleanContentType(contentType)
	if inline && !strings.HasPrefix(ct, "image/") {
		return nil, ErrInlineNotImage
	}
	lr := &limitReader{r: r, n: s.o.MaxUploadBytes}
	sum, size, err := s.o.Blobs.Put(ctx, lr)
	if lr.exceeded {
		return nil, ErrTooLarge
	}
	if err != nil {
		return nil, err
	}
	sha, _ := hex.DecodeString(sum)
	u := &Upload{Filename: cleanFilename(filename), ContentType: ct, Size: size, Inline: inline}
	var cid *string
	if inline {
		u.CID = randomToken() + "@silo"
		cid = &u.CID
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO blobs (sha256, size_bytes, kind, ref_count) VALUES ($1, $2, 'local', 1)
ON CONFLICT (sha256) DO UPDATE SET ref_count = blobs.ref_count + 1`, sha, size); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
INSERT INTO attachments (message_id, user_id, blob_sha256, filename, content_type, size_bytes, part_path, is_inline, content_id)
VALUES (NULL, $1, $2, $3, $4, $5, '', $6, $7) RETURNING id`, userID, sha, u.Filename, u.ContentType, size, inline, cid).Scan(&u.ID)
	})
	if err != nil {
		return nil, fmt.Errorf("drafts: record upload: %w", err)
	}
	return u, nil
}

// GetUpload returns an upload (pinned or not) of the user.
func (s *Service) GetUpload(ctx context.Context, userID, id int64) (*Upload, error) {
	u := &Upload{ID: id}
	err := s.o.Pool.QueryRow(ctx, `
SELECT a.filename, a.content_type, a.size_bytes, a.is_inline, COALESCE(a.content_id, '') FROM attachments a
WHERE a.id = $1 AND a.user_id = $2 AND a.part_path = '' AND a.blob_sha256 IS NOT NULL`, id, userID).Scan(&u.Filename, &u.ContentType, &u.Size, &u.Inline, &u.CID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// ---- drafts ------------------------------------------------------------------

// CreateParams is a new draft. AccountID is the resolved account of a new
// message; a reply takes the account of the replied message.
type CreateParams struct {
	UserID, AccountID int64
	ReplyTo           int64 // message replied to or forwarded (0: new message)
	Mode              string
	Origin            string
}

type original struct {
	accountID, threadID int64
	msgIDHdr, subject   string
	from                mail.Address
	replyTo, to, cc     []mail.Address
	bodyText            string
	date                time.Time
	refs                []string
	bodyState, snippet  string
	hasHTML             bool
	// draftHTML is the body of a local draft (or a message sent from Silo
	// whose drafts row remains), quoted as is.
	draftHTML *string
}

func (s *Service) loadOriginal(ctx context.Context, userID, id int64) (*original, error) {
	o := &original{}
	var replyTo, to, cc []byte
	err := s.o.Pool.QueryRow(ctx, `
SELECT m.account_id, m.thread_id, m.message_id_hdr, m.subject, m.from_addr::text, m.from_name, m.reply_to, m.to_addrs, m.cc_addrs,
       m.body_text, COALESCE(m.header_date, m.internal_date),
       COALESCE((SELECT array_agg(r.ref_message_id_hdr ORDER BY r.position) FROM message_references r WHERE r.message_id = m.id), '{}'),
       m.body_state, m.snippet, m.has_html, (SELECT d.body_html FROM drafts d WHERE d.message_id = m.id)
FROM messages m WHERE m.id = $1 AND m.user_id = $2`, id, userID).Scan(&o.accountID, &o.threadID, &o.msgIDHdr, &o.subject,
		&o.from.Email, &o.from.Name, &replyTo, &to, &cc, &o.bodyText, &o.date, &o.refs, &o.bodyState, &o.snippet, &o.hasHTML, &o.draftHTML)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(replyTo, &o.replyTo)
	_ = json.Unmarshal(to, &o.to)
	_ = json.Unmarshal(cc, &o.cc)
	return o, nil
}

func prefixed(subject, prefix string, alts ...string) string {
	l := strings.ToLower(strings.TrimSpace(subject))
	for _, p := range append(alts, strings.ToLower(prefix)) {
		if strings.HasPrefix(l, p) {
			return subject
		}
	}
	return prefix + " " + subject
}

// addrSet collects recipients without duplicates and without the account.
type addrSet struct {
	seen map[string]bool
}

func (a *addrSet) add(dst *[]mail.Address, l ...mail.Address) {
	for _, x := range l {
		e := strings.ToLower(x.Email)
		if e == "" || a.seen[e] {
			continue
		}
		a.seen[e] = true
		*dst = append(*dst, mail.Address{Name: x.Name, Email: e})
	}
}

func quoteHeader(o *original) string {
	who := o.from.Email
	if o.from.Name != "" {
		who = o.from.Name + " <" + o.from.Email + ">"
	}
	return o.date.Format("Mon, 2 Jan 2006 at 15:04") + ", " + who
}

// prefill fills recipients, subject and body of a reply or forward (plan
// 3.8/3.9; the same for every client). quote is the HTML of the original
// (quoteOf).
func prefill(mode, self string, o *original, quote string) (f mail.DraftFields) {
	set := &addrSet{seen: map[string]bool{self: true}}
	switch mode {
	case ModeReply, ModeReplyAll:
		f.Subject = prefixed(o.subject, "Re:")
		switch {
		case strings.EqualFold(o.from.Email, self): // reply to own message: same recipients
			set.add(&f.To, o.to...)
		case len(o.replyTo) > 0:
			set.add(&f.To, o.replyTo...)
		default:
			set.add(&f.To, o.from)
		}
		if mode == ModeReplyAll {
			set.add(&f.To, o.to...)
			set.add(&f.Cc, o.cc...)
		}
		f.BodyHTML = "<p></p><p>On " + html.EscapeString(quoteHeader(o)) + " wrote:</p><blockquote>" + quote + "</blockquote>"
	case ModeForward:
		f.Subject = prefixed(o.subject, "Fwd:", "fw:")
		var to []string
		for _, a := range o.to {
			to = append(to, a.Email)
		}
		f.BodyHTML = "<p></p><p>---------- Forwarded message ---------<br>From: " + html.EscapeString(quoteHeader(o)) +
			"<br>Subject: " + html.EscapeString(o.subject) + "<br>To: " + html.EscapeString(strings.Join(to, ", ")) + "</p>" +
			quote
	}
	return f
}

func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Create makes a new draft: a local message with the DRAFT label (in the
// thread of the replied message for reply/replyAll, else in a new thread) and
// its drafts row, prefilled for replies and forwards. Nothing is written to
// the server until the first save.
func (s *Service) Create(ctx context.Context, p CreateParams) (*Draft, error) {
	var o *original
	switch {
	case p.ReplyTo != 0:
		if p.Mode == "" {
			p.Mode = ModeReply
		}
		if p.Mode != ModeReply && p.Mode != ModeReplyAll && p.Mode != ModeForward {
			return nil, invalid("unknown mode %q", p.Mode)
		}
		var err error
		if o, err = s.loadOriginal(ctx, p.UserID, p.ReplyTo); err != nil {
			return nil, err
		}
		p.AccountID = o.accountID
	case p.Mode != "":
		return nil, invalid("mode needs replyToMessageId")
	}
	var self, name string
	err := s.o.Pool.QueryRow(ctx, `SELECT lower(email::text), display_name FROM accounts WHERE id = $1 AND user_id = $2`, p.AccountID, p.UserID).Scan(&self, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	nd := mail.NewDraft{AccountID: p.AccountID, UserID: p.UserID, SiloDraftID: "d_" + randomToken()}
	domain := "silo.invalid"
	if i := strings.LastIndexByte(self, '@'); i >= 0 && i < len(self)-1 {
		domain = self[i+1:]
	}
	nd.MessageIDHdr = randomToken() + "@" + domain
	var inReplyTo *int64
	var pc *prefillContent
	if o != nil {
		if pc, err = s.prepare(ctx, p.UserID, p.ReplyTo, p.Mode, o); err != nil {
			return nil, err
		}
		nd.DraftFields = prefill(p.Mode, self, o, pc.quote)
		for _, a := range pc.copies {
			nd.HasAttachments = nd.HasAttachments || !a.inline || a.cid == ""
		}
		inReplyTo = &p.ReplyTo
		if p.Mode != ModeForward {
			nd.ThreadID = o.threadID
			nd.InReplyTo = o.msgIDHdr
			nd.Refs = append(slices.Clone(o.refs), o.msgIDHdr)
			if len(nd.Refs) > 50 {
				nd.Refs = nd.Refs[len(nd.Refs)-50:]
			}
		}
	}
	nd.From = mail.Address{Name: name, Email: self}
	var msgID int64
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := mail.LockAccount(ctx, tx, p.AccountID); err != nil {
			return err
		}
		var err error
		if msgID, _, err = mail.CreateDraftMessage(ctx, tx, nd); err != nil {
			return err
		}
		attIDs := []int64{}
		if pc != nil && len(pc.copies) > 0 {
			if attIDs, err = copyAttachments(ctx, tx, p.UserID, msgID, pc.copies); err != nil {
				return fmt.Errorf("drafts: copy attachments: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO drafts (message_id, user_id, account_id, to_addrs, cc_addrs, bcc_addrs, subject, body_html,
  in_reply_to_message_id, silo_draft_id, attachment_ids)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`, msgID, p.UserID, p.AccountID, addrJSON(nd.To), addrJSON(nd.Cc),
			addrJSON(nd.Bcc), nd.Subject, nd.BodyHTML, inReplyTo, nd.SiloDraftID, attIDs); err != nil {
			return fmt.Errorf("drafts: insert: %w", err)
		}
		_, err = mail.RefreshMessages(ctx, tx, p.AccountID, []int64{msgID}, p.Origin)
		return err
	})
	if err != nil {
		return nil, err
	}
	d, err := s.load(ctx, s.o.Pool, p.UserID, msgID)
	if err == nil && pc != nil {
		d.Warnings = pc.warnings
	}
	return d, err
}

func addrJSON(l []mail.Address) []byte {
	if l == nil {
		l = []mail.Address{}
	}
	b, _ := json.Marshal(l)
	return b
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// load reads a draft of the user; ErrNotFound without a drafts row.
func (s *Service) load(ctx context.Context, db querier, userID, id int64) (*Draft, error) {
	d := &Draft{ID: id}
	var to, cc, bcc []byte
	var ids []int64
	var obID *int64
	var obStatus, obErr *string
	var obFailed []string
	var obAt *time.Time
	err := db.QueryRow(ctx, `
SELECT d.account_id, m.thread_id, d.to_addrs, d.cc_addrs, d.bcc_addrs, d.subject, d.body_html, d.body_json,
       d.in_reply_to_message_id, d.attachment_ids, d.version, d.updated_at, d.server_synced_version,
       d.server_synced_version > 0 AND NOT EXISTS (
         SELECT 1 FROM message_locations l WHERE l.message_id = d.message_id AND NOT l.flags @> ARRAY['\Deleted']),
       o.id, o.status, o.last_error, o.failed_recipients, o.updated_at
FROM drafts d JOIN messages m ON m.id = d.message_id
LEFT JOIN outbound o ON o.message_id = d.message_id AND o.status = 'failed'
WHERE d.message_id = $1 AND d.user_id = $2 AND `+notSending, id, userID).Scan(&d.AccountID, &d.ThreadID, &to, &cc, &bcc, &d.Subject,
		&d.BodyHTML, &d.BodyJSON, &d.InReplyTo, &ids, &d.Version, &d.UpdatedAt, &d.SyncedVersion, &d.ServerChanged,
		&obID, &obStatus, &obErr, &obFailed, &obAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if obID != nil {
		d.LastSend = &LastSend{OutboundID: *obID, Status: *obStatus, FailedRecipients: obFailed, UpdatedAt: *obAt}
		if obErr != nil {
			d.LastSend.Error = *obErr
		}
	}
	for _, p := range []struct {
		raw []byte
		dst *[]mail.Address
	}{{to, &d.To}, {cc, &d.Cc}, {bcc, &d.Bcc}} {
		*p.dst = []mail.Address{}
		_ = json.Unmarshal(p.raw, p.dst)
	}
	rows, err := db.Query(ctx, `
SELECT id, filename, content_type, size_bytes, is_inline, COALESCE(content_id, '') FROM attachments WHERE message_id = $1
ORDER BY array_position($2::bigint[], id) NULLS LAST, id`, id, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	d.Attachments = []Upload{}
	for rows.Next() {
		var u Upload
		if err := rows.Scan(&u.ID, &u.Filename, &u.ContentType, &u.Size, &u.Inline, &u.CID); err != nil {
			return nil, err
		}
		d.Attachments = append(d.Attachments, u)
	}
	return d, rows.Err()
}

// Get returns a draft. A message with the DRAFT label written by another
// client (no drafts row yet) is opened as a draft: its body is downloaded if
// needed and a drafts row is created from the parsed MIME (plan 3.8 step 3).
func (s *Service) Get(ctx context.Context, userID, id int64) (*Draft, error) {
	d, err := s.load(ctx, s.o.Pool, userID, id)
	if !errors.Is(err, ErrNotFound) {
		return d, err
	}
	return s.adopt(ctx, userID, id)
}

func (s *Service) adopt(ctx context.Context, userID, id int64) (*Draft, error) {
	var accountID int64
	err := s.o.Pool.QueryRow(ctx, `
SELECT m.account_id FROM messages m
WHERE m.id = $1 AND m.user_id = $2 AND m.orphaned_at IS NULL AND EXISTS (
  SELECT 1 FROM message_labels ml JOIN labels l ON l.id = ml.label_id
  WHERE ml.message_id = m.id AND l.system_key = 'DRAFT')`, id, userID).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && s.o.OpenRaw == nil {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rc, err := s.o.OpenRaw(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(rc, 64<<20))
	_ = rc.Close()
	if err != nil {
		return nil, err
	}
	pd := mail.ParseDraft(raw)
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := mail.LockAccount(ctx, tx, accountID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
INSERT INTO drafts (message_id, user_id, account_id, to_addrs, cc_addrs, bcc_addrs, subject, body_html,
  attachment_ids, version, server_synced_version, silo_draft_id)
SELECT $1, $2, $3, $4, $5, $6, $7, $8,
       COALESCE((SELECT array_agg(a.id ORDER BY a.id) FROM attachments a WHERE a.message_id = $1 AND a.blob_sha256 IS NOT NULL), '{}'),
       1, 1, $9
ON CONFLICT (message_id) DO NOTHING`, id, userID, accountID, addrJSON(pd.To), addrJSON(pd.Cc), addrJSON(pd.Bcc),
			pd.Subject, pd.BodyHTML, "d_"+randomToken())
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = mail.RefreshMessages(ctx, tx, accountID, []int64{id}, "")
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("drafts: open draft of another client: %w", err)
	}
	return s.load(ctx, s.o.Pool, userID, id)
}

// UpdateParams is a save of a draft (PUT /drafts/{id}).
type UpdateParams struct {
	UserID, ID    int64
	Version       int32 // the version the client edited
	To, Cc, Bcc   []mail.Address
	Subject       string
	BodyHTML      string
	BodyJSON      json.RawMessage
	AttachmentIDs []int64
	Origin        string
	// SyncNow saves the copy in the Drafts folder right away instead of
	// SyncDelay later (the compose window is closing; plan 3.8 step 2).
	SyncNow bool
	// AccountID moves a new-message draft to another account of the user
	// (the "From" choice; see moveDraft); 0 keeps the account.
	AccountID int64
}

func cleanAddrs(field string, l []mail.Address) ([]mail.Address, error) {
	out := make([]mail.Address, 0, len(l))
	for _, a := range l {
		e := strings.ToLower(strings.TrimSpace(a.Email))
		if i := strings.IndexByte(e, '@'); i <= 0 || i == len(e)-1 || strings.ContainsAny(e, " <>,;\"\r\n") {
			return nil, &AddressError{Field: field, Address: a.Email, base: ErrInvalid}
		}
		out = append(out, mail.Address{Name: strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "").Replace(a.Name)), Email: e})
	}
	return out, nil
}

// Update saves a draft with optimistic concurrency: the client sends the
// version it edited; another save in between makes it ErrConflict. The
// uploads in AttachmentIDs are pinned to the draft (others unpinned), the
// local message and thread are refreshed (thread.updated, draft.updated) and
// the copy in the server's Drafts folder is scheduled SyncDelay after the
// last change (one pending append job per draft, postponed by every save).
func (s *Service) Update(ctx context.Context, p UpdateParams) (*Draft, error) {
	var err error
	if p.To, err = cleanAddrs("to", p.To); err != nil {
		return nil, err
	}
	if p.Cc, err = cleanAddrs("cc", p.Cc); err != nil {
		return nil, err
	}
	if p.Bcc, err = cleanAddrs("bcc", p.Bcc); err != nil {
		return nil, err
	}
	p.Subject = strings.NewReplacer("\r", "", "\n", " ").Replace(p.Subject)
	slices.Sort(p.AttachmentIDs)
	p.AttachmentIDs = slices.Compact(p.AttachmentIDs)
	if p.AttachmentIDs == nil {
		p.AttachmentIDs = []int64{}
	}
	if len(p.BodyJSON) == 0 || string(p.BodyJSON) == "null" {
		p.BodyJSON = nil
	}

	var accountID int64
	err = s.o.Pool.QueryRow(ctx, `SELECT account_id FROM drafts d WHERE message_id = $1 AND user_id = $2 AND `+notSending, p.ID, p.UserID).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if p.AccountID != 0 && p.AccountID != accountID {
		return s.moveDraft(ctx, p, accountID)
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := mail.LockAccount(ctx, tx, accountID); err != nil {
			return err
		}
		var version int32
		var from mail.Address
		err := tx.QueryRow(ctx, `
SELECT d.version, a.email::text, a.display_name FROM drafts d JOIN accounts a ON a.id = d.account_id
WHERE d.message_id = $1 AND d.user_id = $2 AND `+notSending+` FOR UPDATE OF d`, p.ID, p.UserID).Scan(&version, &from.Email, &from.Name)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if version != p.Version {
			return ErrConflict
		}
		var n int
		if err := tx.QueryRow(ctx, `
SELECT count(*) FROM attachments
WHERE id = ANY($1) AND user_id = $2 AND blob_sha256 IS NOT NULL AND (message_id IS NULL OR message_id = $3)`,
			p.AttachmentIDs, p.UserID, p.ID).Scan(&n); err != nil {
			return err
		}
		if n != len(p.AttachmentIDs) {
			return invalid("unknown attachment id")
		}
		if _, err := tx.Exec(ctx, `UPDATE attachments SET message_id = NULL, created_at = now() WHERE message_id = $1 AND NOT (id = ANY($2))`, p.ID, p.AttachmentIDs); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE attachments SET message_id = $1 WHERE id = ANY($2) AND message_id IS NULL`, p.ID, p.AttachmentIDs); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
UPDATE drafts SET to_addrs = $2, cc_addrs = $3, bcc_addrs = $4, subject = $5, body_html = $6, body_json = $7,
  attachment_ids = $8, version = version + 1, updated_at = now()
WHERE message_id = $1 RETURNING version`, p.ID, addrJSON(p.To), addrJSON(p.Cc), addrJSON(p.Bcc), p.Subject, p.BodyHTML,
			[]byte(p.BodyJSON), p.AttachmentIDs).Scan(&version); err != nil {
			return err
		}
		var hasAtt bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM attachments WHERE message_id = $1 AND NOT is_inline)`, p.ID).Scan(&hasAtt); err != nil {
			return err
		}
		if err := mail.UpdateDraftMessage(ctx, tx, p.ID, mail.DraftFields{
			From: from, To: p.To, Cc: p.Cc, Bcc: p.Bcc, Subject: p.Subject, BodyHTML: p.BodyHTML, HasAttachments: hasAtt,
		}); err != nil {
			return err
		}
		if _, err := mail.RefreshMessages(ctx, tx, accountID, []int64{p.ID}, p.Origin); err != nil {
			return err
		}
		ev := map[string]any{"draftId": p.ID, "accountId": accountID, "version": version, "source": "silo"}
		if p.Origin != "" {
			ev["origin"] = p.Origin
		}
		if err := mail.WriteEvent(ctx, tx, p.UserID, EventDraftUpdated, ev); err != nil {
			return err
		}
		delay := s.o.SyncDelay
		if p.SyncNow {
			delay = 0
		}
		return s.scheduleAppend(ctx, tx, accountID, p.UserID, p.ID, delay)
	})
	if err != nil {
		return nil, err
	}
	return s.load(ctx, s.o.Pool, p.UserID, p.ID)
}

// EventDraftUpdated is written when a draft is saved (plan 5.4):
// {draftId, accountId, version, source}.
const EventDraftUpdated = "draft.updated"

// unclaimedAppend selects the pending append jobs of a draft that no worker
// has claimed.
const unclaimedAppend = `
SELECT j.id FROM jobs j JOIN op_messages om ON om.job_id = j.id
WHERE om.message_id = $1 AND j.kind = 'imap_op' AND j.status = 'pending' AND j.payload->>'op' = 'append'
  AND (j.locked_until IS NULL OR j.locked_until < now())`

// scheduleAppend moves the pending append job of the draft to delay from
// now, or enqueues one.
func (s *Service) scheduleAppend(ctx context.Context, tx pgx.Tx, accountID, userID, msgID int64, delay time.Duration) error {
	tag, err := tx.Exec(ctx, `UPDATE jobs SET run_at = now() + make_interval(secs => $2) WHERE id IN (`+unclaimedAppend+`)`,
		msgID, delay.Seconds())
	if err != nil || tag.RowsAffected() > 0 {
		return err
	}
	payload, _ := json.Marshal(mail.OpPayload{Op: mail.OpAppend, ToRole: "drafts"})
	var jobID int64
	if err := tx.QueryRow(ctx, `
INSERT INTO jobs (kind, account_id, payload, run_at, max_attempts)
VALUES ($1, $2, $3, now() + make_interval(secs => $4), $5) RETURNING id`,
		ops.JobKind, accountID, payload, delay.Seconds(), ops.MaxAttempts).Scan(&jobID); err != nil {
		return fmt.Errorf("drafts: enqueue append: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO op_messages (job_id, message_id, account_id, user_id) VALUES ($1, $2, $3, $4)`, jobID, msgID, accountID, userID)
	return err
}

// Delete discards a draft: the local message goes (attachments released,
// thread recomputed or deleted), a pending save is cancelled and, when a copy
// may be on the server, a job removes it from the Drafts folder (plan 3.8
// step 4). A message with the DRAFT label from another client is discarded
// the same way.
func (s *Service) Delete(ctx context.Context, userID, id int64, origin string) error {
	var accountID int64
	err := s.o.Pool.QueryRow(ctx, `
SELECT m.account_id FROM messages m
WHERE m.id = $1 AND m.user_id = $2 AND NOT EXISTS (
  SELECT 1 FROM outbound o WHERE o.message_id = m.id AND o.status IN ('queued', 'sending', 'sent')) AND (EXISTS (SELECT 1 FROM drafts d WHERE d.message_id = m.id) OR EXISTS (
  SELECT 1 FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE ml.message_id = m.id AND l.system_key = 'DRAFT'))`,
		id, userID).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := mail.LockAccount(ctx, tx, accountID); err != nil {
			return err
		}
		var sending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM outbound WHERE message_id = $1 AND status IN ('queued', 'sending', 'sent'))`, id).Scan(&sending); err != nil {
			return err
		}
		if sending { // sent meanwhile
			return ErrNotFound
		}
		if _, err := removeServerCopy(ctx, tx, accountID, id); err != nil {
			return err
		}
		return mail.DeleteMessages(ctx, tx, accountID, []int64{id}, origin)
	})
}

// removeServerCopy cancels the pending save of a draft and, when a copy may
// be in the server's Drafts folder, enqueues a delete_draft job for it (plan
// 3.8 step 4: discarded or sent). It returns the Drafts folder (0 if none).
func removeServerCopy(ctx context.Context, tx pgx.Tx, accountID, id int64) (int64, error) {
	var draftID string
	var synced int32
	err := tx.QueryRow(ctx, `SELECT silo_draft_id, server_synced_version FROM drafts WHERE message_id = $1`, id).Scan(&draftID, &synced)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	var folderID int64
	var uids []int64
	if err := tx.QueryRow(ctx, `SELECT id FROM mailbox_folders WHERE account_id = $1 AND role = 'drafts' ORDER BY id LIMIT 1`, accountID).Scan(&folderID); err == nil {
		r, err := tx.Query(ctx, `SELECT uid FROM message_locations WHERE folder_id = $1 AND message_id = $2`, folderID, id)
		if err != nil {
			return 0, err
		}
		if uids, err = pgx.CollectRows(r, pgx.RowTo[int64]); err != nil {
			return 0, err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	var claimed bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM jobs j JOIN op_messages om ON om.job_id = j.id
               WHERE om.message_id = $1 AND j.kind = 'imap_op' AND j.status = 'pending' AND j.payload->>'op' = 'append'
                 AND j.locked_until > now())`, id).Scan(&claimed); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM jobs WHERE id IN (`+unclaimedAppend+`)`, id); err != nil {
		return 0, err
	}
	if synced > 0 || len(uids) > 0 || claimed {
		p := mail.OpPayload{Op: mail.OpDeleteDraft, DraftID: draftID, FolderID: folderID}
		for _, u := range uids {
			p.UIDs = append(p.UIDs, uint32(u))
		}
		payload, _ := json.Marshal(p)
		if _, err := tx.Exec(ctx, `INSERT INTO jobs (kind, account_id, payload, max_attempts) VALUES ($1, $2, $3, $4)`,
			ops.JobKind, accountID, payload, ops.MaxAttempts); err != nil {
			return 0, fmt.Errorf("drafts: enqueue delete: %w", err)
		}
	}
	return folderID, nil
}
