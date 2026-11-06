package sync

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/silopost/silo-server/internal/imapx"
	"github.com/silopost/silo-server/internal/oauth"
)

// TokenSource hands out OAuth2 access tokens of xoauth2 accounts
// (oauth.Service); nil when no provider is configured.
type TokenSource interface {
	Token(ctx context.Context, accountID int64) (string, error)
}

// authenticate logs a new connection in: LOGIN with the account password, or
// AUTHENTICATE XOAUTH2 with an access token obtained (or refreshed) just
// before (plan 3.15). A refresh token the provider refused is ErrAuthFailed,
// so the worker stops in auth_failed until the user signs in again.
func (w *AccountWorker) authenticate(ctx context.Context, cl *imapx.Client, acc *account) error {
	var authType string
	if err := w.d.Pool.QueryRow(ctx, `SELECT auth_type FROM accounts WHERE id = $1`, acc.id).Scan(&authType); err != nil {
		return fmt.Errorf("sync: load auth type of account %d: %w", acc.id, err)
	}
	if authType == "xoauth2" {
		if w.d.OAuth == nil {
			return fmt.Errorf("%w: %w: no OAuth provider on this instance", imapx.ErrAuthFailed, oauth.ErrNotConfigured)
		}
		tok, err := w.d.OAuth.Token(ctx, acc.id)
		if errors.Is(err, oauth.ErrReauthRequired) {
			return fmt.Errorf("%w: %w", imapx.ErrAuthFailed, err)
		}
		if err != nil {
			return err
		}
		return cl.Authenticate(ctx, oauth.NewXOAuth2Client(acc.username, tok))
	}
	pass, err := w.d.Keys.Decrypt(acc.secret, []byte(strconv.FormatInt(acc.id, 10)))
	if err != nil {
		return fmt.Errorf("%w: decrypt account secret: %v", imapx.ErrAuthFailed, err)
	}
	return cl.Login(ctx, acc.username, string(pass))
}

// authFailedDetail is accounts.sync_error of an account in auth_failed.
func authFailedDetail(err error) string {
	if errors.Is(err, oauth.ErrReauthRequired) {
		return oauth.ReauthMessage
	}
	return "authentication failed"
}
