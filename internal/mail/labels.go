package mail

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// System label keys (plan 5.5). SCHEDULED is local (never derived here).
const (
	LabelInbox     = "INBOX"
	LabelSent      = "SENT"
	LabelDraft     = "DRAFT"
	LabelTrash     = "TRASH"
	LabelSpam      = "SPAM"
	LabelArchive   = "ARCHIVE"
	LabelSnoozed   = "SNOOZED"
	LabelStarred   = "STARRED"
	LabelImportant = "IMPORTANT"
	LabelScheduled = "SCHEDULED"
)

// SystemLabelKeys lists every system label created by EnsureSystemLabels.
var SystemLabelKeys = []string{
	LabelInbox, LabelSent, LabelDraft, LabelTrash, LabelSpam, LabelArchive,
	LabelSnoozed, LabelStarred, LabelImportant, LabelScheduled,
}

// roleLabel maps mailbox_folders.role to the system label of messages located
// in such a folder (plan 4.4). Role "all" has no label, "user" gets a folder label.
var roleLabel = map[string]string{
	"inbox": LabelInbox, "sent": LabelSent, "drafts": LabelDraft, "trash": LabelTrash,
	"junk": LabelSpam, "archive": LabelArchive, "snoozed": LabelSnoozed,
}

// EnsureSystemLabels creates the system labels of an account that do not
// exist yet. Idempotent; the sync engine calls it once the account exists
// (before the first ingest), DeriveSystemLabels calls it lazily as a safety net.
func EnsureSystemLabels(ctx context.Context, db DB, accountID int64) error {
	_, err := db.Exec(ctx, `
INSERT INTO labels (user_id, account_id, name, type, system_key, position)
SELECT a.user_id, a.id, k.key, 'system', k.key, k.pos
FROM accounts a, unnest($2::text[]) WITH ORDINALITY AS k(key, pos)
WHERE a.id = $1
ON CONFLICT DO NOTHING`, accountID, SystemLabelKeys)
	if err != nil {
		return fmt.Errorf("mail: ensure system labels: %w", err)
	}
	return nil
}

// EnsureFolderLabel returns the label of type folder for a mailbox_folders
// row, creating it (and linking mailbox_folders.label_id) when missing. The
// label is named after the folder; on a name clash with another label it gets
// a " (folder)" suffix.
func EnsureFolderLabel(ctx context.Context, db DB, folderID int64) (int64, error) {
	var labelID *int64
	var name string
	var accountID, userID int64
	err := db.QueryRow(ctx, `SELECT label_id, name, account_id, user_id FROM mailbox_folders WHERE id = $1`, folderID).
		Scan(&labelID, &name, &accountID, &userID)
	if err != nil {
		return 0, fmt.Errorf("mail: load folder %d: %w", folderID, err)
	}
	if labelID != nil {
		return *labelID, nil
	}
	for _, n := range []string{name, name + " (folder)", name + " (folder 2)", name + " (folder 3)"} {
		var id int64
		err := db.QueryRow(ctx, `
INSERT INTO labels (user_id, account_id, name, type, folder_id)
VALUES ($1, $2, $3, 'folder', $4) ON CONFLICT DO NOTHING RETURNING id`, userID, accountID, n, folderID).Scan(&id)
		if err == pgx.ErrNoRows {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("mail: create folder label: %w", err)
		}
		if _, err := db.Exec(ctx, `UPDATE mailbox_folders SET label_id = $1 WHERE id = $2`, id, folderID); err != nil {
			return 0, fmt.Errorf("mail: link folder label: %w", err)
		}
		return id, nil
	}
	return 0, fmt.Errorf("mail: cannot find a free label name for folder %q", name)
}

// DeriveSystemLabels recomputes the system and folder labels, the keyword
// derived user labels and is_unread of one message from its message_locations
// (folder roles and flags) and writes only the difference (plan 4.4). It does
// not recompute the thread aggregates; call RecomputeThread afterwards.
//
// A message without locations (orphaned) loses its system/folder labels and
// keeps its user labels. Overlaying pending write-back operations belongs
// here as well, once the ops package exists.
func DeriveSystemLabels(ctx context.Context, db DB, messageID int64) error {
	return DeriveSystemLabelsBatch(ctx, db, []int64{messageID})
}

// DeriveSystemLabelsBatch is DeriveSystemLabels for many messages with a
// constant number of queries. The messages may belong to different accounts.
func DeriveSystemLabelsBatch(ctx context.Context, db DB, messageIDs []int64) error {
	_, err := deriveLabels(ctx, db, messageIDs, true)
	return err
}

