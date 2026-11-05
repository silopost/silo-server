// Package auth implements sign-in (mode imap), Silo sessions and the login
// rate limits (plan 3.15). HTTP concerns (cookies, CSRF) live in internal/http/api.
package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/imapx"
	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
	"github.com/silopost/silo-server/internal/worker"
)

// Session lifetimes (plan 3.15): sliding idle expiry and absolute cap.
const (
	SlidingTTL  = 30 * 24 * time.Hour
	AbsoluteTTL = 90 * 24 * time.Hour

	// Bearer sessions (native clients, ADR 0007) live longer: a phone that is
	// opened every few weeks must not sign out.
	BearerSlidingTTL  = 90 * 24 * time.Hour
	BearerAbsoluteTTL = 365 * 24 * time.Hour
)

// Session kinds (sessions.kind): the channel a session token is accepted on.
const (
	KindCookie = "cookie" // browser: silo_session cookie + CSRF header
	KindBearer = "bearer" // native client: Authorization: Bearer
)

// ttls returns the sliding and absolute lifetime of a session kind.
func ttls(kind string) (sliding, absolute time.Duration) {
	if kind == KindBearer {
		return BearerSlidingTTL, BearerAbsoluteTTL
	}
	return SlidingTTL, AbsoluteTTL
}

// Job kinds enqueued when an account must be (re)started. The account sync
// engine registers handlers for them.
const (
	JobAccountInitialSync = "account.initial_sync"
	JobAccountReconnect   = "account.reconnect"
)

// Account sync states (accounts.sync_state) used here.
const (
	StateConnecting = "connecting"
	StateBackoff    = "backoff"
	StateAuthFailed = "auth_failed"
	StateDisabled   = "disabled"
)

// Errors returned by the service.
var (
	// ErrInvalidCredentials is returned for every failed sign-in, whatever the cause.
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	// ErrUpstream means the mail server is unreachable and Silo cannot verify the user offline.
	ErrUpstream = errors.New("auth: mail server unavailable")
	// ErrUnauthenticated means the session token is missing, unknown or expired.
	ErrUnauthenticated = errors.New("auth: no valid session")
	// ErrNotFound is returned for sessions that do not exist or belong to someone else.
	ErrNotFound = errors.New("auth: not found")
)

// RateLimitError reports a throttled sign-in attempt.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("auth: rate limited, retry after %s", e.RetryAfter.Round(time.Second))
}

// Options configures a Service. Zero values select the defaults noted.
type Options struct {
	Pool   *pgxpool.Pool
	Keys   *secrets.Keyring
	Config config.Config
	Logger *slog.Logger

	// MinLoginDuration is the minimum time a sign-in attempt takes, so a fast
	// rejection looks like a slow one (default 1s).
	MinLoginDuration time.Duration
	// IMAPTimeout bounds the IMAP sign-in (default 15s).
	IMAPTimeout time.Duration
	// IPLimit and LoginLimit throttle POST /session per client IP and per login
	// (defaults: burst 20 refilled every 3s; burst 8 refilled every 15s).
	IPLimit    Limit
	LoginLimit Limit
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// Service signs users in and manages their sessions.
type Service struct {
	o        Options
	log      *slog.Logger
	hash     *hasher
	ipLim    *limiter
	loginLim *limiter
}

// New returns a Service.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MinLoginDuration == 0 {
		o.MinLoginDuration = time.Second
	}
	if o.IMAPTimeout == 0 {
		o.IMAPTimeout = imapx.DefaultDialTimeout
	}
	if o.IPLimit.Burst == 0 {
		o.IPLimit = Limit{Burst: 20, Every: 3 * time.Second}
	}
	if o.LoginLimit.Burst == 0 {
		o.LoginLimit = Limit{Burst: 8, Every: 15 * time.Second}
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		o: o, log: log.With("component", "auth"), hash: newHasher(),
		ipLim: newLimiter(o.IPLimit, o.Now), loginLim: newLimiter(o.LoginLimit, o.Now),
	}
}

// Client describes the caller of a sign-in.
type Client struct {
	IP        string // remote address, with or without port
	UserAgent string
	Kind      string // session kind, KindCookie when empty
	Name      string // device name given by a native client
}

