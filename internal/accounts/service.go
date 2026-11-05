// Package accounts manages a user's mail accounts: listing, connection tests,
// creation, updates and removal (plan 3.15, 5.1).
package accounts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
	"github.com/silopost/silo-server/internal/worker"
)

// Errors returned by the service. Handlers map them to HTTP problems.
var (
	ErrNotFound            = errors.New("accounts: not found")
	ErrCustomServersDenied = errors.New("accounts: custom servers are disabled on this instance")
	ErrInvalid             = errors.New("accounts: invalid parameters")
	ErrLimitReached        = errors.New("accounts: account limit reached")
	ErrExists              = errors.New("accounts: account already exists")
)

// TestFailedError carries the per-step result of a failed connection test.
type TestFailedError struct{ Result TestResult }

func (*TestFailedError) Error() string { return "accounts: connection test failed" }

// Options configures a Service.
type Options struct {
	Pool   *pgxpool.Pool
	Keys   *secrets.Keyring
	Config config.Config
	Logger *slog.Logger
	// Resolver overrides DNS lookups of the anti-SSRF check (tests).
	Resolver resolver
}

// Service implements the accounts operations.
type Service struct {
	o      Options
	policy hostPolicy
	log    *slog.Logger
}

// New returns a Service.
func New(o Options) *Service {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	// Mode local has no implied mail server: users add accounts on any host,
	// each checked by the anti-SSRF policy (the default host, when configured,
	// stays trusted as in mode imap).
	if o.Config.AuthMode == config.AuthModeLocal {
		o.Config.AllowCustomServers = true
	}
	return &Service{o: o, policy: newHostPolicy(o.Config.CustomServerAllowlist, o.Resolver), log: log.With("component", "accounts")}
}

// Connection holds the server settings and credentials of an account to test
// or create. Empty hosts, ports and TLS modes fall back to the instance defaults.
type Connection struct {
	Email    string
	Username string // default: Email
	Password string
	IMAPHost string
	IMAPPort int
	IMAPTLS  string
	SMTPHost string
	SMTPPort int
	SMTPTLS  string
}

// Create adds a mail account after a successful connection test.
type Create struct {
	Connection
	DisplayName string
}

// Update changes an account; nil fields stay as they are.
type Update struct {
	DisplayName *string
	Password    *string
	IMAPPort    *int
	IMAPTLS     *string
	SMTPPort    *int
	SMTPTLS     *string
	SentAppend  *bool
}

// List returns the user's accounts in display order.
func (s *Service) List(ctx context.Context, userID int64) ([]sqlcgen.Account, error) {
	return sqlcgen.New(s.o.Pool).ListAccountsByUser(ctx, userID)
}

