package netx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// Resolver is the part of net.Resolver the dialer needs (replaceable in tests).
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DialFunc is the signature of net.Dialer.DialContext.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Dialer resolves the host itself, filters the addresses through Policy and
// connects to the first allowed one (then the next ones on failure), so the
// address checked is the address used: a DNS answer that changes between a
// check and the connection (rebinding) cannot reach an internal service.
// Callers keep the host name for TLS (ServerName) and protocol greetings.
//
// A host allowlisted by name, or any host when Policy is nil (the instance's
// own default servers), is not filtered but still dialled here, so there is
// one connection path.
type Dialer struct {
	Policy   *Policy
	Resolver Resolver      // net.DefaultResolver when nil
	Timeout  time.Duration // per connection attempt; none when 0 (the context bounds it)
	// DialAddr connects to one resolved "ip:port"; net.Dialer when nil (tests
	// replace it).
	DialAddr DialFunc
}

// DialContext connects to address ("host:port") over network ("tcp", "tcp4"
// or "tcp6"). It fails with ErrHostNotAllowed, before any connection attempt,
// when the policy refuses every address of the host.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	host = NormalizeHost(host)
	if host == "" {
		return nil, fmt.Errorf("%w: empty host", ErrHostNotAllowed)
	}
	addrs, err := d.lookup(ctx, network, host)
	if err != nil {
		return nil, err
	}
	if d.Policy != nil && !d.Policy.HostAllowed(host) {
		allowed := addrs[:0:0]
		var refused netip.Addr
		for _, a := range addrs {
			if d.Policy.AddrAllowed(a) {
				allowed = append(allowed, a)
			} else if !refused.IsValid() {
				refused = a
			}
		}
		if len(allowed) == 0 {
			return nil, fmt.Errorf("%w: %s resolves to non-public address %s", ErrHostNotAllowed, host, refused)
		}
		addrs = allowed
	}
	dial := d.DialAddr
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	var errs []error
	for _, a := range addrs {
		actx, cancel := ctx, context.CancelFunc(func() {})
		if d.Timeout > 0 {
			actx, cancel = context.WithTimeout(ctx, d.Timeout)
		}
		conn, err := dial(actx, network, net.JoinHostPort(a.String(), port))
		cancel()
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	if len(errs) == 1 {
		return nil, errs[0]
	}
	return nil, errors.Join(errs...)
}

func (d *Dialer) lookup(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Zone() != "" {
			return nil, fmt.Errorf("%w: zoned address %s", ErrHostNotAllowed, host)
		}
		return []netip.Addr{a.Unmap()}, nil
	}
	r := d.Resolver
	if r == nil {
		r = net.DefaultResolver
	}
	ipNet := "ip"
	switch network {
	case "tcp4":
		ipNet = "ip4"
	case "tcp6":
		ipNet = "ip6"
	}
	addrs, err := r.LookupNetIP(ctx, ipNet, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	for i := range addrs {
		addrs[i] = addrs[i].Unmap()
	}
	return addrs, nil
}