type loc struct {
	folderID int64
	role     string
	labelID  *int64
	flags    []string
}

type msgState struct {
	id, accountID, userID int64
	isUnread              bool
	locs                  []loc
}

type labelIndex struct {
	accountID int64
	userID    int64
	prefix    string
	system    map[string]int64 // system_key -> id
	keyword   map[string]kwLabel
}

type kwLabel struct {
	id       int64
	mirrored bool
}

// desired is the derived state of one message.
type desired struct {
	labels   map[int64]bool // system + folder + keyword user labels that should exist
	isUnread bool
}

func loadStates(ctx context.Context, db DB, ids []int64) ([]*msgState, error) {
	rows, err := db.Query(ctx, `
SELECT m.id, m.account_id, m.user_id, m.is_unread,
       l.folder_id, f.role, f.label_id, l.flags
FROM messages m
LEFT JOIN message_locations l ON l.message_id = m.id
LEFT JOIN mailbox_folders f ON f.id = l.folder_id
WHERE m.id = ANY($1)
ORDER BY m.id, l.folder_id, l.uid`, ids)
	if err != nil {
		return nil, fmt.Errorf("mail: load message locations: %w", err)
	}
	defer rows.Close()
	var out []*msgState
	byID := map[int64]*msgState{}
	for rows.Next() {
		var id, acc, user int64
		var unread bool
		var folderID *int64
		var role *string
		var labelID *int64
		var flags []string
		if err := rows.Scan(&id, &acc, &user, &unread, &folderID, &role, &labelID, &flags); err != nil {
			return nil, err
		}
		st := byID[id]
		if st == nil {
			st = &msgState{id: id, accountID: acc, userID: user, isUnread: unread}
			byID[id] = st
			out = append(out, st)
		}
		if folderID != nil {
			st.locs = append(st.locs, loc{folderID: *folderID, role: *role, labelID: labelID, flags: flags})
		}
	}
	return out, rows.Err()
}

func hasFlag(flags []string, want string) bool {
	return slices.ContainsFunc(flags, func(f string) bool { return strings.EqualFold(f, want) })
}

// loadLabelIndex loads system and keyword labels of an account. With create
// set, missing system labels are created.
func loadLabelIndex(ctx context.Context, db DB, accountID int64, create bool) (*labelIndex, error) {
	idx := &labelIndex{accountID: accountID, system: map[string]int64{}, keyword: map[string]kwLabel{}}
	if err := db.QueryRow(ctx, `SELECT user_id, keyword_prefix FROM accounts WHERE id = $1`, accountID).Scan(&idx.userID, &idx.prefix); err != nil {
		return nil, fmt.Errorf("mail: load account %d: %w", accountID, err)
	}
	load := func() error {
		rows, err := db.Query(ctx, `
SELECT id, system_key, imap_keyword, keyword_state FROM labels
WHERE account_id = $1 AND (type = 'system' OR imap_keyword IS NOT NULL)`, accountID)
		if err != nil {
			return fmt.Errorf("mail: load labels: %w", err)
		}
		defer rows.Close()
		clear(idx.system)
		clear(idx.keyword)
		for rows.Next() {
			var id int64
			var key, kw *string
			var state string
			if err := rows.Scan(&id, &key, &kw, &state); err != nil {
				return err
			}
			if key != nil {
				idx.system[*key] = id
			}
			if kw != nil {
				idx.keyword[strings.ToLower(*kw)] = kwLabel{id: id, mirrored: state == "mirrored"}
			}
		}
		return rows.Err()
	}
	if err := load(); err != nil {
		return nil, err
	}
	if create && len(idx.system) < len(SystemLabelKeys) {
		if err := EnsureSystemLabels(ctx, db, accountID); err != nil {
			return nil, err
		}
		if err := load(); err != nil {
			return nil, err
		}
	}
	return idx, nil
}