// Login is the outcome of a successful sign-in.
type Login struct {
	Token   string // raw session token (cookie or bearer); never stored
	Session sqlcgen.Session
	User    sqlcgen.User
}

// Login verifies the mailbox credentials on the default IMAP server (or, when
// it is unreachable, against the offline verifier) and opens a session.
func (s *Service) Login(ctx context.Context, email, password string, c Client) (*Login, error) {
	email = strings.TrimSpace(email)
	if retry := s.throttle(c.IP, email); retry > 0 {
		return nil, &RateLimitError{RetryAfter: retry}
	}
	start := s.o.Now()
	// Every outcome takes at least MinLoginDuration (no timing oracle).
	defer func() {
		if wait := s.o.MinLoginDuration - s.o.Now().Sub(start); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
			}
		}
	}()

	addr, username, ok := s.loginIdentity(email)
	if !ok || password == "" || len(password) > 1024 {
		return nil, ErrInvalidCredentials
	}

	ierr := s.imapLogin(ctx, username, password)
	switch {
	case ierr == nil:
		return s.loginOnline(ctx, addr, username, password, c)
	case errors.Is(ierr, imapx.ErrAuthFailed):
		return nil, ErrInvalidCredentials
	case errors.Is(ierr, imapx.ErrUnavailable):
		s.log.Warn("imap login: server unavailable, trying offline verifier", "error", ierr)
		return s.loginOffline(ctx, addr, username, password, c)
	default:
		s.log.Warn("imap login failed", "error", ierr)
		return nil, fmt.Errorf("%w: %w", ErrUpstream, ierr)
	}
}

// throttle returns how long the caller must wait, or 0 when allowed.
func (s *Service) throttle(ip, login string) time.Duration {
	var wait time.Duration
	if ok, w := s.ipLim.allow(hostOnly(ip)); !ok {
		wait = w
	}
	if ok, w := s.loginLim.allow(strings.ToLower(login)); !ok && w > wait {
		wait = w
	}
	return wait
}

// loginIdentity maps the typed login to the sender address and IMAP username
// per SILO_DEFAULT_LOGIN_FORMAT.
func (s *Service) loginIdentity(in string) (addr, username string, ok bool) {
	if in == "" || len(in) > 254 || strings.ContainsAny(in, " \t\r\n") {
		return "", "", false
	}
	cfg := s.o.Config
	local, domain, hasAt := strings.Cut(in, "@")
	if local == "" || (hasAt && domain == "") || strings.Contains(domain, "@") {
		return "", "", false
	}
	if cfg.DefaultLoginFormat == config.LoginFormatLocalpart {
		if hasAt && !strings.EqualFold(domain, cfg.DefaultDomain) {
			return "", "", false
		}
		return local + "@" + cfg.DefaultDomain, local, true
	}
	if !hasAt {
		return "", "", false
	}
	return in, in, true
}

func (s *Service) imapLogin(ctx context.Context, username, password string) error {
	ctx, cancel := context.WithTimeout(ctx, s.o.IMAPTimeout)
	defer cancel()
	cfg := s.o.Config
	cl, err := imapx.Dial(ctx, cfg.DefaultIMAPHost, cfg.DefaultIMAPPort, imapx.TLSMode(cfg.DefaultIMAPTLS),
		cfg.AllowInsecureTransport, imapx.WithDialTimeout(s.o.IMAPTimeout))
	if err != nil {
		return err
	}
	defer cl.Close()
	return cl.Login(ctx, username, password)
}

// loginOnline handles a successful IMAP LOGIN: upsert user and account, refresh
// the offline verifier and open a session.
func (s *Service) loginOnline(ctx context.Context, addr, username, password string, c Client) (*Login, error) {
	pwHash, err := s.hash.hash(password)
	if err != nil {
		return nil, err
	}
	// Two first logins of the same user can race on the unique email.
	for attempt := 0; ; attempt++ {
		res, err := s.loginOnlineTx(ctx, addr, username, password, pwHash, c)
		var pgErr *pgconn.PgError
		if attempt == 0 && errors.As(err, &pgErr) && pgErr.Code == "23505" {
			continue
		}
		return res, err
	}
}

