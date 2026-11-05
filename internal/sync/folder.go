package sync

import (
	"context"
	"slices"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
)

// ensureSelected SELECTs f unless it is already the selected folder, and
// applies the folder bookkeeping (first SELECT, UIDVALIDITY change).
func (s *session) ensureSelected(ctx context.Context, f *folder) (*imap.SelectData, error) {
	if s.sc.selected == f.name && s.sc.sel != nil {
		return s.sc.sel, nil
	}
	sel, err := s.sc.selectFolder(ctx, f.name)
	if err != nil {
		return nil, err
	}
	if err := s.prepare(ctx, f, sel); err != nil {
		return nil, err
	}
	return sel, nil
}

// prepare initialises a folder on its first SELECT (watermarks and cursor)
// and handles a changed UIDVALIDITY (plan 3.5.4): every location becomes
// stale, the header backfill restarts from the top, the matcher re-attaches
// the messages and stale locations left unmatched vanish at the end of the
// backfill. On INBOX it also records keywords_supported.
func (s *session) prepare(ctx context.Context, f *folder, sel *imap.SelectData) error {
	top := uint32(sel.UIDNext)
	if top == 0 {
		top = 1
	}
	if f.role == RoleInbox {
		kw := slices.Contains(sel.PermanentFlags, imap.FlagWildcard)
		if kw != s.acc.keywordsSupp {
			if err := inTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE accounts SET keywords_supported = $2 WHERE id = $1`, s.acc.id, kw)
				return err
			}); err != nil {
				return err
			}
			s.acc.keywordsSupp = kw
		}
	}
	reset := f.uidValidity != 0 && f.uidValidity != sel.UIDValidity
	if f.uidValidity != 0 && !reset && f.phase != "" {
		return nil
	}
	phase := "headers"
	if !reset && !s.acc.initialDone && (f.role == RoleInbox || f.role == RoleSent) {
		phase = "headers_recent"
	}
	if reset {
		s.log.Warn("UIDVALIDITY changed; refetching folder", "folder", f.name, "old", f.uidValidity, "new", sel.UIDValidity)
	}
	perm := flagStrings(sel.PermanentFlags)
	err := inTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if reset {
			if _, err := tx.Exec(ctx, `UPDATE message_locations SET stale = true WHERE folder_id = $1`, f.id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
UPDATE mailbox_folders SET uidvalidity = $2, uidnext = $3, highestmodseq = $4, messages_count = $5, permanent_flags = $6,
  last_full_flags_at = NULL
WHERE id = $1`, f.id, int64(sel.UIDValidity), int64(top), nullUint(sel.HighestModSeq), int(sel.NumMessages), perm); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
INSERT INTO sync_cursors (folder_id, phase, low_uid, done_count, total_count, bytes_done, updated_at)
VALUES ($1, $2, $3, 0, $4, 0, now())
ON CONFLICT (folder_id) DO UPDATE SET phase = EXCLUDED.phase, low_uid = EXCLUDED.low_uid, done_count = 0,
  total_count = EXCLUDED.total_count, bytes_done = 0, updated_at = now()`, f.id, phase, int64(top), int(sel.NumMessages))
		return err
	})
	if err != nil {
		return err
	}
	f.uidValidity, f.uidNext, f.highestModSeq, f.messages = sel.UIDValidity, top, sel.HighestModSeq, int(sel.NumMessages)
	f.phase, f.lowUID, f.lastFullFlags = phase, top, nil
	delete(s.candidates, f.id)
	s.bodyWork = true
	return nil
}

func nullUint(v uint64) *int64 {
	if v == 0 {
		return nil
	}
	n := int64(v)
	return &n
}

// localLocations returns the UIDs of a folder known locally, with their
// stale flag.
func localLocations(ctx context.Context, db mail.DB, folderID int64) (map[uint32]bool, error) {
	rows, err := db.Query(ctx, `SELECT uid, stale FROM message_locations WHERE folder_id = $1`, folderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint32]bool{}
	for rows.Next() {
		var uid int64
		var stale bool
		if err := rows.Scan(&uid, &stale); err != nil {
			return nil, err
		}
		out[uint32(uid)] = stale
	}
	return out, rows.Err()
}

// syncFolder is the incremental sync of one folder without QRESYNC
// (plan 3.5.4, ADR 0006): new UIDs >= uidnext, UIDs missing locally in the
// covered range, deletions by UID SEARCH ALL set difference, and flag changes
// (CHANGEDSINCE with CONDSTORE, otherwise a full FLAGS comparison). force
// disables the "STATUS unchanged" shortcut of servers without CONDSTORE.
func (s *session) syncFolder(ctx context.Context, f *folder, force bool, st *imap.StatusData) error {
	if !f.synced() {
		return nil
	}
	condstore := s.sc.caps.HasCondstore()
	if !condstore && !force && f.uidValidity != 0 && f.phase != "" && f.lastFullFlags != nil && time.Since(*f.lastFullFlags) < fullFlagsMaxAge {
		if st == nil {
			err := s.sc.await(ctx, 0, func() error {
				var err error
				st, err = s.sc.imap().Status(f.name, &imap.StatusOptions{NumMessages: true, UIDNext: true, UIDValidity: true}).Wait()
				return err
			})
			if err != nil {
				return err
			}
		}
		if st.NumMessages != nil && int(*st.NumMessages) == f.messages && uint32(st.UIDNext) == f.uidNext && st.UIDValidity == f.uidValidity {
			return s.touchFolder(ctx, f)
		}
	}

	s.sc.selected = "" // always a fresh SELECT: its counters drive this pass
	sel, err := s.ensureSelected(ctx, f)
	if err != nil {
		return err
	}
	oldNext := f.uidNext
	top := uint32(sel.UIDNext)

	// 1. New messages (UID >= uidnext).
	var newInbox []mail.IngestedMessage
	if top > oldNext {
		uids, err := s.sc.searchUIDs(ctx, uidRange(oldNext, 0))
		if err != nil {
			return err
		}
		uids = slices.DeleteFunc(uids, func(u uint32) bool { return u < oldNext })
		got, err := s.ingestUIDs(ctx, f, uids)
		if err != nil {
			return err
		}
		newInbox = append(newInbox, got...)
	}

	// 2. Server set vs local set.
	server, err := s.sc.searchUIDs(ctx, &imap.SearchCriteria{})
	if err != nil {
		return err
	}
	local, err := localLocations(ctx, s.d.Pool, f.id)
	if err != nil {
		return err
	}
	onServer := make(map[uint32]bool, len(server))
	var missing []uint32
	low := f.coverLow()
	for _, u := range server {
		onServer[u] = true
		if _, ok := local[u]; !ok && u >= low && u < oldNext {
			missing = append(missing, u)
		}
	}
	if len(missing) > 0 {
		got, err := s.ingestUIDs(ctx, f, missing)
		if err != nil {
			return err
		}
		newInbox = append(newInbox, got...)
	}
	var vanished []uint32
	for u := range local {
		if !onServer[u] {
			vanished = append(vanished, u)
		}
	}
	slices.Sort(vanished)
	for len(vanished) > 0 {
		n := min(len(vanished), flagsBatch)
		chunk := vanished[:n]
		vanished = vanished[n:]
		var res *mail.VanishedResult
		if err := inTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
			var err error
			res, err = mail.LocationVanished(ctx, tx, f.id, chunk)
			return err
		}); err != nil {
			return err
		}
		if res.Removed > 0 { // plan 3.5.6 step 3: find where it went
			s.w.request(func(p *pending) { p.sweep = true })
		}
	}

	// 3. Flags of known, non-stale locations.
	known := func(u uint32) bool { stale, ok := local[u]; return ok && !stale && onServer[u] }
	fullFlags := false
	var updates []mail.FlagUpdate
	switch {
	case condstore && f.highestModSeq > 0 && sel.HighestModSeq > f.highestModSeq:
		all, err := s.sc.fetchFlags(ctx, imap.UIDSet{imap.UIDRange{Start: 1, Stop: 0}}, f.highestModSeq)
		if err != nil {
			return err
		}
		for _, u := range all {
			if known(u.UID) {
				updates = append(updates, u)
			}
		}
	case !condstore:
		var cand []uint32
		for _, u := range server {
			if known(u) {
				cand = append(cand, u)
			}
		}
		fullFlags = true
		if len(server) > bigFolder && f.lastFullFlags != nil && time.Since(*f.lastFullFlags) < bigFolderFullAge && len(cand) > bigFolderWindow {
			cand, fullFlags = cand[len(cand)-bigFolderWindow:], false
		}
		for len(cand) > 0 {
			n := min(len(cand), flagsBatch)
			got, err := s.sc.fetchFlags(ctx, uidSet(cand[:n]), 0)
			if err != nil {
				return err
			}
			updates = append(updates, got...)
			cand = cand[n:]
		}
	}
	for len(updates) > 0 {
		n := min(len(updates), flagsBatch)
		chunk := updates[:n]
		updates = updates[n:]
		if err := inTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
			_, err := mail.ApplyFlagsBatch(ctx, tx, f.id, chunk)
			return err
		}); err != nil {
			return err
		}
	}

	// 4. Watermarks.
	newNext := max(oldNext, top)
	err = inTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
UPDATE mailbox_folders SET uidnext = $2, highestmodseq = COALESCE($3, highestmodseq), messages_count = $4,
  last_sync_at = now(), last_full_flags_at = CASE WHEN $5 THEN now() ELSE last_full_flags_at END,
  last_error = NULL, error_count = 0
WHERE id = $1`, f.id, int64(newNext), nullUint(sel.HighestModSeq), int(sel.NumMessages), fullFlags || condstore)
		return err
	})
	if err != nil {
		return err
	}
	now := time.Now()
	f.uidNext, f.messages, f.lastSyncAt = newNext, int(sel.NumMessages), &now
	if sel.HighestModSeq > 0 {
		f.highestModSeq = sel.HighestModSeq
	}
	if fullFlags || condstore {
		f.lastFullFlags = &now
	}

	// 5. New INBOX messages: bodies right away (priority over the backfill).
	if f.role == RoleInbox && len(newInbox) > 0 {
		return s.fetchNewBodies(ctx, f, newInbox)
	}
	return nil
}

