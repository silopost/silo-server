package main

import (
	"log/slog"

	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/discover"
	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/netx"
)

// discoveryOptions configures POST /accounts/discover. Its requests (ISPDB,
// autoconfig files, handshakes with guessed hosts) go through an anti-SSRF
// dialer with the custom server allowlist: public addresses and allowlisted
// hosts only. The default servers are not trusted here; they answer without
// any network access when they are the only ones allowed.
func discoveryOptions(cfg config.Config, logger *slog.Logger) api.DiscoveryOptions {
	return api.DiscoveryOptions{
		Discoverer: &discover.Discoverer{
			Dialer:   &netx.Dialer{Policy: netx.NewPolicy(cfg.CustomServerAllowlist)},
			ISPDBURL: cfg.AutoconfigISPDBURL,
			Logger:   logger.With("component", "discover"),
		},
		DefaultIMAP:        discover.Server{Host: cfg.DefaultIMAPHost, Port: cfg.DefaultIMAPPort, TLS: cfg.DefaultIMAPTLS},
		DefaultSMTP:        discover.Server{Host: cfg.DefaultSMTPHost, Port: cfg.DefaultSMTPPort, TLS: cfg.DefaultSMTPTLS},
		DefaultLoginFormat: cfg.DefaultLoginFormat,
	}
}
