package http

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// RealIP replaces chi's middleware.RealIP, which trusts X-Forwarded-For from
// anyone. The headers are honoured only when the direct peer (RemoteAddr) is
// one of the trusted proxies (SILO_TRUSTED_PROXIES); with none configured they
// are ignored. From X-Forwarded-For the right-most address that is not a
// trusted proxy is the client (every proxy appends the address it saw, so
// entries left of it may be forged); when all entries are trusted proxies,
// the left-most one is used. X-Real-IP is used only without X-Forwarded-For.
// An unparsable entry ends the walk and the peer address is kept.
//
// The client address replaces r.RemoteAddr (without port, as chi does), so
// sign-in throttling, rate limits and logs need no change.
func RealIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if len(trusted) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ip, ok := clientIP(r, trusted); ok {
				r.RemoteAddr = ip.String()
			}
			next.ServeHTTP(w, r)
		})
	}
}

func clientIP(r *http.Request, trusted []netip.Prefix) (netip.Addr, bool) {
	peer, ok := parseIP(r.RemoteAddr)
	if !ok || !isTrusted(peer, trusted) {
		return netip.Addr{}, false
	}
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		var hops []string
		for _, v := range xff {
			hops = append(hops, strings.Split(v, ",")...)
		}
		var client netip.Addr
		for i := len(hops) - 1; i >= 0; i-- {
			a, ok := parseIP(hops[i])
			if !ok {
				return netip.Addr{}, false
			}
			client = a
			if !isTrusted(a, trusted) {
				break
			}
		}
		return client, client.IsValid()
	}
	if a, ok := parseIP(r.Header.Get("X-Real-IP")); ok {
		return a, true
	}
	return netip.Addr{}, false
}

// parseIP accepts "ip", "ip:port" and "[ipv6]:port"; zones are refused.
func parseIP(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, false
	}
	if a, err := netip.ParseAddr(s); err == nil && a.Zone() == "" {
		return a.Unmap(), true
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		if a, err := netip.ParseAddr(host); err == nil && a.Zone() == "" {
			return a.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// remoteIP is the client address of r after RealIP, without port.
func remoteIP(r *http.Request) string {
	if a, ok := parseIP(r.RemoteAddr); ok {
		return a.String()
	}
	return r.RemoteAddr
}