// importKeyword returns the user label for an IMAP keyword seen on the server,
// creating it (plan 3.7) when unknown. A local label with the same name and no
// keyword yet adopts the keyword instead of creating a duplicate.
func importKeyword(ctx context.Context, db DB, idx *labelIndex, kw string) (int64, error) {
	name := kw
	if n, ok := thunderbirdLabels[strings.ToLower(kw)]; ok {
		name = n
	} else if idx.prefix != "" && len(kw) > len(idx.prefix) && strings.EqualFold(kw[:len(idx.prefix)], idx.prefix) {
		name = kw[len(idx.prefix):]
	}
	var id int64
	err := db.QueryRow(ctx, `
UPDATE labels SET imap_keyword = $3, keyword_state = 'mirrored'
WHERE id = (SELECT id FROM labels WHERE account_id = $1 AND type = 'user' AND imap_keyword IS NULL AND lower(name) = lower($2))
RETURNING id`, idx.accountID, name, kw).Scan(&id)
	if err == nil {
		idx.keyword[strings.ToLower(kw)] = kwLabel{id: id, mirrored: true}
		return id, nil
	}
	if err != pgx.ErrNoRows {
		return 0, fmt.Errorf("mail: adopt keyword label: %w", err)
	}
	for i := 1; i <= 20; i++ {
		n := name
		if i > 1 {
			n = fmt.Sprintf("%s (%d)", name, i)
		}
		err := db.QueryRow(ctx, `
INSERT INTO labels (user_id, account_id, name, type, imap_keyword, keyword_state)
VALUES ($1, $2, $3, 'user', $4, 'mirrored') ON CONFLICT DO NOTHING RETURNING id`, idx.userID, idx.accountID, n, kw).Scan(&id)
		if err == nil {
			idx.keyword[strings.ToLower(kw)] = kwLabel{id: id, mirrored: true}
			return id, nil
		}
		if err != pgx.ErrNoRows {
			return 0, fmt.Errorf("mail: create keyword label: %w", err)
		}
		// Conflict: either the keyword is taken (other case) or the name is.
		var existing int64
		e2 := db.QueryRow(ctx, `SELECT id FROM labels WHERE account_id = $1 AND lower(imap_keyword) = lower($2)`, idx.accountID, kw).Scan(&existing)
		if e2 == nil {
			idx.keyword[strings.ToLower(kw)] = kwLabel{id: existing, mirrored: true}
			return existing, nil
		}
	}
	return 0, fmt.Errorf("mail: cannot create label for keyword %q", kw)
}

// compute derives the label set of one message. keywordLabel resolves keyword
// labels (false = ignore the keyword).
func (idx *labelIndex) compute(st *msgState, keywordLabel func(kw string) (int64, bool)) desired {
	d := desired{labels: map[int64]bool{}, isUnread: st.isUnread}
	if len(st.locs) == 0 {
		return d
	}
	d.isUnread = false
	add := func(key string) {
		if id, ok := idx.system[key]; ok {
			d.labels[id] = true
		}
	}
	for _, l := range st.locs {
		if key, ok := roleLabel[l.role]; ok {
			add(key)
		} else if l.role == "user" && l.labelID != nil {
			d.labels[*l.labelID] = true
		}
		if hasFlag(l.flags, "\\Draft") {
			add(LabelDraft)
		}
		if hasFlag(l.flags, "\\Flagged") {
			add(LabelStarred)
		}
		if hasFlag(l.flags, "$Important") {
			add(LabelImportant)
		}
		if !hasFlag(l.flags, "\\Seen") {
			d.isUnread = true
		}
		for _, f := range l.flags {
			if IsLabelKeyword(f) {
				if id, ok := keywordLabel(f); ok {
					d.labels[id] = true
				}
			}
		}
	}
	return d
}

type labelMeta struct {
	typ      string
	key      string
	keyword  bool
	mirrored bool
}

func loadLabelMeta(ctx context.Context, db DB, accountIDs []int64) (map[int64]labelMeta, error) {
	rows, err := db.Query(ctx, `
SELECT id, type, COALESCE(system_key, ''), imap_keyword IS NOT NULL, keyword_state = 'mirrored'
FROM labels WHERE account_id = ANY($1)`, accountIDs)
	if err != nil {
		return nil, fmt.Errorf("mail: load label meta: %w", err)
	}
	defer rows.Close()
	out := map[int64]labelMeta{}
	for rows.Next() {
		var id int64
		var m labelMeta
		if err := rows.Scan(&id, &m.typ, &m.key, &m.keyword, &m.mirrored); err != nil {
			return nil, err
		}
		out[id] = m
	}
	return out, rows.Err()
}

// isManaged reports whether a message_labels row is owned by the derivation:
// system labels (except SCHEDULED), folder labels, and user labels that mirror
// an IMAP keyword. Plain local user labels are never touched.
func (m labelMeta) isManaged() bool {
	switch m.typ {
	case "system":
		return m.key != LabelScheduled
	case "folder":
		return true
	default:
		return m.keyword && m.mirrored
	}
}

// labelPlan is the difference between stored and derived state.
type labelPlan struct {
	addMsg, addLabel, addUser []int64
	delMsg, delLabel          []int64
	unreadMsg                 []int64
	unreadVal                 []bool
}

