package api_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/silopost/silo-server/internal/accounts"
	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/discover"
	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/netx"
	"github.com/silopost/silo-server/internal/ratelimit"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

// discoverDNS knows no host and no SRV record. With private set, every name
// but the passthrough ones (the default servers of the test instance, looked
// up normally) resolves to a private address, so that the anti-SSRF check
// refuses it before any connection: no test reaches the real network.
type discoverDNS struct {
	private     bool
	passthrough map[string]bool
}

func (r discoverDNS) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if r.passthrough[host] {
		return net.DefaultResolver.LookupNetIP(ctx, network, host)
	}
	if r.private {
		return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (discoverDNS) LookupSRV(_ context.Context, service, proto, name string) (string, []*net.SRV, error) {
	return "", nil, &net.DNSError{Err: "no such host", Name: "_" + service + "._" + proto + "." + name, IsNotFound: true}
}

const discoverISPDB = `<?xml version="1.0"?>
<clientConfig version="1.1"><emailProvider id="example.test">
  <domain>example.test</domain><displayName>Example Mail</displayName>
  <incomingServer type="imap"><hostname>imap.example.test</hostname><port>993</port><socketType>SSL</socketType>
    <username>%EMAILADDRESS%</username><authentication>password-cleartext</authentication></incomingServer>
  <outgoingServer type="smtp"><hostname>smtp.example.test</hostname><port>587</port><socketType>STARTTLS</socketType>
    <username>%EMAILADDRESS%</username><authentication>password-cleartext</authentication></outgoingServer>
</emailProvider></clientConfig>`

type discoveryBody struct {
	Email       string                `json:"email"`
	Domain      string                `json:"domain"`
	IMAP        *api.DiscoveredServer `json:"imap"`
	SMTP        *api.DiscoveredServer `json:"smtp"`
	Username    string                `json:"username"`
	LoginFormat string                `json:"loginFormat"`
	DisplayName string                `json:"displayName"`
	Source      *string               `json:"source"`
	Sources     []api.DiscoverySource `json:"sources"`
	Provider    string                `json:"provider"`
	Params      map[string]any        `json:"params"`
}

func TestDiscoverAccount(t *testing.T) {
	e := newEnv(t)
	var hits atomic.Int32
	ispdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1.1/example.test" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(discoverISPDB))
	}))
	t.Cleanup(ispdb.Close)
	// The fake ISPDB listens on 127.0.0.1, which only the allowlist lets
	// through; autoconfig, SRV and guessed hosts do not resolve.
	disc := &discover.Discoverer{
		Dialer:   &netx.Dialer{Policy: netx.NewPolicy([]string{"127.0.0.1"}), Resolver: discoverDNS{}},
		Resolver: discoverDNS{}, ISPDBURL: ispdb.URL + "/v1.1/", StepTimeout: time.Second, Timeout: 3 * time.Second,
	}
	mount := func(allowCustom bool, limit ratelimit.Limit) http.Handler {
		cfg := e.cfg
		cfg.AuthMode, cfg.AllowCustomServers = config.AuthModeIMAP, allowCustom
		r := chi.NewRouter()
		api.Mount(r, api.Options{
			AuthMode: cfg.AuthMode, AllowCustomServers: allowCustom, PublicURL: publicURL,
			Auth: auth.New(auth.Options{Pool: e.pool, Keys: e.ring, Config: cfg}),
			// Custom hosts resolve to a private address: POST /accounts/test
			// refuses them after validation, without a connection.
			Accounts: accounts.New(accounts.Options{Pool: e.pool, Keys: e.ring, Config: cfg, Resolver: discoverDNS{private: true, passthrough: map[string]bool{cfg.DefaultIMAPHost: true, cfg.DefaultSMTPHost: true}}}),
			Discovery: api.DiscoveryOptions{
				Discoverer:         disc,
				DefaultIMAP:        discover.Server{Host: cfg.DefaultIMAPHost, Port: cfg.DefaultIMAPPort, TLS: cfg.DefaultIMAPTLS},
				DefaultSMTP:        discover.Server{Host: cfg.DefaultSMTPHost, Port: cfg.DefaultSMTPPort, TLS: cfg.DefaultSMTPTLS},
				DefaultLoginFormat: cfg.DefaultLoginFormat,
				Limit:              limit,
			},
		})
		return r
	}
	u := newUserSession(t, e, "discover@silo.test")
	post := func(h http.Handler, l login, path string, body any) resp {
		return serve(h, req{method: "POST", path: path, body: body, cookie: l.cookie, csrf: l.csrf})
	}
	// testParams turns a discovery into a POST /accounts/test body.
	testParams := func(d discoveryBody) map[string]any {
		p := map[string]any{"password": imaptest.Pass}
		for k, v := range d.Params {
			p[k] = v
		}
		return p
	}

	t.Run("default servers only", func(t *testing.T) {
		h := mount(false, ratelimit.Limit{})
		r := post(h, u, "/accounts/discover", map[string]string{"email": imaptest.User + "@Silo.Test"})
		if r.Code != http.StatusOK {
			t.Fatalf("status %d: %s", r.Code, r.Body)
		}
		var d discoveryBody
		r.json(&d)
		if d.Source == nil || *d.Source != "default" || d.IMAP == nil || d.IMAP.Host != e.cfg.DefaultIMAPHost || d.IMAP.Port != e.cfg.DefaultIMAPPort ||
			d.SMTP == nil || d.SMTP.Port != e.cfg.DefaultSMTPPort || d.Username != imaptest.User || d.LoginFormat != "localpart" ||
			d.Email != imaptest.User+"@silo.test" || d.Provider != "generic" || len(d.Sources) != 1 {
			t.Fatalf("discovery %s", r.Body)
		}
		if n := hits.Load(); n != 0 {
			t.Fatalf("%d requests to the ISPDB without custom servers", n)
		}
		// The result is a valid POST /accounts/test body (and here the
		// default server accepts it).
		tr := post(h, u, "/accounts/test", testParams(d))
		if tr.Code != http.StatusOK {
			t.Fatalf("POST /accounts/test: %d %s", tr.Code, tr.Body)
		}
		var res accounts.TestResult
		tr.json(&res)
		if !res.IMAP.OK || !res.SMTP.OK {
			t.Fatalf("connection test of the default servers: %s", tr.Body)
		}
	})

	t.Run("invalid email", func(t *testing.T) {
		h := mount(true, ratelimit.Limit{})
		for _, body := range []any{map[string]string{"email": "not-an-email"}, map[string]string{"email": "jan@[10.0.0.1]"}, map[string]string{}} {
			if r := post(h, u, "/accounts/discover", body); r.Code != http.StatusBadRequest || r.code() != api.CodeValidation {
				t.Fatalf("%v: %d %s", body, r.Code, r.Body)
			}
		}
		if r := serve(h, req{method: "POST", path: "/accounts/discover", body: map[string]string{"email": "jan@example.test"}}); r.Code != http.StatusUnauthorized {
			t.Fatalf("without a session: %d", r.Code)
		}
	})

	t.Run("custom servers", func(t *testing.T) {
		hits.Store(0)
		h := mount(true, ratelimit.Limit{})
		r := post(h, u, "/accounts/discover", map[string]string{"email": "jan@example.test"})
		if r.Code != http.StatusOK {
			t.Fatalf("status %d: %s", r.Code, r.Body)
		}
		var d discoveryBody
		r.json(&d)
		if d.Source == nil || *d.Source != "ispdb" || d.IMAP == nil || *d.IMAP != (api.DiscoveredServer{Host: "imap.example.test", Port: 993, TLS: "implicit"}) ||
			d.SMTP == nil || *d.SMTP != (api.DiscoveredServer{Host: "smtp.example.test", Port: 587, TLS: "starttls"}) ||
			d.Username != "jan@example.test" || d.LoginFormat != "email" || d.DisplayName != "Example Mail" || d.Domain != "example.test" {
			t.Fatalf("discovery %s", r.Body)
		}
		if d.Params["imapHost"] != "imap.example.test" || d.Params["smtpTls"] != "starttls" || d.Params["username"] != "jan@example.test" {
			t.Fatalf("params %v", d.Params)
		}
		if hits.Load() != 1 || len(d.Sources) != 3 || !d.Sources[0].OK {
			t.Fatalf("hits %d, sources %+v", hits.Load(), d.Sources)
		}
		// Formally accepted by POST /accounts/test: it gets past validation
		// to the anti-SSRF check (the fake DNS gives a private address).
		if tr := post(h, u, "/accounts/test", testParams(d)); tr.Code != http.StatusUnprocessableEntity || tr.code() != api.CodeHostNotAllowed {
			t.Fatalf("POST /accounts/test: %d %s", tr.Code, tr.Body)
		}
		// Nothing found: 200 with nulls and the reasons.
		r = post(h, u, "/accounts/discover", map[string]string{"email": "jan@nothing.test"})
		var none discoveryBody
		r.json(&none)
		if r.Code != http.StatusOK || none.IMAP != nil || none.SMTP != nil || none.Source != nil || len(none.Sources) != 4 {
			t.Fatalf("nothing found: %d %s", r.Code, r.Body)
		}
	})

	t.Run("rate limit per user", func(t *testing.T) {
		h := mount(false, ratelimit.Limit{Burst: 2, Every: time.Minute})
		for i := range 2 {
			if r := post(h, u, "/accounts/discover", map[string]string{"email": "jan@silo.test"}); r.Code != http.StatusOK {
				t.Fatalf("call %d: %d %s", i, r.Code, r.Body)
			}
		}
		r := post(h, u, "/accounts/discover", map[string]string{"email": "jan@silo.test"})
		if r.Code != http.StatusTooManyRequests || r.code() != api.CodeRateLimited || r.Header().Get("Retry-After") == "" {
			t.Fatalf("third call: %d %s", r.Code, r.Body)
		}
		other := newUserSession(t, e, "discover-other@silo.test")
		if r := post(h, other, "/accounts/discover", map[string]string{"email": "jan@silo.test"}); r.Code != http.StatusOK {
			t.Fatalf("another user: %d %s", r.Code, r.Body)
		}
	})
}

// DiscoveredServer is answered as null when not found.
func TestDiscoveryContractNullable(t *testing.T) {
	a := api.Mount(chi.NewRouter(), api.Options{})
	schemas := a.OpenAPI().Components.Schemas.Map()
	if s := schemas["DiscoveredServer"]; s == nil || !s.Nullable {
		t.Fatal("DiscoveredServer is not nullable")
	}
	op := a.OpenAPI().Paths["/accounts/discover"]
	if op == nil || op.Post == nil || op.Post.Responses["429"] == nil || op.Post.Responses["400"] == nil {
		t.Fatal("POST /accounts/discover lacks its 400/429 responses")
	}
}
