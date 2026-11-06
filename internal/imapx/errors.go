package imapx

import (
	"context"
	"errors"
	"fmt"

	"github.com/emersion/go-imap/v2"
)

var (
	// ErrAuthFailed means the server rejected the credentials (tagged NO to
	// LOGIN/AUTHENTICATE). Retrying with the same credentials is pointless and
	// may trigger server-side lockouts, so callers should stop and ask the user.
	ErrAuthFailed = errors.New("imapx: authentication failed")

	// ErrUnavailable means the server could not be reached or dropped the
	// connection (network, TLS, timeout, BYE, [UNAVAILABLE]). Retry with backoff.
	ErrUnavailable = errors.New("imapx: server unavailable")
)

// wrapLoginError classifies an error from LOGIN/AUTHENTICATE.
func wrapLoginError(err error) error {
	if err == nil {
		return nil
	}
	// A SASL client that gives up on a challenge (XOAUTH2: the server sent
	// its JSON error for a refused token) reports rejected credentials.
	var af interface{ AuthFailed() bool }
	if errors.As(err, &af) && af.AuthFailed() {
		return fmt.Errorf("%w: %w", ErrAuthFailed, err)
	}
	var ie *imap.Error
	if errors.As(err, &ie) {
		switch ie.Type {
		case imap.StatusResponseTypeNo:
			switch ie.Code {
			case imap.ResponseCodeUnavailable, imap.ResponseCodeServerBug, imap.ResponseCodeInUse,
				imap.ResponseCodeLimit, imap.ResponseCodeOverQuota:
				return fmt.Errorf("%w: %w", ErrUnavailable, err)
			}
			return fmt.Errorf("%w: %w", ErrAuthFailed, err)
		case imap.StatusResponseTypeBad:
			return fmt.Errorf("imapx: login rejected as malformed: %w", err)
		case imap.StatusResponseTypeBye:
			return fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

// wrapNetError marks transport-level failures as ErrUnavailable and leaves
// protocol errors (*imap.Error) untouched.
func wrapNetError(err error) error {
	if err == nil {
		return nil
	}
	var ie *imap.Error
	if errors.As(err, &ie) || errors.Is(err, ErrUnavailable) || errors.Is(err, context.Canceled) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}
