package ops

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
)

// OperationRetention is how long an operation can be undone after its jobs
// ran (plan 5.2): the inverse is kept in operations for this long.
const OperationRetention = 24 * time.Hour

// ErrNotUndoable means the operation cannot be undone (a permanent delete).
var ErrNotUndoable = errors.New("ops: operation cannot be undone")

// invItem is how to undo the change of one message: move it back to MoveTo
// (from the target of the operation, ExpectFolder or the folder of
// ExpectRole) with the flags Add/Remove, or, without MoveTo, change the flags
// back; LabelAdd/LabelRemove are user labels to put back or take off. Only
// changes that really happened are recorded (a flag already set is not
// removed by the undo).
type invItem struct {
	MessageID    int64    `json:"m"`
	MoveTo       int64    `json:"moveTo,omitempty"`
	ExpectFolder int64    `json:"expectFolder,omitempty"`
	ExpectRole   string   `json:"expectRole,omitempty"`
	Add          []string `json:"add,omitempty"`
	Remove       []string `json:"remove,omitempty"`
	LabelAdd     []int64  `json:"labelAdd,omitempty"`
	LabelRemove  []int64  `json:"labelRemove,omitempty"`
	// SnoozeThread undoes a snooze of the thread (plan 3.10): its snoozes row
	// is deleted, or gets back the wake time SnoozePrev of the snooze it
	// replaced. The item has no move: the moves are items of their own.
	SnoozeThread int64      `json:"snoozeThread,omitempty"`
	SnoozePrev   *time.Time `json:"snoozePrev,omitempty"`
}

// changedFlags returns the flags of the list that the operation really
// changes on m: with adding, those no location has yet; otherwise those
// some location has.
func changedFlags(m *msgInfo, flags []string, adding bool) []string {
	var out []string
	for _, f := range flags {
		has := slices.ContainsFunc(m.locs, func(l location) bool {
			return slices.ContainsFunc(l.flags, func(x string) bool { return strings.EqualFold(x, f) })
		})
		if has != adding {
			out = append(out, f)
		}
	}
	return out
}

