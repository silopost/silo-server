package imapx

import (
	"errors"
	"io"
	"net"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestWrapLoginError(t *testing.T) {
	no := func(code imap.ResponseCode) error {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: code, Text: "x"}
	}
	tests := []struct {
		name    string
		err     error
		auth    bool
		unavail bool
	}{
		{"plain NO", no(""), true, false},
		{"authfailed", no(imap.ResponseCodeAuthenticationFailed), true, false},
		{"unavailable", no(imap.ResponseCodeUnavailable), false, true},
		{"inuse", no(imap.ResponseCodeInUse), false, true},
		{"bye", &imap.Error{Type: imap.StatusResponseTypeBye}, false, true},
		{"eof", io.EOF, false, true},
		{"net", &net.OpError{Op: "read", Err: errors.New("reset")}, false, true},
	}
	for _, tc := range tests {
		got := wrapLoginError(tc.err)
		if errors.Is(got, ErrAuthFailed) != tc.auth || errors.Is(got, ErrUnavailable) != tc.unavail {
			t.Errorf("%s: auth=%v unavail=%v err=%v", tc.name, errors.Is(got, ErrAuthFailed), errors.Is(got, ErrUnavailable), got)
		}
	}
	if wrapLoginError(nil) != nil {
		t.Error("nil")
	}
}
