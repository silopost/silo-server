package discover

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/silopost/silo-server/internal/netx"
)

// maxRedirects bounds the redirects followed by one autoconfig request.
const maxRedirects = 3

// newHTTPClient builds the client of the ISPDB and autoconfig requests: every
// connection goes through dialer (anti-SSRF on the address actually dialled),
// proxies from the environment are ignored, and redirects are followed only to
// https on the same host or the ISPDB host.
func newHTTPClient(dialer *netx.Dialer, roots *x509.CertPool, ispdbHost string) *http.Client {
	tr := &http.Transport{
		Proxy:                  nil,
		DialContext:            dialer.DialContext,
		TLSClientConfig:        &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    DefaultStepTimeout,
		ResponseHeaderTimeout:  DefaultStepTimeout,
		MaxResponseHeaderBytes: 32 << 10,
		DisableKeepAlives:      true,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to %s refused: not https", req.URL.Scheme)
			}
			host := netx.NormalizeHost(req.URL.Hostname())
			if host != netx.NormalizeHost(via[0].URL.Hostname()) && (ispdbHost == "" || host != ispdbHost) {
				return fmt.Errorf("redirect to another host (%s) refused", host)
			}
			return nil
		},
	}
}

// ispdbBase is the ISPDB base URL, or "" when disabled.
func (d *Discoverer) ispdbBase() string {
	switch strings.TrimSpace(d.ISPDBURL) {
	case "":
		return DefaultISPDBURL
	case "off":
		return ""
	}
	return strings.TrimSpace(d.ISPDBURL)
}

func (d *Discoverer) ispdbHost() string {
	u, err := url.Parse(d.ispdbBase())
	if err != nil {
		return ""
	}
	return netx.NormalizeHost(u.Hostname())
}

// ispdb looks the domain up in the Thunderbird ISPDB.
func (d *Discoverer) ispdb(ctx context.Context, email, domain string) found {
	base := d.ispdbBase()
	if base == "" {
		return found{detail: "disabled (SILO_AUTOCONFIG_ISPDB_URL=off)"}
	}
	u := strings.TrimSuffix(base, "/") + "/" + domain
	body, err := d.fetch(ctx, u)
	if err != nil {
		return found{err: err}
	}
	return parseConfig(body, email, domain)
}

// autoconfig fetches the domain's own autoconfig file: first from
// autoconfig.<domain>, then from the well-known path of the domain.
func (d *Discoverer) autoconfig(ctx context.Context, email, domain string) found {
	urls := []string{
		"https://autoconfig." + domain + "/mail/config-v1.1.xml?emailaddress=" + url.QueryEscape(email),
		"https://" + domain + "/.well-known/autoconfig/mail/config-v1.1.xml",
	}
	var errs []string
	for _, u := range urls {
		body, err := d.fetch(ctx, u)
		if err == nil {
			f := parseConfig(body, email, domain)
			if f.ok() {
				f.detail = joinDetail(hostOf(u), f.detail)
				return f
			}
			err = f.err
			if err == nil {
				err = errors.New(f.detail)
			}
		}
		errs = append(errs, hostOf(u)+": "+errText(err))
		if ctx.Err() != nil {
			break
		}
	}
	return found{err: errors.New(strings.Join(errs, "; "))}
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Host
}

func errText(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

func joinDetail(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "; ")
}

