package oauth

import (
	"errors"
	"testing"
)

// TestXOAuth2Format checks the initial response of Google's documentation
// (https://developers.google.com/gmail/imap/xoauth2-protocol): user and
// bearer token separated by ^A, closed by ^A^A.
func TestXOAuth2Format(t *testing.T) {
	c := NewXOAuth2Client("someuser@example.com", "ya29.vF9dft4qmTc2Nvb3RlckBhdHRhdmlzdGEuY29tCg")
	mech, ir, err := c.Start()
	if err != nil || mech != "XOAUTH2" {
		t.Fatalf("Start = %q, %v", mech, err)
	}
	want := "user=someuser@example.com\x01auth=Bearer ya29.vF9dft4qmTc2Nvb3RlckBhdHRhdmlzdGEuY29tCg\x01\x01"
	if string(ir) != want {
		t.Fatalf("initial response = %q, want %q", ir, want)
	}
	_, err = c.Next([]byte(`{"status":"401","schemes":"bearer","scope":"https://mail.google.com/"}`))
	var xe *XOAuth2Error
	if !errors.As(err, &xe) || xe.Response != `{"status":"401","schemes":"bearer","scope":"https://mail.google.com/"}` || !xe.AuthFailed() {
		t.Fatalf("Next = %v", err)
	}
}