// Get returns one of the user's accounts (ErrNotFound for others').
func (s *Service) Get(ctx context.Context, userID, id int64) (sqlcgen.Account, error) {
	a, err := sqlcgen.New(s.o.Pool).GetAccount(ctx, sqlcgen.GetAccountParams{ID: id, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// Test connects to the IMAP and SMTP servers without storing anything.
func (s *Service) Test(ctx context.Context, c Connection) (TestResult, error) {
	imap, smtp, err := s.resolve(ctx, c)
	if err != nil {
		return TestResult{}, err
	}
	return runProbes(ctx, imap, smtp), nil
}

func runProbes(ctx context.Context, imap, smtp endpoint) TestResult {
	return TestResult{IMAP: probeIMAP(ctx, imap), SMTP: probeSMTP(ctx, smtp)}
}

// Create tests and stores a new account and queues its initial sync.
func (s *Service) Create(ctx context.Context, userID int64, in Create) (sqlcgen.Account, error) {
	c := in.Connection
	if strings.TrimSpace(c.Email) == "" {
		return sqlcgen.Account{}, fmt.Errorf("%w: email is required", ErrInvalid)
	}
	imap, smtp, err := s.resolve(ctx, c)
	if err != nil {
		return sqlcgen.Account{}, err
	}
	q := sqlcgen.New(s.o.Pool)
	n, err := q.CountAccountsByUser(ctx, userID)
	if err != nil {
		return sqlcgen.Account{}, err
	}
	if int(n) >= s.o.Config.MaxAccountsPerUser {
		return sqlcgen.Account{}, ErrLimitReached
	}
	if res := runProbes(ctx, imap, smtp); !res.OK() {
		return sqlcgen.Account{}, &TestFailedError{Result: res}
	}

	var acc sqlcgen.Account
	tx, err := s.o.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return acc, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tq := sqlcgen.New(tx)
	pos, err := tq.NextAccountPosition(ctx, userID)
	if err != nil {
		return acc, err
	}
	acc, err = tq.InsertAccount(ctx, sqlcgen.InsertAccountParams{
		UserID: userID, Email: c.Email, DisplayName: in.DisplayName, Provider: "generic",
		ImapHost: imap.Host, ImapPort: int32(imap.Port), ImapTls: imap.TLS,
		SmtpHost: smtp.Host, SmtpPort: int32(smtp.Port), SmtpTls: smtp.TLS,
		Username: imap.Username, AuthType: "password", SecretEnc: []byte{}, Position: pos,
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return acc, ErrExists
	}
	if err != nil {
		return acc, err
	}
	if err := s.seal(ctx, tq, acc.ID, c.Password); err != nil {
		return acc, err
	}
	if _, err := worker.Enqueue(ctx, tx, worker.NewJob{Kind: auth.JobAccountInitialSync, AccountID: &acc.ID}); err != nil {
		return acc, err
	}
	if err := tx.Commit(ctx); err != nil {
		return acc, err
	}
	return s.Get(ctx, userID, acc.ID)
}

// Update changes the editable settings of an account.
func (s *Service) Update(ctx context.Context, userID, id int64, in Update) (sqlcgen.Account, error) {
	for _, m := range []*string{in.IMAPTLS, in.SMTPTLS} {
		if m != nil {
			if err := s.checkTransport(*m); err != nil {
				return sqlcgen.Account{}, err
			}
		}
	}
	for _, p := range []*int{in.IMAPPort, in.SMTPPort} {
		if p != nil && (*p < 1 || *p > 65535) {
			return sqlcgen.Account{}, fmt.Errorf("%w: port out of range", ErrInvalid)
		}
	}
	if in.Password != nil && *in.Password == "" {
		return sqlcgen.Account{}, fmt.Errorf("%w: empty password", ErrInvalid)
	}

	tx, err := s.o.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return sqlcgen.Account{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	p := sqlcgen.UpdateAccountParams{ID: id, UserID: userID}
	if in.DisplayName != nil {
		p.DisplayName = pgtype.Text{String: strings.TrimSpace(*in.DisplayName), Valid: true}
	}
	if in.IMAPPort != nil {
		p.ImapPort = pgtype.Int4{Int32: int32(*in.IMAPPort), Valid: true}
	}
	if in.IMAPTLS != nil {
		p.ImapTls = pgtype.Text{String: *in.IMAPTLS, Valid: true}
	}
	if in.SMTPPort != nil {
		p.SmtpPort = pgtype.Int4{Int32: int32(*in.SMTPPort), Valid: true}
	}
	if in.SMTPTLS != nil {
		p.SmtpTls = pgtype.Text{String: *in.SMTPTLS, Valid: true}
	}
	if in.SentAppend != nil {
		p.SentAppend = pgtype.Bool{Bool: *in.SentAppend, Valid: true}
	}
	acc, err := q.UpdateAccount(ctx, p)
	if errors.Is(err, pgx.ErrNoRows) {
		return acc, ErrNotFound
	}
	if err != nil {
		return acc, err
	}
	if in.Password != nil {
		if err := s.seal(ctx, q, acc.ID, *in.Password); err != nil {
			return acc, err
		}
		// A new password is the fix for auth_failed: let the sync retry.
		if acc.SyncState == auth.StateAuthFailed {
			if err := q.SetAccountSyncState(ctx, sqlcgen.SetAccountSyncStateParams{ID: acc.ID, UserID: userID, SyncState: auth.StateConnecting}); err != nil {
				return acc, err
			}
			if _, err := worker.Enqueue(ctx, tx, worker.NewJob{Kind: auth.JobAccountReconnect, AccountID: &acc.ID}); err != nil {
				return acc, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return acc, err
	}
	return s.Get(ctx, userID, id)
}

// Delete removes the account and, by cascade, everything cached for it. The
// mail server is not touched.
func (s *Service) Delete(ctx context.Context, userID, id int64) error {
	n, err := sqlcgen.New(s.o.Pool).DeleteAccount(ctx, sqlcgen.DeleteAccountParams{ID: id, UserID: userID})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) seal(ctx context.Context, q *sqlcgen.Queries, accountID int64, password string) error {
	sealed, err := s.o.Keys.Encrypt([]byte(password), []byte(strconv.FormatInt(accountID, 10)))
	if err != nil {
		return err
	}
	return q.UpdateAccountSecret(ctx, sqlcgen.UpdateAccountSecretParams{ID: accountID, SecretEnc: sealed})
}

// resolve fills defaults, validates the request against the instance policy and
// returns the IMAP and SMTP endpoints to contact.
func (s *Service) resolve(ctx context.Context, c Connection) (imap, smtp endpoint, err error) {
	cfg := s.o.Config
	c.Email = strings.TrimSpace(c.Email)
	if c.Username = strings.TrimSpace(c.Username); c.Username == "" {
		c.Username = c.Email
	}
	if c.Username == "" || c.Password == "" {
		return imap, smtp, fmt.Errorf("%w: username and password are required", ErrInvalid)
	}
	imap = endpoint{Host: or(c.IMAPHost, cfg.DefaultIMAPHost), Port: orInt(c.IMAPPort, cfg.DefaultIMAPPort), TLS: or(c.IMAPTLS, cfg.DefaultIMAPTLS)}
	smtp = endpoint{Host: or(c.SMTPHost, cfg.DefaultSMTPHost), Port: orInt(c.SMTPPort, cfg.DefaultSMTPPort), TLS: or(c.SMTPTLS, cfg.DefaultSMTPTLS)}
	// Without an explicit SMTP host, a custom IMAP host implies the same SMTP host.
	if c.SMTPHost == "" && c.IMAPHost != "" {
		smtp.Host = c.IMAPHost
	}
	for _, e := range []*endpoint{&imap, &smtp} {
		e.Host = strings.TrimSpace(e.Host)
		e.Username, e.Password, e.Insecure = c.Username, c.Password, cfg.AllowInsecureTransport
		if e.Host == "" || e.Port < 1 || e.Port > 65535 {
			return imap, smtp, fmt.Errorf("%w: host and port are required", ErrInvalid)
		}
		if err := s.checkTransport(e.TLS); err != nil {
			return imap, smtp, err
		}
	}
	for _, h := range []struct{ got, def string }{{imap.Host, cfg.DefaultIMAPHost}, {smtp.Host, cfg.DefaultSMTPHost}} {
		if strings.EqualFold(h.got, h.def) {
			continue
		}
		if !cfg.AllowCustomServers {
			return imap, smtp, ErrCustomServersDenied
		}
		if err := s.policy.check(ctx, h.got); err != nil {
			if errors.Is(err, ErrHostNotAllowed) {
				return imap, smtp, err
			}
			return imap, smtp, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	return imap, smtp, nil
}

func (s *Service) checkTransport(mode string) error {
	switch mode {
	case config.TLSImplicit, config.TLSStartTLS:
		return nil
	case config.TLSNone:
		if s.o.Config.AllowInsecureTransport {
			return nil
		}
		return fmt.Errorf("%w: unencrypted transport is disabled on this instance", ErrInvalid)
	}
	return fmt.Errorf("%w: tls must be implicit, starttls or none", ErrInvalid)
}

func or(v, def string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

func orInt(v, def int) int {
	if v != 0 {
		return v
	}
	return def
}