// fetch GETs rawURL and returns at most MaxResponseBytes of a 200 response
// (larger responses are an error).
func (d *Discoverer) fetch(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/xml, text/xml")
	req.Header.Set("User-Agent", "silo-server autoconfig")
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, errors.New(errText(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("not found")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, errors.New(errText(err))
	}
	if len(body) > MaxResponseBytes {
		return nil, fmt.Errorf("response larger than %d bytes", MaxResponseBytes)
	}
	return body, nil
}

// Thunderbird autoconfig format (clientConfig version 1.1).
type clientConfig struct {
	XMLName   xml.Name        `xml:"clientConfig"`
	Providers []emailProvider `xml:"emailProvider"`
}

type emailProvider struct {
	DisplayName string      `xml:"displayName"`
	Incoming    []xmlServer `xml:"incomingServer"`
	Outgoing    []xmlServer `xml:"outgoingServer"`
}

type xmlServer struct {
	Type       string   `xml:"type,attr"`
	Hostname   string   `xml:"hostname"`
	Port       string   `xml:"port"`
	SocketType string   `xml:"socketType"`
	Username   string   `xml:"username"`
	Auth       []string `xml:"authentication"`
}

// parseConfig reads an autoconfig document: the first acceptable IMAP
// incomingServer and SMTP outgoingServer of the first emailProvider. POP3,
// unencrypted servers and servers without a password or OAuth2 method are
// skipped (and noted in the detail).
func parseConfig(data []byte, email, domain string) found {
	var cfg clientConfig
	if err := xml.Unmarshal(data, &cfg); err != nil {
		return found{err: fmt.Errorf("invalid autoconfig XML: %w", err)}
	}
	if len(cfg.Providers) == 0 {
		return found{err: errors.New("no emailProvider in autoconfig XML")}
	}
	p := cfg.Providers[0]
	var notes []string
	pick := func(list []xmlServer, typ string) (*Server, string) {
		for _, s := range list {
			if !strings.EqualFold(s.Type, typ) {
				continue
			}
			srv, oauth, why := convert(s, domain)
			if srv == nil {
				notes = append(notes, fmt.Sprintf("%s %s skipped: %s", typ, strings.ReplaceAll(s.Hostname, "%EMAILDOMAIN%", domain), why))
				continue
			}
			if oauth != "" {
				notes = append(notes, fmt.Sprintf("%s %s: %s", typ, srv.Host, oauth))
			}
			return srv, s.Username
		}
		return nil, ""
	}
	f := found{displayName: strings.TrimSpace(p.DisplayName)}
	var userIMAP, userSMTP string
	f.imap, userIMAP = pick(p.Incoming, "imap")
	f.smtp, userSMTP = pick(p.Outgoing, "smtp")
	user := userIMAP
	if f.imap == nil {
		user = userSMTP
	}
	switch strings.TrimSpace(user) {
	case "%EMAILLOCALPART%":
		f.loginFormat = LoginLocalpart
	case "%EMAILADDRESS%", "":
		f.loginFormat = LoginEmail
	default:
		f.loginFormat = LoginEmail
		notes = append(notes, fmt.Sprintf("username %q not supported, using the address", user))
	}
	if f.imap == nil && f.smtp == nil {
		notes = append(notes, "no usable IMAP or SMTP server")
	}
	f.detail = strings.Join(notes, "; ")
	return f
}

// convert maps an autoconfig server to a Server. It returns nil and the
// reason when the server is unusable, and a note when it offers OAuth2.
func convert(s xmlServer, domain string) (srv *Server, oauth, why string) {
	host := netx.NormalizeHost(strings.ReplaceAll(s.Hostname, "%EMAILDOMAIN%", domain))
	if !validDomain(host) {
		return nil, "", "invalid hostname"
	}
	port, err := strconv.Atoi(strings.TrimSpace(s.Port))
	if err != nil || port < 1 || port > 65535 {
		return nil, "", "invalid port"
	}
	var mode string
	switch strings.ToUpper(strings.TrimSpace(s.SocketType)) {
	case "SSL", "TLS":
		mode = TLSImplicit
	case "STARTTLS":
		mode = TLSStartTLS
	default:
		return nil, "", "unencrypted (socketType " + strings.TrimSpace(s.SocketType) + ")"
	}
	password, oauth2 := len(s.Auth) == 0, false
	for _, a := range s.Auth {
		switch strings.ToLower(strings.TrimSpace(a)) {
		case "password-cleartext", "password-encrypted":
			password = true
		case "oauth2":
			oauth2 = true
		}
	}
	switch {
	case !password && !oauth2:
		return nil, "", "no supported authentication (" + strings.Join(s.Auth, ", ") + ")"
	case oauth2 && !password:
		oauth = "OAuth2 only"
	case oauth2:
		oauth = "OAuth2 available"
	}
	return &Server{Host: host, Port: port, TLS: mode}, oauth, ""
}
