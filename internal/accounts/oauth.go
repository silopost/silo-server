package accounts

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/emersion/go-sasl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/netx"
	"github.com/silopost/silo-server/internal/oauth"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
	"github.com/silopost/silo-server/internal/worker"
)

// ErrAccountMismatch means a re-authorization signed in with another address than
// the account's.
var ErrAccountMismatch = errors.New("accounts: the signed-in address is not the one of the account")

// AuthXOAuth2 is accounts.auth_type of OAuth2 accounts.
const AuthXOAuth2 = "xoauth2"

// OAuthAccount is an account signed in with OAuth2 (the callback of
// /oauth/{provider}).
type OAuthAccount struct {
	// Provider is accounts.provider: gmail or microsoft.
	Provider string
	// Email is the address, also the IMAP/SMTP user name.
	Email        string
	DisplayName  string
	RefreshToken string
	// AccessToken authenticates the connection test.
	AccessToken string
	IMAP, SMTP  oauth.Server
	SentAppend  bool
	// AccountID is the account to re-authorize (0: a new account, or the
	// existing one with the same server and user name).
	AccountID int64
}

// SaveOAuth tests the provider's servers with XOAUTH2, then creates the
// account (its initial sync queued) or, when it exists, stores the new
// refresh token and lets a failing account reconnect, as a new sign-in does
// for a password. It returns the account and whether it was created. A
// failed test creates nothing (*TestFailedError).
func (s *Service) SaveOAuth(ctx context.Context, userID int64, in OAuthAccount) (sqlcgen.Account, bool, error) {
	in.Email = strings.TrimSpace(in.Email)
	if in.Email == "" || in.RefreshToken == "" || in.AccessToken == "" {
		return sqlcgen.Account{}, false, fmt.Errorf("%w: email and tokens are required", ErrInvalid)
	}
	q := sqlcgen.New(s.o.Pool)
	var existing *sqlcgen.Account
	if in.AccountID != 0 {
		acc, err := s.Get(ctx, userID, in.AccountID)
		if err != nil {
			return acc, false, err
		}
		if !strings.EqualFold(acc.Username, in.Email) {
			return acc, false, ErrAccountMismatch
		}
		existing = &acc
	} else {
		acc, err := q.GetAccountByLogin(ctx, sqlcgen.GetAccountByLoginParams{UserID: userID, ImapHost: in.IMAP.Host, Username: in.Email})
		switch {
		case err == nil:
			existing = &acc
		case !errors.Is(err, pgx.ErrNoRows):
			return acc, false, err
		}
	}
	if existing == nil {
		n, err := q.CountAccountsByUser(ctx, userID)
		if err != nil {
			return sqlcgen.Account{}, false, err
		}
		if int(n) >= s.o.Config.MaxAccountsPerUser {
			return sqlcgen.Account{}, false, ErrLimitReached
		}
	}

	// The provider's servers are fixed by the instance (like its default
	// servers): no custom-server policy, dialled without the anti-SSRF filter.
	var eps [2]endpoint
	for i, srv := range []oauth.Server{in.IMAP, in.SMTP} {
		if err := s.checkTransport(srv.TLS); err != nil {
			return sqlcgen.Account{}, false, err
		}
		eps[i] = endpoint{
			Host: srv.Host, Port: srv.Port, TLS: srv.TLS, Username: in.Email, Insecure: s.o.Config.AllowInsecureTransport,
			Dial: (&netx.Dialer{Resolver: s.policy.resolver}).DialContext,
			SASL: func() sasl.Client { return oauth.NewXOAuth2Client(in.Email, in.AccessToken) },
		}
	}
	if res := runProbes(ctx, eps[0], eps[1]); !res.OK() {
		return sqlcgen.Account{}, false, &TestFailedError{Result: res}
	}

	tx, err := s.o.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return sqlcgen.Account{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tq := sqlcgen.New(tx)
	var id int64
	created := existing == nil
	if existing != nil {
		id = existing.ID
		// A password account of the same mailbox becomes an OAuth account.
		if _, err := tx.Exec(ctx, `
UPDATE accounts SET provider = $2, auth_type = 'xoauth2', imap_port = $3, imap_tls = $4, smtp_host = $5, smtp_port = $6, smtp_tls = $7
WHERE id = $1`, id, in.Provider, in.IMAP.Port, in.IMAP.TLS, in.SMTP.Host, in.SMTP.Port, in.SMTP.TLS); err != nil {
			return sqlcgen.Account{}, false, err
		}
		if err := s.seal(ctx, tq, id, in.RefreshToken); err != nil {
			return sqlcgen.Account{}, false, err
		}
		if existing.SyncState == auth.StateAuthFailed || existing.SyncState == auth.StateBackoff {
			if err := tq.SetAccountSyncState(ctx, sqlcgen.SetAccountSyncStateParams{ID: id, UserID: userID, SyncState: auth.StateConnecting}); err != nil {
				return sqlcgen.Account{}, false, err
			}
			if _, err := worker.Enqueue(ctx, tx, worker.NewJob{Kind: auth.JobAccountReconnect, AccountID: &id}); err != nil {
				return sqlcgen.Account{}, false, err
			}
		}
	} else {
		pos, err := tq.NextAccountPosition(ctx, userID)
		if err != nil {
			return sqlcgen.Account{}, false, err
		}
		acc, err := tq.InsertAccount(ctx, sqlcgen.InsertAccountParams{
			UserID: userID, Email: in.Email, DisplayName: strings.TrimSpace(in.DisplayName), Provider: in.Provider,
			ImapHost: in.IMAP.Host, ImapPort: int32(in.IMAP.Port), ImapTls: in.IMAP.TLS,
			SmtpHost: in.SMTP.Host, SmtpPort: int32(in.SMTP.Port), SmtpTls: in.SMTP.TLS,
			Username: in.Email, AuthType: AuthXOAuth2, SecretEnc: []byte{}, Position: pos,
		})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return acc, false, ErrExists
		}
		if err != nil {
			return acc, false, err
		}
		id = acc.ID
		if acc.SentAppend != in.SentAppend {
			if _, err := tx.Exec(ctx, `UPDATE accounts SET sent_append = $2 WHERE id = $1`, id, in.SentAppend); err != nil {
				return acc, false, err
			}
		}
		if err := s.seal(ctx, tq, id, in.RefreshToken); err != nil {
			return acc, false, err
		}
		if _, err := worker.Enqueue(ctx, tx, worker.NewJob{Kind: auth.JobAccountInitialSync, AccountID: &id}); err != nil {
			return acc, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return sqlcgen.Account{}, false, err
	}
	acc, err := s.Get(ctx, userID, id)
	return acc, created, err
}
