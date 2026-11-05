package http

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestRealIP(t *testing.T) {
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.168.1.1/32")}
	tests := []struct {
		name     string
		trusted  []netip.Prefix
		remote   string
		xff      []string
		realIP   string
		expected string
	}{
		{"no trusted proxies: header ignored", nil, "203.0.113.9:4000", []string{"1.2.3.4"}, "", "203.0.113.9:4000"},
		{"untrusted peer: header ignored", proxies, "203.0.113.9:4000", []string{"1.2.3.4"}, "5.6.7.8", "203.0.113.9:4000"},
		{"trusted peer: right-most address", proxies, "10.1.2.3:4000", []string{"6.6.6.6, 1.2.3.4"}, "", "1.2.3.4"},
		{"two trusted hops", proxies, "10.1.2.3:4000", []string{"6.6.6.6, 1.2.3.4, 192.168.1.1"}, "", "1.2.3.4"},
		{"several headers", proxies, "10.1.2.3:4000", []string{"6.6.6.6", "1.2.3.4, 10.9.9.9"}, "", "1.2.3.4"},
		{"all trusted: left-most", proxies, "10.1.2.3:4000", []string{"10.5.5.5, 192.168.1.1"}, "", "10.5.5.5"},
		{"ipv6 and port in entry", proxies, "10.1.2.3:4000", []string{"[2001:db8::1]:443"}, "", "2001:db8::1"},
		{"mapped ipv4", proxies, "[::ffff:10.1.2.3]:4000", []string{"::ffff:1.2.3.4"}, "", "1.2.3.4"},
		{"garbage: remote kept", proxies, "10.1.2.3:4000", []string{"1.2.3.4, not-an-ip"}, "", "10.1.2.3:4000"},
		{"garbage only", proxies, "10.1.2.3:4000", []string{"<script>"}, "", "10.1.2.3:4000"},
		{"empty entry", proxies, "10.1.2.3:4000", []string{"1.2.3.4,"}, "", "10.1.2.3:4000"},
		{"x-real-ip from trusted peer", proxies, "10.1.2.3:4000", nil, "1.2.3.4", "1.2.3.4"},
		{"x-real-ip garbage", proxies, "10.1.2.3:4000", nil, "nope", "10.1.2.3:4000"},
		{"zone refused", proxies, "10.1.2.3:4000", []string{"fe80::1%eth0"}, "", "10.1.2.3:4000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			h := RealIP(tc.trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.RemoteAddr }))
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if tc.realIP != "" {
				r.Header.Set("X-Real-IP", tc.realIP)
			}
			h.ServeHTTP(httptest.NewRecorder(), r)
			if got != tc.expected {
				t.Errorf("RemoteAddr = %q, want %q", got, tc.expected)
			}
		})
	}
}
