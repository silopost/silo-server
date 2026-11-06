package imapx_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/imapx"
	"github.com/silopost/silo-server/internal/oauth"
)

// fakeXOAuth2 is a minimal IMAP server that only knows AUTHENTICATE XOAUTH2
// (SASL-IR) and LOGOUT. A wrong token gets Gmail's answer: a continuation
// with a base64 JSON error, then NO after the client's (empty) response.
func fakeXOAuth2(t *testing.T, user, token string) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
				say("* OK [CAPABILITY IMAP4rev1 SASL-IR AUTH=XOAUTH2] fake ready")
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					f := strings.Fields(line)
					if len(f) < 2 {
						continue
					}
					tag := f[0]
					switch strings.ToUpper(f[1]) {
					case "AUTHENTICATE":
						ir, _ := base64.StdEncoding.DecodeString(f[len(f)-1])
						if len(f) == 4 && strings.EqualFold(f[2], "XOAUTH2") && string(ir) == "user="+user+"\x01auth=Bearer "+token+"\x01\x01" {
							say(tag + " OK [CAPABILITY IMAP4rev1 IDLE] authenticated")
							continue
						}
						say("+ " + base64.StdEncoding.EncodeToString([]byte(`{"status":"400","schemes":"Bearer","scope":"https://mail.google.com/"}`)))
						if _, err := r.ReadString('\n'); err != nil {
							return
						}
						say(tag + " NO [AUTHENTICATIONFAILED] Invalid credentials (Failure)")
					case "LOGOUT":
						say("* BYE")
						say(tag + " OK bye")
						return
					default:
						say(tag + " BAD unknown command")
					}
				}
			}()
		}
	}()
	return "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
}

func TestAuthenticateXOAuth2(t *testing.T) {
	host, port := fakeXOAuth2(t, "u@gmail.test", "ya29.good")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, err := imapx.Dial(ctx, host, port, imapx.TLSNone, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Authenticate(ctx, oauth.NewXOAuth2Client("u@gmail.test", "ya29.good")); err != nil {
		t.Fatalf("good token: %v", err)
	}
	_ = c.Logout(ctx)

	c, err = imapx.Dial(ctx, host, port, imapx.TLSNone, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.Authenticate(ctx, oauth.NewXOAuth2Client("u@gmail.test", "ya29.expired"))
	var xe *oauth.XOAuth2Error
	if !errors.Is(err, imapx.ErrAuthFailed) || !errors.As(err, &xe) || !strings.Contains(xe.Response, `"status":"400"`) {
		t.Fatalf("refused token: %v", err)
	}
}
