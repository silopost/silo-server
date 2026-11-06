package backup

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/blob"
	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/version"
)

// ExportOptions configure Export.
type ExportOptions struct {
	Keys  *secrets.Keyring // decrypts and re-seals account and TOTP secrets
	Blobs blob.BlobStore   // source of the draft blobs
	// User limits the export to one user (email); empty exports everybody.
	User string
	// NoLocalBlobs leaves the bytes of draft attachments and queued messages
	// out (their sha256 stays in the drafts section).
	NoLocalBlobs bool
}

// Stats counts what an export wrote or an import read.
type Stats struct {
	Users, Accounts, Labels, LabelAssignments, Snoozes, Filters, SearchHistory, Contacts int
	Blobs                                                                                int
	BlobBytes                                                                            int64
	Drafts, Outbound                                                                     int
	// Warnings are problems that did not stop the run (missing blobs,
	// unknown sections, ...).
	Warnings []string
}

func (s *Stats) warn(format string, args ...any) {
	s.Warnings = append(s.Warnings, fmt.Sprintf(format, args...))
}

// Export writes the metadata document to w. It reads one consistent
// snapshot of the database (REPEATABLE READ) and streams blob contents from
// the blob store, one at a time.
func Export(ctx context.Context, pool *pgxpool.Pool, w io.Writer, o ExportOptions) (*Stats, error) {
	if o.Keys == nil {
		return nil, errors.New("backup: export needs the secret key (SILO_SECRET_KEY)")
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e := &exporter{ctx: ctx, tx: tx, o: o, st: &Stats{}, bw: bufio.NewWriterSize(w, 64<<10),
		users: map[int64]string{}, accounts: map[int64]AccountRef{}}
	if err := e.run(); err != nil {
		return nil, err
	}
	if err := e.bw.Flush(); err != nil {
		return nil, err
	}
	return e.st, nil
}

type exporter struct {
	ctx context.Context
	tx  pgx.Tx
	o   ExportOptions
	st  *Stats
	bw  *bufio.Writer

	users      map[int64]string     // id -> email
	accounts   map[int64]AccountRef // id -> key
	accountIDs []int64
	first      bool // no element written yet in the current array
}

func (e *exporter) raw(s string) { _, _ = e.bw.WriteString(s) }

// item writes one array element.
func (e *exporter) item(v any) error {
	if !e.first {
		e.raw(",")
	}
	e.first = false
	enc := json.NewEncoder(e.bw)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func (e *exporter) section(name string, first bool, fn func() error) error {
	if !first {
		e.raw(",")
	}
	e.raw(strconv.Quote(name) + ":[")
	e.first = true
	if err := fn(); err != nil {
		return fmt.Errorf("backup: export %s: %w", name, err)
	}
	e.raw("]")
	return nil
}

func (e *exporter) run() error {
	var schema int64
	if err := e.tx.QueryRow(e.ctx, `SELECT version FROM schema_migrations LIMIT 1`).Scan(&schema); err != nil {
		return fmt.Errorf("backup: schema version: %w", err)
	}
	h, _ := json.Marshal(Header{Format: Format, Version: Version, ExportedAt: time.Now().UTC(),
		ServerVersion: version.Version, SchemaVersion: schema, LocalBlobs: !e.o.NoLocalBlobs})
	e.raw(string(h[:len(h)-1]) + `,"sections":{`)

	if err := e.section(SectionUsers, true, e.exportUsers); err != nil {
		return err
	}
	if e.o.User != "" && len(e.users) == 0 {
		return fmt.Errorf("backup: no user %q", e.o.User)
	}
	// Drafts are read up front: their blobs are written before them.
	drafts, blobs, err := e.loadDrafts()
	if err != nil {
		return fmt.Errorf("backup: export drafts: %w", err)
	}
	steps := []struct {
		name string
		fn   func() error
	}{
		{SectionAccounts, e.exportAccounts},
		{SectionLabels, e.exportLabels},
		{SectionLabelAssignments, e.exportAssignments},
		{SectionSnoozes, e.exportSnoozes},
		{SectionFilters, e.exportFilters},
		{SectionSearchHistory, e.exportSearchHistory},
		{SectionContacts, e.exportContacts},
		{SectionBlobs, func() error { return e.exportBlobs(blobs) }},
		{SectionDrafts, func() error {
			for _, d := range drafts {
				if err := e.item(d); err != nil {
					return err
				}
			}
			e.st.Drafts = len(drafts)
			return nil
		}},
		{SectionOutbound, e.exportOutbound},
	}
	for _, s := range steps {
		if err := e.section(s.name, false, s.fn); err != nil {
			return err
		}
	}
	e.raw("}}\n")
	return nil
}

func (e *exporter) exportUsers() error {
	rows, err := e.tx.Query(e.ctx, `
SELECT id, email::text, display_name, password_hash, is_admin, settings, totp_secret_enc, totp_enabled_at,
       totp_last_step, totp_backup_codes, disabled_at, password_changed_at, created_at
FROM users WHERE $1 = '' OR email = $1::citext ORDER BY id`, e.o.User)
	if err != nil {
		return err
	}
	var list []User
	var ids []int64
	for rows.Next() {
		var id int64
		var u User
		var totp []byte
		if err := rows.Scan(&id, &u.Email, &u.DisplayName, &u.PasswordHash, &u.IsAdmin, &u.Settings, &totp, &u.TOTPEnabledAt,
			&u.TOTPLastStep, &u.TOTPBackupCodes, &u.DisabledAt, &u.PasswordChangedAt, &u.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		if totp != nil {
			plain, err := e.o.Keys.Decrypt(totp, []byte(strconv.FormatInt(id, 10)))
			if err != nil {
				rows.Close()
				return fmt.Errorf("decrypt TOTP secret of %s (wrong SILO_SECRET_KEY?): %w", u.Email, err)
			}
			if u.TOTPSecretEnc, err = e.o.Keys.Encrypt(plain, TOTPAAD(u.Email)); err != nil {
				rows.Close()
				return err
			}
		}
		e.users[id] = u.Email
		ids = append(ids, id)
		list = append(list, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// The accounts of the exported users, for every later section.
	arows, err := e.tx.Query(e.ctx, `SELECT id, user_id, imap_host, username FROM accounts WHERE user_id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return err
	}
	for arows.Next() {
		var id, uid int64
		var r AccountRef
		if err := arows.Scan(&id, &uid, &r.IMAPHost, &r.Username); err != nil {
			arows.Close()
			return err
		}
		r.User = e.users[uid]
		e.accounts[id] = r
		e.accountIDs = append(e.accountIDs, id)
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return err
	}
	for _, u := range list {
		// active_account_id is a local id: exported as the account key.
		var settings map[string]json.RawMessage
		if json.Unmarshal(u.Settings, &settings) == nil {
			if v, ok := settings[settingsKeyActiveAccount]; ok {
				var id int64
				if json.Unmarshal(v, &id) == nil {
					if r, ok := e.accounts[id]; ok {
						u.ActiveAccount = &r
					}
				}
				delete(settings, settingsKeyActiveAccount)
				u.Settings, _ = json.Marshal(settings)
			}
		}
		if err := e.item(u); err != nil {
			return err
		}
	}
	e.st.Users = len(list)
	return nil
}

func (e *exporter) exportAccounts() error {
	rows, err := e.tx.Query(e.ctx, `
SELECT id, email::text, display_name, provider, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, auth_type,
       secret_enc, folder_mapping, keywords_supported, keyword_prefix, sent_append, position, filters_since, created_at
FROM accounts WHERE id = ANY($1) ORDER BY id`, e.accountIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var a Account
		var secret []byte
		if err := rows.Scan(&id, &a.Email, &a.DisplayName, &a.Provider, &a.IMAPPort, &a.IMAPTLS, &a.SMTPHost, &a.SMTPPort,
			&a.SMTPTLS, &a.AuthType, &secret, &a.FolderMapping, &a.KeywordsSupported, &a.KeywordPrefix, &a.SentAppend,
			&a.Position, &a.FiltersSince, &a.CreatedAt); err != nil {
			return err
		}
		a.AccountRef = e.accounts[id]
		plain, err := e.o.Keys.Decrypt(secret, []byte(strconv.FormatInt(id, 10)))
		if err != nil {
			return fmt.Errorf("decrypt secret of account %s/%s (wrong SILO_SECRET_KEY?): %w", a.IMAPHost, a.Username, err)
		}
		if a.SecretEnc, err = e.o.Keys.Encrypt(plain, AccountAAD(a.AccountRef)); err != nil {
			return err
		}
		if err := e.item(a); err != nil {
			return err
		}
		e.st.Accounts++
	}
	return rows.Err()
}

func (e *exporter) exportLabels() error {
	rows, err := e.tx.Query(e.ctx, `
SELECT l.account_id, l.name, l.imap_keyword, l.keyword_state, l.color_bg, l.color_text, l.show_in_list, p.name, l.position
FROM labels l LEFT JOIN labels p ON p.id = l.parent_id
WHERE l.account_id = ANY($1) AND l.type = 'user'
ORDER BY l.account_id, l.position, l.id`, e.accountIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var acc int64
		var l Label
		if err := rows.Scan(&acc, &l.Name, &l.IMAPKeyword, &l.KeywordState, &l.ColorBG, &l.ColorText, &l.ShowInList, &l.Parent, &l.Position); err != nil {
			return err
		}
		l.Account = e.accounts[acc]
		if err := e.item(l); err != nil {
			return err
		}
		e.st.Labels++
	}
	return rows.Err()
}

func (e *exporter) exportAssignments() error {
	rows, err := e.tx.Query(e.ctx, `
SELECT DISTINCT l.account_id, l.name, m.message_id_hdr
FROM message_labels ml
JOIN labels l ON l.id = ml.label_id
JOIN messages m ON m.id = ml.message_id
WHERE l.account_id = ANY($1) AND l.type = 'user'
ORDER BY 1, 2, 3`, e.accountIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var acc int64
		var a LabelAssignment
		if err := rows.Scan(&acc, &a.Label, &a.MessageIDHdr); err != nil {
			return err
		}
		a.Account = e.accounts[acc]
		if err := e.item(a); err != nil {
			return err
		}
		e.st.LabelAssignments++
	}
	return rows.Err()
}

// snoozeRows reads the snoozes of accounts with their items keyed by
// Message-ID (shared with the rebuild, which stages them).
func snoozeRows(ctx context.Context, db pgx.Tx, accountIDs []int64) (map[int64][]Snooze, error) {
	rows, err := db.Query(ctx, `
SELECT s.account_id, s.wake_at, s.created_at,
       COALESCE(array_agg(m.message_id_hdr ORDER BY m.id) FILTER (WHERE m.id IS NOT NULL), '{}'),
       COALESCE(array_agg(COALESCE(f.name, '') ORDER BY m.id) FILTER (WHERE m.id IS NOT NULL), '{}')
FROM snoozes s
LEFT JOIN snooze_items si ON si.snooze_id = s.id
LEFT JOIN messages m ON m.id = si.message_id
LEFT JOIN mailbox_folders f ON f.id = si.source_folder_id
WHERE s.account_id = ANY($1)
GROUP BY s.id ORDER BY s.id`, accountIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]Snooze{}
	for rows.Next() {
		var acc int64
		var s Snooze
		var folders []string
		if err := rows.Scan(&acc, &s.WakeAt, &s.CreatedAt, &s.Items, &folders); err != nil {
			return nil, err
		}
		for i, it := range s.Items {
			if i < len(folders) && folders[i] != "" {
				if s.SourceFolders == nil {
					s.SourceFolders = map[string]string{}
				}
				s.SourceFolders[it] = folders[i]
			}
		}
		out[acc] = append(out[acc], s)
	}
	return out, rows.Err()
}

func (e *exporter) exportSnoozes() error {
	byAcc, err := snoozeRows(e.ctx, e.tx, e.accountIDs)
	if err != nil {
		return err
	}
	for _, id := range e.accountIDs {
		for _, s := range byAcc[id] {
			s.Account = e.accounts[id]
			if err := e.item(s); err != nil {
				return err
			}
			e.st.Snoozes++
		}
	}
	return nil
}

func (e *exporter) exportFilters() error {
	names := map[int64]string{} // user label id -> name
	lrows, err := e.tx.Query(e.ctx, `SELECT id, name FROM labels WHERE account_id = ANY($1) AND type = 'user'`, e.accountIDs)
	if err != nil {
		return err
	}
	for lrows.Next() {
		var id int64
		var name string
		if err := lrows.Scan(&id, &name); err != nil {
			lrows.Close()
			return err
		}
		names[id] = name
	}
	if err := lrows.Err(); err != nil {
		return err
	}
	rows, err := e.tx.Query(e.ctx, `
SELECT account_id, name, criteria, actions, enabled, position, created_at, updated_at FROM filters
WHERE account_id = ANY($1) ORDER BY account_id, position, id`, e.accountIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var acc int64
		var f Filter
		var enabled bool
		var updated time.Time
		if err := rows.Scan(&acc, &f.Name, &f.Criteria, &f.Actions, &enabled, &f.Position, &f.CreatedAt, &updated); err != nil {
			return err
		}
		f.Enabled, f.UpdatedAt = &enabled, &updated
		f.Account = e.accounts[acc]
		if f.Actions, err = exportFilterActions(f.Actions, names, func(id int64) {
			e.st.warn("filter %q of %s: label %d is gone, left out of its actions", f.Name, f.Account.Username, id)
		}); err != nil {
			return err
		}
		if err := e.item(f); err != nil {
			return err
		}
		e.st.Filters++
	}
	return rows.Err()
}

func (e *exporter) exportSearchHistory() error {
	rows, err := e.tx.Query(e.ctx, `
SELECT account_id, query, used_at FROM search_history
WHERE account_id = ANY($1) ORDER BY account_id, used_at, id`, e.accountIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var acc int64
		var s SearchHistory
		if err := rows.Scan(&acc, &s.Query, &s.UsedAt); err != nil {
			return err
		}
		s.Account = e.accounts[acc]
		if err := e.item(s); err != nil {
			return err
		}
		e.st.SearchHistory++
	}
	return rows.Err()
}

func (e *exporter) exportContacts() error {
	ids := make([]int64, 0, len(e.users))
	for id := range e.users {
		ids = append(ids, id)
	}
	rows, err := e.tx.Query(e.ctx, `
SELECT user_id, email::text, name, sent_count, received_count, last_interaction_at FROM contacts
WHERE user_id = ANY($1) AND is_manual ORDER BY user_id, id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var uid int64
		var c Contact
		if err := rows.Scan(&uid, &c.Email, &c.Name, &c.SentCount, &c.ReceivedCount, &c.LastInteractionAt); err != nil {
			return err
		}
		c.User = e.users[uid]
		if err := e.item(c); err != nil {
			return err
		}
		e.st.Contacts++
	}
	return rows.Err()
}

func hexPtr(b []byte) *string {
	if b == nil {
		return nil
	}
	s := hex.EncodeToString(b)
	return &s
}

// loadDrafts reads the drafts with their messages and attachments, and the
// blobs they reference (sha256 hex -> size).
func (e *exporter) loadDrafts() ([]Draft, map[string]int64, error) {
	rows, err := e.tx.Query(e.ctx, `
SELECT d.message_id, d.account_id, d.silo_draft_id, m.message_id_hdr, m.in_reply_to_hdr,
       COALESCE((SELECT array_agg(r.ref_message_id_hdr ORDER BY r.position) FROM message_references r WHERE r.message_id = m.id), '{}'),
       (SELECT rm.message_id_hdr FROM messages rm WHERE rm.id = d.in_reply_to_message_id),
       m.from_addr::text, m.from_name, d.to_addrs, d.cc_addrs, d.bcc_addrs, d.subject, d.body_html, d.body_json,
       d.version, d.server_synced_version, d.updated_at, m.internal_date, m.raw_blob_sha256,
       COALESCE((SELECT b.size_bytes FROM blobs b WHERE b.sha256 = m.raw_blob_sha256), 0), d.attachment_ids
FROM drafts d JOIN messages m ON m.id = d.message_id
WHERE d.account_id = ANY($1)
ORDER BY d.account_id, d.message_id`, e.accountIDs)
	if err != nil {
		return nil, nil, err
	}
	var out []Draft
	var msgIDs []int64
	listed := map[int64]bool{}
	byMsg := map[int64]int{}
	blobs := map[string]int64{}
	for rows.Next() {
		var msgID, acc int64
		var d Draft
		var raw []byte
		var attIDs []int64
		if err := rows.Scan(&msgID, &acc, &d.SiloDraftID, &d.MessageIDHdr, &d.InReplyTo, &d.References, &d.ReplyToMessageIDHdr,
			&d.FromEmail, &d.FromName, &d.To, &d.Cc, &d.Bcc, &d.Subject, &d.BodyHTML, &d.BodyJSON,
			&d.Version, &d.ServerSyncedVersion, &d.UpdatedAt, &d.InternalDate, &raw, &d.RawSize, &attIDs); err != nil {
			rows.Close()
			return nil, nil, err
		}
		d.Account = e.accounts[acc]
		d.RawSHA256 = hexPtr(raw)
		if d.RawSHA256 != nil {
			blobs[*d.RawSHA256] = d.RawSize
		}
		d.Attachments = []Attachment{}
		for _, id := range attIDs {
			listed[id] = true
		}
		byMsg[msgID] = len(out)
		msgIDs = append(msgIDs, msgID)
		out = append(out, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	arows, err := e.tx.Query(e.ctx, `
SELECT a.id, a.message_id, a.blob_sha256, a.filename, a.content_type, a.size_bytes, a.content_id, a.is_inline, a.part_path,
       COALESCE(b.size_bytes, 0)
FROM attachments a LEFT JOIN blobs b ON b.sha256 = a.blob_sha256
WHERE a.message_id = ANY($1) ORDER BY a.id`, msgIDs)
	if err != nil {
		return nil, nil, err
	}
	defer arows.Close()
	for arows.Next() {
		var id, msgID, blobSize int64
		var a Attachment
		var sha []byte
		if err := arows.Scan(&id, &msgID, &sha, &a.Filename, &a.ContentType, &a.Size, &a.ContentID, &a.IsInline, &a.PartPath, &blobSize); err != nil {
			return nil, nil, err
		}
		a.SHA256, a.Listed = hexPtr(sha), listed[id]
		if a.SHA256 != nil {
			blobs[*a.SHA256] = blobSize
		}
		d := &out[byMsg[msgID]]
		d.Attachments = append(d.Attachments, a)
	}
	return out, blobs, arows.Err()
}

// exportBlobs streams every referenced blob as base64.
func (e *exporter) exportBlobs(blobs map[string]int64) error {
	if e.o.NoLocalBlobs || len(blobs) == 0 {
		return nil
	}
	if e.o.Blobs == nil {
		return errors.New("no blob store")
	}
	keys := make([]string, 0, len(blobs))
	for k := range blobs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, sum := range keys {
		rc, err := e.o.Blobs.Open(e.ctx, sum)
		if errors.Is(err, blob.ErrNotFound) {
			e.st.warn("blob %s is missing from the blob store; exported without content", sum)
			continue
		}
		if err != nil {
			return err
		}
		if !e.first {
			e.raw(",")
		}
		e.first = false
		e.raw(fmt.Sprintf(`{"sha256":%q,"size":%d,"data":"`, sum, blobs[sum]))
		enc := base64.NewEncoder(base64.StdEncoding, e.bw)
		n, err := io.Copy(enc, rc)
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("read blob %s: %w", sum, err)
		}
		if err := enc.Close(); err != nil {
			return err
		}
		e.raw("\"}\n")
		e.st.Blobs++
		e.st.BlobBytes += n
	}
	return nil
}

func (e *exporter) exportOutbound() error {
	rows, err := e.tx.Query(e.ctx, `
SELECT o.account_id, d.silo_draft_id, o.send_at, o.attempts
FROM outbound o JOIN drafts d ON d.message_id = o.message_id
WHERE o.account_id = ANY($1) AND o.status IN ('queued', 'sending')
ORDER BY o.account_id, o.id`, e.accountIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var acc int64
		var o Outbound
		if err := rows.Scan(&acc, &o.SiloDraftID, &o.SendAt, &o.Attempts); err != nil {
			return err
		}
		o.Account = e.accounts[acc]
		if err := e.item(o); err != nil {
			return err
		}
		e.st.Outbound++
	}
	return rows.Err()
}

// exportFilterActions replaces the label ids of addLabelIds by the names of
// the labels (FilterActionLabels); ids without a user label are reported to
// missing and left out.
func exportFilterActions(raw json.RawMessage, names map[int64]string, missing func(int64)) (json.RawMessage, error) {
	var a map[string]json.RawMessage
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("{}"), nil
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("backup: filter actions: %w", err)
	}
	ids, ok := a[filterActionLabelIDs]
	if !ok {
		return raw, nil
	}
	delete(a, filterActionLabelIDs)
	var list []int64
	if err := json.Unmarshal(ids, &list); err != nil {
		return nil, fmt.Errorf("backup: filter addLabelIds: %w", err)
	}
	labels := []string{}
	for _, id := range list {
		if n, ok := names[id]; ok {
			labels = append(labels, n)
		} else {
			missing(id)
		}
	}
	if len(labels) > 0 {
		b, _ := json.Marshal(labels)
		a[FilterActionLabels] = b
	}
	return json.Marshal(a)
}
