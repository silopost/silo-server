package backup

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/blob"
	"github.com/silopost/silo-server/internal/drafts"
	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
	"github.com/silopost/silo-server/internal/worker"
)

// Errors of Import.
var (
	ErrFormat     = errors.New("backup: not a Silo metadata export")
	ErrUserExists = errors.New("backup: user already exists (use --merge)")
)

// ImportOptions configure Import.
type ImportOptions struct {
	Keys  *secrets.Keyring // must hold the SILO_SECRET_KEY of the export
	Blobs blob.BlobStore   // receives the draft blobs
	// Merge imports into existing users: accounts found by key get the
	// exported configuration, labels are upserted by name, assignments,
	// snoozes, filters, contacts and search history are added (nothing is
	// duplicated), drafts already present (silo_draft_id) are skipped.
	// Without Merge an existing user is an error.
	Merge bool
	// Now replaces time.Now (tests).
	Now func() time.Time
}

// document is a parsed export; blobs are already in the blob store.
type document struct {
	header           Header
	users            []User
	accounts         []Account
	labels           []Label
	labelAssignments []LabelAssignment
	snoozes          []Snooze
	filters          []Filter
	searchHistory    []SearchHistory
	contacts         []Contact
	drafts           []Draft
	outbound         []Outbound
	blobs            map[string]int64 // sha256 hex -> size, stored by the parser
}

// Import reads an export and writes it into the database in one
// transaction. Blobs are written to the blob store while the document is
// read (their sha256 is checked). Imported accounts start a fresh initial
// sync; user-label assignments and snoozes are staged and applied by the
// metadata_restore job as the messages arrive.
func Import(ctx context.Context, pool *pgxpool.Pool, r io.Reader, o ImportOptions) (*Stats, error) {
	if o.Keys == nil {
		return nil, errors.New("backup: import needs the secret key of the export (SILO_SECRET_KEY)")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	st := &Stats{}
	doc, err := parse(ctx, r, o.Blobs, st)
	if err != nil {
		return nil, err
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		im := &importer{ctx: ctx, tx: tx, o: o, st: st, doc: doc,
			users: map[string]int64{}, accounts: map[AccountRef]int64{}, accUser: map[int64]int64{},
			drafts: map[string]int64{}, refresh: map[int64][]int64{}, blobOK: map[string]bool{}}
		return im.run()
	})
	if err != nil {
		return nil, err
	}
	return st, nil
}

// parse reads the document. Blobs go to the store as they come; every other
// section is decoded into memory (metadata only).
func parse(ctx context.Context, r io.Reader, store blob.BlobStore, st *Stats) (*document, error) {
	dec := json.NewDecoder(r)
	doc := &document{blobs: map[string]int64{}}
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	sawFormat := false
	for dec.More() {
		key, err := stringToken(dec)
		if err != nil {
			return nil, err
		}
		switch key {
		case "format":
			if err := dec.Decode(&doc.header.Format); err != nil {
				return nil, err
			}
			if doc.header.Format != Format {
				return nil, fmt.Errorf("%w: format %q", ErrFormat, doc.header.Format)
			}
			sawFormat = true
		case "version":
			if err := dec.Decode(&doc.header.Version); err != nil {
				return nil, err
			}
			if doc.header.Version > Version {
				return nil, fmt.Errorf("%w: version %d is newer than this server reads (%d)", ErrFormat, doc.header.Version, Version)
			}
		case "exportedAt":
			if err := dec.Decode(&doc.header.ExportedAt); err != nil {
				return nil, err
			}
		case "serverVersion":
			if err := dec.Decode(&doc.header.ServerVersion); err != nil {
				return nil, err
			}
		case "schemaVersion":
			if err := dec.Decode(&doc.header.SchemaVersion); err != nil {
				return nil, err
			}
		case "localBlobs":
			if err := dec.Decode(&doc.header.LocalBlobs); err != nil {
				return nil, err
			}
		case "sections":
			if !sawFormat {
				return nil, fmt.Errorf("%w: sections before the format field", ErrFormat)
			}
			if err := parseSections(ctx, dec, doc, store, st); err != nil {
				return nil, err
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
		}
	}
	if !sawFormat {
		return nil, ErrFormat
	}
	if doc.header.Version < 1 {
		return nil, fmt.Errorf("%w: no version", ErrFormat)
	}
	return doc, nil
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	t, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFormat, err)
	}
	if d, ok := t.(json.Delim); !ok || d != want {
		return fmt.Errorf("%w: expected %q", ErrFormat, want)
	}
	return nil
}

