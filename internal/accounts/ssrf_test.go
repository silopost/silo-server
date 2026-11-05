package accounts

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/silopost/silo-server/internal/config"
)

type fakeResolver map[string][]string

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	ips, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	var out []netip.Addr
	for _, s := range ips {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

func TestHostPolicy(t *testing.T) {
	res := fakeResolver{
		"mail.example.com":    {"93.184.216.34", "2606:2800:220:1::1"},
		"internal.corp":       {"10.1.2.3"},
		"mixed.example.com":   {"93.184.216.34", "192.168.0.5"},
		"meta.example.com":    {"169.254.169.254"},
		"local.example.com":   {"127.0.0.1"},
		"v6local.example.com": {"::1"},
		"mapped.example.com":  {"::ffff:10.0.0.1"},
		"cgnat.example.com":   {"100.64.1.1"},
		"allowed.corp":        {"10.8.1.1"},
		"cidr.corp":           {"10.9.7.7"},
	}
	p := newHostPolicy([]string{"Allowed.corp", "10.9.0.0/16", "127.0.0.1"}, res)

	tests := []struct {
		host    string
		allowed bool
	}{
		{"mail.example.com", true},
		{"internal.corp", false},
		{"mixed.example.com", false},
		{"meta.example.com", false},
		{"local.example.com", true}, // 127.0.0.1 is allowlisted
		{"v6local.example.com", false},
		{"mapped.example.com", false},
		{"cgnat.example.com", false},
		{"allowed.corp", true}, // by name
		{"cidr.corp", true},    // by CIDR
		{"10.1.1.1", false},    // literal private IP
		{"10.9.2.2", true},     // literal inside an allowlisted CIDR
		{"8.8.8.8", true},      // literal public IP
		{"0.0.0.0", false},
		{"::ffff:192.168.1.1", false},
	}
	for _, tc := range tests {
		err := p.check(context.Background(), tc.host)
		if tc.allowed && err != nil {
			t.Errorf("%s: unexpected error %v", tc.host, err)
		}
		if !tc.allowed && !errors.Is(err, ErrHostNotAllowed) {
			t.Errorf("%s: err = %v, want ErrHostNotAllowed", tc.host, err)
		}
	}

	// Resolution failures are not policy violations.
	if err := p.check(context.Background(), "nx.example.com"); err == nil || errors.Is(err, ErrHostNotAllowed) {
		t.Errorf("unresolvable host: err = %v", err)
	}
}

// rebinder answers a public address to the first n lookups (the check) and
// loopback afterwards (the connection): DNS rebinding.
type rebinder struct {
	n     int
	calls atomic.Int32
}

func (r *rebinder) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	if int(r.calls.Add(1)) <= r.n {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

func TestTestRefusesRebindingHost(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	res := &rebinder{n: 2} // one check per protocol
	s := New(Options{Resolver: res, Config: config.Config{
		AllowCustomServers: true, AllowInsecureTransport: true,
		DefaultIMAPHost: "mail.silo.test", DefaultIMAPPort: 993, DefaultIMAPTLS: "implicit",
		DefaultSMTPHost: "mail.silo.test", DefaultSMTPPort: 587, DefaultSMTPTLS: "starttls",
	}})
	r, err := s.Test(context.Background(), Connection{Email: "a@rebind.example.com", Password: "x",
		IMAPHost: "rebind.example.com", IMAPPort: port, IMAPTLS: "none",
		SMTPHost: "rebind.example.com", SMTPPort: port, SMTPTLS: "none"})
	if err != nil {
		t.Fatalf("the check itself saw a public address: %v", err)
	}
	for _, p := range []ProtocolResult{r.IMAP, r.SMTP} {
		if p.OK || len(p.Steps) == 0 || p.Steps[0].Step != "connect" || !strings.Contains(p.Steps[0].Detail, "host not allowed") {
			t.Errorf("result %+v", p)
		}
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("%d connections reached the loopback listener", n)
	}
}
