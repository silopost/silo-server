package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
)

// errAccountGone stops a worker whose account was deleted or disabled.
var errAccountGone = errors.New("sync: account deleted or disabled")

type account struct {
	id, userID     int64
	email          string
	host           string
	port           int
	tls            string
	username       string
	secret         []byte
	mapping        map[string]string
	state          string
	initialDone    bool
	keywordsSupp   bool
	capabilities   []string
	filtersSinceOK bool
}

func loadAccount(ctx context.Context, db mail.DB, id int64) (*account, error) {
	a := &account{id: id}
	var mapping []byte
	var initialDone, filtersSince *time.Time
	err := db.QueryRow(ctx, `
SELECT user_id, email::text, imap_host, imap_port, imap_tls, username, secret_enc, folder_mapping, sync_state,
       initial_sync_done_at, keywords_supported, capabilities, filters_since
FROM accounts WHERE id = $1`, id).Scan(&a.userID, &a.email, &a.host, &a.port, &a.tls, &a.username, &a.secret,
		&mapping, &a.state, &initialDone, &a.keywordsSupp, &a.capabilities, &filtersSince)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errAccountGone
	}
	if err != nil {
		return nil, fmt.Errorf("sync: load account %d: %w", id, err)
	}
	a.initialDone, a.filtersSinceOK = initialDone != nil, filtersSince != nil
	a.mapping = map[string]string{}
	if len(mapping) > 0 {
		var raw map[string]any
		if err := json.Unmarshal(mapping, &raw); err == nil {
			for k, v := range raw {
				if s, ok := v.(string); ok {
					a.mapping[k] = s
				}
			}
		}
	}
	return a, nil
}

// folder is a mailbox_folders row with its sync cursor.
type folder struct {
	id            int64
	name          string
	delim         string
	specialUse    string
	role          string
	labelID       *int64
	uidValidity   uint32 // 0 = never selected
	uidNext       uint32 // incremental sync covers UIDs >= uidNext
	highestModSeq uint64
	messages      int
	syncMode      string
	lastSyncAt    *time.Time
	lastFullFlags *time.Time
	// cursor (zero when the folder was never initialised)
	phase  string
	lowUID uint32 // header backfill covers UIDs < lowUID
}

func (f *folder) synced() bool { return f.syncMode == "sync" }

// headersDone reports that the header backfill of the folder is complete.
func (f *folder) headersDone() bool { return f.phase == "bodies" || f.phase == "done" }

// coverLow is the lowest UID that the incremental sync takes responsibility
// for (UIDs below are the header backfill's).
func (f *folder) coverLow() uint32 {
	if f.uidValidity == 0 {
		return ^uint32(0)
	}
	if f.headersDone() || f.lowUID == 0 {
		return 1
	}
	return f.lowUID
}

func loadFolders(ctx context.Context, db mail.DB, accountID int64) ([]*folder, error) {
	rows, err := db.Query(ctx, `
SELECT f.id, f.name, COALESCE(f.delimiter, ''), COALESCE(f.special_use, ''), f.role, f.label_id, f.uidvalidity,
       f.uidnext, COALESCE(f.highestmodseq, 0), f.messages_count, f.sync_mode, f.last_sync_at, f.last_full_flags_at,
       COALESCE(c.phase, ''), COALESCE(c.low_uid, 0)
FROM mailbox_folders f LEFT JOIN sync_cursors c ON c.folder_id = f.id
WHERE f.account_id = $1 AND f.selectable
ORDER BY f.id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("sync: load folders: %w", err)
	}
	defer rows.Close()
	var out []*folder
	for rows.Next() {
		f := &folder{}
		var uv, un, low, hms int64
		if err := rows.Scan(&f.id, &f.name, &f.delim, &f.specialUse, &f.role, &f.labelID, &uv, &un, &hms, &f.messages,
			&f.syncMode, &f.lastSyncAt, &f.lastFullFlags, &f.phase, &low); err != nil {
			return nil, err
		}
		f.uidValidity, f.uidNext, f.lowUID, f.highestModSeq = uint32(uv), uint32(un), uint32(low), uint64(hms)
		out = append(out, f)
	}
	return out, rows.Err()
}

// setState writes accounts.sync_state / sync_error and the account.status
// event (account.error too when errCode is set). A disabled or deleted
// account yields errAccountGone.
func setState(ctx context.Context, d *Deps, accountID int64, state, errCode, detail string) error {
	return inTx(ctx, d.Pool, func(tx pgx.Tx) error {
		var userID int64
		var prev string
		var lastSync *time.Time
		err := tx.QueryRow(ctx, `SELECT user_id, sync_state, last_sync_at FROM accounts WHERE id = $1 FOR UPDATE`, accountID).
			Scan(&userID, &prev, &lastSync)
		if errors.Is(err, pgx.ErrNoRows) {
			return errAccountGone
		}
		if err != nil {
			return err
		}
		if prev == StateDisabled {
			return errAccountGone
		}
		var errText *string
		if detail != "" {
			errText = &detail
		}
		if _, err := tx.Exec(ctx, `UPDATE accounts SET sync_state = $2, sync_error = $3 WHERE id = $1`, accountID, state, errText); err != nil {
			return err
		}
		if prev != state {
			if err := mail.WriteEvent(ctx, tx, userID, EventAccountStatus, map[string]any{
				"accountId": accountID, "state": state, "lastSyncAt": lastSync,
			}); err != nil {
				return err
			}
		}
		if errCode != "" {
			return mail.WriteEvent(ctx, tx, userID, EventAccountError, map[string]any{
				"accountId": accountID, "code": errCode, "detail": detail,
			})
		}
		return nil
	})
}

// folderMessageIDs returns the distinct messages located in a folder.
func folderMessageIDs(ctx context.Context, db mail.DB, folderID int64) ([]int64, error) {
	rows, err := db.Query(ctx, `SELECT DISTINCT message_id FROM message_locations WHERE folder_id = $1`, folderID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// rederive recomputes labels and threads of the given messages after a
// folder changed role (discovery or PUT /folders).
func rederive(ctx context.Context, db mail.DB, msgIDs []int64) error {
	if len(msgIDs) == 0 {
		return nil
	}
	if err := mail.DeriveSystemLabelsBatch(ctx, db, msgIDs); err != nil {
		return err
	}
	rows, err := db.Query(ctx, `SELECT DISTINCT thread_id FROM messages WHERE id = ANY($1)`, msgIDs)
	if err != nil {
		return err
	}
	threads, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	return mail.RecomputeThread(ctx, db, threads...)
}

// vanishAll runs the disappearance procedure for every location of a folder
// (or only the stale ones), in chunks.
func vanishAll(ctx context.Context, db mail.DB, folderID int64, onlyStale bool) (*mail.VanishedResult, error) {
	q := `SELECT uid FROM message_locations WHERE folder_id = $1`
	if onlyStale {
		q += ` AND stale`
	}
	rows, err := db.Query(ctx, q, folderID)
	if err != nil {
		return nil, err
	}
	uids64, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	total := &mail.VanishedResult{}
	for len(uids64) > 0 {
		n := min(len(uids64), flagsBatch)
		chunk := make([]uint32, n)
		for i, u := range uids64[:n] {
			chunk[i] = uint32(u)
		}
		uids64 = uids64[n:]
		r, err := mail.LocationVanished(ctx, db, folderID, chunk)
		if err != nil {
			return nil, err
		}
		total.Removed += r.Removed
		total.Orphaned += r.Orphaned
	}
	return total, nil
}
