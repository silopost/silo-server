package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/discover"
	"github.com/silopost/silo-server/internal/ratelimit"
)

// Discoverer finds the server settings of an address (*discover.Discoverer).
type Discoverer interface {
	Discover(ctx context.Context, email string) (discover.Result, error)
}

// DiscoveryOptions configures POST /accounts/discover.
type DiscoveryOptions struct {
	// Discoverer runs the network discovery when custom servers are allowed
	// (Options.AllowCustomServers or mode local); a zero discover.Discoverer
	// when nil.
	Discoverer Discoverer
	// DefaultIMAP, DefaultSMTP and DefaultLoginFormat (SILO_DEFAULT_*) answer
	// without any network access when only the default servers may be used.
	DefaultIMAP, DefaultSMTP discover.Server
	DefaultLoginFormat       string
	// Limit is the per-user limit of the endpoint (DefaultDiscoveryLimit when
	// zero): each call may make several outgoing requests.
	Limit ratelimit.Limit
	// Now is the clock of the limit (time.Now when nil).
	Now func() time.Time
}

// DefaultDiscoveryLimit is 10 discoveries per minute and user.
var DefaultDiscoveryLimit = ratelimit.Limit{Burst: 10, Every: 6 * time.Second}

// DiscoverRequest is the body of POST /accounts/discover.
type DiscoverRequest struct {
	Email string `json:"email" maxLength:"254" doc:"Address of the account to configure."`
}

// DiscoveredServer is a discovered IMAP or SMTP server. The schema is
// nullable: imap and smtp are null when not found.
type DiscoveredServer struct {
	_    struct{} `nullable:"true"`
	Host string   `json:"host"`
	Port int      `json:"port" minimum:"1" maximum:"65535"`
	TLS  string   `json:"tls" enum:"implicit,starttls,none" doc:"none only for the default servers of the instance (source default); discovery never returns an unencrypted server."`
}

// DiscoverySource reports one source that was consulted.
type DiscoverySource struct {
	Source string `json:"source" enum:"ispdb,autoconfig,srv,guess,default"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail" doc:"What was found, or why the source gave nothing (for people, not for parsing)."`
}

// DiscoveredParams are the discovered settings in the shape of
// ConnectionParams: add `password` and send them to POST /accounts/test and
// POST /accounts. Fields not found are omitted (the instance defaults then
// apply).
type DiscoveredParams struct {
	Email    string `json:"email"`
	Username string `json:"username"`
	IMAPHost string `json:"imapHost,omitempty"`
	IMAPPort int    `json:"imapPort,omitempty"`
	IMAPTLS  string `json:"imapTls,omitempty" enum:"implicit,starttls,none"`
	SMTPHost string `json:"smtpHost,omitempty"`
	SMTPPort int    `json:"smtpPort,omitempty"`
	SMTPTLS  string `json:"smtpTls,omitempty" enum:"implicit,starttls,none"`
}

// Discovery is the result of POST /accounts/discover.
type Discovery struct {
	Email       string            `json:"email"`
	Domain      string            `json:"domain"`
	IMAP        *DiscoveredServer `json:"imap" doc:"Null when no source found an IMAP server."`
	SMTP        *DiscoveredServer `json:"smtp" doc:"Null when no source found an SMTP server."`
	Username    string            `json:"username" doc:"Login name to use (the address or its local part)."`
	LoginFormat string            `json:"loginFormat" enum:"email,localpart"`
	DisplayName string            `json:"displayName,omitempty" doc:"Name of the mail provider, when the source gives one."`
	Source      *string           `json:"source" doc:"ispdb, autoconfig, srv, guess or default: the source of the IMAP server (else of the SMTP server); null when nothing was found."`
	Sources     []DiscoverySource `json:"sources" doc:"Every source consulted, in order of preference."`
	Provider    string            `json:"provider" enum:"generic,gmail,microsoft" doc:"gmail and microsoft are the providers with OAuth2 and provider-specific sync."`
	Params      DiscoveredParams  `json:"params" doc:"The same settings as ConnectionParams fields."`
}

