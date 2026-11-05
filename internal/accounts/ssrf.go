package accounts

import (
	"context"
	"fmt"
	"net"
	"net/netip"

	"github.com/silopost/silo-server/internal/netx"
)

// ErrHostNotAllowed means a custom server host resolves to a non-public address
// that is not on SILO_CUSTOM_SERVER_ALLOWLIST (anti-SSRF, plan 3.15).
var ErrHostNotAllowed = netx.ErrHostNotAllowed

// resolver is the part of net.Resolver the check needs (replaceable in tests).
type resolver = netx.Resolver

// hostPolicy decides whether a user-supplied host may be contacted. check runs
// when an account is saved or tested; the connections themselves go through
// dialer, which applies the same policy to the addresses it dials (DNS
// rebinding between the check and the connection).
type hostPolicy struct {
	p        *netx.Policy
	resolver resolver
}

func newHostPolicy(allowlist []string, r resolver) hostPolicy {
	if r == nil {
		r = net.DefaultResolver
	}
	return hostPolicy{p: netx.NewPolicy(allowlist), resolver: r}
}

// dialer connects to a custom host under the policy.
func (p hostPolicy) dialer() *netx.Dialer {
	return &netx.Dialer{Policy: p.p, Resolver: p.resolver}
}

// check returns nil when host is allowlisted or every address it resolves to is
// public. Resolution failures are reported as errors other than
// ErrHostNotAllowed.
func (p hostPolicy) check(ctx context.Context, host string) error {
	host = netx.NormalizeHost(host)
	if host == "" {
		return fmt.Errorf("%w: empty host", ErrHostNotAllowed)
	}
	if p.p.HostAllowed(host) {
		return nil
	}
	addrs, err := p.lookup(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, a := range addrs {
		if !p.p.AddrAllowed(a) {
			return fmt.Errorf("%w: %s resolves to non-public address %s", ErrHostNotAllowed, host, a)
		}
	}
	return nil
}

func (p hostPolicy) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a.Unmap()}, nil
	}
	addrs, err := p.resolver.LookupNetIP(ctx, "ip", host)
	for i := range addrs {
		addrs[i] = addrs[i].Unmap()
	}
	return addrs, err
}
