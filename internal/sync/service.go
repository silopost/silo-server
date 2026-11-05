package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/worker"
)

// Errors of the Service.
var (
	ErrNotFound       = errors.New("sync: not found")
	ErrInvalid        = errors.New("sync: invalid request")
	// ErrRebuildBlocked wraps mail.ErrRebuildBlocked with the reason.
	ErrRebuildBlocked = mail.ErrRebuildBlocked
)

// Service backs the sync endpoints of the API. It never talks to IMAP:
// requests that need the server become jobs for the account's worker.
type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service.
func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// FolderInfo is one folder of GET /accounts/{id}/folders and sync-status.
type FolderInfo struct {
	ID         int64
	Name       string
	Role       string
	SpecialUse string
	Messages   int
	Unread     int
	SyncMode   string
	LastSyncAt *time.Time
	Error      *string
	ErrorCount int
}

// Status is GET /accounts/{id}/sync-status.
type Status struct {
	AccountID         int64
	Email             string
	State             string
	LastSyncAt        *time.Time
	Error             *string
	Capabilities      []string
	KeywordsSupported bool
	InitialSync       Progress
	InitialSyncDoneAt *time.Time
	PendingOps        int
	DeadOps           int
	// MissingRoles lists roles without a folder that Silo will need (archive:
	// created by the write-back of the first archive operation, next slice).
	MissingRoles []string
	Folders      []FolderInfo
}

func (s *Service) checkAccount(ctx context.Context, db mail.DB, userID, accountID int64) error {
	var ok bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM accounts WHERE id = $1 AND user_id = $2)`, accountID, userID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// Status returns the sync state of an account of the user.
func (s *Service) Status(ctx context.Context, userID, accountID int64) (*Status, error) {
	st := &Status{AccountID: accountID}
	err := s.pool.QueryRow(ctx, `
SELECT email::text, sync_state, last_sync_at, sync_error, capabilities, keywords_supported, initial_sync_done_at
FROM accounts WHERE id = $1 AND user_id = $2`, accountID, userID).
		Scan(&st.Email, &st.State, &st.LastSyncAt, &st.Error, &st.Capabilities, &st.KeywordsSupported, &st.InitialSyncDoneAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if st.InitialSync, err = loadProgress(ctx, s.pool, accountID); err != nil {
		return nil, err
	}
	// Write-back (imap_op) arrives with the next slice; counted already.
	if err := s.pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE status = 'pending' AND kind = 'imap_op')::int, count(*) FILTER (WHERE status = 'dead')::int
FROM jobs WHERE account_id = $1 AND EXISTS (SELECT 1 FROM accounts a WHERE a.id = $1 AND a.user_id = $2)`, accountID, userID).
		Scan(&st.PendingOps, &st.DeadOps); err != nil {
		return nil, err
	}
	if st.Folders, err = s.folders(ctx, s.pool, userID, accountID); err != nil {
		return nil, err
	}
	hasArchive := false
	for _, f := range st.Folders {
		hasArchive = hasArchive || f.Role == RoleArchive
	}
	st.MissingRoles = []string{}
	if !hasArchive {
		st.MissingRoles = append(st.MissingRoles, RoleArchive)
	}
	if st.Capabilities == nil {
		st.Capabilities = []string{}
	}
	return st, nil
}

// Folders lists the folders of an account with roles, counts and sync mode.
func (s *Service) Folders(ctx context.Context, userID, accountID int64) ([]FolderInfo, error) {
	if err := s.checkAccount(ctx, s.pool, userID, accountID); err != nil {
		return nil, err
	}
	return s.folders(ctx, s.pool, userID, accountID)
}