func (s *Service) loginOnlineTx(ctx context.Context, addr, username, password, pwHash string, c Client) (*Login, error) {
	cfg := s.o.Config
	var out *Login
	err := s.inTx(ctx, func(q *sqlcgen.Queries, tx pgx.Tx) error {
		user, err := q.GetUserByEmail(ctx, addr)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			user, err = q.InsertUser(ctx, sqlcgen.InsertUserParams{Email: addr, PasswordHash: pgtype.Text{String: pwHash, Valid: true}})
			if err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if err := q.UpdateUserPasswordHash(ctx, sqlcgen.UpdateUserPasswordHashParams{ID: user.ID, PasswordHash: pgtype.Text{String: pwHash, Valid: true}}); err != nil {
				return err
			}
		}

		acc, err := q.GetAccountByLogin(ctx, sqlcgen.GetAccountByLoginParams{UserID: user.ID, ImapHost: cfg.DefaultIMAPHost, Username: username})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			pos, err := q.NextAccountPosition(ctx, user.ID)
			if err != nil {
				return err
			}
			acc, err = q.InsertAccount(ctx, sqlcgen.InsertAccountParams{
				UserID: user.ID, Email: addr, Provider: "generic",
				ImapHost: cfg.DefaultIMAPHost, ImapPort: int32(cfg.DefaultIMAPPort), ImapTls: cfg.DefaultIMAPTLS,
				SmtpHost: cfg.DefaultSMTPHost, SmtpPort: int32(cfg.DefaultSMTPPort), SmtpTls: cfg.DefaultSMTPTLS,
				Username: username, AuthType: "password", SecretEnc: []byte{}, Position: pos,
			})
			if err != nil {
				return err
			}
			if err := s.sealSecret(ctx, q, acc.ID, password); err != nil {
				return err
			}
			if _, err := worker.Enqueue(ctx, tx, worker.NewJob{Kind: JobAccountInitialSync, AccountID: &acc.ID}); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			// The password may have changed on the server.
			if err := s.sealSecret(ctx, q, acc.ID, password); err != nil {
				return err
			}
			if acc.SyncState == StateAuthFailed || acc.SyncState == StateBackoff {
				if err := q.SetAccountSyncState(ctx, sqlcgen.SetAccountSyncStateParams{ID: acc.ID, UserID: user.ID, SyncState: StateConnecting}); err != nil {
					return err
				}
				if _, err := worker.Enqueue(ctx, tx, worker.NewJob{Kind: JobAccountReconnect, AccountID: &acc.ID}); err != nil {
					return err
				}
			}
		}

		out, err = s.newSession(ctx, q, user, c)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.cleanupExpired(ctx)
	return out, nil
}

