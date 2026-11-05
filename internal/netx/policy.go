// Package netx holds the anti-SSRF network policy (plan 3.15) and a dialer
// that enforces it on the addresses it actually connects to.
package netx

import (
	"errors"
	"net/netip"
	"strings"
)

// ErrHostNotAllowed means a host resolves only to addresses the policy refuses
// (non-public and not on SILO_CUSTOM_SERVER_ALLOWLIST).
var ErrHostNotAllowed = errors.New("host not allowed")

// Policy decides which hosts and addresses user-supplied servers may use:
// public unicast addresses always, others only when allowlisted by host name,
// IP or CIDR.
type Policy struct {
	names    map[string]bool
	addrs    map[netip.Addr]bool
	prefixes []netip.Prefix
}

// NewPolicy parses allowlist entries (host names, IPs or CIDRs; case and
// surrounding space ignored).
func NewPolicy(allowlist []string) *Policy {
	p := &Policy{names: map[string]bool{}, addrs: map[netip.Addr]bool{}}
	for _, e := range allowlist {
		e = NormalizeHost(e)
		if e == "" {
			continue
		}
		p.names[e] = true
		if pfx, err := netip.ParsePrefix(e); err == nil {
			p.prefixes = append(p.prefixes, pfx.Masked())
		} else if a, err := netip.ParseAddr(e); err == nil {
			p.addrs[a.Unmap()] = true
		}
	}
	return p
}

// NormalizeHost lower-cases host and drops a trailing dot.
func NormalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

// HostAllowed reports whether host is allowlisted as written (its addresses
// are then not checked).
func (p *Policy) HostAllowed(host string) bool {
	return p != nil && p.names[NormalizeHost(host)]
}

// AddrAllowed reports whether a may be contacted: allowlisted or public.
func (p *Policy) AddrAllowed(a netip.Addr) bool {
	return p.Allowlisted(a) || IsPublic(a)
}

// Allowlisted reports whether an allowlist IP or CIDR covers a.
func (p *Policy) Allowlisted(a netip.Addr) bool {
	if p == nil {
		return false
	}
	a = a.Unmap()
	if p.addrs[a] {
		return true
	}
	for _, pfx := range p.prefixes {
		if pfx.Contains(a) {
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

// IsPublic reports whether a is a globally routable unicast address.
func IsPublic(a netip.Addr) bool {
	a = a.Unmap()
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
