package imapx_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/imapx"
	"github.com/silopost/silo-server/internal/netx"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

func hostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return h, n
}

// DoD of phase 1: connect to Dovecot, log in, read CAPABILITY with QRESYNC MOVE UIDPLUS.
func TestDialLoginCapabilities(t *testing.T) {
	addr, user, pass := imaptest.Start(t, imaptest.Full)
	host, port := hostPort(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := imapx.Dial(ctx, host, port, imapx.TLSNone, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Login(ctx, user, pass); err != nil {
		t.Fatal(err)
	}
	caps, err := c.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !caps.HasQRESYNC() || !caps.HasCondstore() || !caps.HasMove() || !caps.HasUIDPlus() || !caps.HasIdle() || !caps.HasSpecialUse() {
		t.Fatalf("capabilities: %v", caps.Names())
	}
	t.Logf("CAPABILITY: %s", strings.Join(caps.Names(), " "))
}

func TestLoginWrongPassword(t *testing.T) {
	addr, user, _ := imaptest.Start(t, imaptest.Full)
	host, port := hostPort(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := imapx.Dial(ctx, host, port, imapx.TLSNone, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.Login(ctx, user, "wrong")
	if !errors.Is(err, imapx.ErrAuthFailed) || errors.Is(err, imapx.ErrUnavailable) {
		t.Fatalf("want ErrAuthFailed, got %v", err)
	}
}

func TestDialUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listens any more
	host, port := hostPort(t, addr)
	_, err = imapx.Dial(context.Background(), host, port, imapx.TLSNone, false)
	if !errors.Is(err, imapx.ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}

func TestDialGreetingTimeout(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0") // accepts but never greets
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	host, port := hostPort(t, l.Addr().String())
	start := time.Now()
	_, err = imapx.Dial(context.Background(), host, port, imapx.TLSNone, false, imapx.WithDialTimeout(300*time.Millisecond))
	if !errors.Is(err, imapx.ErrUnavailable) || time.Since(start) > 3*time.Second {
		t.Fatalf("want fast ErrUnavailable, got %v after %v", err, time.Since(start))
	}
}

type staticResolver map[string]string

func (r staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if ip, ok := r[host]; ok {
		return []netip.Addr{netip.MustParseAddr(ip)}, nil
	}
	return nil, errors.New("no such host")
}

// WithDialContext routes the connection through the anti-SSRF dialer: the host
// name is resolved and checked there.
func TestDialWithSafeDialer(t *testing.T) {
	addr, user, pass := imaptest.Start(t, imaptest.Full)
	host, port := hostPort(t, addr)
	ips, err := net.DefaultResolver.LookupNetIP(context.Background(), "ip4", host)
	if err != nil || len(ips) == 0 {
		t.Fatalf("resolve %s: %v", host, err)
	}
	res := staticResolver{"mail.example.com": ips[0].String()}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The instance's own server (no policy): connects.
	c, err := imapx.Dial(ctx, "mail.example.com", port, imapx.TLSNone, false,
		imapx.WithDialContext((&netx.Dialer{Resolver: res}).DialContext))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login(ctx, user, pass); err != nil {
		t.Fatal(err)
	}
	c.Close()

	// A custom server resolving to a loopback address: refused before connecting.
	_, err = imapx.Dial(ctx, "mail.example.com", port, imapx.TLSNone, false,
		imapx.WithDialContext((&netx.Dialer{Policy: netx.NewPolicy(nil), Resolver: res}).DialContext))
	if !errors.Is(err, netx.ErrHostNotAllowed) {
		t.Fatalf("want ErrHostNotAllowed, got %v", err)
	}
}
