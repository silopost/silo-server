package ops

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
)

// Modify is a label change in the Gmail model on threads or on messages.
type Modify struct {
	UserID     int64
	ThreadIDs  []int64 // either threads ...
	MessageIDs []int64 // ... or messages
	Add        []LabelRef
	Remove     []LabelRef
	Origin     string // X-Silo-Client, echoed in the events
	// Immediate runs folder moves at once (no undo window) and records no
	// undoable operation (OperationID stays empty): the actions of filters
	// (plan 3.11). A job given up still writes operation.failed.
	Immediate bool
}

type location struct {
	folderID int64
	role     string
	flags    []string
}

type msgInfo struct {
	id, accountID, threadID int64
	locs                    []location
	restore                 *int64
}

type labelInfo struct {
	id        int64
	accountID int64
	typ       string
	key       string
	folderID  *int64
	keyword   *string
	mirrored  bool
	name      string
}

// opBuild collects the operations of one account.
type opBuild struct {
	payload mail.OpPayload
	items   []item
	delay   time.Duration
}

// ModifyLabels applies a label change (plan 4.4 "ModifyLabels = tłumaczenie
// na operacje") in one transaction: user labels are written at once, every
// other change becomes an imap_op job (folder moves after the undo window,
// flags at once) whose effect is visible immediately through the overlay of
// mail.DeriveSystemLabels; threads are recomputed and thread.updated
// (pendingSync) / label.counts written.
func (s *Service) ModifyLabels(ctx context.Context, m Modify) (*Result, error) {
	for _, a := range m.Add {
		if slices.Contains(m.Remove, a) {
			return nil, fmt.Errorf("%w: label both added and removed", ErrInvalid)
		}
	}
	return s.run(ctx, m.UserID, m.ThreadIDs, m.MessageIDs, m.Origin, func(ctx context.Context, tx pgx.Tx, b *builder) error {
		b.immediate, b.noUndo = m.Immediate, b.noUndo || m.Immediate
		return b.modify(ctx, tx, m.Add, m.Remove)
	})
}

// Trash is ModifyLabels +TRASH.
func (s *Service) Trash(ctx context.Context, userID int64, threadIDs []int64, origin string) (*Result, error) {
	return s.ModifyLabels(ctx, Modify{UserID: userID, ThreadIDs: threadIDs, Add: []LabelRef{{System: mail.LabelTrash}}, Origin: origin})
}

// Untrash is ModifyLabels -TRASH.
func (s *Service) Untrash(ctx context.Context, userID int64, threadIDs []int64, origin string) (*Result, error) {
	return s.ModifyLabels(ctx, Modify{UserID: userID, ThreadIDs: threadIDs, Remove: []LabelRef{{System: mail.LabelTrash}}, Origin: origin})
}

// Delete removes the messages of the threads that are in Trash or Spam for
// good (\Deleted + UID EXPUNGE when the job runs). Threads without such
// messages are refused.
func (s *Service) Delete(ctx context.Context, userID int64, threadIDs []int64, origin string) (*Result, error) {
	return s.run(ctx, userID, threadIDs, nil, origin, func(_ context.Context, _ pgx.Tx, b *builder) error {
		for _, msg := range b.msgs {
			for _, l := range msg.locs {
				if l.role == "trash" || l.role == "junk" {
					b.final = true
					b.add(mail.OpPayload{Op: mail.OpExpunge}, msg.id, l.folderID, 0)
				}
			}
		}
		if len(b.ops) == 0 {
			return fmt.Errorf("%w: only messages in Trash or Spam can be deleted for good", ErrInvalid)
		}
		return nil
	})
}

// builder is the per-account state of one request.
type builder struct {
	s          *Service
	accountID  int64
	userID     int64
	keywords   bool
	msgs       []*msgInfo
	byRole     map[string]int64 // role -> folder id
	folderRole map[int64]string
	ops        map[string]*opBuild
	local      []int64   // messages whose user labels changed locally
	inv        []invItem // how to undo the changes once the jobs ran
	final      bool      // the operation cannot be undone (permanent delete)
	// record keeps the operation (and its undo window) even when no job was
	// queued: a local change such as a new snooze time is undoable too.
	record bool
	// noUndo: the change is not an undoable operation (waking a snoozed
	// thread); no operations row is written and OperationID stays empty.
	noUndo bool
	// immediate: folder moves without the undo window (filters).
	immediate bool
}

