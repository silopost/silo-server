// Package admin implements the instance administration of the admin panel
// (plan 5.1, phase 5): users, the state of every mail account and the job
// queue. Callers must have checked that the acting user is an admin.
package admin

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
	"github.com/silopost/silo-server/internal/worker"
)

// Errors returned by the service.
var (
	ErrNotFound = errors.New("admin: not found")
	// ErrSelf refuses changes that would lock the acting admin out: removing
	// their own admin rights, disabling or deleting themselves.
	ErrSelf = errors.New("admin: cannot apply this change to yourself")
)

// Service administers the instance.
type Service struct {
	pool *pgxpool.Pool
	auth *auth.Service
}

// New returns a Service. Passwords and TOTP go through authSvc.
func New(pool *pgxpool.Pool, authSvc *auth.Service) *Service {
	return &Service{pool: pool, auth: authSvc}
}

// IsAdmin reports whether the user has admin rights.
func (s *Service) IsAdmin(ctx context.Context, userID int64) (bool, error) {
	u, err := sqlcgen.New(s.pool).GetUserByID(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return u.IsAdmin, err
}

// UserView is a user with their mail accounts.
type UserView struct {
	User     sqlcgen.User
	Accounts []sqlcgen.Account
}

// Users lists every user with their accounts.
func (s *Service) Users(ctx context.Context) ([]UserView, error) {
	q := sqlcgen.New(s.pool)
	users, err := q.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	accs, err := q.AdminListUserAccounts(ctx)
	if err != nil {
		return nil, err
	}
	byUser := map[int64][]sqlcgen.Account{}
	for _, a := range accs {
		byUser[a.UserID] = append(byUser[a.UserID], a)
	}
	out := make([]UserView, 0, len(users))
	for _, u := range users {
		out = append(out, UserView{User: u, Accounts: byUser[u.ID]})
	}
	return out, nil
}

// User returns one user with their accounts.
func (s *Service) User(ctx context.Context, id int64) (UserView, error) {
	q := sqlcgen.New(s.pool)
	u, err := q.GetUserByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserView{}, ErrNotFound
	}
	if err != nil {
		return UserView{}, err
	}
	accs, err := q.ListAccountsByUser(ctx, id)
	return UserView{User: u, Accounts: accs}, err
}

// CreateUser adds a user with a Silo password (mode local only). Without a
// password one is generated and returned (shown to the admin once).
func (s *Service) CreateUser(ctx context.Context, n auth.NewUser) (UserView, string, error) {
	if !s.auth.Local() {
		return UserView{}, "", auth.ErrNotAvailable
	}
	generated := ""
	if n.Password == "" {
		p, err := auth.GeneratePassword()
		if err != nil {
			return UserView{}, "", err
		}
		n.Password, generated = p, p
	}
	u, err := s.auth.CreateUser(ctx, n)
	if err != nil {
		return UserView{}, "", err
	}
	return UserView{User: u}, generated, nil
}

// UserPatch changes a user; nil fields stay as they are.
type UserPatch struct {
	Disabled    *bool
	IsAdmin     *bool
	DisplayName *string
	// Password sets a new Silo password (mode local) and signs the user out.
	Password *string
}

