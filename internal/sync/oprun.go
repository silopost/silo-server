package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/worker"
)

// Event and error codes of the write-back.
const (
	ErrCodeKeywords = "keywords_unsupported"

	FailMessageGone = "message_gone"
	FailRejected    = "server_rejected"
	FailExpired     = "expired"
	FailNoFolder    = "no_folder"
)

// opMaxAge: an operation waiting longer for its server is given up (plan 3.6).
const opMaxAge = 7 * 24 * time.Hour

// opRequest hands an imap_op job to the session that executes it.
type opRequest struct {
	job worker.Job
	// fn, when set, replaces the job: work on the ops connection that is not
	// a queued operation (content on demand).
	fn   func(ctx context.Context, s *session, c *conn) error
	done chan error
}

// RunOp executes an imap_op job of this account (plan 3.6), called from the
// account's job loop. Without a connected session the job waits (deferred,
// attempts not counted) up to 7 days; then, or after the last rejected
// attempt, the local state is reverted and operation.failed written.
func (w *AccountWorker) RunOp(ctx context.Context, job worker.Job) error {
	if job.CreatedAt.Valid && time.Since(job.CreatedAt.Time) > opMaxAge {
		if err := failOp(ctx, w.d, job, FailExpired, "server unavailable for 7 days"); err != nil {
			return err
		}
		return worker.Permanent(errors.New("operation expired"))
	}
	// Right after start the session is usually seconds away: wait a little
	// before deferring the job.
	var s *session
	for deadline := time.Now().Add(10 * time.Second); ; {
		w.mu.Lock()
		s = w.live
		state := w.state
		w.mu.Unlock()
		if s != nil {
			break
		}
		if time.Now().After(deadline) || state == StateBackoff || state == StateAuthFailed {
			return worker.Defer(15*time.Second, "account not connected")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	req := opRequest{job: job, done: make(chan error, 1)}
	select {
	case s.opsReq <- req:
	case <-s.ended:
		return worker.Defer(5*time.Second, "session ended")
	case <-ctx.Done():
		return ctx.Err()
	}
	var err error
	select {
	case err = <-req.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	switch {
	case err == nil:
		return nil
	case isCommandError(err) && job.Attempts >= job.MaxAttempts:
		if e := failOp(ctx, w.d, job, FailRejected, err.Error()); e != nil {
			return e
		}
		return worker.Permanent(err)
	case isCommandError(err), errors.Is(err, errOpPermanent):
		if errors.Is(err, errOpPermanent) {
			if e := failOp(ctx, w.d, job, FailNoFolder, err.Error()); e != nil {
				return e
			}
			return worker.Permanent(err)
		}
		return err // retried with backoff
	}
	return worker.Defer(5*time.Second, err.Error()) // connection trouble: the session reconnects
}

var errOpPermanent = errors.New("sync: operation cannot be executed")

// failOp gives an operation up: the job becomes dead (the overlay drops it,
// so the visible state returns to the server state) and operation.failed is
// written with the threads involved.
func failOp(ctx context.Context, d *Deps, job worker.Job, reason, detail string) error {
	accountID := job.AccountID.Int64
	return inTx(ctx, d.Pool, func(tx pgx.Tx) error {
		if err := mail.LockAccount(ctx, tx, accountID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET status = 'dead', locked_until = NULL, last_error = $2 WHERE id = $1`, job.ID, reason+": "+detail); err != nil {
			return err
		}
		r, err := tx.Query(ctx, `SELECT message_id FROM op_messages WHERE job_id = $1`, job.ID)
		if err != nil {
			return err
		}
		msgs, err := pgx.CollectRows(r, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		threads, err := mail.RefreshMessages(ctx, tx, accountID, msgs, "")
		if err != nil {
			return err
		}
		return writeFailed(ctx, tx, accountID, job, reason, threads)
	})
}

func writeFailed(ctx context.Context, tx pgx.Tx, accountID int64, job worker.Job, reason string, threads []int64) error {
	var userID int64
	if err := tx.QueryRow(ctx, `SELECT user_id FROM accounts WHERE id = $1`, accountID).Scan(&userID); err != nil {
		return err
	}
	op := ""
	if job.GroupID.Valid {
		b := job.GroupID.Bytes
		op = fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	if threads == nil {
		threads = []int64{}
	}
	return mail.WriteEvent(ctx, tx, userID, mail.EventOperationFailed, map[string]any{
		"operationId": op, "accountId": accountID, "reason": reason, "threadIds": threads,
	})
}

// opLoc is one location of a message of the job.
type opLoc struct {
	msgID    int64
	folderID int64
	uid      uint32
	flags    []string
}

type opFolder struct {
	id    int64
	name  string
	role  string
	delim string
}

// execOp runs one imap_op job on connection c and records the outcome in one
// transaction: server-side effects (COPYUID locations, flags, expunged
// locations), the job deleted, messages re-derived (pendingSync false), and
// operation.failed for messages that disappeared meanwhile.
func (s *session) execOp(ctx context.Context, c *conn, job worker.Job) (err error) {
	defer func() { c.selected = "" }() // the sync scheduler must SELECT afresh
	var p mail.OpPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return fmt.Errorf("%w: bad payload: %v", errOpPermanent, err)
	}
	accountID := s.acc.id
	type opItem struct {
		msg    int64
		expect *int64
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT message_id, expect_folder_id FROM op_messages WHERE job_id = $1 ORDER BY message_id`, job.ID)
	if err != nil {
		return err
	}
	var items []opItem
	for rows.Next() {
		var it opItem
		if err := rows.Scan(&it.msg, &it.expect); err != nil {
			rows.Close()
			return err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	msgIDs := make([]int64, len(items))
	for i, it := range items {
		msgIDs[i] = it.msg
	}
	folders, err := s.opFolders(ctx)
	if err != nil {
		return err
	}
	locs := map[int64][]opLoc{}
	if len(msgIDs) > 0 {
		r, err := s.d.Pool.Query(ctx, `
SELECT l.message_id, l.folder_id, l.uid, l.flags FROM message_locations l
JOIN mailbox_folders f ON f.id = l.folder_id AND f.selectable
WHERE l.message_id = ANY($1) AND NOT l.stale AND NOT (l.flags @> ARRAY['\Deleted'])
ORDER BY l.message_id, l.folder_id`, msgIDs)
		if err != nil {
			return err
		}
		for r.Next() {
			var l opLoc
			var uid int64
			if err := r.Scan(&l.msgID, &l.folderID, &uid, &l.flags); err != nil {
				r.Close()
				return err
			}
			l.uid = uint32(uid)
			locs[l.msgID] = append(locs[l.msgID], l)
		}
		r.Close()
		if err := r.Err(); err != nil {
			return err
		}
	}
	var gone []int64
	for _, it := range items {
		// A draft is saved whether or not it has a copy on the server yet.
		if len(locs[it.msg]) == 0 && p.Op != mail.OpAppend {
			gone = append(gone, it.msg)
		}
	}

	// Server commands; their effects are applied to the mirror in fin.
	var fin []func(tx pgx.Tx) error
	touched := map[int64]bool{}
	needDiscover := false
	byFolder := func(sel func(it opItem) []opLoc) map[int64][]opLoc {
		out := map[int64][]opLoc{}
		for _, it := range items {
			for _, l := range sel(it) {
				out[l.folderID] = append(out[l.folderID], l)
			}
		}
		return out
	}
	switch p.Op {
	case mail.OpFlags:
		groups := byFolder(func(it opItem) []opLoc { return locs[it.msg] })
		for _, fid := range sortedKeys(groups) {
			f := folders[fid]
			if f == nil {
				continue
			}
			ls := groups[fid]
			add, remove, err := s.storeFlags(ctx, c, f, ls, p.Add, p.Remove)
			if err != nil {
				return err
			}
			touched[fid] = true
			fin = append(fin, setFlags(fid, ls, add, remove))
		}
	case mail.OpMove:
		target, err := s.moveTarget(ctx, c, folders, p)
		if err != nil {
			return err
		}
		groups := byFolder(func(it opItem) []opLoc {
			ls := locs[it.msg]
			fs := make([]int64, len(ls))
			rs := make([]string, len(ls))
			for i, l := range ls {
				fs[i] = l.folderID
				if f := folders[l.folderID]; f != nil {
					rs[i] = f.role
				}
			}
			i, ok := mail.MoveSource(fs, rs, it.expect, target.id, target.role)
			if !ok {
				return nil
			}
			return []opLoc{ls[i]}
		})
		for _, fid := range sortedKeys(groups) {
			src := folders[fid]
			if src == nil {
				continue
			}
			f, err := s.moveMessages(ctx, c, src, target, groups[fid], p.Add, p.Remove)
			if err != nil {
				return err
			}
			touched[fid], touched[target.id] = true, true
			fin = append(fin, f)
		}
	case mail.OpCreateFolder:
		if err := s.createFolder(ctx, c, p.Name); err != nil {
			return err
		}
		needDiscover = true
	case mail.OpDeleteFolder:
		if f := folders[p.FolderID]; f != nil {
			if err := c.await(ctx, 0, func() error { return c.imap().Delete(f.name).Wait() }); err != nil {
				return err
			}
		}
		needDiscover = true
	case mail.OpExpunge:
		groups := byFolder(func(it opItem) []opLoc {
			var out []opLoc
			for _, l := range locs[it.msg] {
				f := folders[l.folderID]
				if it.expect != nil && l.folderID == *it.expect || it.expect == nil && f != nil && (f.role == "trash" || f.role == "junk") {
					out = append(out, l)
				}
			}
			return out
		})
		for _, fid := range sortedKeys(groups) {
			f := folders[fid]
			if f == nil {
				continue
			}
			g, err := s.expungeMessages(ctx, c, f, groups[fid])
			if err != nil {
				return err
			}
			touched[fid] = true
			fin = append(fin, g)
		}
	case mail.OpAppend:
		doAppend := s.appendDraft
		if p.ToRole == RoleSent {
			doAppend = func(ctx context.Context, c *conn, folders map[int64]*opFolder, _ mail.OpPayload, msgIDs []int64) (func(tx pgx.Tx) error, int64, error) {
				return s.appendSent(ctx, c, folders, msgIDs)
			}
		}
		f, folderID, err := doAppend(ctx, c, folders, p, msgIDs)
		if err != nil {
			return err
		}
		if f != nil {
			touched[folderID] = true
			fin = append(fin, f)
		}
	case mail.OpDeleteDraft:
		f, folderID, err := s.deleteDraftCopies(ctx, c, folders, p)
		if err != nil {
			return err
		}
		if f != nil {
			touched[folderID] = true
			fin = append(fin, f)
		}
	default:
		return fmt.Errorf("%w: unknown op %q", errOpPermanent, p.Op)
	}

	err = inTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if err := mail.LockAccount(ctx, tx, accountID); err != nil {
			return err
		}
		for _, f := range fin {
			if err := f(tx); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM jobs WHERE id = $1`, job.ID); err != nil {
			return err
		}
		threads, err := mail.RefreshMessages(ctx, tx, accountID, msgIDs, "")
		if err != nil {
			return err
		}
		if len(gone) > 0 {
			r, err := tx.Query(ctx, `SELECT DISTINCT thread_id FROM messages WHERE id = ANY($1)`, gone)
			if err != nil {
				return err
			}
			gt, err := pgx.CollectRows(r, pgx.RowTo[int64])
			if err != nil {
				return err
			}
			_ = threads
			return writeFailed(ctx, tx, accountID, job, FailMessageGone, gt)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(gone) > 0 {
		s.log.Info("write-back skipped messages that are gone", "job", job.ID, "messages", len(gone))
	}
	if needDiscover {
		s.w.request(func(pd *pending) { pd.discover = true })
	}
	// Plan 3.5.4: a finished operation triggers an incremental sync of the
	// folders it touched.
	if len(touched) > 0 {
		s.w.request(func(pd *pending) {
			if pd.folders == nil {
				pd.folders = map[int64]bool{}
			}
			for id := range touched {
				if id > 0 {
					pd.folders[id] = true
				}
			}
		})
	}
	return nil
}

func sortedKeys[V any](m map[int64]V) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (s *session) opFolders(ctx context.Context) (map[int64]*opFolder, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT id, name, role, COALESCE(delimiter, '') FROM mailbox_folders WHERE account_id = $1 AND selectable`, s.acc.id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*opFolder{}
	for rows.Next() {
		f := &opFolder{}
		if err := rows.Scan(&f.id, &f.name, &f.role, &f.delim); err != nil {
			return nil, err
		}
		out[f.id] = f
	}
	return out, rows.Err()
}

func uidsOf(ls []opLoc) []uint32 {
	out := make([]uint32, len(ls))
	for i, l := range ls {
		out[i] = l.uid
	}
	return out
}

func toFlags(fs []string) []imap.Flag {
	out := make([]imap.Flag, len(fs))
	for i, f := range fs {
		out[i] = imap.Flag(f)
	}
	return out
}

func (c *conn) store(ctx context.Context, uids []uint32, op imap.StoreFlagsOp, flags []string) error {
	if len(flags) == 0 || len(uids) == 0 {
		return nil
	}
	return c.await(ctx, 0, func() error {
		return c.imap().Store(uidSet(uids), &imap.StoreFlags{Op: op, Silent: true, Flags: toFlags(flags)}, nil).Close()
	})
}

func (c *conn) ensure(ctx context.Context, name string) error {
	if c.selected == name {
		return nil
	}
	_, err := c.selectFolder(ctx, name)
	return err
}

// storeFlags runs UID STORE +FLAGS.SILENT / -FLAGS.SILENT in a folder. A
// rejected keyword (no \* in PERMANENTFLAGS, the 26 keywords of a maildir
// folder) turns its labels local-only (plan 3.7) and the system flags are
// stored without it. It returns the flags actually stored.
func (s *session) storeFlags(ctx context.Context, c *conn, f *opFolder, ls []opLoc, add, remove []string) ([]string, []string, error) {
	if err := c.ensure(ctx, f.name); err != nil {
		return nil, nil, err
	}
	uids := uidsOf(ls)
	err := c.store(ctx, uids, imap.StoreFlagsAdd, add)
	if err == nil {
		err = c.store(ctx, uids, imap.StoreFlagsDel, remove)
	}
	if err == nil || !isCommandError(err) {
		return add, remove, err
	}
	sys := func(fs []string) (keep, kws []string) {
		for _, f := range fs {
			if strings.HasPrefix(f, `\`) {
				keep = append(keep, f)
			} else {
				kws = append(kws, f)
			}
		}
		return keep, kws
	}
	add2, kwA := sys(add)
	rem2, kwR := sys(remove)
	kws := append(kwA, kwR...)
	if len(kws) == 0 {
		return nil, nil, err
	}
	s.log.Warn("server rejected keywords; labels become local only", "folder", f.name, "keywords", kws, "err", err)
	if e := s.keywordsRejected(ctx, kws, err); e != nil {
		return nil, nil, e
	}
	if e := c.store(ctx, uids, imap.StoreFlagsAdd, add2); e != nil {
		return nil, nil, e
	}
	if e := c.store(ctx, uids, imap.StoreFlagsDel, rem2); e != nil {
		return nil, nil, e
	}
	return add2, rem2, nil
}

func (s *session) keywordsRejected(ctx context.Context, kws []string, cause error) error {
	return inTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE labels SET keyword_state = 'local_only' WHERE account_id = $1 AND type = 'user' AND imap_keyword = ANY($2)`, s.acc.id, kws); err != nil {
			return err
		}
		return mail.WriteEvent(ctx, tx, s.acc.userID, EventAccountError, map[string]any{
			"accountId": s.acc.id, "code": ErrCodeKeywords, "detail": cause.Error(),
		})
	})
}

func setFlags(folderID int64, ls []opLoc, add, remove []string) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		for _, l := range ls {
			fl := applyFlags(l.flags, add, remove)
			if _, err := tx.Exec(context.Background(), `UPDATE message_locations SET flags = $3 WHERE folder_id = $1 AND uid = $2`, folderID, int64(l.uid), fl); err != nil {
				return err
			}
		}
		return nil
	}
}

func applyFlags(flags, add, remove []string) []string {
	out := slices.DeleteFunc(slices.Clone(flags), func(f string) bool {
		return slices.ContainsFunc(remove, func(r string) bool { return strings.EqualFold(r, f) })
	})
	for _, a := range add {
		if !slices.ContainsFunc(out, func(f string) bool { return strings.EqualFold(f, a) }) {
			out = append(out, a)
		}
	}
	slices.Sort(out)
	return out
}

// moveTarget resolves the target folder; a missing Archive is created on the
// server (plan 3.4 step 4, "create_folder") and recorded with its role.
func (s *session) moveTarget(ctx context.Context, c *conn, folders map[int64]*opFolder, p mail.OpPayload) (*opFolder, error) {
	if p.ToFolderID != 0 {
		if f := folders[p.ToFolderID]; f != nil {
			return f, nil
		}
		return nil, fmt.Errorf("%w: target folder %d is gone", errOpPermanent, p.ToFolderID)
	}
	for _, id := range sortedKeys(folders) {
		if folders[id].role == p.ToRole {
			return folders[id], nil
		}
	}
	if p.ToRole != RoleArchive {
		return nil, fmt.Errorf("%w: the account has no %s folder", errOpPermanent, p.ToRole)
	}
	return s.createRoleFolder(ctx, c, folders, RoleArchive, "Archive")
}

// createRoleFolder creates a missing special folder (Archive, Drafts) as base
// or INBOX<delim>base and records it with its role.
func (s *session) createRoleFolder(ctx context.Context, c *conn, folders map[int64]*opFolder, role, base string) (*opFolder, error) {
	names := []string{base}
	for _, f := range folders {
		if f.role == RoleInbox && f.delim != "" {
			names = append(names, "INBOX"+f.delim+base)
		}
	}
	var name string
	var lastErr error
	for _, n := range names {
		err := c.await(ctx, 0, func() error { return c.imap().Create(n, nil).Wait() })
		var ie *imap.Error
		if err == nil || errors.As(err, &ie) && ie.Code == imap.ResponseCodeAlreadyExists {
			name = n
			break
		}
		if !isCommandError(err) {
			return nil, err
		}
		lastErr = err
	}
	if name == "" {
		return nil, lastErr
	}
	s.log.Info("created a special folder", "role", role, "name", name)
	f := &opFolder{name: name, role: role}
	err := inTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if err := mail.LockAccount(ctx, tx, s.acc.id); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `
INSERT INTO mailbox_folders (account_id, user_id, name, name_raw, role, uidvalidity)
VALUES ($1, $2, $3, $3, $4, 0)
ON CONFLICT (account_id, name_raw) DO UPDATE SET role = $4, selectable = true, sync_mode = 'sync'
RETURNING id`, s.acc.id, s.acc.userID, name, role).Scan(&f.id)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE accounts SET folder_mapping = folder_mapping || jsonb_build_object($3::text, $2::text) WHERE id = $1`, s.acc.id, name, role)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.w.request(func(p *pending) { p.discover = true })
	return f, nil
}

// moveMessages moves UIDs of one folder (plan 3.6, ADR 0006): UID MOVE when
// available; otherwise UID COPY + \Deleted, then UID EXPUNGE with UIDPLUS, a
// plain EXPUNGE only in Trash/Drafts, and nothing else (the source copy stays
// flagged \Deleted, which Silo treats as gone). COPYUID places the message in
// the target without waiting for the sync.
func (s *session) moveMessages(ctx context.Context, c *conn, src, dst *opFolder, ls []opLoc, add, remove []string) (func(tx pgx.Tx) error, error) {
	if err := c.ensure(ctx, src.name); err != nil {
		return nil, err
	}
	uids := uidsOf(ls)
	if len(add)+len(remove) > 0 {
		a, r, err := s.storeFlags(ctx, c, src, ls, add, remove)
		if err != nil {
			return nil, err
		}
		add, remove = a, r
	}
	var srcUIDs, dstUIDs []imap.UID
	expunged := true
	if c.caps.HasMove() {
		var data *imapclient.MoveData
		err := c.await(ctx, 0, func() error {
			var err error
			data, err = c.imap().Move(uidSet(uids), dst.name).Wait()
			return err
		})
		if err != nil {
			return nil, err
		}
		if data != nil {
			srcUIDs, dstUIDs = numsOf(data.SourceUIDs), numsOf(data.DestUIDs)
		}
	} else {
		var data *imap.CopyData
		err := c.await(ctx, 0, func() error {
			var err error
			data, err = c.imap().Copy(uidSet(uids), dst.name).Wait()
			return err
		})
		if err != nil {
			return nil, err
		}
		if data != nil {
			srcUIDs, dstUIDs = numsOf(data.SourceUIDs), numsOf(data.DestUIDs)
		}
		if err := c.store(ctx, uids, imap.StoreFlagsAdd, []string{`\Deleted`}); err != nil {
			return nil, err
		}
		switch {
		case c.caps.HasUIDPlus():
			err = c.await(ctx, 0, func() error { return c.imap().UIDExpunge(uidSet(uids)).Close() })
		case src.role == RoleTrash || src.role == RoleDrafts:
			err = c.await(ctx, 0, func() error { return c.imap().Expunge().Close() })
		default:
			expunged = false
		}
		if err != nil {
			return nil, err
		}
	}
	dest := map[uint32]uint32{}
	if len(srcUIDs) == len(dstUIDs) {
		for i := range srcUIDs {
			dest[uint32(srcUIDs[i])] = uint32(dstUIDs[i])
		}
	}
	return func(tx pgx.Tx) error {
		ctx := context.Background()
		var vanished []uint32
		for _, l := range ls {
			flags := applyFlags(l.flags, add, remove)
			if du, ok := dest[l.uid]; ok && dst.id > 0 {
				if _, err := tx.Exec(ctx, `
INSERT INTO message_locations (folder_id, uid, message_id, account_id, user_id, flags)
VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (folder_id, uid) DO NOTHING`, dst.id, int64(du), l.msgID, s.acc.id, s.acc.userID, flags); err != nil {
					return err
				}
			}
			if expunged {
				vanished = append(vanished, l.uid)
			} else if _, err := tx.Exec(ctx, `UPDATE message_locations SET flags = $3 WHERE folder_id = $1 AND uid = $2`,
				src.id, int64(l.uid), applyFlags(flags, []string{`\Deleted`}, nil)); err != nil {
				return err
			}
		}
		if len(vanished) > 0 {
			if _, err := mail.LocationVanished(ctx, tx, src.id, vanished); err != nil {
				return err
			}
		}
		return nil
	}, nil
}

// expungeMessages deletes messages for good: \Deleted, then UID EXPUNGE
// (UIDPLUS) or EXPUNGE in Trash only.
func (s *session) expungeMessages(ctx context.Context, c *conn, f *opFolder, ls []opLoc) (func(tx pgx.Tx) error, error) {
	if err := c.ensure(ctx, f.name); err != nil {
		return nil, err
	}
	uids := uidsOf(ls)
	if err := c.store(ctx, uids, imap.StoreFlagsAdd, []string{`\Deleted`}); err != nil {
		return nil, err
	}
	expunged := true
	var err error
	switch {
	case c.caps.HasUIDPlus():
		err = c.await(ctx, 0, func() error { return c.imap().UIDExpunge(uidSet(uids)).Close() })
	case f.role == RoleTrash:
		err = c.await(ctx, 0, func() error { return c.imap().Expunge().Close() })
	default:
		expunged = false
	}
	if err != nil {
		return nil, err
	}
	return func(tx pgx.Tx) error {
		ctx := context.Background()
		if expunged {
			_, err := mail.LocationVanished(ctx, tx, f.id, uids)
			return err
		}
		for _, l := range ls {
			if _, err := tx.Exec(ctx, `UPDATE message_locations SET flags = $3 WHERE folder_id = $1 AND uid = $2`,
				f.id, int64(l.uid), applyFlags(l.flags, []string{`\Deleted`}, nil)); err != nil {
				return err
			}
		}
		return nil
	}, nil
}

func numsOf(s imap.NumSet) []imap.UID {
	if us, ok := s.(imap.UIDSet); ok {
		n, _ := us.Nums()
		return n
	}
	return nil
}

// createFolder runs CREATE for the labels API; an existing folder is fine.
func (s *session) createFolder(ctx context.Context, c *conn, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: empty folder name", errOpPermanent)
	}
	err := c.await(ctx, 0, func() error { return c.imap().Create(name, nil).Wait() })
	var ie *imap.Error
	if err == nil || errors.As(err, &ie) && ie.Code == imap.ResponseCodeAlreadyExists {
		s.log.Info("created folder", "name", name)
		return nil
	}
	return err
}