func (b *builder) add(p mail.OpPayload, msgID, from int64, delay time.Duration) {
	p = canonical(p)
	key := string(payloadJSON(p))
	ob := b.ops[key]
	if ob == nil {
		ob = &opBuild{payload: p, delay: delay}
		b.ops[key] = ob
	}
	f := from
	var expect *int64
	if f != 0 {
		expect = &f
	}
	for _, it := range ob.items {
		if it.messageID == msgID {
			return
		}
	}
	ob.items = append(ob.items, item{messageID: msgID, expect: expect})
}

// run loads the messages, groups them by account and calls fn per account,
// then enqueues the collected operations and refreshes the messages.
func (s *Service) run(ctx context.Context, userID int64, threadIDs, msgIDs []int64, origin string, fn func(context.Context, pgx.Tx, *builder) error) (*Result, error) {
	res := &Result{}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var group string
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&group); err != nil {
			return err
		}
		msgs, err := loadMessages(ctx, tx, userID, threadIDs, msgIDs)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return ErrNotFound
		}
		byAccount := map[int64][]*msgInfo{}
		var accounts []int64
		for _, m := range msgs {
			if byAccount[m.accountID] == nil {
				accounts = append(accounts, m.accountID)
			}
			byAccount[m.accountID] = append(byAccount[m.accountID], m)
		}
		slices.Sort(accounts)
		sentAny, folderOp, final, record, noUndo := false, false, false, false, false
		var inv []invItem
		for _, acc := range accounts {
			if err := mail.LockAccount(ctx, tx, acc); err != nil {
				return err
			}
			b := &builder{s: s, accountID: acc, userID: userID, msgs: byAccount[acc], ops: map[string]*opBuild{}}
			if err := b.load(ctx, tx); err != nil {
				return err
			}
			if err := fn(ctx, tx, b); err != nil {
				return err
			}
			inv, final = append(inv, b.inv...), final || b.final
			record, noUndo = record || b.record, noUndo || b.noUndo
			touched := slices.Clone(b.local)
			keys := make([]string, 0, len(b.ops))
			for k := range b.ops {
				keys = append(keys, k)
			}
			slices.Sort(keys) // deterministic job order
			for _, k := range keys {
				ob := b.ops[k]
				jobID, t, err := enqueue(ctx, tx, acc, userID, group, ob.payload, ob.items, ob.delay, b.folderRole)
				if err != nil {
					return err
				}
				touched = append(touched, t...)
				if jobID != 0 {
					sentAny = true
					folderOp = folderOp || ob.delay > 0
				}
			}
			for _, m := range b.msgs {
				touched = append(touched, m.id)
			}
			threads, err := mail.RefreshMessages(ctx, tx, acc, touched, origin)
			if err != nil {
				return err
			}
			res.ThreadIDs = append(res.ThreadIDs, threads...)
		}
		if (sentAny || record) && !noUndo {
			res.OperationID = group
			if folderOp || record {
				until := time.Now().Add(s.undoWindow)
				res.UndoUntil = &until
			}
			if err := saveOperation(ctx, tx, group, userID, res.UndoUntil, inv, final); err != nil {
				return err
			}
		}
		res.ThreadIDs = uniq(res.ThreadIDs)
		res.Threads, err = LoadThreads(ctx, tx, userID, res.ThreadIDs)
		return err
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func uniq(ids []int64) []int64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// loadMessages loads the messages with their locations. A copy flagged
// \Deleted is already gone for Silo (ADR 0006: without UIDPLUS a move leaves
// the source copy flagged), as in the label derivation and the executor.
func loadMessages(ctx context.Context, tx pgx.Tx, userID int64, threadIDs, msgIDs []int64) ([]*msgInfo, error) {
	q := `
SELECT m.id, m.account_id, m.thread_id, m.restore_folder_id, l.folder_id, f.role, l.flags
FROM messages m
LEFT JOIN message_locations l ON l.message_id = m.id AND NOT l.stale AND NOT ('\Deleted' = ANY(l.flags))
LEFT JOIN mailbox_folders f ON f.id = l.folder_id
WHERE m.user_id = $1 AND m.orphaned_at IS NULL AND `
	var arg []int64
	if len(threadIDs) > 0 {
		q, arg = q+`m.thread_id = ANY($2)`, threadIDs
	} else {
		q, arg = q+`m.id = ANY($2)`, msgIDs
	}
	rows, err := tx.Query(ctx, q+` ORDER BY m.id, l.folder_id`, userID, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*msgInfo
	byID := map[int64]*msgInfo{}
	for rows.Next() {
		var id, acc, thread int64
		var restore, folder *int64
		var role *string
		var flags []string
		if err := rows.Scan(&id, &acc, &thread, &restore, &folder, &role, &flags); err != nil {
			return nil, err
		}
		m := byID[id]
		if m == nil {
			m = &msgInfo{id: id, accountID: acc, threadID: thread, restore: restore}
			byID[id] = m
			out = append(out, m)
		}
		if folder != nil {
			m.locs = append(m.locs, location{folderID: *folder, role: *role, flags: flags})
		}
	}
	return out, rows.Err()
}

func (b *builder) load(ctx context.Context, tx pgx.Tx) error {
	if err := tx.QueryRow(ctx, `SELECT keywords_supported FROM accounts WHERE id = $1`, b.accountID).Scan(&b.keywords); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id, role FROM mailbox_folders WHERE account_id = $1 AND selectable AND sync_mode = 'sync' ORDER BY id`, b.accountID)
	if err != nil {
		return err
	}
	defer rows.Close()
	b.byRole, b.folderRole = map[string]int64{}, map[int64]string{}
	for rows.Next() {
		var id int64
		var role string
		if err := rows.Scan(&id, &role); err != nil {
			return err
		}
		b.folderRole[id] = role
		if _, ok := b.byRole[role]; !ok && role != "user" {
			b.byRole[role] = id
		}
	}
	return rows.Err()
}

func (b *builder) loadLabels(ctx context.Context, tx pgx.Tx, refs []LabelRef) (map[int64]*labelInfo, error) {
	var ids []int64
	for _, r := range refs {
		if r.ID != 0 {
			ids = append(ids, r.ID)
		}
	}
	out := map[int64]*labelInfo{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `
SELECT id, account_id, type, COALESCE(system_key, ''), folder_id, imap_keyword, keyword_state = 'mirrored', name
FROM labels WHERE id = ANY($1) AND user_id = $2`, ids, b.userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		l := &labelInfo{}
		if err := rows.Scan(&l.id, &l.accountID, &l.typ, &l.key, &l.folderID, &l.keyword, &l.mirrored, &l.name); err != nil {
			return nil, err
		}
		out[l.id] = l
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if out[id] == nil {
			return nil, fmt.Errorf("%w: unknown label %d", ErrInvalid, id)
		}
	}
	return out, nil
}

// in returns the first location of m with one of the roles.
func in(m *msgInfo, roles ...string) (location, bool) {
	for _, l := range m.locs {
		if slices.Contains(roles, l.role) {
			return l, true
		}
	}
	return location{}, false
}

// modify translates the label changes into operations (plan 4.4 table).
func (b *builder) modify(ctx context.Context, tx pgx.Tx, add, remove []LabelRef) error {
	labels, err := b.loadLabels(ctx, tx, append(slices.Clone(add), remove...))
	if err != nil {
		return err
	}
	window := b.s.undoWindow
	if b.immediate {
		window = 0
	}
	move := func(m *msgInfo, from location, role string, folderID int64, extraAdd, extraRemove []string) {
		b.add(mail.OpPayload{Op: mail.OpMove, ToRole: role, ToFolderID: folderID, Add: extraAdd, Remove: extraRemove}, m.id, from.folderID, window)
		b.inv = append(b.inv, invItem{MessageID: m.id, MoveTo: from.folderID, ExpectFolder: folderID, ExpectRole: role,
			Add: changedFlags(m, extraRemove, false), Remove: changedFlags(m, extraAdd, true)})
	}
	flags := func(addF, removeF []string) {
		for _, m := range b.msgs {
			if len(m.locs) > 0 {
				b.add(mail.OpPayload{Op: mail.OpFlags, Add: addF, Remove: removeF}, m.id, 0, 0)
				if it := (invItem{MessageID: m.id, Add: changedFlags(m, removeF, false), Remove: changedFlags(m, addF, true)}); len(it.Add)+len(it.Remove) > 0 {
					b.inv = append(b.inv, it)
				}
			}
		}
	}
	apply := func(ref LabelRef, plus bool) error {
		pm := func(f string) ([]string, []string) {
			if plus {
				return []string{f}, nil
			}
			return nil, []string{f}
		}
		if ref.ID != 0 {
			l := labels[ref.ID]
			if l.accountID != b.accountID {
				return nil // label of another account: not for these messages
			}
			switch l.typ {
			case "user":
				return b.userLabel(ctx, tx, l, plus)
			case "folder":
				if l.folderID == nil {
					return fmt.Errorf("%w: folder label without folder", ErrInvalid)
				}
				for _, m := range b.msgs {
					if plus {
						if _, ok := in(m, "inbox", "archive", "user", "all"); ok && !hasFolder(m, *l.folderID) {
							from, _ := in(m, "inbox", "archive", "user", "all")
							move(m, from, "", *l.folderID, nil, nil)
						}
					} else if hasFolder(m, *l.folderID) {
						move(m, location{folderID: *l.folderID}, "archive", 0, nil, nil)
					}
				}
				return nil
			}
			ref.System = l.key
		}
		switch ref.System {
		case LabelUnread:
			a, r := pm(`\Seen`)
			flags(r, a) // +UNREAD = -\Seen
		case mail.LabelStarred:
			flags(pm(`\Flagged`))
		case mail.LabelImportant:
			flags(pm("$Important"))
		case mail.LabelInbox:
			for _, m := range b.msgs {
				if plus {
					if _, ok := in(m, "inbox"); ok {
						continue
					}
					if from, ok := in(m, "archive", "user", "junk", "trash", "all"); ok {
						move(m, from, "inbox", 0, nil, nil)
					}
				} else if from, ok := in(m, "inbox"); ok {
					move(m, from, "archive", 0, nil, nil)
				}
			}
		case mail.LabelTrash:
			for _, m := range b.msgs {
				if plus {
					if _, ok := in(m, "trash"); ok || len(m.locs) == 0 {
						continue
					}
					from := m.locs[0]
					if l, ok := in(m, "inbox"); ok {
						from = l
					}
					if _, err := tx.Exec(ctx, `UPDATE messages SET restore_folder_id = $2 WHERE id = $1`, m.id, from.folderID); err != nil {
						return err
					}
					move(m, from, "trash", 0, nil, nil)
				} else if from, ok := in(m, "trash"); ok {
					if m.restore != nil && b.folderRole[*m.restore] != "" && b.folderRole[*m.restore] != "trash" {
						move(m, from, "", *m.restore, nil, nil)
					} else {
						move(m, from, "inbox", 0, nil, nil)
					}
				}
			}
		case mail.LabelSpam:
			for _, m := range b.msgs {
				if plus {
					if from, ok := in(m, "inbox", "archive", "user", "all"); ok {
						move(m, from, "junk", 0, []string{"$Junk"}, []string{"$NotJunk"})
					}
				} else if from, ok := in(m, "junk"); ok {
					move(m, from, "inbox", 0, []string{"$NotJunk"}, []string{"$Junk"})
				}
			}
		case mail.LabelSent, mail.LabelDraft, mail.LabelSnoozed, mail.LabelScheduled, mail.LabelArchive:
			return fmt.Errorf("%w: %s", ErrNotModifiable, ref.System)
		default:
			return fmt.Errorf("%w: unknown label %q", ErrInvalid, ref.System)
		}
		return nil
	}
	for _, r := range add {
		if err := apply(r, true); err != nil {
			return err
		}
	}
	for _, r := range remove {
		if err := apply(r, false); err != nil {
			return err
		}
	}
	return nil
}

func hasFolder(m *msgInfo, folderID int64) bool {
	return slices.ContainsFunc(m.locs, func(l location) bool { return l.folderID == folderID })
}

// userLabel writes a user label locally (the source of truth, plan 3.3) and,
// when the server keeps keywords, mirrors it with a flags operation.
func (b *builder) userLabel(ctx context.Context, tx pgx.Tx, l *labelInfo, plus bool) error {
	ids := make([]int64, len(b.msgs))
	for i, m := range b.msgs {
		ids[i] = m.id
	}
	if err := b.recordLabel(ctx, tx, l.id, ids, plus); err != nil {
		return err
	}
	if plus {
		if _, err := tx.Exec(ctx, `INSERT INTO message_labels (message_id, label_id, user_id) SELECT unnest($1::bigint[]), $2, $3 ON CONFLICT DO NOTHING`, ids, l.id, b.userID); err != nil {
			return err
		}
	} else if _, err := tx.Exec(ctx, `DELETE FROM message_labels WHERE label_id = $2 AND message_id = ANY($1)`, ids, l.id); err != nil {
		return err
	}
	b.local = append(b.local, ids...)
	if !b.keywords {
		return nil
	}
	if l.keyword == nil {
		kw, err := mail.AssignKeyword(ctx, tx, b.accountID, l.name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE labels SET imap_keyword = $2, keyword_state = 'mirrored' WHERE id = $1`, l.id, kw); err != nil {
			return err
		}
		l.keyword, l.mirrored = &kw, true
	}
	if !l.mirrored {
		return nil // rejected by the server earlier: local only (plan 3.7)
	}
	for _, m := range b.msgs {
		if len(m.locs) == 0 {
			continue
		}
		p := mail.OpPayload{Op: mail.OpFlags}
		if plus {
			p.Add = []string{*l.keyword}
		} else {
			p.Remove = []string{*l.keyword}
		}
		b.add(p, m.id, 0, 0)
	}
	return nil
}