func discoveryOf(r discover.Result) Discovery {
	out := Discovery{
		Email: r.Email, Domain: r.Domain, Username: r.Username, LoginFormat: r.LoginFormat,
		DisplayName: r.DisplayName, Provider: r.Provider, Sources: make([]DiscoverySource, 0, len(r.Sources)),
		Params: DiscoveredParams{Email: r.Email, Username: r.Username},
	}
	if r.Source != "" {
		out.Source = &r.Source
	}
	if r.IMAP != nil {
		out.IMAP = &DiscoveredServer{Host: r.IMAP.Host, Port: r.IMAP.Port, TLS: r.IMAP.TLS}
		out.Params.IMAPHost, out.Params.IMAPPort, out.Params.IMAPTLS = r.IMAP.Host, r.IMAP.Port, r.IMAP.TLS
	}
	if r.SMTP != nil {
		out.SMTP = &DiscoveredServer{Host: r.SMTP.Host, Port: r.SMTP.Port, TLS: r.SMTP.TLS}
		out.Params.SMTPHost, out.Params.SMTPPort, out.Params.SMTPTLS = r.SMTP.Host, r.SMTP.Port, r.SMTP.TLS
	}
	for _, s := range r.Sources {
		out.Sources = append(out.Sources, DiscoverySource{Source: s.Source, OK: s.OK, Detail: s.Detail})
	}
	return out
}

type discoveryOutput struct{ Body Discovery }

// registerDiscover adds POST /accounts/discover.
func (d *deps) registerDiscover(a huma.API, o Options) {
	do := o.Discovery
	network := o.AllowCustomServers || o.AuthMode == config.AuthModeLocal
	disc := do.Discoverer
	if disc == nil {
		disc = &discover.Discoverer{Logger: d.log}
	}
	limit := do.Limit
	if !limit.Enabled() {
		limit = DefaultDiscoveryLimit
	}
	lim := ratelimit.New(limit, do.Now)

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "discoverAccount", Method: http.MethodPost, Path: "/accounts/discover", Tags: []string{"accounts"},
		Summary: "Find the server settings of an address",
		Description: "Looks the address up in the Thunderbird ISPDB, the autoconfig file of its domain " +
			"(autoconfig.<domain>, /.well-known/autoconfig), its SRV records (RFC 6186) and, last, the usual host " +
			"names checked by a TLS handshake (no login), within 8 s. The response is 200 even when nothing is " +
			"found (`imap` and `smtp` null, reasons in `sources`). `params` plus a password is a ConnectionParams " +
			"body for POST /accounts/test. When the instance only allows its default servers " +
			"(SILO_ALLOW_CUSTOM_SERVERS=false in mode imap) it answers with them at once (`source: default`) " +
			"without contacting anything. Limited per user (10 per minute): 429 rate_limited.",
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests},
	}), func(ctx context.Context, in *struct{ Body DiscoverRequest }) (*discoveryOutput, error) {
		if ok, wait := lim.Allow(strconv.FormatInt(sessionFrom(ctx).UserID, 10)); !ok {
			return nil, errRateLimited(wait)
		}
		var (
			res discover.Result
			err error
		)
		if network {
			res, err = disc.Discover(ctx, in.Body.Email)
		} else {
			res, err = discover.Default(in.Body.Email, do.DefaultIMAP, do.DefaultSMTP, do.DefaultLoginFormat)
		}
		if errors.Is(err, discover.ErrInvalidEmail) {
			return nil, problem(http.StatusBadRequest, CodeValidation, "email must be a valid address (user@domain).")
		}
		if err != nil {
			return nil, errInternal(err)
		}
		return &discoveryOutput{Body: discoveryOf(res)}, nil
	})
}