func stringToken(dec *json.Decoder) (string, error) {
	t, err := dec.Token()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrFormat, err)
	}
	s, ok := t.(string)
	if !ok {
		return "", fmt.Errorf("%w: expected a key", ErrFormat)
	}
	return s, nil
}

func parseSections(ctx context.Context, dec *json.Decoder, doc *document, store blob.BlobStore, st *Stats) error {
	if err := expectDelim(dec, '{'); err != nil {
		return err
	}
	for dec.More() {
		name, err := stringToken(dec)
		if err != nil {
			return err
		}
		var target any
		switch name {
		case SectionUsers:
			target = &doc.users
		case SectionAccounts:
			target = &doc.accounts
		case SectionLabels:
			target = &doc.labels
		case SectionLabelAssignments:
			target = &doc.labelAssignments
		case SectionSnoozes:
			target = &doc.snoozes
		case SectionFilters:
			target = &doc.filters
		case SectionSearchHistory:
			target = &doc.searchHistory
		case SectionContacts:
			target = &doc.contacts
		case SectionDrafts:
			target = &doc.drafts
		case SectionOutbound:
			target = &doc.outbound
		case SectionBlobs:
			if err := parseBlobs(ctx, dec, doc, store, st); err != nil {
				return err
			}
			continue
		default:
			st.warn("unknown section %q skipped", name)
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return err
			}
			continue
		}
		if err := dec.Decode(target); err != nil {
			return fmt.Errorf("backup: section %s: %w", name, err)
		}
	}
	return expectDelim(dec, '}')
}

// parseBlobs stores each blob as it is read (one in memory at a time) and
// checks its sha256.
func parseBlobs(ctx context.Context, dec *json.Decoder, doc *document, store blob.BlobStore, st *Stats) error {
	if err := expectDelim(dec, '['); err != nil {
		return err
	}
	for dec.More() {
		var b Blob
		if err := dec.Decode(&b); err != nil {
			return fmt.Errorf("backup: section blobs: %w", err)
		}
		if store == nil {
			return errors.New("backup: no blob store")
		}
		sum, size, err := store.Put(ctx, bytes.NewReader(b.Data))
		if err != nil {
			return fmt.Errorf("backup: store blob %s: %w", b.SHA256, err)
		}
		if sum != strings.ToLower(b.SHA256) {
			return fmt.Errorf("backup: blob %s: content has sha256 %s", b.SHA256, sum)
		}
		doc.blobs[sum] = size
		st.Blobs++
		st.BlobBytes += size
	}
	return expectDelim(dec, ']')
}

type importer struct {
	ctx context.Context
	tx  pgx.Tx
	o   ImportOptions
	st  *Stats
	doc *document

	users    map[string]int64     // lower(email) -> id
	newUsers map[string]bool      // created by this import
	accounts map[AccountRef]int64 // key (user lower-cased) -> id
	accUser  map[int64]int64      // account -> user
	drafts   map[string]int64     // silo_draft_id -> message id (imported now)
	refresh  map[int64][]int64    // account -> local messages to derive
	blobOK   map[string]bool      // blob usable (registered or present in the store)
	restore  []int64              // accounts that get a metadata_restore job
}

func normRef(r AccountRef) AccountRef {
	r.User = strings.ToLower(r.User)
	r.IMAPHost = strings.ToLower(r.IMAPHost)
	return r
}

func (im *importer) account(r AccountRef) (int64, bool) {
	id, ok := im.accounts[normRef(r)]
	return id, ok
}

