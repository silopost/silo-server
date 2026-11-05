package accounts

import (
	"context"
	"errors"
	"net/netip"
	"testing"
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
