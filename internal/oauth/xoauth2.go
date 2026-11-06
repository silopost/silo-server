package oauth

import (
	"strings"

	"github.com/emersion/go-sasl"
)

// XOAuth2 is the SASL mechanism of Gmail and Microsoft 365 (not in go-sasl).
const XOAuth2 = "XOAUTH2"

// NewXOAuth2Client returns a SASL client that authenticates user with an
// OAuth2 access token. The initial response is
// "user=" user "^Aauth=Bearer " token "^A^A"
// (https://developers.google.com/gmail/imap/xoauth2-protocol; Microsoft
// uses the same format).
func NewXOAuth2Client(user, token string) sasl.Client {
	return &xoauth2Client{user: user, token: token}
}

type xoauth2Client struct{ user, token string }

func (c *xoauth2Client) Start() (string, []byte, error) {
	return XOAuth2, []byte("user=" + c.user + "\x01auth=Bearer " + c.token + "\x01\x01"), nil
}

// Next is only called when the server refused the token: it sends a
// challenge with a JSON error ({"status":"401","schemes":"bearer",...}) and
// expects an empty response before its final failure. The JSON is returned
// as the error, which also aborts the exchange.
func (c *xoauth2Client) Next(challenge []byte) ([]byte, error) {
	return nil, &XOAuth2Error{Response: strings.TrimSpace(string(challenge))}
}

// XOAuth2Error is a token the server refused; Response is its JSON error.
type XOAuth2Error struct{ Response string }

func (e *XOAuth2Error) Error() string {
	if e.Response == "" {
		return "xoauth2: the server refused the access token"
	}
	return "xoauth2: the server refused the access token: " + e.Response
}

// AuthFailed marks the error as rejected credentials (imapx classifies it as
// ErrAuthFailed, not as an unreachable server).
func (e *XOAuth2Error) AuthFailed() bool { return true }
