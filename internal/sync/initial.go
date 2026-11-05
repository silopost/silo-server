package sync

import (
	"context"
	"slices"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
)

// phaseA is the quick start of the initial sync (plan 3.5.3 step 2): the
// last 30 days of INBOX, then of Sent, newest first, 200 per transaction.
// Messages already present (a previous, interrupted run) are skipped, so a
// restart at any point resumes without duplicates.
func (s *session) phaseA(ctx context.Context) error {
	for _, role := range []string{RoleInbox, RoleSent} {
		f := s.folderByRole(role)
		if f == nil || !f.synced() {
			continue
		}
		if _, err := s.ensureSelected(ctx, f); err != nil {
			if isCommandError(err) {
				if err := s.folderError(ctx, f, err); err != nil {
					return err
				}
				continue
			}
			return err
		}
		if f.phase != "headers_recent" {
			continue
		}
		crit := &imap.SearchCriteria{Since: s.d.now().Add(-recentWindow)}
		if f.lowUID > 1 {
			crit.UID = []imap.UIDSet{{imap.UIDRange{Start: 1, Stop: imap.UID(f.lowUID - 1)}}}
		}
		var uids []uint32
		if f.lowUID > 1 {
			var err error
			if uids, err = s.sc.searchUIDs(ctx, crit); err != nil {
				return err
			}
		}
		slices.Reverse(uids) // newest first
		for len(uids) > 0 && ctx.Err() == nil {
			n := min(len(uids), headerBatch)
			chunk := uids[:n]
			uids = uids[n:]
			todo, err := s.notLocal(ctx, f, chunk)
			if err != nil {
				return err
			}
			if err := s.headerBatch(ctx, f, "headers_recent", todo, 0); err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.tx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE sync_cursors SET phase = 'headers', updated_at = now() WHERE folder_id = $1`, f.id); err != nil {
				return err
			}
			return s.progress(ctx, tx, true)
		}); err != nil {
			return err
		}
		f.phase = "headers"
	}
	return nil
}

// notLocal filters out UIDs that already have a (non-stale) location.
func (s *session) notLocal(ctx context.Context, f *folder, uids []uint32) ([]uint32, error) {
	ids := make([]int64, len(uids))
	for i, u := range uids {
		ids[i] = int64(u)
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT uid FROM message_locations WHERE folder_id = $1 AND uid = ANY($2) AND NOT stale`, f.id, ids)
	if err != nil {
		return nil, err
	}
	have, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	set := map[uint32]bool{}
	for _, u := range have {
		set[uint32(u)] = true
	}
	out := make([]uint32, 0, len(uids))
	for _, u := range uids {
		if !set[u] {
			out = append(out, u)
		}
	}
	return out, nil
}

// headerBatch fetches and ingests one batch and advances the cursor in the
// same transaction (newLow > 0 moves low_uid down to it).
func (s *session) headerBatch(ctx context.Context, f *folder, phase string, uids []uint32, newLow uint32) error {
	msgs, err := s.sc.fetchHeaders(ctx, uids)
	if err != nil {
		return err
	}
	var bytes int64
	for _, m := range msgs {
		bytes += m.Size
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if len(msgs) > 0 {
			if _, err := mail.IngestBatch(ctx, tx, s.acc.id, f.id, msgs); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
UPDATE sync_cursors SET done_count = done_count + $2, bytes_done = bytes_done + $3,
  low_uid = CASE WHEN $4 > 0 THEN LEAST(COALESCE(low_uid, $4), $4) ELSE low_uid END, updated_at = now()
WHERE folder_id = $1`, f.id, len(msgs), bytes, int64(newLow)); err != nil {
			return err
		}
		return s.progress(ctx, tx, false)
	})
	if err != nil {
		return err
	}
	if newLow > 0 && newLow < f.lowUID {
		f.lowUID = newLow
	}
	s.bodyWork = true
	s.afterBatch(phase, len(msgs))
	return nil
}

// backfillStep does one batch of phase B (headers) or, when every folder has
// its headers, phase C (bodies). It reports whether it did any work.
func (s *session) backfillStep(ctx context.Context) (bool, error) {
	var next *folder
	for _, f := range s.folders {
		if !f.synced() || f.headersDone() {
			continue
		}
		if next == nil || backfillOrder(f.role) < backfillOrder(next.role) ||
			(backfillOrder(f.role) == backfillOrder(next.role) && f.id < next.id) {
			next = f
		}
	}
	if next != nil {
		if !s.w.acquire(ctx) {
			return false, nil
		}
		defer s.w.release()
		err := s.phaseBBatch(ctx, next)
		if isCommandError(err) {
			s.sc.selected = ""
			if e := s.folderError(ctx, next, err); e != nil {
				return false, e
			}
			next.phase = "bodies" // retried after the next session start
			return true, nil
		}
		return err == nil, err
	}
	if !s.bodyWork || time.Now().Before(s.backfillAt) {
		return false, nil
	}
	if !s.w.acquire(ctx) {
		return false, nil
	}
	defer s.w.release()
	return s.phaseCBatch(ctx)
}

// phaseBBatch processes the next 200 UIDs below the folder's cursor
// (plan 3.5.3 step 3).
func (s *session) phaseBBatch(ctx context.Context, f *folder) error {
	if _, err := s.ensureSelected(ctx, f); err != nil {
		return err
	}
	if f.phase == "headers_recent" { // a folder phase A did not reach (role changed)
		f.phase = "headers"
	}
	list, ok := s.candidates[f.id]
	if !ok {
		if f.lowUID > 1 {
			var err error
			if list, err = s.sc.searchUIDs(ctx, uidRange(1, f.lowUID-1)); err != nil {
				return err
			}
		}
		list = slices.DeleteFunc(list, func(u uint32) bool { return u >= f.lowUID })
	}
	if len(list) == 0 {
		delete(s.candidates, f.id)
		return s.finishHeaders(ctx, f)
	}
	n := min(len(list), headerBatch)
	chunk := slices.Clone(list[len(list)-n:])
	s.candidates[f.id] = list[:len(list)-n]
	slices.Reverse(chunk) // newest first
	todo, err := s.notLocal(ctx, f, chunk)
	if err != nil {
		return err
	}
	return s.headerBatch(ctx, f, "headers", todo, chunk[len(chunk)-1])
}

// finishHeaders ends the header backfill of a folder: stale locations still
// unmatched after a UIDVALIDITY change vanish now.
func (s *session) finishHeaders(ctx context.Context, f *folder) error {
	var res *mail.VanishedResult
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var err error
		if res, err = vanishAll(ctx, tx, f.id, true); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sync_cursors SET phase = 'bodies', low_uid = 1, updated_at = now() WHERE folder_id = $1`, f.id); err != nil {
			return err
		}
		return s.progress(ctx, tx, true)
	})
	if err != nil {
		return err
	}
	if res.Removed > 0 {
		s.w.request(func(p *pending) { p.sweep = true })
	}
	f.phase, f.lowUID = "bodies", 1
	return nil
}