// UpdateUser applies p to user id on behalf of actor (whose current session
// actorSession survives a change of their own password).
//
// Disabling a user deletes all their sessions and sets their accounts to
// sync state disabled: the sync engine stops their workers (at its next
// reconcile, within 30 s) and refuses to restart them. Enabling sets the
// accounts back to connecting and queues a reconnect for each.
func (s *Service) UpdateUser(ctx context.Context, actor, actorSession, id int64, p UserPatch) (UserView, error) {
	if id == actor && ((p.Disabled != nil && *p.Disabled) || (p.IsAdmin != nil && !*p.IsAdmin)) {
		return UserView{}, ErrSelf
	}
	if p.Password != nil {
		if !s.auth.Local() {
			return UserView{}, auth.ErrNotAvailable
		}
		if len([]rune(*p.Password)) < auth.MinPasswordLen {
			return UserView{}, auth.ErrWeakPassword
		}
	}
	err := inTx(ctx, s.pool, func(q *sqlcgen.Queries, tx pgx.Tx) error {
		prev, err := q.LockUser(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		arg := sqlcgen.UpdateUserAdminParams{ID: id}
		if p.DisplayName != nil {
			arg.DisplayName = pgtype.Text{String: *p.DisplayName, Valid: true}
		}
		if p.IsAdmin != nil {
			arg.IsAdmin = pgtype.Bool{Bool: *p.IsAdmin, Valid: true}
		}
		if p.Disabled != nil {
			arg.Disabled = pgtype.Bool{Bool: *p.Disabled, Valid: true}
		}
		if _, err := q.UpdateUserAdmin(ctx, arg); err != nil {
			return err
		}
		switch {
		case p.Disabled == nil:
		case *p.Disabled:
			if _, err := q.DeleteUserSessions(ctx, id); err != nil {
				return err
			}
			if _, err := q.AdminSetUserAccountsDisabled(ctx, id); err != nil {
				return err
			}
		case prev.DisabledAt.Valid:
			ids, err := q.AdminReenableUserAccounts(ctx, id)
			if err != nil {
				return err
			}
			for _, accID := range ids {
				if _, err := worker.Enqueue(ctx, tx, worker.NewJob{Kind: auth.JobAccountReconnect, AccountID: &accID}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return UserView{}, err
	}
	if p.Password != nil {
		keep := int64(0)
		if id == actor {
			keep = actorSession
		}
		if err := s.auth.SetPassword(ctx, id, *p.Password, keep); err != nil {
			return UserView{}, err
		}
	}
	return s.User(ctx, id)
}

// ResetTOTP turns the user's TOTP off (lost device).
func (s *Service) ResetTOTP(ctx context.Context, id int64) error {
	if _, err := s.User(ctx, id); err != nil {
		return err
	}
	return s.auth.ResetTOTP(ctx, id)
}

// DeleteUser removes a user and, by cascade, their accounts, mirror metadata
// and sessions. The mail servers are not touched.
func (s *Service) DeleteUser(ctx context.Context, actor, id int64) error {
	if id == actor {
		return ErrSelf
	}
	n, err := sqlcgen.New(s.pool).DeleteUser(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Accounts lists every mail account with its state and queued operations.
func (s *Service) Accounts(ctx context.Context) ([]sqlcgen.AdminListAccountsRow, error) {
	return sqlcgen.New(s.pool).AdminListAccounts(ctx)
}

// JobFilter narrows Jobs; zero values do not filter.
type JobFilter struct {
	Status    string // "pending" or "dead"
	AccountID int64
}

// Jobs lists the newest 200 jobs matching f.
func (s *Service) Jobs(ctx context.Context, f JobFilter) ([]sqlcgen.AdminListJobsRow, error) {
	var arg sqlcgen.AdminListJobsParams
	if f.Status != "" {
		arg.Status = pgtype.Text{String: f.Status, Valid: true}
	}
	if f.AccountID != 0 {
		arg.AccountID = pgtype.Int8{Int64: f.AccountID, Valid: true}
	}
	return sqlcgen.New(s.pool).AdminListJobs(ctx, arg)
}

// RetryJob makes a dead or waiting job due now with a fresh attempt count
// and wakes the workers.
func (s *Service) RetryJob(ctx context.Context, id int64) (sqlcgen.AdminListJobsRow, error) {
	r, err := sqlcgen.New(s.pool).AdminRetryJob(ctx, id)
	j := sqlcgen.AdminListJobsRow(r)
	if errors.Is(err, pgx.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	payload := ""
	if j.AccountID.Valid {
		payload = strconv.FormatInt(j.AccountID.Int64, 10)
	}
	if _, err := s.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, worker.Channel, payload); err != nil {
		return j, fmt.Errorf("notify workers: %w", err)
	}
	return j, nil
}

// DeleteJob drops a job that is not running (a dead operation is discarded).
func (s *Service) DeleteJob(ctx context.Context, id int64) error {
	n, err := sqlcgen.New(s.pool).AdminDeleteJob(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(*sqlcgen.Queries, pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(sqlcgen.New(tx), tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
