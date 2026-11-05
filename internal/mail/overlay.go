package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Operations of imap_op jobs (plan 3.6). The payload of a job holds the
// operation only; its messages are rows of op_messages (job_id, message_id,
// expect_folder_id), so that coalescing is a row insert/delete and the
// overlay below is an index lookup per batch of messages.
const (
	OpMove    = "move"
	OpFlags   = "flags"
	OpExpunge = "expunge"

	// Folder operations (labels API): no messages, the job carries Name or
	// FolderID. They are invisible to the overlay (it joins op_messages).
	OpCreateFolder = "create_folder"
	OpDeleteFolder = "delete_folder"

	// Draft operations (plan 3.8). append (ToRole "drafts") saves the current
	// version of the local draft of its message in the Drafts folder and
	// removes the older copies; delete_draft removes the copies of a
	// discarded draft (DraftID, plus UIDs in FolderID for a draft that came
	// from another client). The overlay does not change labels for them and
	// append is not counted in pending_ops (a draft is saved locally).
	OpAppend      = "append"
	OpDeleteDraft = "delete_draft"
)

// OpPayload is jobs.payload of an imap_op job.
type OpPayload struct {
	Op string `json:"op"`
	// move: target folder by role (archive, inbox, trash, junk) or by id.
	ToRole     string `json:"toRole,omitempty"`
	ToFolderID int64  `json:"toFolderId,omitempty"`
	// flags (and move: flags set before moving, e.g. $Junk).
	Add    []string `json:"add,omitempty"`
	Remove []string `json:"remove,omitempty"`
	// create_folder: the folder name as the user typed it (hierarchy with /).
	Name string `json:"name,omitempty"`
	// delete_folder: mailbox_folders.id; delete_draft: the Drafts folder of UIDs.
	FolderID int64 `json:"folderId,omitempty"`
	// delete_draft: X-Silo-Draft-Id of the draft and UIDs of its copies.
	DraftID string   `json:"draftId,omitempty"`
	UIDs    []uint32 `json:"uids,omitempty"`
}

// pendingOp is one pending operation of one message.
type pendingOp struct {
	OpPayload
	expect *int64
}

type folderMeta struct {
	id      int64
	role    string
	labelID *int64
}