type bodyItem struct {
	messageID int64
	folderID  int64
	uid       uint32
	size      int64
}

// phaseCBatch downloads the bodies of the newest messages that only have
// headers (plan 3.5.3 step 4), paced by SILO_SYNC_BACKFILL_RATE.
func (s *session) phaseCBatch(ctx context.Context) (bool, error) {
	set := s.d.Settings.withDefaults()
	if set.QuotaPerAccount > 0 {
		var used int64
		if err := s.d.Pool.QueryRow(ctx, `SELECT COALESCE(sum(size_bytes), 0)::bigint FROM messages WHERE account_id = $1 AND body_state = 'full'`, s.acc.id).Scan(&used); err != nil {
			return false, err
		}
		if used >= set.QuotaPerAccount {
			if !s.quotaHit {
				s.quotaHit = true
				s.log.Warn("mirror quota reached; body backfill stopped", "used", used)
				_ = s.tx(ctx, func(tx pgx.Tx) error {
					return mail.WriteEvent(ctx, tx, s.acc.userID, EventAccountError, map[string]any{
						"accountId": s.acc.id, "code": ErrCodeQuota, "detail": "SILO_MIRROR_QUOTA_PER_ACCOUNT reached",
					})
				})
			}
			s.bodyWork = false
			return false, nil
		}
	}
	skip := make([]int64, 0, len(s.bodySkip))
	for id, at := range s.bodySkip {
		if time.Since(at) < 10*time.Minute {
			skip = append(skip, id)
		} else {
			delete(s.bodySkip, id)
		}
	}
	rows, err := s.d.Pool.Query(ctx, `
SELECT m.id, l.folder_id, l.uid, m.size_bytes FROM messages m
JOIN LATERAL (SELECT l.folder_id, l.uid FROM message_locations l JOIN mailbox_folders f ON f.id = l.folder_id
              WHERE l.message_id = m.id AND NOT l.stale AND f.sync_mode = 'sync' ORDER BY l.folder_id LIMIT 1) l ON true
WHERE m.account_id = $1 AND m.body_state = 'headers' AND m.orphaned_at IS NULL AND NOT (m.id = ANY($2))
ORDER BY m.internal_date DESC, m.id DESC LIMIT 100`, s.acc.id, skip)
	if err != nil {
		return false, err
	}
	var items []bodyItem
	for rows.Next() {
		var it bodyItem
		var uid int64
		if err := rows.Scan(&it.messageID, &it.folderID, &uid, &it.size); err != nil {
			rows.Close()
			return false, err
		}
		it.uid = uint32(uid)
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(items) == 0 {
		s.bodyWork = false
		return false, s.finishBodies(ctx)
	}
	// Too large to mirror: deferred (content on demand).
	var deferred []int64
	for _, it := range items {
		if it.size > set.MaxMessageBytes {
			deferred = append(deferred, it.messageID)
		}
	}
	if len(deferred) > 0 {
		return true, s.tx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE messages SET body_state = 'deferred' WHERE id = ANY($1) AND body_state = 'headers'`, deferred)
			return err
		})
	}
	// One folder per batch, bounded by count and by one second of the rate.
	budget := max(set.BackfillRate, 1<<20)
	var batch []bodyItem
	var bytes int64
	for _, it := range items {
		if it.folderID != items[0].folderID {
			continue
		}
		if len(batch) > 0 && (len(batch) >= 25 || bytes+it.size > budget) {
			break
		}
		batch = append(batch, it)
		bytes += it.size
	}
	f := s.folderByID(batch[0].folderID)
	if f == nil {
		for _, it := range batch {
			s.bodySkip[it.messageID] = time.Now()
		}
		return true, nil
	}
	n, err := s.fetchBodyItems(ctx, f, batch)
	if isQuotaError(err) {
		s.log.Warn("server limit while fetching bodies; slowing down the backfill", "err", err)
		s.slowUntil = time.Now().Add(10 * time.Minute)
		s.backfillAt = time.Now().Add(time.Minute)
		return true, nil
	}
	if isCommandError(err) {
		for _, it := range batch {
			s.bodySkip[it.messageID] = time.Now()
		}
		return true, s.folderError(ctx, f, err)
	}
	if err != nil {
		return false, err
	}
	rate := set.BackfillRate
	if time.Now().Before(s.slowUntil) {
		rate = max(rate/4, 1)
	}
	s.backfillAt = time.Now().Add(time.Duration(float64(n) / float64(rate) * float64(time.Second)))
	return true, nil
}

// fetchBodyItems downloads BODY.PEEK[] of items located in f and stores them
// with mail.IngestBody in one transaction. Items the server did not return
// are skipped for a while. It returns the bytes downloaded.
func (s *session) fetchBodyItems(ctx context.Context, f *folder, items []bodyItem) (int64, error) {
	maxBytes := s.d.Settings.withDefaults().MaxMessageBytes
	var uids []uint32
	var expect int64
	var deferred []int64
	for _, it := range items {
		if it.size > maxBytes {
			deferred = append(deferred, it.messageID)
			continue
		}
		uids = append(uids, it.uid)
		expect += it.size
	}
	t0 := time.Now()
	var bodies map[uint32][]byte
	if len(uids) > 0 {
		if _, err := s.ensureSelected(ctx, f); err != nil {
			return 0, err
		}
		var err error
		if bodies, err = s.sc.fetchBodies(ctx, uids, expect); err != nil {
			return 0, err
		}
	}
	t1 := time.Now()
	var got int64
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if len(deferred) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE messages SET body_state = 'deferred' WHERE id = ANY($1) AND body_state = 'headers'`, deferred); err != nil {
				return err
			}
		}
		for _, it := range items {
			raw, ok := bodies[it.uid]
			if !ok {
				continue
			}
			if _, err := mail.IngestBody(ctx, tx, s.d.Blobs, it.messageID, raw); err != nil {
				return err
			}
			got += int64(len(raw))
		}
		return s.progress(ctx, tx, false)
	})
	if err != nil {
		return 0, err
	}
	s.log.Debug("bodies stored", "folder", f.name, "n", len(bodies), "bytes", got, "fetch", t1.Sub(t0), "store", time.Since(t1))
	for _, it := range items {
		if _, ok := bodies[it.uid]; !ok && it.size <= maxBytes {
			s.bodySkip[it.messageID] = time.Now()
		}
	}
	s.afterBatch("bodies", len(bodies))
	return got, nil
}

// finishBodies closes the initial sync once headers and bodies are complete.
func (s *session) finishBodies(ctx context.Context) error {
	for _, f := range s.folders {
		if f.synced() && !f.headersDone() {
			return nil
		}
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
UPDATE sync_cursors c SET phase = 'done', updated_at = now() FROM mailbox_folders f
WHERE f.id = c.folder_id AND f.account_id = $1 AND c.phase = 'bodies'`, s.acc.id)
		if err != nil {
			return err
		}
		done, err := tx.Exec(ctx, `UPDATE accounts SET initial_sync_done_at = now() WHERE id = $1 AND initial_sync_done_at IS NULL`, s.acc.id)
		if err != nil {
			return err
		}
		for _, f := range s.folders {
			if f.phase == "bodies" {
				f.phase = "done"
			}
		}
		if done.RowsAffected() > 0 {
			s.acc.initialDone = true
			s.log.Info("initial sync complete")
		}
		if tag.RowsAffected() > 0 || done.RowsAffected() > 0 {
			return s.progress(ctx, tx, true)
		}
		return nil
	})
}