func (im *importer) run() error {
	steps := []func() error{
		im.importUsers, im.importAccounts, im.importLabels, im.stageAssignments, im.stageSnoozes,
		im.importFilters, im.importSearchHistory, im.importContacts, im.importDrafts, im.importOutbound,
		im.finish,
	}
	for _, s := range steps {
		if err := s(); err != nil {
			return err
		}
	}
	return nil
}

func (im *importer) importUsers() error {
	im.newUsers = map[string]bool{}
	for _, u := range im.doc.users {
		key := strings.ToLower(u.Email)
		var id int64
		err := im.tx.QueryRow(im.ctx, `SELECT id FROM users WHERE email = $1`, u.Email).Scan(&id)
		switch {
		case err == nil && !im.o.Merge:
			return fmt.Errorf("%w: %s", ErrUserExists, u.Email)
		case err == nil:
			im.users[key] = id
			continue
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		settings := u.Settings
		if len(settings) == 0 || string(settings) == "null" {
			settings = json.RawMessage("{}")
		}
		codes := u.TOTPBackupCodes
		if codes == nil {
			codes = []string{}
		}
		if err := im.tx.QueryRow(im.ctx, `
INSERT INTO users (email, display_name, password_hash, is_admin, settings, totp_enabled_at, totp_last_step,
  totp_backup_codes, disabled_at, password_changed_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id`,
			u.Email, u.DisplayName, u.PasswordHash, u.IsAdmin, []byte(settings), u.TOTPEnabledAt, u.TOTPLastStep,
			codes, u.DisabledAt, u.PasswordChangedAt, u.CreatedAt).Scan(&id); err != nil {
			return fmt.Errorf("backup: insert user %s: %w", u.Email, err)
		}
		if u.TOTPSecretEnc != nil {
			plain, err := im.o.Keys.Decrypt(u.TOTPSecretEnc, TOTPAAD(u.Email))
			if err != nil {
				return fmt.Errorf("backup: TOTP secret of %s does not decrypt (another SILO_SECRET_KEY?): %w", u.Email, err)
			}
			sealed, err := im.o.Keys.Encrypt(plain, []byte(strconv.FormatInt(id, 10)))
			if err != nil {
				return err
			}
			if _, err := im.tx.Exec(im.ctx, `UPDATE users SET totp_secret_enc = $2 WHERE id = $1`, id, sealed); err != nil {
				return err
			}
		}
		im.users[key] = id
		im.newUsers[key] = true
		im.st.Users++
	}
	return nil
}

// userID resolves a user of the document, or an existing user (sections
// that refer to users outside the users section).
func (im *importer) userID(email string) (int64, bool, error) {
	key := strings.ToLower(email)
	if id, ok := im.users[key]; ok {
		return id, true, nil
	}
	var id int64
	err := im.tx.QueryRow(im.ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !im.o.Merge {
		return 0, false, fmt.Errorf("%w: %s", ErrUserExists, email)
	}
	im.users[key] = id
	return id, true, nil
}

func (im *importer) importAccounts() error {
	for _, a := range im.doc.accounts {
		uid, ok, err := im.userID(a.User)
		if err != nil {
			return err
		}
		if !ok {
			im.st.warn("account %s/%s: user %s is not in the export; skipped", a.IMAPHost, a.Username, a.User)
			continue
		}
		plain, err := im.o.Keys.Decrypt(a.SecretEnc, AccountAAD(a.AccountRef))
		if err != nil {
			return fmt.Errorf("backup: secret of account %s/%s does not decrypt (the import needs the SILO_SECRET_KEY of the export): %w",
				a.IMAPHost, a.Username, err)
		}
		mapping := a.FolderMapping
		if len(mapping) == 0 || string(mapping) == "null" {
			mapping = json.RawMessage("{}")
		}
		var id int64
		err = im.tx.QueryRow(im.ctx, `SELECT id FROM accounts WHERE user_id = $1 AND imap_host = $2 AND username = $3`,
			uid, a.IMAPHost, a.Username).Scan(&id)
		existed := err == nil
		switch {
		case existed:
			if _, err := im.tx.Exec(im.ctx, `
UPDATE accounts SET email = $3, display_name = $4, provider = $5, imap_port = $6, imap_tls = $7, smtp_host = $8,
  smtp_port = $9, smtp_tls = $10, auth_type = $11, folder_mapping = $12, keyword_prefix = $13, sent_append = $14
WHERE id = $1 AND user_id = $2`, id, uid, a.Email, a.DisplayName, a.Provider, a.IMAPPort, a.IMAPTLS, a.SMTPHost,
				a.SMTPPort, a.SMTPTLS, a.AuthType, []byte(mapping), a.KeywordPrefix, a.SentAppend); err != nil {
				return fmt.Errorf("backup: update account %s/%s: %w", a.IMAPHost, a.Username, err)
			}
		case errors.Is(err, pgx.ErrNoRows):
			if err := im.tx.QueryRow(im.ctx, `
INSERT INTO accounts (user_id, email, display_name, provider, imap_host, imap_port, imap_tls, smtp_host, smtp_port,
  smtp_tls, username, auth_type, secret_enc, folder_mapping, keywords_supported, keyword_prefix, sent_append,
  position, filters_since, created_at, sync_state)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, '\x', $13, $14, $15, $16, $17, $18, $19, 'connecting')
RETURNING id`, uid, a.Email, a.DisplayName, a.Provider, a.IMAPHost, a.IMAPPort, a.IMAPTLS, a.SMTPHost, a.SMTPPort,
				a.SMTPTLS, a.Username, a.AuthType, []byte(mapping), a.KeywordsSupported, a.KeywordPrefix, a.SentAppend,
				a.Position, a.FiltersSince, a.CreatedAt).Scan(&id); err != nil {
				return fmt.Errorf("backup: insert account %s/%s: %w", a.IMAPHost, a.Username, err)
			}
		default:
			return err
		}
		sealed, err := im.o.Keys.Encrypt(plain, []byte(strconv.FormatInt(id, 10)))
		if err != nil {
			return err
		}
		if _, err := im.tx.Exec(im.ctx, `UPDATE accounts SET secret_enc = $3 WHERE id = $1 AND user_id = $2`, id, uid, sealed); err != nil {
			return err
		}
		// As POST /accounts (new) or a changed password (existing): the
		// account's worker connects and syncs.
		kind := auth.JobAccountInitialSync
		if existed {
			kind = auth.JobAccountReconnect
		}
		if _, err := worker.Enqueue(im.ctx, im.tx, worker.NewJob{Kind: kind, AccountID: &id}); err != nil {
			return err
		}
		im.accounts[normRef(a.AccountRef)] = id
		im.accUser[id] = uid
		im.restore = append(im.restore, id)
		im.st.Accounts++
	}
	// settings.active_account_id of the new users.
	for _, u := range im.doc.users {
		if u.ActiveAccount == nil || !im.newUsers[strings.ToLower(u.Email)] {
			continue
		}
		if id, ok := im.account(*u.ActiveAccount); ok {
			if _, err := im.tx.Exec(im.ctx, `UPDATE users SET settings = settings || jsonb_build_object($2::text, $3::bigint) WHERE id = $1`,
				im.users[strings.ToLower(u.Email)], settingsKeyActiveAccount, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// accountOf resolves the account of a row; false (with a warning) when the
// account is not part of the import.
func (im *importer) accountOf(r AccountRef, what string) (int64, bool) {
	id, ok := im.account(r)
	if !ok {
		im.st.warn("%s of unknown account %s/%s/%s skipped", what, r.User, r.IMAPHost, r.Username)
	}
	return id, ok
}

func (im *importer) importLabels() error {
	ids := map[int64]map[string]int64{} // account -> lower(name) -> id
	for _, l := range im.doc.labels {
		acc, ok := im.accountOf(l.Account, "label "+l.Name)
		if !ok {
			continue
		}
		uid := im.accUser[acc]
		state := l.KeywordState
		if state != "mirrored" {
			state = "local_only"
		}
		show := l.ShowInList
		if show == "" {
			show = "show"
		}
		var id int64
		var typ string
		err := im.tx.QueryRow(im.ctx, `SELECT id, type FROM labels WHERE account_id = $1 AND user_id = $2 AND lower(name) = lower($3)`,
			acc, uid, l.Name).Scan(&id, &typ)
		switch {
		case err == nil && typ != "user":
			im.st.warn("label %q: a %s label of that name exists; skipped", l.Name, typ)
			continue
		case err == nil:
			if _, err := im.tx.Exec(im.ctx, `
UPDATE labels SET color_bg = $3, color_text = $4, show_in_list = $5, position = $6,
  imap_keyword = COALESCE(imap_keyword, $7), keyword_state = CASE WHEN imap_keyword IS NULL AND $7::text IS NOT NULL THEN $8 ELSE keyword_state END
WHERE id = $1 AND user_id = $2`, id, uid, l.ColorBG, l.ColorText, show, l.Position, l.IMAPKeyword, state); err != nil {
				return fmt.Errorf("backup: update label %q: %w", l.Name, err)
			}
		case errors.Is(err, pgx.ErrNoRows):
			kw := l.IMAPKeyword
			if kw != nil {
				var taken bool
				if err := im.tx.QueryRow(im.ctx, `SELECT EXISTS (SELECT 1 FROM labels WHERE account_id = $1 AND user_id = $2 AND lower(imap_keyword) = lower($3))`,
					acc, uid, *kw).Scan(&taken); err != nil {
					return err
				}
				if taken {
					im.st.warn("label %q: keyword %q is used by another label; imported without keyword", l.Name, *kw)
					kw, state = nil, "local_only"
				}
			}
			if err := im.tx.QueryRow(im.ctx, `
INSERT INTO labels (user_id, account_id, name, type, imap_keyword, keyword_state, color_bg, color_text, show_in_list, position)
VALUES ($1, $2, $3, 'user', $4, $5, $6, $7, $8, $9) RETURNING id`,
				uid, acc, l.Name, kw, state, l.ColorBG, l.ColorText, show, l.Position).Scan(&id); err != nil {
				return fmt.Errorf("backup: insert label %q: %w", l.Name, err)
			}
		default:
			return err
		}
		if ids[acc] == nil {
			ids[acc] = map[string]int64{}
		}
		ids[acc][strings.ToLower(l.Name)] = id
		im.st.Labels++
	}
	for _, l := range im.doc.labels {
		if l.Parent == nil {
			continue
		}
		acc, ok := im.account(l.Account)
		if !ok {
			continue
		}
		child, parent := ids[acc][strings.ToLower(l.Name)], ids[acc][strings.ToLower(*l.Parent)]
		if child == 0 || parent == 0 || child == parent {
			continue
		}
		if _, err := im.tx.Exec(im.ctx, `UPDATE labels SET parent_id = $3 WHERE id = $1 AND user_id = $2`, child, im.accUser[acc], parent); err != nil {
			return err
		}
	}
	return nil
}

// restoreChunk is the number of Message-IDs in one staged label row.
const restoreChunk = 1000

func (im *importer) stage(acc int64, kind string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return sqlcgen.New(im.tx).InsertMetadataRestore(im.ctx, sqlcgen.InsertMetadataRestoreParams{
		UserID: im.accUser[acc], AccountID: acc, Kind: kind, Payload: b})
}

func (im *importer) stageAssignments() error {
	type key struct {
		acc   int64
		label string
	}
	groups := map[key][]string{}
	var order []key
	for _, a := range im.doc.labelAssignments {
		acc, ok := im.accountOf(a.Account, "label assignment")
		if !ok {
			continue
		}
		k := key{acc, a.Label}
		if groups[k] == nil {
			order = append(order, k)
		}
		groups[k] = append(groups[k], a.MessageIDHdr)
		im.st.LabelAssignments++
	}
	for _, k := range order {
		ids := groups[k]
		for len(ids) > 0 {
			n := min(len(ids), restoreChunk)
			if err := im.stage(k.acc, restoreLabel, labelPayload{Label: k.label, MessageIDHdrs: ids[:n]}); err != nil {
				return err
			}
			ids = ids[n:]
		}
	}
	return nil
}

func (im *importer) stageSnoozes() error {
	for _, s := range im.doc.snoozes {
		acc, ok := im.accountOf(s.Account, "snooze")
		if !ok || len(s.Items) == 0 {
			continue
		}
		if err := im.stage(acc, restoreSnooze, snoozePayload{WakeAt: s.WakeAt, CreatedAt: s.CreatedAt,
			Items: s.Items, SourceFolders: s.SourceFolders}); err != nil {
			return err
		}
		im.st.Snoozes++
	}
	return nil
}

func (im *importer) importFilters() error {
	for _, f := range im.doc.filters {
		acc, ok := im.accountOf(f.Account, "filter")
		if !ok {
			continue
		}
		actions := f.Actions
		if len(actions) == 0 || string(actions) == "null" {
			actions = json.RawMessage("{}")
		}
		tag, err := im.tx.Exec(im.ctx, `
INSERT INTO filters (user_id, account_id, criteria, actions, position, created_at)
SELECT $1, $2, $3, $4, $5, $6
WHERE NOT EXISTS (SELECT 1 FROM filters WHERE account_id = $2 AND user_id = $1 AND criteria = $3 AND actions = $4::jsonb)`,
			im.accUser[acc], acc, f.Criteria, []byte(actions), f.Position, f.CreatedAt)
		if err != nil {
			return fmt.Errorf("backup: insert filter: %w", err)
		}
		im.st.Filters += int(tag.RowsAffected())
	}
	return nil
}

func (im *importer) importSearchHistory() error {
	for _, s := range im.doc.searchHistory {
		acc, ok := im.accountOf(s.Account, "search history")
		if !ok {
			continue
		}
		if _, err := im.tx.Exec(im.ctx, `
INSERT INTO search_history (user_id, account_id, query, used_at) VALUES ($1, $2, $3, $4)
ON CONFLICT (account_id, query) DO UPDATE SET used_at = GREATEST(search_history.used_at, EXCLUDED.used_at)`,
			im.accUser[acc], acc, s.Query, s.UsedAt); err != nil {
			return fmt.Errorf("backup: insert search history: %w", err)
		}
		im.st.SearchHistory++
	}
	return nil
}

func (im *importer) importContacts() error {
	for _, c := range im.doc.contacts {
		uid, ok, err := im.userID(c.User)
		if err != nil {
			return err
		}
		if !ok {
			im.st.warn("contact %s of unknown user %s skipped", c.Email, c.User)
			continue
		}
		if _, err := im.tx.Exec(im.ctx, `
INSERT INTO contacts (user_id, email, name, sent_count, received_count, last_interaction_at, is_manual)
VALUES ($1, $2, $3, $4, $5, $6, true)
ON CONFLICT (user_id, email) DO UPDATE SET is_manual = true,
  name = CASE WHEN contacts.name = '' THEN EXCLUDED.name ELSE contacts.name END`,
			uid, c.Email, c.Name, c.SentCount, c.ReceivedCount, c.LastInteractionAt); err != nil {
			return fmt.Errorf("backup: insert contact: %w", err)
		}
		im.st.Contacts++
	}
	return nil
}

// useBlob registers one reference to a blob for a draft (kind local) when
// its content is available: imported now, or already in the blob store
// (--no-local-blobs onto the same blob store). It returns false otherwise.
func (im *importer) useBlob(sum string) (bool, error) {
	sum = strings.ToLower(sum)
	ok, seen := im.blobOK[sum]
	if !seen {
		if _, in := im.doc.blobs[sum]; in {
			ok = true
		} else if im.o.Blobs != nil {
			rc, err := im.o.Blobs.Open(im.ctx, sum)
			switch {
			case err == nil:
				_ = rc.Close()
				ok = true
			case !errors.Is(err, blob.ErrNotFound):
				return false, err
			}
		}
		im.blobOK[sum] = ok
	}
	if !ok {
		return false, nil
	}
	size, in := im.doc.blobs[sum]
	if !in {
		rc, err := im.o.Blobs.Open(im.ctx, sum)
		if err != nil {
			return false, err
		}
		size, err = rc.Seek(0, io.SeekEnd)
		_ = rc.Close()
		if err != nil {
			return false, err
		}
	}
	sha, err := hex.DecodeString(sum)
	if err != nil {
		return false, err
	}
	_, err = im.tx.Exec(im.ctx, `
INSERT INTO blobs (sha256, size_bytes, kind, ref_count) VALUES ($1, $2, 'local', 1)
ON CONFLICT (sha256) DO UPDATE SET ref_count = blobs.ref_count + 1, kind = 'local'`, sha, size)
	return err == nil, err
}

func addrs(raw json.RawMessage) []mail.Address {
	var out []mail.Address
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = []mail.Address{}
	}
	return out
}

func (im *importer) importDrafts() error {
	for _, d := range im.doc.drafts {
		acc, ok := im.accountOf(d.Account, "draft")
		if !ok {
			continue
		}
		uid := im.accUser[acc]
		var exists bool
		if err := im.tx.QueryRow(im.ctx, `SELECT EXISTS (SELECT 1 FROM drafts WHERE silo_draft_id = $1)`, d.SiloDraftID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue // --merge: the draft is here already
		}
		if err := mail.LockAccount(im.ctx, im.tx, acc); err != nil {
			return err
		}
		// A reply joins the thread of its original when that is mirrored
		// already (merge); otherwise threading joins them when it arrives.
		var replyTo *int64
		var threadID int64
		if d.ReplyToMessageIDHdr != nil {
			var id, th int64
			err := im.tx.QueryRow(im.ctx, `SELECT id, thread_id FROM messages WHERE account_id = $1 AND user_id = $2 AND message_id_hdr = $3 ORDER BY id LIMIT 1`,
				acc, uid, *d.ReplyToMessageIDHdr).Scan(&id, &th)
			if err == nil {
				replyTo, threadID = &id, th
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		hasAtt := false
		for _, a := range d.Attachments {
			hasAtt = hasAtt || !a.IsInline
		}
		inReplyTo := ""
		if d.InReplyTo != nil {
			inReplyTo = *d.InReplyTo
		}
		msgID, _, err := mail.CreateDraftMessage(im.ctx, im.tx, mail.NewDraft{
			AccountID: acc, UserID: uid, ThreadID: threadID, MessageIDHdr: d.MessageIDHdr, SiloDraftID: d.SiloDraftID,
			InReplyTo: inReplyTo, Refs: d.References,
			DraftFields: mail.DraftFields{From: mail.Address{Email: d.FromEmail, Name: d.FromName},
				To: addrs(d.To), Cc: addrs(d.Cc), Bcc: addrs(d.Bcc), Subject: d.Subject, BodyHTML: d.BodyHTML, HasAttachments: hasAtt},
		})
		if err != nil {
			return fmt.Errorf("backup: draft %s: %w", d.SiloDraftID, err)
		}
		if _, err := im.tx.Exec(im.ctx, `UPDATE messages SET internal_date = $3 WHERE id = $1 AND user_id = $2`, msgID, uid, d.InternalDate); err != nil {
			return err
		}
		if d.RawSHA256 != nil {
			ok, err := im.useBlob(*d.RawSHA256)
			if err != nil {
				return err
			}
			if ok {
				sha, _ := hex.DecodeString(*d.RawSHA256)
				if _, err := im.tx.Exec(im.ctx, `UPDATE messages SET raw_blob_sha256 = $3, size_bytes = COALESCE((SELECT size_bytes FROM blobs WHERE sha256 = $3), size_bytes)
WHERE id = $1 AND user_id = $2`, msgID, uid, sha); err != nil {
					return err
				}
			} else {
				im.st.warn("draft %s: stored message %s is not available", d.SiloDraftID, *d.RawSHA256)
			}
		}
		listed := []int64{}
		for _, a := range d.Attachments {
			var sha []byte
			if a.SHA256 != nil {
				ok, err := im.useBlob(*a.SHA256)
				if err != nil {
					return err
				}
				if ok {
					sha, _ = hex.DecodeString(*a.SHA256)
				} else {
					im.st.warn("draft %s: attachment %q (%s) has no content", d.SiloDraftID, a.Filename, *a.SHA256)
				}
			}
			var id int64
			if err := im.tx.QueryRow(im.ctx, `
INSERT INTO attachments (message_id, user_id, blob_sha256, filename, content_type, size_bytes, content_id, is_inline, part_path)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`, msgID, uid, sha, a.Filename, a.ContentType, a.Size,
				a.ContentID, a.IsInline, a.PartPath).Scan(&id); err != nil {
				return fmt.Errorf("backup: draft attachment: %w", err)
			}
			if a.Listed && sha != nil {
				listed = append(listed, id)
			}
		}
		var bodyJSON []byte
		if len(d.BodyJSON) > 0 && string(d.BodyJSON) != "null" {
			bodyJSON = d.BodyJSON
		}
		if _, err := im.tx.Exec(im.ctx, `
INSERT INTO drafts (message_id, user_id, account_id, to_addrs, cc_addrs, bcc_addrs, subject, body_html, body_json,
  in_reply_to_message_id, attachment_ids, version, updated_at, server_synced_version, silo_draft_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
			msgID, uid, acc, rawOr(d.To, "[]"), rawOr(d.Cc, "[]"), rawOr(d.Bcc, "[]"), d.Subject, d.BodyHTML, bodyJSON,
			replyTo, listed, max(d.Version, 1), d.UpdatedAt, d.ServerSyncedVersion, d.SiloDraftID); err != nil {
			return fmt.Errorf("backup: insert draft %s: %w", d.SiloDraftID, err)
		}
		im.drafts[d.SiloDraftID] = msgID
		im.refresh[acc] = append(im.refresh[acc], msgID)
		im.st.Drafts++
	}
	return nil
}

func rawOr(r json.RawMessage, def string) []byte {
	if len(r) == 0 || string(r) == "null" {
		return []byte(def)
	}
	return r
}

// importOutbound queues the messages that were waiting to be sent again, as
// drafts.Send does: an outbound row (queued) and a send job at send_at (now
// when that has passed).
func (im *importer) importOutbound() error {
	now := im.o.Now()
	for _, o := range im.doc.outbound {
		acc, ok := im.accountOf(o.Account, "queued message")
		if !ok {
			continue
		}
		msgID, ok := im.drafts[o.SiloDraftID]
		if !ok {
			continue // not imported now (merge) or unknown
		}
		var hasRaw bool
		if err := im.tx.QueryRow(im.ctx, `SELECT raw_blob_sha256 IS NOT NULL FROM messages WHERE id = $1`, msgID).Scan(&hasRaw); err != nil {
			return err
		}
		if !hasRaw {
			im.st.warn("queued message %s has no stored content; restored as a draft", o.SiloDraftID)
			continue
		}
		var id int64
		if err := im.tx.QueryRow(im.ctx, `
INSERT INTO outbound (user_id, account_id, message_id, status, send_at, attempts)
VALUES ($1, $2, $3, 'queued', $4, $5) RETURNING id`, im.accUser[acc], acc, msgID, o.SendAt, o.Attempts).Scan(&id); err != nil {
			return fmt.Errorf("backup: insert outbound: %w", err)
		}
		runAt := o.SendAt
		if runAt.Before(now) {
			runAt = now
		}
		if _, err := worker.Enqueue(im.ctx, im.tx, worker.NewJob{Kind: drafts.JobSend, Payload: map[string]int64{"outboundId": id}, RunAt: runAt}); err != nil {
			return err
		}
		im.st.Outbound++
	}
	return nil
}

// finish derives the labels of the imported local messages (DRAFT,
// SCHEDULED) and queues the metadata_restore job of every imported account.
func (im *importer) finish() error {
	for acc, ids := range im.refresh {
		if _, err := mail.RefreshMessages(im.ctx, im.tx, acc, ids, "import"); err != nil {
			return err
		}
	}
	for _, acc := range im.restore {
		if err := EnqueueRestore(im.ctx, im.tx, acc, time.Time{}); err != nil {
			return err
		}
	}
	return nil
}
