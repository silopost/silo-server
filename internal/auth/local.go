package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

// MinPasswordLen is the shortest Silo password accepted (mode local).
const MinPasswordLen = 10

// Errors of mode local, TOTP and user management.
var (
	// ErrTOTPRequired means the password was right but the user has TOTP on
	// and sent no code: the client asks for one and signs in again.
	ErrTOTPRequired = errors.New("auth: totp code required")
	// ErrNotAvailable means the operation does not exist in the configured
	// SILO_AUTH_MODE (e.g. a Silo password in mode imap).
	ErrNotAvailable = errors.New("auth: not available in this auth mode")
	// ErrWeakPassword means a new password is shorter than MinPasswordLen.
	ErrWeakPassword = errors.New("auth: password too short")
	// ErrUserExists means a user with that email exists.
	ErrUserExists = errors.New("auth: user exists")
	// ErrInvalidEmail means an email is not a plausible address.
	ErrInvalidEmail = errors.New("auth: invalid email")
)

// dummyHash is verified instead of a real one when the user is unknown or has
// no password, so that every sign-in costs one argon2id run with our
// parameters (no timing oracle on which emails exist). Its key matches nothing.
const dummyHash = "$argon2id$v=19$m=65536,t=1,p=4$c2lsby1kdW1teS1zYWx0IQ$" +
	"ZHVtbXkga2V5IHRoYXQgbWF0Y2hlcyBub3RoaW5nISE"

// Credentials are what POST /session carries.
type Credentials struct {
	Email    string
	Password string
	// TOTPCode is a 6-digit TOTP code or a backup code (mode local, users
	// with TOTP enabled).
	TOTPCode string
}

// Local reports whether the instance runs SILO_AUTH_MODE=local.
func (s *Service) Local() bool { return s.o.Config.AuthMode == config.AuthModeLocal }

// SignIn signs a user in according to SILO_AUTH_MODE: with the mailbox
// credentials (imap, see Login) or with the Silo password and, when enabled,
// a TOTP code (local).
func (s *Service) SignIn(ctx context.Context, cr Credentials, c Client) (*Login, error) {
	if !s.Local() {
		return s.Login(ctx, cr.Email, cr.Password, c)
	}
	return s.loginLocal(ctx, cr, c)
}