func (s *Service) folders(ctx context.Context, db mail.DB, userID, accountID int64) ([]FolderInfo, error) {
	rows, err := db.Query(ctx, `
SELECT f.id, f.name, f.role, COALESCE(f.special_use, ''), f.messages_count,
       (SELECT count(*) FROM message_locations l WHERE l.folder_id = f.id AND NOT ('\Seen' = ANY(l.flags)))::int,
       f.sync_mode, f.last_sync_at, f.last_error, f.error_count
FROM mailbox_folders f
WHERE f.account_id = $1 AND f.user_id = $2 AND f.selectable
ORDER BY CASE f.role WHEN 'inbox' THEN 0 WHEN 'user' THEN 2 ELSE 1 END, lower(f.name)`, accountID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FolderInfo{}
	for rows.Next() {
		var f FolderInfo
		if err := rows.Scan(&f.ID, &f.Name, &f.Role, &f.SpecialUse, &f.Messages, &f.Unread, &f.SyncMode, &f.LastSyncAt, &f.Error, &f.ErrorCount); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FolderUpdate is one entry of PUT /accounts/{id}/folders; nil fields stay.
type FolderUpdate struct {
	ID       int64
	Role     *string
	SyncMode *string
}

// UpdateFolders applies a manual role mapping and sync modes (plan 3.4: the
// manual mapping always wins over SPECIAL-USE and names). Every non-user role
// may be held by one folder; INBOX keeps its role and cannot be ignored. The
// mapping is stored in accounts.folder_mapping (an empty name means "no
// folder for this role"), labels of re-roled folders are re-derived, folders
// switched to ignore leave the mirror (their locations vanish), and the
// worker is asked to rediscover and resync.
func (s *Service) UpdateFolders(ctx context.Context, userID, accountID int64, updates []FolderUpdate) ([]FolderInfo, error) {
	var out []FolderInfo
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM accounts WHERE id = $1 AND user_id = $2 FOR UPDATE`, accountID, userID); err != nil {
			return err
		}
		if err := s.checkAccount(ctx, tx, userID, accountID); err != nil {
			return err
		}
		cur, err := s.folders(ctx, tx, userID, accountID)
		if err != nil {
			return err
		}
		byID := map[int64]*FolderInfo{}
		for i := range cur {
			byID[cur[i].ID] = &cur[i]
		}
		roleChanged := map[int64]bool{}
		modeChanged := map[int64]bool{}
		for _, u := range updates {
			f := byID[u.ID]
			if f == nil {
				return fmt.Errorf("%w: unknown folder %d", ErrInvalid, u.ID)
			}
			if u.Role != nil && *u.Role != f.Role {
				if f.Role == RoleInbox || *u.Role == RoleInbox {
					return fmt.Errorf("%w: the INBOX role cannot be changed", ErrInvalid)
				}
				if *u.Role != RoleUser && !slices.Contains(MappableRoles, *u.Role) {
					return fmt.Errorf("%w: unknown role %q", ErrInvalid, *u.Role)
				}
				f.Role, roleChanged[f.ID] = *u.Role, true
			}
			if u.SyncMode != nil && *u.SyncMode != f.SyncMode {
				if *u.SyncMode != "sync" && *u.SyncMode != "ignore" {
					return fmt.Errorf("%w: syncMode must be sync or ignore", ErrInvalid)
				}
				if f.Role == RoleInbox && *u.SyncMode == "ignore" {
					return fmt.Errorf("%w: INBOX cannot be ignored", ErrInvalid)
				}
				f.SyncMode, modeChanged[f.ID] = *u.SyncMode, true
			}
		}
		holder := map[string]string{}
		for _, f := range cur {
			if f.Role == RoleUser || f.Role == RoleInbox {
				continue
			}
			if other, dup := holder[f.Role]; dup {
				return fmt.Errorf("%w: role %s given to both %q and %q", ErrInvalid, f.Role, other, f.Name)
			}
			holder[f.Role] = f.Name
		}
		if len(roleChanged) > 0 {
			mapping := map[string]string{}
			for _, r := range MappableRoles {
				mapping[r] = holder[r]
			}
			raw, _ := json.Marshal(mapping)
			if _, err := tx.Exec(ctx, `UPDATE accounts SET folder_mapping = $3 WHERE id = $1 AND user_id = $2`, accountID, userID, raw); err != nil {
				return err
			}
		}
		for id := range roleChanged {
			f := byID[id]
			if _, err := tx.Exec(ctx, `UPDATE mailbox_folders SET role = $3 WHERE id = $1 AND user_id = $2`, id, userID, f.Role); err != nil {
				return err
			}
			if f.Role == RoleUser {
				if _, err := mail.EnsureFolderLabel(ctx, tx, id); err != nil {
					return err
				}
			}
			ids, err := folderMessageIDs(ctx, tx, id)
			if err != nil {
				return err
			}
			if err := rederive(ctx, tx, ids); err != nil {
				return err
			}
		}
		for id := range modeChanged {
			f := byID[id]
			if _, err := tx.Exec(ctx, `UPDATE mailbox_folders SET sync_mode = $3 WHERE id = $1 AND user_id = $2`, id, userID, f.SyncMode); err != nil {
				return err
			}
			if f.SyncMode == "ignore" {
				if _, err := vanishAll(ctx, tx, id, false); err != nil {
					return err
				}
				// Re-enabling starts the folder from scratch.
				if _, err := tx.Exec(ctx, `UPDATE mailbox_folders SET uidvalidity = 0, uidnext = 1, highestmodseq = NULL WHERE id = $1 AND user_id = $2`, id, userID); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `DELETE FROM sync_cursors WHERE folder_id = $1`, id); err != nil {
					return err
				}
			}
		}
		if len(roleChanged)+len(modeChanged) > 0 {
			if _, err := worker.Enqueue(ctx, tx, worker.NewJob{Kind: JobResync, AccountID: &accountID, Payload: map[string]string{"mode": ResyncIncremental}}); err != nil {
				return err
			}
		}
		out, err = s.folders(ctx, tx, userID, accountID)
		return err
	})
	return out, err
}

// RequestResync enqueues a resync for the account's worker: incremental (all
// folders now), full (also a full flag comparison) or rebuild (drop the cache
// and run the initial sync from scratch, plan 3.3; see mail.WipeCache for what
// stays). A rebuild is refused with ErrRebuildBlocked while sends are in
// flight or operations wait for write-back.
func (s *Service) RequestResync(ctx context.Context, userID, accountID int64, mode string) error {
	switch mode {
	case ResyncIncremental, ResyncFull, ResyncRebuild:
	default:
		return fmt.Errorf("%w: mode must be incremental, full or rebuild", ErrInvalid)
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.checkAccount(ctx, tx, userID, accountID); err != nil {
			return err
		}
		if mode == ResyncRebuild {
			if err := mail.LockAccount(ctx, tx, accountID); err != nil {
				return err
			}
			if err := RequestRebuild(ctx, tx, accountID); err != nil {
				return err
			}
		}
		_, err := worker.Enqueue(ctx, tx, worker.NewJob{Kind: JobResync, AccountID: &accountID, Payload: map[string]string{"mode": mode}})
		return err
	})
}

// RequestRebuild records a cache rebuild of an account (the caller holds the
// account lock and enqueues the job that wakes the worker; the CLI
// rebuild-cache shares it): ErrRebuildBlocked while sends are in flight or
// operations wait for write-back.
func RequestRebuild(ctx context.Context, tx pgx.Tx, accountID int64) error {
	why, err := mail.RebuildBlocker(ctx, tx, accountID)
	if err != nil {
		return err
	}
	if why != "" {
		return fmt.Errorf("%w: %s", ErrRebuildBlocked, why)
	}
	return mail.RequestRebuild(ctx, tx, accountID)
}

// RebuildAccount is RequestResync rebuild without a user (CLI rebuild-cache).
func RebuildAccount(ctx context.Context, pool *pgxpool.Pool, accountID int64) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM accounts WHERE id = $1)`, accountID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		if err := mail.LockAccount(ctx, tx, accountID); err != nil {
			return err
		}
		if err := RequestRebuild(ctx, tx, accountID); err != nil {
			return err
		}
		_, err := worker.Enqueue(ctx, tx, worker.NewJob{Kind: JobResync, AccountID: &accountID, Payload: map[string]string{"mode": ResyncRebuild}})
		return err
	})
}
