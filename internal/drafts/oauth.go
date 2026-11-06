package drafts

import (
	"context"
	"errors"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/silopost/silo-server/internal/oauth"
)

// OAuthTokens hands out OAuth2 access tokens of xoauth2 accounts
// (oauth.Service).
type OAuthTokens interface {
	Token(ctx context.Context, accountID int64) (string, error)
}

// xoauth2Auth is AUTH XOAUTH2 with an access token refreshed just before
// (plan 3.9 step 4, 3.15). A refresh token the provider refused, or a server
// without XOAUTH2, fails the send at once: retrying cannot help until the
// user signs in again.
func xoauth2Auth(ctx context.Context, c *smtp.Client, a *smtpAccount) (sasl.Client, *sendError) {
	if !c.SupportsAuth(oauth.XOAuth2) {
		_, mechs := c.Extension("AUTH")
		return nil, permanent("auth: the server does not offer XOAUTH2 (it offers %s)", mechs)
	}
	tok, err := a.oauthToken(ctx)
	if errors.Is(err, oauth.ErrReauthRequired) {
		return nil, permanent("auth: %s", oauth.ReauthMessage)
	}
	if err != nil {
		return nil, &sendError{temporary: true, msg: "auth: " + err.Error()}
	}
	return oauth.NewXOAuth2Client(a.username, tok), nil
}

// classifyAuth is classify for AUTH; a token the server refused is permanent.
func classifyAuth(err error) *sendError {
	var xe *oauth.XOAuth2Error
	if errors.As(err, &xe) {
		return permanent("auth: %s", xe.Error())
	}
	return classify("auth", err)
}