// loginLocal verifies the Silo password (argon2id) and the second factor. It
// never creates users or accounts. Unknown, password-less and disabled users
// get ErrInvalidCredentials after the same work as a wrong password.
func (s *Service) loginLocal(ctx context.Context, cr Credentials, c Client) (*Login, error) {
	email := strings.TrimSpace(cr.Email)
	// TOTP attempts take tokens from the same per-login bucket as passwords.
	if retry := s.throttle(c.IP, email); retry > 0 {
		return nil, &RateLimitError{RetryAfter: retry}
	}
	start := s.o.Now()
	defer s.padLogin(ctx, start)

	if len(cr.Password) > 1024 {
		return nil, ErrInvalidCredentials
	}
	q := sqlcgen.New(s.o.Pool)
	user, err := q.GetUserByEmail(ctx, email)
	known := err == nil && user.PasswordHash.Valid
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	hash := dummyHash
	if known {
		hash = user.PasswordHash.String
	}
	ok := s.hash.verify(cr.Password, hash)
	if !ok || !known || cr.Password == "" || user.DisabledAt.Valid {
		return nil, ErrInvalidCredentials
	}
	if user.TotpEnabledAt.Valid && strings.TrimSpace(cr.TOTPCode) == "" {
		return nil, ErrTOTPRequired
	}

	var out *Login
	err = s.inTx(ctx, func(q *sqlcgen.Queries, _ pgx.Tx) error {
		locked, err := q.LockUser(ctx, user.ID)
		if err != nil {
			return err
		}
		if locked.TotpEnabledAt.Valid {
			ok, err := s.checkSecondFactor(ctx, q, locked, cr.TOTPCode)
			if err != nil {
				return err
			}
			if !ok {
				return ErrInvalidCredentials
			}
		}
		out, err = s.newSession(ctx, q, locked, c)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.cleanupExpired(ctx)
	return out, nil
}

// padLogin sleeps until MinLoginDuration has passed since start.
func (s *Service) padLogin(ctx context.Context, start time.Time) {
	if wait := s.o.MinLoginDuration - s.o.Now().Sub(start); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
}

// ChangePassword sets a new Silo password after checking the current one
// (mode local) and signs the user out everywhere except keepSession.
func (s *Service) ChangePassword(ctx context.Context, userID, keepSession int64, current, next string) error {
	if !s.Local() {
		return ErrNotAvailable
	}
	if len([]rune(next)) < MinPasswordLen {
		return ErrWeakPassword
	}
	user, err := s.User(ctx, userID)
	if err != nil {
		return err
	}
	if err := s.reauth(user, current); err != nil {
		return err
	}
	return s.setPassword(ctx, userID, next, keepSession)
}

// SetPassword replaces a user's Silo password (admin panel, CLI) and ends
// all their sessions except keepSession (0: all).
func (s *Service) SetPassword(ctx context.Context, userID int64, password string, keepSession int64) error {
	if len([]rune(password)) < MinPasswordLen {
		return ErrWeakPassword
	}
	return s.setPassword(ctx, userID, password, keepSession)
}

func (s *Service) setPassword(ctx context.Context, userID int64, password string, keepSession int64) error {
	h, err := s.hash.hash(password)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(q *sqlcgen.Queries, _ pgx.Tx) error {
		if err := q.SetUserPassword(ctx, sqlcgen.SetUserPasswordParams{ID: userID, PasswordHash: pgtype.Text{String: h, Valid: true}}); err != nil {
			return err
		}
		if keepSession == 0 {
			_, err = q.DeleteUserSessions(ctx, userID)
		} else {
			_, err = q.DeleteOtherUserSessions(ctx, sqlcgen.DeleteOtherUserSessionsParams{UserID: userID, ID: keepSession})
		}
		return err
	})
}

// reauth checks the password of a signed-in user before a sensitive change.
// It is throttled like a sign-in of the same login.
func (s *Service) reauth(user sqlcgen.User, password string) error {
	if ok, w := s.loginLim.allow(strings.ToLower(user.Email)); !ok {
		return &RateLimitError{RetryAfter: w}
	}
	if !user.PasswordHash.Valid || len(password) > 1024 || !s.hash.verify(password, user.PasswordHash.String) {
		return ErrInvalidCredentials
	}
	return nil
}

// NewUser describes a user created by an admin (panel or CLI).
type NewUser struct {
	Email       string
	DisplayName string
	IsAdmin     bool
	// Password is the Silo password (mode local, at least MinPasswordLen);
	// empty stores none (mode imap: the user signs in with the mailbox).
	Password string
}

// CreateUser adds a user. It creates no mail account.
func (s *Service) CreateUser(ctx context.Context, n NewUser) (sqlcgen.User, error) {
	email := strings.TrimSpace(n.Email)
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") || len(email) > 254 || strings.ContainsAny(email, " \t\r\n") {
		return sqlcgen.User{}, ErrInvalidEmail
	}
	var hash pgtype.Text
	if n.Password != "" {
		if len([]rune(n.Password)) < MinPasswordLen {
			return sqlcgen.User{}, ErrWeakPassword
		}
		h, err := s.hash.hash(n.Password)
		if err != nil {
			return sqlcgen.User{}, err
		}
		hash = pgtype.Text{String: h, Valid: true}
	}
	u, err := sqlcgen.New(s.o.Pool).InsertUser(ctx, sqlcgen.InsertUserParams{
		Email: email, DisplayName: strings.TrimSpace(n.DisplayName), PasswordHash: hash, IsAdmin: n.IsAdmin,
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return sqlcgen.User{}, ErrUserExists
	}
	return u, err
}

// GeneratePassword returns a random 24-character password (144 bits).
func GeneratePassword() (string, error) {
	p, err := randomToken(18)
	if err != nil {
		return "", fmt.Errorf("auth: generate password: %w", err)
	}
	return p, nil
}
