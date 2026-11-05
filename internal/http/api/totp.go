package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/auth"
)

// Error codes of mode local, TOTP and the admin panel (plan 5.3).
const (
	CodeNotAvailable    = "not_available"
	CodeTOTPRequired    = "totp_required"
	CodeTOTPEnabled     = "totp_enabled"
	CodeTOTPNotEnabled  = "totp_not_enabled"
	CodeTOTPNotSetUp    = "totp_not_set_up"
	CodeInvalidTOTPCode = "invalid_totp_code"
	CodeUserExists      = "user_exists"
	CodeSelfChange      = "admin_self_change"
)

func errTOTPRequired() error {
	return problem(http.StatusUnauthorized, CodeTOTPRequired, "A TOTP code or a backup code is required.")
}

func errNotAvailable() error {
	return problem(http.StatusConflict, CodeNotAvailable, "Not available in this instance's sign-in mode.")
}

// authError maps the errors of password, TOTP and user management. A failed
// re-authentication of a signed-in user is 403 invalid_credentials (not 401,
// which clients read as "signed out").
func authError(err error) error {
	var rl *auth.RateLimitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &rl):
		return errRateLimited(rl.RetryAfter)
	case errors.Is(err, auth.ErrNotAvailable):
		return errNotAvailable()
	case errors.Is(err, auth.ErrInvalidCredentials):
		return problem(http.StatusForbidden, CodeInvalidCredentials, "Invalid password or code.")
	case errors.Is(err, auth.ErrWeakPassword):
		return problem(http.StatusBadRequest, CodeValidation, "The password must have at least 10 characters.")
	case errors.Is(err, auth.ErrInvalidEmail):
		return problem(http.StatusBadRequest, CodeValidation, "Invalid email address.")
	case errors.Is(err, auth.ErrUserExists):
		return problem(http.StatusConflict, CodeUserExists, "A user with this email exists.")
	case errors.Is(err, auth.ErrTOTPEnabled):
		return problem(http.StatusConflict, CodeTOTPEnabled, "TOTP is already enabled.")
	case errors.Is(err, auth.ErrTOTPNotEnabled):
		return problem(http.StatusConflict, CodeTOTPNotEnabled, "TOTP is not enabled.")
	case errors.Is(err, auth.ErrTOTPNotSetUp):
		return problem(http.StatusConflict, CodeTOTPNotSetUp, "Call POST /totp/setup first.")
	case errors.Is(err, auth.ErrInvalidTOTPCode):
		return problem(http.StatusUnprocessableEntity, CodeInvalidTOTPCode, "The code does not match; check the device clock and try the next code.")
	}
	return errInternal(err)
}

type changePasswordInput struct {
	Body struct {
		CurrentPassword string `json:"currentPassword" minLength:"1" maxLength:"1024"`
		NewPassword     string `json:"newPassword" minLength:"10" maxLength:"1024" doc:"At least 10 characters."`
	}
}

// TOTPSetupBody is the response of POST /totp/setup.
type TOTPSetupBody struct {
	Secret     string `json:"secret" doc:"Base32 secret (no padding) for manual entry." example:"JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"`
	OTPAuthURL string `json:"otpauthUrl" doc:"otpauth:// URI to show as a QR code." example:"otpauth://totp/Silo:ala@example.com?secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP&issuer=Silo&algorithm=SHA1&digits=6&period=30"`
}

// BackupCodesBody carries a new set of backup codes (shown once).
type BackupCodesBody struct {
	BackupCodes []string `json:"backupCodes" doc:"10 single-use codes of 10 characters; only their hashes are stored."`
}

type totpSetupOutput struct{ Body TOTPSetupBody }

type backupCodesOutput struct{ Body BackupCodesBody }

type totpEnableInput struct {
	Body struct {
		Code string `json:"code" minLength:"6" maxLength:"6" pattern:"^[0-9]{6}$" doc:"Current code of the authenticator app."`
	}
}