func (s *session) touchFolder(ctx context.Context, f *folder) error {
	return inTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE mailbox_folders SET last_sync_at = now(), last_error = NULL, error_count = 0 WHERE id = $1`, f.id)
		return err
	})
}

// ingestUIDs fetches and ingests UIDs of the selected folder in batches and
// returns the messages that are new to the account.
func (s *session) ingestUIDs(ctx context.Context, f *folder, uids []uint32) ([]mail.IngestedMessage, error) {
	var created []mail.IngestedMessage
	for len(uids) > 0 {
		n := min(len(uids), headerBatch)
		msgs, err := s.sc.fetchHeaders(ctx, uids[:n])
		if err != nil {
			return nil, err
		}
		uids = uids[n:]
		if len(msgs) == 0 {
			continue
		}
		var res *mail.IngestResult
		if err := inTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
			var err error
			res, err = mail.IngestBatch(ctx, tx, s.acc.id, f.id, msgs)
			return err
		}); err != nil {
			return nil, err
		}
		for _, m := range res.Messages {
			if m.Outcome == mail.OutcomeNew {
				created = append(created, m)
			}
		}
		s.bodyWork = true
	}
	return created, nil
}

// fetchNewBodies downloads the full message of new INBOX messages at once.
func (s *session) fetchNewBodies(ctx context.Context, f *folder, msgs []mail.IngestedMessage) error {
	ids := make([]int64, len(msgs))
	for i, m := range msgs {
		ids[i] = m.MessageID
	}
	rows, err := s.d.Pool.Query(ctx, `
SELECT m.id, l.uid, m.size_bytes FROM messages m JOIN message_locations l ON l.message_id = m.id AND l.folder_id = $2
WHERE m.id = ANY($1) AND m.body_state = 'headers'`, ids, f.id)
	if err != nil {
		return err
	}
	var todo []bodyItem
	for rows.Next() {
		var it bodyItem
		var uid int64
		if err := rows.Scan(&it.messageID, &uid, &it.size); err != nil {
			rows.Close()
			return err
		}
		it.uid, it.folderID = uint32(uid), f.id
		todo = append(todo, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.fetchBodyItems(ctx, f, todo)
	return err
}
