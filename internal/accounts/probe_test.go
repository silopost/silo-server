package accounts

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
	"github.com/silopost/silo-server/internal/testutil/smtptest"
)

func stepNames(r ProtocolResult) []string {
	var out []string
	for _, s := range r.Steps {
		out = append(out, s.Step)
	}
	return out
}

func lastStep(r ProtocolResult) Step { return r.Steps[len(r.Steps)-1] }

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestProbeSMTP(t *testing.T) {
	host, port := smtptest.Start(t, "silo", "silopass")
	e := endpoint{Host: host, Port: port, TLS: "none", Username: "silo", Password: "silopass"}

	ok := probeSMTP(context.Background(), e)
	if !ok.OK {
		t.Fatalf("good credentials failed: %+v", ok)
	}
	if got := stepNames(ok); len(got) != 3 || got[0] != "connect" || got[1] != "ehlo" || got[2] != "auth" {
		t.Errorf("steps = %v", got)
	}

	e.Password = "wrong"
	bad := probeSMTP(context.Background(), e)
	if bad.OK || lastStep(bad).Step != "auth" || lastStep(bad).OK {
		t.Fatalf("wrong password: %+v", bad)
	}
	if d := lastStep(bad).Detail; !strings.Contains(d, "535") || strings.Contains(d, "wrong") {
		t.Errorf("auth detail = %q", d)
	}

	// STARTTLS requested but not offered: fails at that step, never reaching AUTH.
	e.TLS, e.Password = "starttls", "silopass"
	st := probeSMTP(context.Background(), e)
	if st.OK || lastStep(st).Step != "starttls" {
		t.Fatalf("starttls on a server without it: %+v", st)
	}

	// Nothing listening: the connect step reports it and stops.
	e.Port = freePort(t)
	e.TLS = "none"
	down := probeSMTP(context.Background(), e)
	if down.OK || len(down.Steps) != 1 || down.Steps[0].Step != "connect" {
		t.Fatalf("closed port: %+v", down)
	}
}

func TestProbeIMAP(t *testing.T) {
	addr, user, pass := imaptest.Start(t, imaptest.Full)
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(p)
	e := endpoint{Host: h, Port: port, TLS: "none", Username: user, Password: pass}

	res := probeIMAP(context.Background(), e)
	if !res.OK {
		t.Fatalf("good credentials failed: %+v", res)
	}
	want := []string{"connect", "tls", "login", "capability", "list"}
	got := stepNames(res)
	if len(got) != len(want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] || !res.Steps[i].OK {
			t.Errorf("step %d = %+v, want ok %s", i, res.Steps[i], want[i])
		}
	}
	if d := res.Steps[3].Detail; !strings.Contains(d, "IDLE") {
		t.Errorf("capability detail = %q", d)
	}

	e.Password = "nope"
	bad := probeIMAP(context.Background(), e)
	if bad.OK || lastStep(bad).Step != "login" || lastStep(bad).OK {
		t.Fatalf("wrong password: %+v", bad)
	}
}

// TestServiceTestPolicy runs the check through the service, including the
// instance policy for custom servers.
func TestServiceTestPolicy(t *testing.T) {
	cfg := config.Config{
		DefaultIMAPHost: "imap.default.test", DefaultIMAPPort: 993, DefaultIMAPTLS: "implicit",
		DefaultSMTPHost: "imap.default.test", DefaultSMTPPort: 587, DefaultSMTPTLS: "starttls",
	}
	conn := Connection{Email: "a@b.c", Password: "x", IMAPHost: "127.0.0.1", IMAPPort: 1, IMAPTLS: "implicit"}
	ctx := context.Background()

	// Custom servers disabled: refused before any connection.
	if _, err := New(Options{Config: cfg}).Test(ctx, conn); !errors.Is(err, ErrCustomServersDenied) {
		t.Fatalf("disabled: err = %v", err)
	}
	// Enabled, but loopback is neither public nor allowlisted.
	cfg.AllowCustomServers = true
	if _, err := New(Options{Config: cfg}).Test(ctx, conn); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("loopback: err = %v", err)
	}
	// Allowlisted: the check runs (and cannot connect to port 1).
	cfg.CustomServerAllowlist = []string{"127.0.0.1"}
	conn.IMAPPort = freePort(t)
	res, err := New(Options{Config: cfg}).Test(ctx, conn)
	if err != nil || res.OK() || res.IMAP.Steps[0].OK {
		t.Fatalf("allowlisted: res=%+v err=%v", res, err)
	}
	// Unencrypted transport needs SILO_ALLOW_INSECURE_TRANSPORT.
	conn.IMAPTLS = "none"
	if _, err := New(Options{Config: cfg}).Test(ctx, conn); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tls none: err = %v", err)
	}
}