type secondFactorInput struct {
	Body struct {
		Password string `json:"password" minLength:"1" maxLength:"1024"`
		Code     string `json:"code" minLength:"1" maxLength:"32" doc:"TOTP code or a backup code (consumed)."`
	}
}

// registerLocalAuth adds the Silo password and TOTP endpoints (mode local;
// 409 not_available in mode imap).
func (d *deps) registerLocalAuth(a huma.API) {
	huma.Register(a, d.secured(huma.Operation{
		OperationID: "changePassword", Method: http.MethodPost, Path: "/session/password", Tags: []string{"session"},
		Summary: "Change the Silo password", DefaultStatus: http.StatusNoContent,
		Description: "Mode local only (409 not_available in mode imap, where the mailbox password is used). " +
			"A wrong currentPassword is 403 invalid_credentials (throttled like sign-in). " +
			"Every other session of the user is signed out; the current one stays.",
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict, http.StatusTooManyRequests},
	}), func(ctx context.Context, in *changePasswordInput) (*struct{}, error) {
		sess := sessionFrom(ctx)
		err := d.auth.ChangePassword(ctx, sess.UserID, sess.ID, in.Body.CurrentPassword, in.Body.NewPassword)
		return nil, authError(err)
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "setupTotp", Method: http.MethodPost, Path: "/totp/setup", Tags: []string{"totp"},
		Summary: "Start TOTP setup",
		Description: "Mode local only. Generates a new secret (replacing one whose setup was not finished); " +
			"TOTP is enforced only after POST /totp/enable. 409 totp_enabled when it is already on.",
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict},
	}), func(ctx context.Context, _ *struct{}) (*totpSetupOutput, error) {
		res, err := d.auth.SetupTOTP(ctx, sessionFrom(ctx).UserID)
		if err != nil {
			return nil, authError(err)
		}
		return &totpSetupOutput{Body: TOTPSetupBody{Secret: res.Secret, OTPAuthURL: res.OTPAuthURL}}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "enableTotp", Method: http.MethodPost, Path: "/totp/enable", Tags: []string{"totp"},
		Summary: "Enable TOTP",
		Description: "Confirms the secret of POST /totp/setup with a current code and turns TOTP on. " +
			"Returns the backup codes, shown only this once. A wrong code is 422 invalid_totp_code.",
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity},
	}), func(ctx context.Context, in *totpEnableInput) (*backupCodesOutput, error) {
		codes, err := d.auth.EnableTOTP(ctx, sessionFrom(ctx).UserID, in.Body.Code)
		if err != nil {
			return nil, authError(err)
		}
		return &backupCodesOutput{Body: BackupCodesBody{BackupCodes: codes}}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "disableTotp", Method: http.MethodDelete, Path: "/totp", Tags: []string{"totp"},
		Summary: "Disable TOTP", DefaultStatus: http.StatusNoContent,
		Description: "Needs the password and a TOTP or backup code; a wrong one is 403 invalid_credentials.",
		Errors:      []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict, http.StatusTooManyRequests},
	}), func(ctx context.Context, in *secondFactorInput) (*struct{}, error) {
		return nil, authError(d.auth.DisableTOTP(ctx, sessionFrom(ctx).UserID, in.Body.Password, in.Body.Code))
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "regenerateTotpBackupCodes", Method: http.MethodPost, Path: "/totp/backup-codes", Tags: []string{"totp"},
		Summary: "Replace the TOTP backup codes",
		Description: "Needs the password and a TOTP or backup code; the previous backup codes stop working. " +
			"The new ones are shown only this once.",
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict, http.StatusTooManyRequests},
	}), func(ctx context.Context, in *secondFactorInput) (*backupCodesOutput, error) {
		codes, err := d.auth.RegenerateBackupCodes(ctx, sessionFrom(ctx).UserID, in.Body.Password, in.Body.Code)
		if err != nil {
			return nil, authError(err)
		}
		return &backupCodesOutput{Body: BackupCodesBody{BackupCodes: codes}}, nil
	})
}
