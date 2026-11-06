package smtptest_test

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/silopost/silo-server/internal/oauth"
	"github.com/silopost/silo-server/internal/testutil/smtptest"
)

func TestXOAuth2(t *testing.T) {
	s := smtptest.StartServer(t, "u", "p", 0)
	dial := func() *smtp.Client {
		c, err := smtp.Dial(net.JoinHostPort(s.Host, strconv.Itoa(s.Port)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	// Not offered until enabled; PLAIN keeps working.
	c := dial()
	if err := c.Hello("t"); err != nil || c.SupportsAuth("XOAUTH2") {
		t.Fatalf("XOAUTH2 offered by default (%v)", err)
	}
	if err := c.Auth(sasl.NewPlainClient("", "u", "p")); err != nil {
		t.Fatal(err)
	}

	s.AcceptXOAuth2("me@x.test", func(tok string) bool { return tok == "good" })
	c = dial()
	if err := c.Auth(oauth.NewXOAuth2Client("me@x.test", "good")); err != nil {
		t.Fatalf("good token: %v", err)
	}
	if err := c.Mail("me@x.test", nil); err != nil {
		t.Fatalf("MAIL after XOAUTH2: %v", err)
	}

	// A refused token: Gmail's 334 with a JSON error; the client gives up.
	err := dial().Auth(oauth.NewXOAuth2Client("me@x.test", "bad"))
	var xe *oauth.XOAuth2Error
	if !errors.As(err, &xe) || !strings.Contains(xe.Response, `"status":"400"`) {
		t.Fatalf("bad token: %v", err)
	}
	// A client that answers the challenge gets 535.
	err = dial().Auth(&answering{oauth.NewXOAuth2Client("other@x.test", "good")})
	var se *smtp.SMTPError
	if !errors.As(err, &se) || se.Code != 535 {
		t.Fatalf("wrong user: %v", err)
	}
}

// answering sends the empty response after the error challenge (Google's
// documented client behaviour).
type answering struct{ sasl.Client }

func (a *answering) Next([]byte) ([]byte, error) { return []byte{}, nil }