// deriveLabels computes the plan and, with apply set, writes it (creating
// missing system/folder/keyword labels). Without apply it only reads.
func deriveLabels(ctx context.Context, db DB, ids []int64, apply bool) (*labelPlan, error) {
	plan := &labelPlan{}
	if len(ids) == 0 {
		return plan, nil
	}
	states, err := loadStates(ctx, db, ids)
	if err != nil {
		return nil, err
	}
	accIdx := map[int64]*labelIndex{}
	var accountIDs []int64
	folderLabel := map[int64]int64{}
	for _, st := range states {
		if _, ok := accIdx[st.accountID]; !ok {
			idx, err := loadLabelIndex(ctx, db, st.accountID, apply)
			if err != nil {
				return nil, err
			}
			accIdx[st.accountID] = idx
			accountIDs = append(accountIDs, st.accountID)
		}
		for i := range st.locs {
			l := &st.locs[i]
			if l.role != "user" {
				continue
			}
			if l.labelID == nil && apply {
				id, ok := folderLabel[l.folderID]
				if !ok {
					if id, err = EnsureFolderLabel(ctx, db, l.folderID); err != nil {
						return nil, err
					}
					folderLabel[l.folderID] = id
				}
				l.labelID = &id
			}
		}
	}
	meta, err := loadLabelMeta(ctx, db, accountIDs)
	if err != nil {
		return nil, err
	}
	existing := map[int64]map[int64]bool{}
	rows, err := db.Query(ctx, `SELECT message_id, label_id FROM message_labels WHERE message_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("mail: load message labels: %w", err)
	}
	for rows.Next() {
		var m, l int64
		if err := rows.Scan(&m, &l); err != nil {
			rows.Close()
			return nil, err
		}
		if existing[m] == nil {
			existing[m] = map[int64]bool{}
		}
		existing[m][l] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, st := range states {
		idx := accIdx[st.accountID]
		var kwErr error
		d := idx.compute(st, func(kw string) (int64, bool) {
			if l, ok := idx.keyword[strings.ToLower(kw)]; ok {
				return l.id, true
			}
			if !apply {
				return 0, false
			}
			id, err := importKeyword(ctx, db, idx, kw)
			if err != nil {
				kwErr = err
				return 0, false
			}
			meta[id] = labelMeta{typ: "user", keyword: true, mirrored: true}
			return id, true
		})
		if kwErr != nil {
			return nil, kwErr
		}
		have := existing[st.id]
		for id := range d.labels {
			if !have[id] {
				plan.addMsg, plan.addLabel, plan.addUser = append(plan.addMsg, st.id), append(plan.addLabel, id), append(plan.addUser, st.userID)
			}
		}
		for id := range have {
			m := meta[id]
			if !m.isManaged() || d.labels[id] {
				continue
			}
			// A keyword-mirrored user label is only removed while the message
			// is on the server (otherwise there is no flag to judge by).
			if m.typ == "user" && len(st.locs) == 0 {
				continue
			}
			plan.delMsg, plan.delLabel = append(plan.delMsg, st.id), append(plan.delLabel, id)
		}
		if d.isUnread != st.isUnread {
			plan.unreadMsg, plan.unreadVal = append(plan.unreadMsg, st.id), append(plan.unreadVal, d.isUnread)
		}
	}
	if !apply {
		return plan, nil
	}
	if len(plan.addMsg) > 0 {
		if _, err := db.Exec(ctx, `
INSERT INTO message_labels (message_id, label_id, user_id)
SELECT * FROM unnest($1::bigint[], $2::bigint[], $3::bigint[]) ON CONFLICT DO NOTHING`,
			plan.addMsg, plan.addLabel, plan.addUser); err != nil {
			return nil, fmt.Errorf("mail: add message labels: %w", err)
		}
	}
	if len(plan.delMsg) > 0 {
		if _, err := db.Exec(ctx, `
DELETE FROM message_labels ml USING unnest($1::bigint[], $2::bigint[]) AS d(m, l)
WHERE ml.message_id = d.m AND ml.label_id = d.l`, plan.delMsg, plan.delLabel); err != nil {
			return nil, fmt.Errorf("mail: remove message labels: %w", err)
		}
	}
	if len(plan.unreadMsg) > 0 {
		if _, err := db.Exec(ctx, `
UPDATE messages m SET is_unread = v.u FROM unnest($1::bigint[], $2::bool[]) AS v(id, u)
WHERE m.id = v.id AND m.is_unread IS DISTINCT FROM v.u`, plan.unreadMsg, plan.unreadVal); err != nil {
			return nil, fmt.Errorf("mail: update is_unread: %w", err)
		}
	}
	return plan, nil
}
