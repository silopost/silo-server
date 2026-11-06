// Package discover finds the IMAP and SMTP settings of an email address
// (account autoconfiguration, plan 3.15): the Thunderbird ISPDB, the
// autoconfig file of the mail domain, DNS SRV records (RFC 6186) and, last,
// guessed host names checked with a TLS handshake.
//
// Every outgoing connection goes through a netx.Dialer, so a domain the user
// controls cannot point Silo at an internal address (anti-SSRF).
package discover

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/silopost/silo-server/internal/netx"
)

// TLS modes of a discovered server. Discovery never returns an unencrypted
// server.
const (
	TLSImplicit = "implicit"
	TLSStartTLS = "starttls"
)

// Sources of a result, in order of preference.
const (
	SourceISPDB      = "ispdb"
	SourceAutoconfig = "autoconfig"
	SourceSRV        = "srv"
	SourceGuess      = "guess"
	SourceDefault    = "default"
)

// Login formats (the username is the address or its local part).
const (
	LoginEmail     = "email"
	LoginLocalpart = "localpart"
)

// Providers (accounts.provider).
const (
	ProviderGeneric   = "generic"
	ProviderGmail     = "gmail"
	ProviderMicrosoft = "microsoft"
)

// DefaultISPDBURL is the base URL of the Thunderbird ISPDB; the domain is
// appended.
const DefaultISPDBURL = "https://autoconfig.thunderbird.net/v1.1/"

// Limits of a discovery.
const (
	MaxTimeout         = 8 * time.Second
	DefaultStepTimeout = 3 * time.Second
	MaxResponseBytes   = 256 << 10
)

// ErrInvalidEmail means the address is not a usable user@domain.
var ErrInvalidEmail = errors.New("discover: invalid email address")

// Server is a discovered IMAP or SMTP endpoint.
type Server struct {
	Host string
	Port int
	TLS  string // TLSImplicit or TLSStartTLS
}

func (s *Server) String() string {
	if s == nil {
		return "-"
	}
	return fmt.Sprintf("%s:%d %s", s.Host, s.Port, s.TLS)
}

// Attempt reports one source that was consulted.
type Attempt struct {
	Source string
	OK     bool
	Detail string
}

// Result is the outcome of a discovery. IMAP and SMTP are nil when no source
// found them; Source is then empty.
type Result struct {
	Email       string
	Domain      string
	IMAP        *Server
	SMTP        *Server
	Username    string
	LoginFormat string
	DisplayName string
	Source      string
	Sources     []Attempt
	Provider    string
}

// Resolver is what discovery needs from DNS: SRV records here, addresses for
// the dialer (net.DefaultResolver satisfies it).
type Resolver interface {
	netx.Resolver
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
}

// Discoverer runs discoveries; it is safe for concurrent use once built. The
// zero value works: public DNS, the default ISPDB and a dialer that reaches
// public addresses only.
type Discoverer struct {
	// HTTP fetches the ISPDB and autoconfig files. When nil, a client is
	// built over Dialer (no proxy, https-only redirects, see newHTTPClient).
	// A caller-supplied client must dial through a netx.Dialer itself.
	HTTP *http.Client
	// Dialer makes every connection (HTTP and the guess handshakes). When
	// nil, &netx.Dialer{Resolver: Resolver}; a nil Policy is replaced by the
	// empty policy (public addresses only), never left unfiltered.
	Dialer *netx.Dialer
	// Resolver answers SRV lookups (net.DefaultResolver when nil).
	Resolver Resolver
	// ISPDBURL is the ISPDB base URL (DefaultISPDBURL when empty, "off"
	// disables the ISPDB). The domain is appended.
	ISPDBURL string
	// Timeout bounds a whole discovery (MaxTimeout when 0, never more).
	Timeout time.Duration
	// StepTimeout bounds each source (DefaultStepTimeout when 0).
	StepTimeout time.Duration
	// RootCAs verifies TLS certificates (system roots when nil; tests).
	RootCAs *x509.CertPool
	Logger  *slog.Logger

	once   sync.Once
	dialer *netx.Dialer
	client *http.Client
}

