package auth

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/store/sqlcgen"
	"github.com/silopost/silo-server/internal/totp"
)

// TOTPIssuer is the issuer shown by authenticator apps.
const TOTPIssuer = "Silo"

// TOTP errors.
var (
	// ErrTOTPEnabled means TOTP is already on (setup or enable again).
	ErrTOTPEnabled = errors.New("auth: totp already enabled")
	// ErrTOTPNotEnabled means TOTP is off (disable, new backup codes).
	ErrTOTPNotEnabled = errors.New("auth: totp not enabled")
	// ErrTOTPNotSetUp means enable was called before setup.
	ErrTOTPNotSetUp = errors.New("auth: totp not set up")
	// ErrInvalidTOTPCode means the code that confirms setup is wrong.
	ErrInvalidTOTPCode = errors.New("auth: invalid totp code")
)

// TOTPSetup is a freshly generated, not yet enabled TOTP secret.
type TOTPSetup struct {
	Secret     string // base32, no padding
	OTPAuthURL string
}

// totpAAD binds a sealed TOTP secret to its user row.
func totpAAD(userID int64) []byte { return []byte(strconv.FormatInt(userID, 10)) }

// SetupTOTP generates a new secret for the user (mode local). It replaces a
// secret whose setup was not finished; it refuses when TOTP is enabled.
func (s *Service) SetupTOTP(ctx context.Context, userID int64) (TOTPSetup, error) {
	if !s.Local() {
		return TOTPSetup{}, ErrNotAvailable
	}
	var out TOTPSetup
	err := s.inTx(ctx, func(q *sqlcgen.Queries, _ pgx.Tx) error {
		u, err := q.LockUser(ctx, userID)
		if err != nil {
			return err
		}
		if u.TotpEnabledAt.Valid {
			return ErrTOTPEnabled
		}
		secret, err := totp.NewSecret()
		if err != nil {
			return err
		}
		sealed, err := s.o.Keys.Encrypt(secret, totpAAD(userID))
		if err != nil {
			return err
		}
		if err := q.SetTOTPSecret(ctx, sqlcgen.SetTOTPSecretParams{ID: userID, TotpSecretEnc: sealed}); err != nil {
			return err
		}
		out = TOTPSetup{Secret: totp.EncodeSecret(secret), OTPAuthURL: totp.URL(TOTPIssuer, u.Email, secret)}
		return nil
	})
	return out, err
}

// EnableTOTP turns TOTP on once the user proves their app has the secret, and
// returns the backup codes (shown once).
func (s *Service) EnableTOTP(ctx context.Context, userID int64, code string) ([]string, error) {
	if !s.Local() {
		return nil, ErrNotAvailable
	}
	var codes []string
	err := s.inTx(ctx, func(q *sqlcgen.Queries, _ pgx.Tx) error {
		u, err := q.LockUser(ctx, userID)
		if err != nil {
			return err
		}
		if u.TotpEnabledAt.Valid {
			return ErrTOTPEnabled
		}
		if u.TotpSecretEnc == nil {
			return ErrTOTPNotSetUp
		}
		secret, err := s.o.Keys.Decrypt(u.TotpSecretEnc, totpAAD(userID))
		if err != nil {
			return err
		}
		step, ok := totp.Verify(secret, code, s.o.Now(), 0)
		if !ok {
			return ErrInvalidTOTPCode
		}
		var hashes []string
		if codes, hashes, err = totp.NewBackupCodes(); err != nil {
			return err
		}
		return q.EnableTOTP(ctx, sqlcgen.EnableTOTPParams{ID: userID, TotpLastStep: step, TotpBackupCodes: hashes})
	})
	return codes, err
}

// DisableTOTP turns TOTP off after checking the password and a TOTP or
// backup code.
func (s *Service) DisableTOTP(ctx context.Context, userID int64, password, code string) error {
	return s.withSecondFactor(ctx, userID, password, code, func(q *sqlcgen.Queries) error {
		return q.ClearTOTP(ctx, userID)
	})
}

// RegenerateBackupCodes replaces the backup codes after checking the password
// and a TOTP or backup code.
func (s *Service) RegenerateBackupCodes(ctx context.Context, userID int64, password, code string) ([]string, error) {
	var codes []string
	err := s.withSecondFactor(ctx, userID, password, code, func(q *sqlcgen.Queries) error {
		var hashes []string
		var err error
		if codes, hashes, err = totp.NewBackupCodes(); err != nil {
			return err
		}
		return q.SetTOTPBackupCodes(ctx, sqlcgen.SetTOTPBackupCodesParams{ID: userID, TotpBackupCodes: hashes})
	})
	return codes, err
}

// ResetTOTP turns a user's TOTP off without any check (admin panel).
func (s *Service) ResetTOTP(ctx context.Context, userID int64) error {
	return sqlcgen.New(s.o.Pool).ClearTOTP(ctx, userID)
}

// withSecondFactor runs fn after checking the user's password and second
// factor, in the transaction that consumed the code.
func (s *Service) withSecondFactor(ctx context.Context, userID int64, password, code string, fn func(*sqlcgen.Queries) error) error {
	if !s.Local() {
		return ErrNotAvailable
	}
	return s.inTx(ctx, func(q *sqlcgen.Queries, _ pgx.Tx) error {
		u, err := q.LockUser(ctx, userID)
		if err != nil {
			return err
		}
		if !u.TotpEnabledAt.Valid {
			return ErrTOTPNotEnabled
		}
		if err := s.reauth(u, password); err != nil {
			return err
		}
		ok, err := s.checkSecondFactor(ctx, q, u, code)
		if err != nil {
			return err
		}
		if !ok {
			return ErrInvalidCredentials
		}
		return fn(q)
	})
}

// checkSecondFactor verifies a TOTP code (later than the last used step) or
// consumes a backup code. u must be locked by the transaction of q.
func (s *Service) checkSecondFactor(ctx context.Context, q *sqlcgen.Queries, u sqlcgen.User, code string) (bool, error) {
	if totp.IsBackupCodeShape(code) {
		n, err := q.UseTOTPBackupCode(ctx, sqlcgen.UseTOTPBackupCodeParams{ID: u.ID, CodeHash: totp.HashBackupCode(code)})
		return n == 1, err
	}
	if u.TotpSecretEnc == nil {
		return false, nil
	}
	secret, err := s.o.Keys.Decrypt(u.TotpSecretEnc, totpAAD(u.ID))
	if err != nil {
		return false, err
	}
	step, ok := totp.Verify(secret, code, s.o.Now(), u.TotpLastStep)
	if !ok {
		return false, nil
	}
	return true, q.SetTOTPLastStep(ctx, sqlcgen.SetTOTPLastStepParams{ID: u.ID, TotpLastStep: step})
}