// loginOffline serves the case where the mail server cannot be reached: a known
// user with the right password gets in and the account is marked as backing off.
func (s *Service) loginOffline(ctx context.Context, addr, username, password string, c Client) (*Login, error) {
	var out *Login
	err := s.inTx(ctx, func(q *sqlcgen.Queries, _ pgx.Tx) error {
		user, err := q.GetUserByEmail(ctx, addr)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUpstream
		}
		if err != nil {
			return err
		}
		if !user.PasswordHash.Valid || !s.hash.verify(password, user.PasswordHash.String) {
			return ErrInvalidCredentials
		}
		acc, err := q.GetAccountByLogin(ctx, sqlcgen.GetAccountByLoginParams{UserID: user.ID, ImapHost: s.o.Config.DefaultIMAPHost, Username: username})
		if err == nil && acc.SyncState != StateDisabled {
			err = q.SetAccountSyncState(ctx, sqlcgen.SetAccountSyncStateParams{
				ID: acc.ID, UserID: user.ID, SyncState: StateBackoff,
				SyncError: pgtype.Text{String: "mail server unavailable", Valid: true},
			})
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out, err = s.newSession(ctx, q, user, c)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) sealSecret(ctx context.Context, q *sqlcgen.Queries, accountID int64, password string) error {
	sealed, err := s.o.Keys.Encrypt([]byte(password), []byte(strconv.FormatInt(accountID, 10)))
	if err != nil {
		return err
	}
	return q.UpdateAccountSecret(ctx, sqlcgen.UpdateAccountSecretParams{ID: accountID, SecretEnc: sealed})
}

func (s *Service) newSession(ctx context.Context, q *sqlcgen.Queries, user sqlcgen.User, c Client) (*Login, error) {
	token, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	now := s.o.Now()
	var ip *netip.Addr
	if a, err := netip.ParseAddr(hostOnly(c.IP)); err == nil {
		a = a.Unmap()
		ip = &a
	}
	ua := c.UserAgent
	if len(ua) > 512 {
		ua = ua[:512]
	}
	kind := c.Kind
	if kind == "" {
		kind = KindCookie
	}
	sliding, _ := ttls(kind)
	sess, err := q.InsertSession(ctx, sqlcgen.InsertSessionParams{
		UserID: user.ID, TokenHash: hashToken(token), CsrfToken: csrf,
		ExpiresAt: pgtype.Timestamptz{Time: now.Add(sliding), Valid: true},
		UserAgent: ua, Ip: ip, Kind: kind, ClientName: c.Name,
	})
	if err != nil {
		return nil, err
	}
	return &Login{Token: token, Session: sess, User: user}, nil
}

func (s *Service) cleanupExpired(ctx context.Context) {
	if err := sqlcgen.New(s.o.Pool).DeleteExpiredSessions(ctx); err != nil {
		s.log.Warn("delete expired sessions", "error", err)
	}
}

func (s *Service) inTx(ctx context.Context, fn func(*sqlcgen.Queries, pgx.Tx) error) error {
	tx, err := s.o.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(sqlcgen.New(tx), tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Authenticate resolves a session token presented on the channel kind
// (KindCookie or KindBearer); a token issued for the other channel is rejected.
// It slides the idle expiry (at most once a minute) and enforces the absolute
// lifetime.
func (s *Service) Authenticate(ctx context.Context, token, kind string) (sqlcgen.Session, error) {
	if token == "" {
		return sqlcgen.Session{}, ErrUnauthenticated
	}
	q := sqlcgen.New(s.o.Pool)
	sess, err := q.GetSessionByTokenHash(ctx, hashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.Session{}, ErrUnauthenticated
	}
	if err != nil {
		return sqlcgen.Session{}, err
	}
	if sess.Kind != kind {
		return sqlcgen.Session{}, ErrUnauthenticated
	}
	now := s.o.Now()
	slidingTTL, _ := ttls(sess.Kind)
	absolute := AbsoluteExpiry(sess)
	if !sess.ExpiresAt.Time.After(now) || !absolute.After(now) {
		_, _ = q.DeleteSession(ctx, sqlcgen.DeleteSessionParams{ID: sess.ID, UserID: sess.UserID})
		return sqlcgen.Session{}, ErrUnauthenticated
	}
	next := now.Add(slidingTTL)
	if next.After(absolute) {
		next = absolute
	}
	if err := q.TouchSession(ctx, sqlcgen.TouchSessionParams{ID: sess.ID, UserID: sess.UserID, ExpiresAt: pgtype.Timestamptz{Time: next, Valid: true}}); err != nil {
		return sqlcgen.Session{}, err
	}
	return sess, nil
}

// User loads a user by id.
func (s *Service) User(ctx context.Context, id int64) (sqlcgen.User, error) {
	return sqlcgen.New(s.o.Pool).GetUserByID(ctx, id)
}

// Sessions lists the user's live sessions.
func (s *Service) Sessions(ctx context.Context, userID int64) ([]sqlcgen.Session, error) {
	return sqlcgen.New(s.o.Pool).ListSessionsByUser(ctx, userID)
}

// Revoke deletes one of the user's sessions.
func (s *Service) Revoke(ctx context.Context, userID, sessionID int64) error {
	n, err := sqlcgen.New(s.o.Pool).DeleteSession(ctx, sqlcgen.DeleteSessionParams{ID: sessionID, UserID: userID})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AbsoluteExpiry is when the session ends regardless of activity (and when the
// browser may drop the session cookie).
func AbsoluteExpiry(sess sqlcgen.Session) time.Time {
	_, absolute := ttls(sess.Kind)
	return sess.CreatedAt.Time.Add(absolute)
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}