func (d *Discoverer) init() {
	d.once.Do(func() {
		if d.Resolver == nil {
			d.Resolver = net.DefaultResolver
		}
		if d.Dialer == nil {
			d.dialer = &netx.Dialer{Resolver: d.Resolver}
		} else {
			dd := *d.Dialer
			d.dialer = &dd
		}
		if d.dialer.Policy == nil {
			d.dialer.Policy = netx.NewPolicy(nil)
		}
		d.client = d.HTTP
		if d.client == nil {
			d.client = newHTTPClient(d.dialer, d.RootCAs, d.ispdbHost())
		}
		if d.Logger == nil {
			d.Logger = slog.Default()
		}
	})
}

func (d *Discoverer) timeout() time.Duration {
	if d.Timeout <= 0 || d.Timeout > MaxTimeout {
		return MaxTimeout
	}
	return d.Timeout
}

func (d *Discoverer) stepTimeout() time.Duration {
	if d.StepTimeout <= 0 {
		return DefaultStepTimeout
	}
	return d.StepTimeout
}

// ParseEmail splits an address into its local part and lower-cased ASCII
// domain. Address literals ([1.2.3.4]) and internationalised domains are
// refused.
func ParseEmail(email string) (local, domain string, err error) {
	email = strings.TrimSpace(email)
	at := strings.LastIndexByte(email, '@')
	if len(email) > 254 || at < 1 || at == len(email)-1 {
		return "", "", ErrInvalidEmail
	}
	local, domain = email[:at], strings.ToLower(strings.TrimSuffix(email[at+1:], "."))
	if len(local) > 64 || strings.ContainsAny(local, " \t\r\n<>\"(),;:\\@[]") {
		return "", "", ErrInvalidEmail
	}
	if !validDomain(domain) {
		return "", "", ErrInvalidEmail
	}
	return local, domain, nil
}

