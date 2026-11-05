package netx

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
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

// listener accepts connections on 127.0.0.1 and returns a DialAddr that
// sends every attempt there, counting them and recording the addresses asked.
func listener(t *testing.T) (dial DialFunc, count *atomic.Int32, asked *[]string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	count = new(atomic.Int32)
	var got []string
	asked = &got
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		count.Add(1)
		got = append(got, addr)
		var d net.Dialer
		return d.DialContext(ctx, network, ln.Addr().String())
	}, count, asked
}

func TestDialerRefusesPrivateAddressesWithoutDialling(t *testing.T) {
	dial, count, _ := listener(t)
	d := &Dialer{Policy: NewPolicy(nil), Resolver: fakeResolver{
		"rebind.example.com": {"10.0.0.5"},
		"meta.example.com":   {"169.254.169.254", "fd00::1"},
	}, DialAddr: dial}
	for _, addr := range []string{"rebind.example.com:993", "meta.example.com:993", "127.0.0.1:993", "[::1]:993", "[::ffff:192.168.0.1]:25"} {
		if _, err := d.DialContext(context.Background(), "tcp", addr); !errors.Is(err, ErrHostNotAllowed) {
			t.Errorf("%s: err = %v, want ErrHostNotAllowed", addr, err)
		}
	}
	if n := count.Load(); n != 0 {
		t.Fatalf("dial attempts = %d, want 0", n)
	}
	// Resolution failures are not policy violations.
	if _, err := d.DialContext(context.Background(), "tcp", "nx.example.com:993"); err == nil || errors.Is(err, ErrHostNotAllowed) {
		t.Errorf("unresolvable: %v", err)
	}
}

func TestDialerConnectsToTheCheckedAddress(t *testing.T) {
	dial, count, asked := listener(t)
	d := &Dialer{Policy: NewPolicy(nil), Resolver: fakeResolver{
		"mail.example.com":  {"93.184.216.34"},
		"mixed.example.com": {"10.0.0.5", "93.184.216.35"},
	}, DialAddr: dial}
	for _, addr := range []string{"mail.example.com:993", "mixed.example.com:993"} {
		c, err := d.DialContext(context.Background(), "tcp", addr)
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		c.Close()
	}
	if count.Load() != 2 || (*asked)[0] != "93.184.216.34:993" || (*asked)[1] != "93.184.216.35:993" {
		t.Fatalf("dialled %v", *asked)
	}
}

func TestDialerFallsBackToTheNextAddress(t *testing.T) {
	ok, _, _ := listener(t)
	var asked []string
	d := &Dialer{Policy: NewPolicy(nil), Resolver: fakeResolver{"mail.example.com": {"93.184.216.34", "93.184.216.35"}},
		DialAddr: func(ctx context.Context, network, addr string) (net.Conn, error) {
			asked = append(asked, addr)
			if len(asked) == 1 {
				return nil, errors.New("connection refused")
			}
			return ok(ctx, network, addr)
		}}
	c, err := d.DialContext(context.Background(), "tcp", "mail.example.com:993")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if len(asked) != 2 {
		t.Fatalf("dialled %v", asked)
	}
}

func TestDialerAllowlist(t *testing.T) {
	dial, count, asked := listener(t)
	d := &Dialer{Policy: NewPolicy([]string{"Mail.Corp.", "10.9.0.0/16", "192.168.5.5"}), Resolver: fakeResolver{
		"mail.corp":   {"10.1.1.1"},
		"cidr.corp":   {"10.9.1.1"},
		"single.corp": {"192.168.5.5"},
		"other.corp":  {"10.1.1.2"},
	}, DialAddr: dial}
	for _, addr := range []string{"mail.corp:143", "cidr.corp:143", "single.corp:143", "10.9.3.3:143"} {
		c, err := d.DialContext(context.Background(), "tcp", addr)
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		c.Close()
	}
	if count.Load() != 4 || (*asked)[0] != "10.1.1.1:143" {
		t.Fatalf("dialled %v", *asked)
	}
	if _, err := d.DialContext(context.Background(), "tcp", "other.corp:143"); !errors.Is(err, ErrHostNotAllowed) {
		t.Errorf("other.corp: %v", err)
	}
	// Without a policy (the instance's own servers) nothing is filtered.
	open := &Dialer{Resolver: fakeResolver{"mailserver": {"172.18.0.4"}}, DialAddr: dial}
	c, err := open.DialContext(context.Background(), "tcp", "mailserver:993")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestIsPublic(t *testing.T) {
	for s, want := range map[string]bool{
		"8.8.8.8": true, "2606:2800:220:1::1": true, "10.0.0.1": false, "100.64.1.1": false, "169.254.1.1": false,
		"::1": false, "fd00::1": false, "::ffff:10.0.0.1": false, "::ffff:8.8.8.8": true, "198.18.0.1": false, "0.1.2.3": false,
	} {
		if got := IsPublic(netip.MustParseAddr(s)); got != want {
			t.Errorf("IsPublic(%s) = %v", s, got)
		}
	}
}