// recordLabel records the inverse of adding (plus) or removing a user label
// on the messages that really change.
func (b *builder) recordLabel(ctx context.Context, tx pgx.Tx, labelID int64, ids []int64, plus bool) error {
	r, err := tx.Query(ctx, `SELECT message_id FROM message_labels WHERE label_id = $1 AND message_id = ANY($2)`, labelID, ids)
	if err != nil {
		return err
	}
	have, err := pgx.CollectRows(r, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, id := range ids {
		switch had := slices.Contains(have, id); {
		case plus && !had:
			b.inv = append(b.inv, invItem{MessageID: id, LabelRemove: []int64{labelID}})
		case !plus && had:
			b.inv = append(b.inv, invItem{MessageID: id, LabelAdd: []int64{labelID}})
		}
	}
	return nil
}

// saveOperation stores the inverse of an operation that queued jobs.
func saveOperation(ctx context.Context, tx pgx.Tx, id string, userID int64, undoUntil *time.Time, inv []invItem, final bool) error {
	var raw []byte
	if !final {
		if inv == nil {
			inv = []invItem{}
		}
		raw, _ = json.Marshal(inv)
	}
	_, err := tx.Exec(ctx, `INSERT INTO operations (id, user_id, undo_until, inverse) VALUES ($1::uuid, $2, $3, $4)`, id, userID, undoUntil, raw)
	return err
}

// applyInverse queues the inverse of the items for the messages of b's
// account, recording in turn how to undo it (the undo is an operation too).
func (b *builder) applyInverse(ctx context.Context, tx pgx.Tx, items []invItem) error {
	byID := map[int64]*msgInfo{}
	for _, m := range b.msgs {
		byID[m.id] = m
	}
	labelMsgs := map[[2]int64][]*msgInfo{} // (label, 1=add/0=remove) -> messages
	for _, it := range items {
		m := byID[it.MessageID]
		if m == nil {
			continue
		}
		if it.SnoozeThread != 0 {
			if err := restoreSnooze(ctx, tx, b.userID, it); err != nil {
				return err
			}
			b.local = append(b.local, m.id)
			continue
		}
		for _, l := range it.LabelAdd {
			labelMsgs[[2]int64{l, 1}] = append(labelMsgs[[2]int64{l, 1}], m)
		}
		for _, l := range it.LabelRemove {
			labelMsgs[[2]int64{l, 0}] = append(labelMsgs[[2]int64{l, 0}], m)
		}
		switch {
		case it.MoveTo != 0:
			if b.folderRole[it.MoveTo] == "" {
				continue // the folder is gone
			}
			expect := it.ExpectFolder
			if expect == 0 && it.ExpectRole != "" {
				expect = b.byRole[it.ExpectRole]
			}
			if b.folderRole[it.MoveTo] == "trash" && expect != 0 {
				if _, err := tx.Exec(ctx, `UPDATE messages SET restore_folder_id = $2 WHERE id = $1`, m.id, expect); err != nil {
					return err
				}
			}
			b.add(mail.OpPayload{Op: mail.OpMove, ToFolderID: it.MoveTo, Add: it.Add, Remove: it.Remove}, m.id, expect, b.s.undoWindow)
			back := expect
			if back == 0 && len(m.locs) > 0 {
				back = m.locs[0].folderID
			}
			// An undone snooze is not redone by undoing the undo: the
			// messages would go back to Snoozed without a wake time.
			if back != 0 && it.ExpectRole != roleSnoozed {
				b.inv = append(b.inv, invItem{MessageID: m.id, MoveTo: back, ExpectFolder: it.MoveTo, Add: it.Remove, Remove: it.Add})
			}
		case len(it.Add)+len(it.Remove) > 0 && len(m.locs) > 0:
			b.add(mail.OpPayload{Op: mail.OpFlags, Add: it.Add, Remove: it.Remove}, m.id, 0, 0)
			b.inv = append(b.inv, invItem{MessageID: m.id, Add: it.Remove, Remove: it.Add})
		}
	}
	if len(labelMsgs) == 0 {
		return nil
	}
	var ids []int64
	for k := range labelMsgs {
		ids = append(ids, k[0])
	}
	r, err := tx.Query(ctx, `SELECT id FROM labels WHERE id = ANY($1) AND user_id = $2 AND account_id = $3 AND type = 'user'`, ids, b.userID, b.accountID)
	if err != nil {
		return err
	}
	exist, err := pgx.CollectRows(r, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	refs := make([]LabelRef, len(exist))
	for i, id := range exist {
		refs[i] = LabelRef{ID: id}
	}
	labels, err := b.loadLabels(ctx, tx, refs)
	if err != nil {
		return err
	}
	all := b.msgs
	defer func() { b.msgs = all }()
	keys := make([][2]int64, 0, len(labelMsgs))
	for k := range labelMsgs {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, c [2]int64) int {
		if a[0] != c[0] {
			return int(a[0] - c[0])
		}
		return int(a[1] - c[1])
	})
	for _, k := range keys {
		l := labels[k[0]]
		if l == nil {
			continue // deleted meanwhile
		}
		b.msgs = labelMsgs[k]
		if err := b.userLabel(ctx, tx, l, k[1] == 1); err != nil {
			return err
		}
	}
	return nil
}

// reverse undoes an operation whose jobs already ran or were claimed: its
// inverse is applied as a new operation (own operationId and undo window).
func (s *Service) reverse(ctx context.Context, userID int64, operationID, origin string) (*Result, error) {
	var created time.Time
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT created_at, inverse FROM operations WHERE id = $1::uuid AND user_id = $2`, operationID, userID).Scan(&created, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if time.Since(created) > OperationRetention {
		return nil, ErrTooLate
	}
	if raw == nil {
		return nil, ErrNotUndoable
	}
	var items []invItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	var ids []int64
	for _, it := range items {
		ids = append(ids, it.MessageID)
	}
	ids = uniq(ids)
	if len(ids) == 0 { // nothing really changed
		if _, err := s.pool.Exec(ctx, `DELETE FROM operations WHERE id = $1::uuid`, operationID); err != nil {
			return nil, err
		}
		return &Result{Threads: []Thread{}}, nil
	}
	claimed := false
	return s.run(ctx, userID, nil, ids, origin, func(ctx context.Context, tx pgx.Tx, b *builder) error {
		if !claimed {
			tag, err := tx.Exec(ctx, `DELETE FROM operations WHERE id = $1::uuid AND user_id = $2`, operationID, userID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound // undone concurrently
			}
			claimed = true
		}
		return b.applyInverse(ctx, tx, items)
	})
}

// revertLabels puts back the user labels of an operation cancelled inside
// its window (they were written locally, not by its jobs) and returns the
// messages changed, by account.
func revertLabels(ctx context.Context, tx pgx.Tx, userID int64, operationID string) (map[int64][]int64, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `DELETE FROM operations WHERE id = $1::uuid AND user_id = $2 RETURNING inverse`, operationID, userID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && raw == nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var items []invItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	var msgs []int64
	for _, it := range items {
		for _, l := range it.LabelAdd {
			if _, err := tx.Exec(ctx, `INSERT INTO message_labels (message_id, label_id, user_id)
SELECT m.id, l.id, $3 FROM messages m, labels l WHERE m.id = $1 AND l.id = $2 AND m.user_id = $3 AND l.user_id = $3
ON CONFLICT DO NOTHING`, it.MessageID, l, userID); err != nil {
				return nil, err
			}
		}
		if len(it.LabelRemove) > 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM message_labels WHERE message_id = $1 AND label_id = ANY($2) AND user_id = $3`, it.MessageID, it.LabelRemove, userID); err != nil {
				return nil, err
			}
		}
		if it.SnoozeThread != 0 {
			if err := restoreSnooze(ctx, tx, userID, it); err != nil {
				return nil, err
			}
		}
		if len(it.LabelAdd)+len(it.LabelRemove) > 0 || it.SnoozeThread != 0 {
			msgs = append(msgs, it.MessageID)
		}
	}
	out := map[int64][]int64{}
	if len(msgs) == 0 {
		return out, nil
	}
	r, err := tx.Query(ctx, `SELECT account_id, id FROM messages WHERE id = ANY($1) AND user_id = $2`, msgs, userID)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	for r.Next() {
		var acc, id int64
		if err := r.Scan(&acc, &id); err != nil {
			return nil, err
		}
		out[acc] = append(out[acc], id)
	}
	return out, r.Err()
}

// PurgeOperations deletes the inverses of operations older than
// OperationRetention before now (part of the periodic gc job).
func (s *Service) PurgeOperations(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM operations WHERE created_at < $1`, now.Add(-OperationRetention))
	return tag.RowsAffected(), err
}
