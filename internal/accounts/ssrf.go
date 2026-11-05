package accounts

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// ErrHostNotAllowed means a custom server host resolves to a non-public address
// that is not on SILO_CUSTOM_SERVER_ALLOWLIST (anti-SSRF, plan 3.15).
var ErrHostNotAllowed = errors.New("accounts: host not allowed")

// resolver is the part of net.Resolver the check needs (replaceable in tests).
type resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// hostPolicy decides whether a user-supplied host may be contacted.
type hostPolicy struct {
	allow    []string // allowlist entries: host names, IPs or CIDRs
	resolver resolver
}

func newHostPolicy(allowlist []string, r resolver) hostPolicy {
	if r == nil {
		r = net.DefaultResolver
	}
	var allow []string
	for _, e := range allowlist {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			allow = append(allow, e)
		}
	}
	return hostPolicy{allow: allow, resolver: r}
}

// check returns nil when host is allowlisted or every address it resolves to is
// public. Resolution failures are reported as errors other than
// ErrHostNotAllowed.
func (p hostPolicy) check(ctx context.Context, host string) error {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return fmt.Errorf("%w: empty host", ErrHostNotAllowed)
	}
	for _, e := range p.allow {
		if e == host {
			return nil
		}
	}
	addrs, err := p.lookup(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, a := range addrs {
		if p.allowedAddr(a) {
			continue
		}
		if !isPublic(a) {
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

// allowedAddr reports whether an allowlist IP or CIDR covers a.
func (p hostPolicy) allowedAddr(a netip.Addr) bool {
	for _, e := range p.allow {
		if pfx, err := netip.ParsePrefix(e); err == nil {
			if pfx.Contains(a) {
				return true
			}
			continue
		}
		if ip, err := netip.ParseAddr(e); err == nil && ip.Unmap() == a {
			return true
		}
	}
	return false
}

var (
	cgnat = netip.MustParsePrefix("100.64.0.0/10")
	// 0.0.0.0/8 and the IPv4 benchmarking, documentation and reserved ranges.
	reserved = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("240.0.0.0/4"),
	}
)

// isPublic reports whether a is a globally routable unicast address.
func isPublic(a netip.Addr) bool {
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsMulticast() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() {
		return false
	}
	if cgnat.Contains(a) {
		return false
	}
	for _, p := range reserved {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