// validDomain reports whether s is an ASCII DNS name with at least two labels
// whose last label is not numeric (an IPv4 address is not a domain).
func validDomain(s string) bool {
	if len(s) > 253 || !strings.Contains(s, ".") {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, c := range l {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	last := s[strings.LastIndexByte(s, '.')+1:]
	return strings.Trim(last, "0123456789") != ""
}

// ProviderOf names the provider of an IMAP host: gmail or microsoft for the
// servers that get OAuth2 and provider-specific sync, generic otherwise.
func ProviderOf(host string) string {
	h := netx.NormalizeHost(host)
	switch {
	case h == "imap.gmail.com" || strings.HasSuffix(h, ".google.com"):
		return ProviderGmail
	case h == "outlook.office365.com" || strings.HasSuffix(h, ".outlook.com"):
		return ProviderMicrosoft
	}
	return ProviderGeneric
}

// Default answers with the instance's default servers without contacting
// anything (SILO_ALLOW_CUSTOM_SERVERS=false in mode imap: no other host could
// be added anyway). loginFormat is SILO_DEFAULT_LOGIN_FORMAT.
func Default(email string, imap, smtp Server, loginFormat string) (Result, error) {
	local, domain, err := ParseEmail(email)
	if err != nil {
		return Result{}, err
	}
	email = local + "@" + domain
	r := Result{Email: email, Domain: domain, LoginFormat: LoginEmail, Username: email, Source: SourceDefault}
	if loginFormat == LoginLocalpart {
		r.LoginFormat, r.Username = LoginLocalpart, local
	}
	if imap.Host != "" {
		r.IMAP = &imap
	}
	if smtp.Host != "" {
		r.SMTP = &smtp
	}
	r.Provider = ProviderOf(imap.Host)
	r.Sources = []Attempt{{Source: SourceDefault, OK: true, Detail: "default servers of this instance; custom servers are disabled"}}
	return r, nil
}

// found is what one source contributes.
type found struct {
	imap, smtp  *Server
	loginFormat string
	displayName string
	detail      string
	err         error
}

func (f found) ok() bool { return f.err == nil && (f.imap != nil || f.smtp != nil) }

// errSkipped marks a source cancelled because a better one answered.
var errSkipped = errors.New("skipped: a better-ranked source answered")

// Discover finds the settings of email. The only error is ErrInvalidEmail;
// failing sources are reported in Result.Sources.
//
// Order: the ISPDB, the domain's autoconfig file and the SRV records are
// queried in parallel, each within StepTimeout, and ranked in that order. As
// soon as a source has answered with both servers and every better-ranked
// source has finished, the rest are cancelled. Only when a protocol is still
// missing are guessed host names tried (also within StepTimeout). The whole
// discovery is bounded by Timeout (at most 8 s). Each protocol takes the
// server of the best source that has one; Source is the source of the IMAP
// server (else of the SMTP server).
func (d *Discoverer) Discover(ctx context.Context, email string) (Result, error) {
	local, domain, err := ParseEmail(email)
	if err != nil {
		return Result{}, err
	}
	d.init()
	email = local + "@" + domain
	ctx, cancel := context.WithTimeout(ctx, d.timeout())
	defer cancel()

	steps := []struct {
		name string
		run  func(context.Context) found
	}{
		{SourceISPDB, func(ctx context.Context) found { return d.ispdb(ctx, email, domain) }},
		{SourceAutoconfig, func(ctx context.Context) found { return d.autoconfig(ctx, email, domain) }},
		{SourceSRV, func(ctx context.Context) found { return d.srv(ctx, domain) }},
	}
	results := make([]*found, len(steps))
	phaseCtx, cancelPhase := context.WithCancel(ctx)
	defer cancelPhase()
	type done struct {
		i int
		f found
	}
	ch := make(chan done, len(steps))
	for i, s := range steps {
		go func() {
			sctx, scancel := context.WithTimeout(phaseCtx, d.stepTimeout())
			defer scancel()
			f := s.run(sctx)
			if !f.ok() && phaseCtx.Err() != nil && ctx.Err() == nil {
				f = found{err: errSkipped} // cancelled: a better-ranked source answered
			}
			ch <- done{i, f}
		}()
	}
	cancelled := false
	for range steps {
		r := <-ch
		results[r.i] = &r.f
		if !cancelled && decided(results) {
			cancelled = true
			cancelPhase()
		}
	}

	res := Result{Email: email, Domain: domain, LoginFormat: LoginEmail}
	for i, s := range steps {
		f := *results[i]
		res.Sources = append(res.Sources, attempt(s.name, f))
		res.merge(s.name, f)
	}
	if res.IMAP == nil || res.SMTP == nil {
		if ctx.Err() != nil {
			res.Sources = append(res.Sources, Attempt{Source: SourceGuess, Detail: "skipped: discovery timed out"})
		} else {
			sctx, scancel := context.WithTimeout(ctx, d.stepTimeout())
			f := d.guess(sctx, domain, res.IMAP == nil, res.SMTP == nil)
			scancel()
			res.Sources = append(res.Sources, attempt(SourceGuess, f))
			res.merge(SourceGuess, f)
		}
	}
	res.Username = email
	if res.LoginFormat == LoginLocalpart {
		res.Username = local
	}
	res.Provider = ProviderGeneric
	if res.IMAP != nil {
		res.Provider = ProviderOf(res.IMAP.Host)
	}
	d.Logger.DebugContext(ctx, "account discovery", "domain", domain, "source", res.Source,
		"imap", res.IMAP.String(), "smtp", res.SMTP.String())
	return res, nil
}

// decided reports whether a finished source has both servers and every
// better-ranked source has finished, so the remaining ones cannot change the
// result.
func decided(results []*found) bool {
	for _, f := range results {
		if f == nil {
			return false
		}
		if f.ok() && f.imap != nil && f.smtp != nil {
			return true
		}
	}
	return false
}

// merge takes the servers of f that are still missing (sources are merged
// best first).
func (r *Result) merge(source string, f found) {
	if !f.ok() {
		return
	}
	if r.IMAP == nil && f.imap != nil {
		r.IMAP = f.imap
		r.takeFrom(source, f)
	}
	if r.SMTP == nil && f.smtp != nil {
		r.SMTP = f.smtp
		if r.Source == "" {
			r.takeFrom(source, f)
		}
	}
}

// takeFrom makes source the source of the result (the one of its IMAP
// server, else of its SMTP server).
func (r *Result) takeFrom(source string, f found) {
	r.Source = source
	r.LoginFormat = LoginEmail
	if f.loginFormat != "" {
		r.LoginFormat = f.loginFormat
	}
	r.DisplayName = f.displayName
}

func attempt(source string, f found) Attempt {
	a := Attempt{Source: source, OK: f.ok(), Detail: f.detail}
	if f.err != nil {
		msg := f.err.Error()
		if errors.Is(f.err, context.DeadlineExceeded) {
			msg = "timed out"
		}
		if a.Detail != "" {
			msg = a.Detail + "; " + msg
		}
		a.Detail = msg
	} else if !a.OK && a.Detail == "" {
		a.Detail = "no IMAP or SMTP server found"
	}
	return a
}
