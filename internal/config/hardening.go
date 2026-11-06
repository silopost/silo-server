package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	"github.com/silopost/silo-server/internal/ratelimit"
)

// TrustedProxyPrefixes parses SILO_TRUSTED_PROXIES: IPs or CIDRs of the
// reverse proxies whose X-Forwarded-For / X-Real-IP headers are honoured.
func (c Config) TrustedProxyPrefixes() ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, e := range c.TrustedProxies {
		if e = strings.TrimSpace(e); e == "" {
			continue
		}
		if p, err := netip.ParsePrefix(e); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(e)
		if err != nil {
			return nil, fmt.Errorf("SILO_TRUSTED_PROXIES: %q is neither an IP nor a CIDR", e)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// APIRateLimit parses SILO_RATE_LIMIT_API (per client IP, /api/v1/*).
func (c Config) APIRateLimit() (ratelimit.Limit, error) {
	l, err := ratelimit.Parse(orDefault(c.RateLimitAPI, DefaultRateLimitAPI))
	if err != nil {
		return l, fmt.Errorf("SILO_RATE_LIMIT_API: %w", err)
	}
	return l, nil
}

// AuthRateLimit parses SILO_RATE_LIMIT_AUTH (per client IP, sign-in,
// password and TOTP endpoints).
func (c Config) AuthRateLimit() (ratelimit.Limit, error) {
	l, err := ratelimit.Parse(orDefault(c.RateLimitAuth, DefaultRateLimitAuth))
	if err != nil {
		return l, fmt.Errorf("SILO_RATE_LIMIT_AUTH: %w", err)
	}
	return l, nil
}

// Defaults of SILO_RATE_LIMIT_API and SILO_RATE_LIMIT_AUTH (also used when the
// value is empty). 20/m is the sign-in throttling of earlier releases: burst 20,
// one more attempt every 3 s.
const (
	DefaultRateLimitAPI  = "600/m"
	DefaultRateLimitAuth = "20/m"
)

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// validateHardening reports invalid hardening settings.
func (c Config) validateHardening(errs *[]error) {
	if _, err := c.TrustedProxyPrefixes(); err != nil {
		*errs = append(*errs, err)
	}
	if _, err := c.APIRateLimit(); err != nil {
		*errs = append(*errs, err)
	}
	if _, err := c.AuthRateLimit(); err != nil {
		*errs = append(*errs, err)
	}
	if v := strings.TrimSpace(c.AutoconfigISPDBURL); v != "" && v != "off" {
		if u, err := url.Parse(v); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			*errs = append(*errs, fmt.Errorf("SILO_AUTOCONFIG_ISPDB_URL must be an http(s) URL or off, got %q", v))
		}
	}
}