// loadPending returns the pending imap_op operations of the messages in
// execution order (run_at, id). Dead jobs are not pending: their changes are
// reverted by the next derivation.
func loadPending(ctx context.Context, db DB, ids []int64) (map[int64][]pendingOp, error) {
	rows, err := db.Query(ctx, `
SELECT om.message_id, om.expect_folder_id, j.payload
FROM op_messages om JOIN jobs j ON j.id = om.job_id
WHERE om.message_id = ANY($1) AND j.status = 'pending'
ORDER BY om.message_id, j.run_at, j.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("mail: load pending operations: %w", err)
	}
	defer rows.Close()
	out := map[int64][]pendingOp{}
	for rows.Next() {
		var id int64
		var p pendingOp
		var raw []byte
		if err := rows.Scan(&id, &p.expect, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &p.OpPayload); err != nil {
			continue // not an operation we know; ignore
		}
		out[id] = append(out[id], p)
	}
	return out, rows.Err()
}

// loadFolderMeta returns the folders of the accounts by id and by role.
func loadFolderMeta(ctx context.Context, db DB, accountIDs []int64) (map[int64]folderMeta, map[int64]map[string]folderMeta, error) {
	rows, err := db.Query(ctx, `SELECT id, account_id, role, label_id FROM mailbox_folders WHERE account_id = ANY($1) AND selectable AND sync_mode = 'sync'`, accountIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("mail: load folders: %w", err)
	}
	defer rows.Close()
	byID := map[int64]folderMeta{}
	byRole := map[int64]map[string]folderMeta{}
	for rows.Next() {
		var f folderMeta
		var acc int64
		if err := rows.Scan(&f.id, &acc, &f.role, &f.labelID); err != nil {
			return nil, nil, err
		}
		byID[f.id] = f
		if byRole[acc] == nil {
			byRole[acc] = map[string]folderMeta{}
		}
		if _, dup := byRole[acc][f.role]; !dup && f.role != "user" {
			byRole[acc][f.role] = f
		}
	}
	return byID, byRole, rows.Err()
}

// MoveSource picks the location a move takes a message from (plan 3.6
// conflicts): the folder it was in when the operation was made; otherwise its
// current folder, except Trash (a message trashed meanwhile is not pulled
// out) and the target itself. ok is false when there is nothing to move.
// roles[i] and folders[i] describe the message's locations. Both the overlay
// and the executor use it, so the visible state matches what will happen.
func MoveSource(folders []int64, roles []string, expect *int64, target int64, targetRole string) (int, bool) {
	if expect != nil {
		if i := slices.Index(folders, *expect); i >= 0 {
			if folders[i] == target {
				return 0, false
			}
			return i, true
		}
	}
	for i := range folders {
		if folders[i] == target || roles[i] == targetRole && targetRole != "user" {
			return 0, false // already there
		}
	}
	for i := range folders {
		if roles[i] != "trash" {
			return i, true
		}
	}
	return 0, false
}

func applyFlagOps(flags, add, remove []string) []string {
	out := slices.Clone(flags)
	for _, r := range remove {
		out = slices.DeleteFunc(out, func(f string) bool { return strings.EqualFold(f, r) })
	}
	for _, a := range add {
		if !hasFlag(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// overlay applies the pending operations to the server state of a message
// (plan 3.6 "Spójność podczas oczekiwania"): message_locations stay the pure
// server state, the derived labels show server state + pending operations.
func overlay(st *msgState, ops []pendingOp, byID map[int64]folderMeta, byRole map[string]folderMeta) {
	for _, op := range ops {
		switch op.Op {
		case OpFlags:
			for i := range st.locs {
				st.locs[i].flags = applyFlagOps(st.locs[i].flags, op.Add, op.Remove)
			}
		case OpMove:
			var target folderMeta
			if op.ToFolderID != 0 {
				f, ok := byID[op.ToFolderID]
				if !ok {
					continue
				}
				target = f
			} else if f, ok := byRole[op.ToRole]; ok {
				target = f
			} else {
				target = folderMeta{id: -1, role: op.ToRole} // created when the job runs (Archive)
			}
			folders := make([]int64, len(st.locs))
			roles := make([]string, len(st.locs))
			for i, l := range st.locs {
				folders[i], roles[i] = l.folderID, l.role
			}
			i, ok := MoveSource(folders, roles, op.expect, target.id, target.role)
			if !ok {
				continue
			}
			st.locs[i] = loc{folderID: target.id, role: target.role, labelID: target.labelID,
				flags: applyFlagOps(st.locs[i].flags, op.Add, op.Remove)}
		case OpExpunge:
			st.locs = slices.DeleteFunc(st.locs, func(l loc) bool {
				if op.expect != nil {
					return l.folderID == *op.expect
				}
				return l.role == "trash" || l.role == "junk"
			})
		}
	}
}

// applyOverlay loads and applies the pending operations of the states and
// returns the number of pending operations per message (messages.pending_ops).
func applyOverlay(ctx context.Context, db DB, states []*msgState, ids []int64) (map[int64]int, error) {
	pend, err := loadPending(ctx, db, ids)
	if err != nil {
		return nil, err
	}
	counts := map[int64]int{}
	if len(pend) == 0 {
		return counts, nil
	}
	var accounts []int64
	for _, st := range states {
		if len(pend[st.id]) > 0 && !slices.Contains(accounts, st.accountID) {
			accounts = append(accounts, st.accountID)
		}
	}
	byID, byRole, err := loadFolderMeta(ctx, db, accounts)
	if err != nil {
		return nil, err
	}
	for _, st := range states {
		ops := pend[st.id]
		if len(ops) == 0 {
			continue
		}
		for _, op := range ops {
			if op.Op != OpAppend {
				counts[st.id]++
			}
		}
		overlay(st, ops, byID, byRole[st.accountID])
	}
	return counts, nil
}
