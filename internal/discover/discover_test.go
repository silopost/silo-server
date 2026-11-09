package discover

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/netx"
)

// Public (TEST-NET-3) addresses the fake DNS hands out; the fake network
// routes them to local listeners. No test touches the real network.
const (
	ipISPDB      = "203.0.113.1"
	ipAutoconfig = "203.0.113.2"
	ipDomain     = "203.0.113.3"
	ipIMAP       = "203.0.113.10"
	ipMail       = "203.0.113.11"
	ipSMTP       = "203.0.113.12"
	ipPrivate    = "10.0.0.5"
)

// fakeDNS answers address and SRV lookups from maps (NXDOMAIN otherwise).
type fakeDNS struct {
	mu    sync.Mutex
	hosts map[string]string
	srv   map[string][]*net.SRV // "imaps" -> records
	hang  bool                  // SRV lookups block until the context ends
}

func (f *fakeDNS) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ip, ok := f.hosts[host]; ok {
		return []netip.Addr{netip.MustParseAddr(ip)}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (f *fakeDNS) LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error) {
	if f.hang {
		<-ctx.Done()
		return "", nil, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if recs, ok := f.srv[service]; ok {
		return "", recs, nil
	}
	return "", nil, &net.DNSError{Err: "no such host", Name: "_" + service + "._" + proto + "." + name, IsNotFound: true}
}

// fakeNet routes "ip:port" to local listener addresses and counts the
// connection attempts per IP.
type fakeNet struct {
	mu     sync.Mutex
	routes map[string]string
	dials  map[string]int
}

func (n *fakeNet) route(ipPort, to string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.routes[ipPort] = to
}

func (n *fakeNet) count(ip string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dials[ip]
}

func (n *fakeNet) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, _ := net.SplitHostPort(addr)
	n.mu.Lock()
	n.dials[host]++
	to, ok := n.routes[addr]
	n.mu.Unlock()
	if !ok {
		return nil, errors.New("connection refused")
	}
	return (&net.Dialer{}).DialContext(ctx, network, to)
}

type world struct {
	t         *testing.T
	dns       *fakeDNS
	net       *fakeNet
	cert      tls.Certificate
	pool      *x509.CertPool
	mux       map[string]http.HandlerFunc // by Host
	ispdbHits atomic.Int32
}

var testNames = []string{"ispdb.test", "example.test", "autoconfig.example.test", "imap.example.test", "mail.example.test", "smtp.example.test"}

func newCert(t *testing.T, names ...string) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf
}

// newWorld starts one HTTPS server for every test host (dispatching on Host)
// and a fake DNS where ispdb.test is public and nothing else resolves yet.
func newWorld(t *testing.T) *world {
	t.Helper()
	cert, leaf := newCert(t, testNames...)
	w := &world{t: t, cert: cert, pool: x509.NewCertPool(), mux: map[string]http.HandlerFunc{},
		dns: &fakeDNS{hosts: map[string]string{"ispdb.test": ipISPDB}, srv: map[string][]*net.SRV{}},
		net: &fakeNet{routes: map[string]string{}, dials: map[string]int{}},
	}
	w.pool.AddCert(leaf)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Host == "ispdb.test" {
			w.ispdbHits.Add(1)
		}
		h, ok := w.mux[r.Host]
		if !ok {
			http.NotFound(rw, r)
			return
		}
		h(rw, r)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	for _, ip := range []string{ipISPDB, ipAutoconfig, ipDomain, ipPrivate} {
		w.net.route(ip+":443", addr)
	}
	return w
}

func (w *world) discoverer() *Discoverer {
	return &Discoverer{
		Dialer:   &netx.Dialer{Policy: netx.NewPolicy(nil), Resolver: w.dns, DialAddr: w.net.dial},
		Resolver: w.dns, ISPDBURL: "https://ispdb.test/v1.1/", RootCAs: w.pool,
		StepTimeout: time.Second, Timeout: 3 * time.Second,
	}
}

func (w *world) serveFile(host, path, file string) {
	data, err := os.ReadFile("testdata/" + file)
	if err != nil {
		w.t.Fatal(err)
	}
	w.mux[host] = func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(rw, r)
			return
		}
		rw.Header().Set("Content-Type", "text/xml")
		_, _ = rw.Write(data)
	}
}

func source(t *testing.T, r Result, name string) Attempt {
	t.Helper()
	for _, a := range r.Sources {
		if a.Source == name {
			return a
		}
	}
	t.Fatalf("no source %s in %+v", name, r.Sources)
	return Attempt{}
}

func run(t *testing.T, d *Discoverer, email string) Result {
	t.Helper()
	r, err := d.Discover(context.Background(), email)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("result %+v imap=%s smtp=%s", r, r.IMAP, r.SMTP)
	return r
}

func wantServer(t *testing.T, what string, got *Server, want Server) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %s, want %s", what, got, &want)
	}
}

func TestISPDBHit(t *testing.T) {
	w := newWorld(t)
	w.serveFile("ispdb.test", "/v1.1/example.test", "ispdb-example.test.xml")
	r := run(t, w.discoverer(), "Jan.Kowalski@Example.TEST")
	wantServer(t, "imap", r.IMAP, Server{"imap.example.test", 993, TLSImplicit})
	wantServer(t, "smtp", r.SMTP, Server{"smtp.example.test", 465, TLSImplicit})
	if r.Source != SourceISPDB || r.Email != "Jan.Kowalski@example.test" || r.Domain != "example.test" ||
		r.Username != "Jan.Kowalski@example.test" || r.LoginFormat != LoginEmail || r.DisplayName != "Example Mail" || r.Provider != ProviderGeneric {
		t.Fatalf("result %+v", r)
	}
	if a := source(t, r, SourceISPDB); !a.OK || !strings.Contains(a.Detail, "OAuth2") {
		t.Fatalf("ispdb attempt %+v", a)
	}
	for _, s := range r.Sources {
		if s.Source == SourceGuess {
			t.Fatal("guess ran although the ISPDB had both servers")
		}
	}
}

func TestAutoconfigOfDomain(t *testing.T) {
	t.Run("autoconfig host", func(t *testing.T) {
		w := newWorld(t)
		w.dns.hosts["autoconfig.example.test"] = ipAutoconfig
		var query string
		w.serveFile("autoconfig.example.test", "/mail/config-v1.1.xml", "autoconfig-localpart.xml")
		inner := w.mux["autoconfig.example.test"]
		w.mux["autoconfig.example.test"] = func(rw http.ResponseWriter, r *http.Request) {
			query = r.URL.Query().Get("emailaddress")
			inner(rw, r)
		}
		r := run(t, w.discoverer(), "jan@example.test")
		wantServer(t, "imap", r.IMAP, Server{"mx.example.test", 143, TLSStartTLS})
		wantServer(t, "smtp", r.SMTP, Server{"mx.example.test", 587, TLSStartTLS})
		if r.Source != SourceAutoconfig || r.LoginFormat != LoginLocalpart || r.Username != "jan" || query != "jan@example.test" {
			t.Fatalf("result %+v, query %q", r, query)
		}
		a := source(t, r, SourceAutoconfig)
		if !strings.Contains(a.Detail, "unencrypted") || !strings.Contains(a.Detail, "no supported authentication") {
			t.Fatalf("plain and GSSAPI servers not reported as skipped: %+v", a)
		}
		if i := source(t, r, SourceISPDB); i.OK || !strings.Contains(i.Detail, "not found") {
			t.Fatalf("ispdb attempt %+v", i)
		}
	})
	t.Run("well-known path", func(t *testing.T) {
		w := newWorld(t)
		w.dns.hosts["example.test"] = ipDomain
		w.serveFile("example.test", "/.well-known/autoconfig/mail/config-v1.1.xml", "autoconfig-localpart.xml")
		r := run(t, w.discoverer(), "jan@example.test")
		if r.Source != SourceAutoconfig || r.IMAP == nil {
			t.Fatalf("result %+v", r)
		}
	})
}

func TestPlainServerSkipped(t *testing.T) {
	w := newWorld(t)
	w.mux["ispdb.test"] = func(rw http.ResponseWriter, _ *http.Request) {
		_, _ = rw.Write([]byte(`<clientConfig version="1.1"><emailProvider id="example.test">
			<incomingServer type="imap"><hostname>imap.example.test</hostname><port>143</port><socketType>plain</socketType>
			<username>%EMAILADDRESS%</username><authentication>password-cleartext</authentication></incomingServer>
			<outgoingServer type="smtp"><hostname>smtp.example.test</hostname><port>25</port><socketType>plain</socketType></outgoingServer>
			</emailProvider></clientConfig>`))
	}
	r := run(t, w.discoverer(), "jan@example.test")
	if r.IMAP != nil || r.SMTP != nil || r.Source != "" {
		t.Fatalf("plain servers used: %+v", r)
	}
	if a := source(t, r, SourceISPDB); a.OK || !strings.Contains(a.Detail, "unencrypted") {
		t.Fatalf("ispdb attempt %+v", a)
	}
}

func TestSRVOnly(t *testing.T) {
	w := newWorld(t)
	d := w.discoverer()
	d.ISPDBURL = "off"
	w.dns.srv["imaps"] = []*net.SRV{{Target: ".", Port: 0, Priority: 0}}
	w.dns.srv["imap"] = []*net.SRV{
		{Target: "backup.example.test.", Port: 143, Priority: 20, Weight: 10},
		{Target: "imap-low.example.test.", Port: 1143, Priority: 10, Weight: 1},
		{Target: "imap-heavy.example.test.", Port: 143, Priority: 10, Weight: 50},
	}
	w.dns.srv["submissions"] = []*net.SRV{{Target: "smtp.example.test.", Port: 465, Priority: 0, Weight: 0}}
	w.dns.srv["submission"] = []*net.SRV{{Target: "smtp.example.test.", Port: 587}}
	r := run(t, d, "jan@example.test")
	wantServer(t, "imap", r.IMAP, Server{"imap-heavy.example.test", 143, TLSStartTLS})
	wantServer(t, "smtp", r.SMTP, Server{"smtp.example.test", 465, TLSImplicit})
	if r.Source != SourceSRV || r.LoginFormat != LoginEmail || r.Username != "jan@example.test" {
		t.Fatalf("result %+v", r)
	}
	if a := source(t, r, SourceISPDB); a.OK || !strings.Contains(a.Detail, "disabled") || w.ispdbHits.Load() != 0 {
		t.Fatalf("ispdb not disabled: %+v, hits %d", a, w.ispdbHits.Load())
	}
}

// fakeSMTP accepts one STARTTLS session per connection with cert.
func fakeSMTP(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				_, _ = c.Write([]byte("220 smtp.example.test ESMTP\r\n"))
				if l, _ := br.ReadString('\n'); !strings.HasPrefix(l, "EHLO") {
					return
				}
				_, _ = c.Write([]byte("250-smtp.example.test\r\n250-PIPELINING\r\n250 STARTTLS\r\n"))
				if l, _ := br.ReadString('\n'); !strings.HasPrefix(l, "STARTTLS") {
					return
				}
				_, _ = c.Write([]byte("220 go ahead\r\n"))
				tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
				_ = tc.Handshake()
			}()
		}
	}()
	return ln.Addr().String()
}

// tlsListener completes TLS handshakes with cert.
func tlsListener(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _ = c.(*tls.Conn).Handshake() }()
		}
	}()
	return ln.Addr().String()
}

func TestGuessOnly(t *testing.T) {
	w := newWorld(t)
	w.dns.hosts["imap.example.test"] = ipIMAP
	w.dns.hosts["mail.example.test"] = ipMail
	w.dns.hosts["smtp.example.test"] = ipSMTP
	// imap.example.test presents a certificate for another name: refused, so
	// mail.example.test:993 wins.
	wrong, _ := newCert(t, "other.test")
	w.net.route(ipIMAP+":993", tlsListener(t, wrong))
	w.net.route(ipMail+":993", tlsListener(t, w.cert))
	w.net.route(ipSMTP+":587", fakeSMTP(t, w.cert))
	r := run(t, w.discoverer(), "jan@example.test")
	wantServer(t, "imap", r.IMAP, Server{"mail.example.test", 993, TLSImplicit})
	wantServer(t, "smtp", r.SMTP, Server{"smtp.example.test", 587, TLSStartTLS})
	a := source(t, r, SourceGuess)
	if r.Source != SourceGuess || !a.OK {
		t.Fatalf("result %+v", r)
	}
}

func TestNothingFound(t *testing.T) {
	w := newWorld(t)
	r := run(t, w.discoverer(), "jan@example.test")
	if r.IMAP != nil || r.SMTP != nil || r.Source != "" || r.Provider != ProviderGeneric || r.Username != "jan@example.test" {
		t.Fatalf("result %+v", r)
	}
	if len(r.Sources) != 4 {
		t.Fatalf("sources %+v", r.Sources)
	}
	for _, s := range r.Sources {
		if s.OK || s.Detail == "" {
			t.Fatalf("source without a reason: %+v", s)
		}
	}
}

func TestRedirects(t *testing.T) {
	for _, tc := range []struct{ name, to, want string }{
		{"to http", "http://autoconfig.example.test/mail/config-v1.1.xml", "not https"},
		{"to another host", "https://example.test/mail/config-v1.1.xml", "another host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.dns.hosts["autoconfig.example.test"] = ipAutoconfig
			w.dns.hosts["example.test"] = ipDomain
			w.mux["autoconfig.example.test"] = func(rw http.ResponseWriter, r *http.Request) {
				http.Redirect(rw, r, tc.to, http.StatusFound)
			}
			w.serveFile("example.test", "/mail/config-v1.1.xml", "autoconfig-localpart.xml")
			r := run(t, w.discoverer(), "jan@example.test")
			if a := source(t, r, SourceAutoconfig); a.OK || !strings.Contains(a.Detail, tc.want) {
				t.Fatalf("autoconfig attempt %+v", a)
			}
			if r.IMAP != nil {
				t.Fatalf("redirect followed: %+v", r)
			}
		})
	}
}

func TestPrivateAddressRefused(t *testing.T) {
	w := newWorld(t)
	w.dns.hosts["autoconfig.example.test"] = ipPrivate
	w.dns.hosts["example.test"] = ipPrivate
	w.dns.hosts["imap.example.test"] = ipPrivate
	w.serveFile("autoconfig.example.test", "/mail/config-v1.1.xml", "autoconfig-localpart.xml")
	r := run(t, w.discoverer(), "jan@example.test")
	if r.IMAP != nil {
		t.Fatalf("internal autoconfig used: %+v", r)
	}
	if a := source(t, r, SourceAutoconfig); !strings.Contains(a.Detail, "host not allowed") {
		t.Fatalf("autoconfig attempt %+v", a)
	}
	if a := source(t, r, SourceGuess); !strings.Contains(a.Detail, "host not allowed") {
		t.Fatalf("guess attempt %+v", a)
	}
	if n := w.net.count(ipPrivate); n != 0 {
		t.Fatalf("%d connections to the private address", n)
	}
}

func TestResponseLimit(t *testing.T) {
	w := newWorld(t)
	w.mux["ispdb.test"] = func(rw http.ResponseWriter, _ *http.Request) {
		_, _ = rw.Write([]byte("<clientConfig>" + strings.Repeat(" ", MaxResponseBytes) + "</clientConfig>"))
	}
	r := run(t, w.discoverer(), "jan@example.test")
	if a := source(t, r, SourceISPDB); a.OK || !strings.Contains(a.Detail, "larger than") {
		t.Fatalf("ispdb attempt %+v", a)
	}
}

func TestTimeout(t *testing.T) {
	w := newWorld(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	w.mux["ispdb.test"] = func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
	w.dns.hang = true
	d := w.discoverer()
	d.StepTimeout, d.Timeout = 300*time.Millisecond, 500*time.Millisecond
	start := time.Now()
	r := run(t, d, "jan@example.test")
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("discovery took %v", el)
	}
	for _, name := range []string{SourceISPDB, SourceSRV} {
		if a := source(t, r, name); a.OK || !strings.Contains(a.Detail, "timed out") {
			t.Fatalf("%s attempt %+v", name, a)
		}
	}
	if (&Discoverer{Timeout: time.Minute}).timeout() != MaxTimeout || (&Discoverer{}).timeout() != MaxTimeout {
		t.Fatal("timeout not capped at MaxTimeout")
	}
}

func TestParseEmail(t *testing.T) {
	for _, s := range []string{"", "jan", "@example.test", "jan@", "jan@localhost", "jan@[10.0.0.1]", "jan@10.0.0.1",
		"jan@exa_mple.test", "a b@example.test", "jan@-x.test", "jan@zażółć.pl", strings.Repeat("a", 65) + "@example.test"} {
		if _, _, err := ParseEmail(s); err == nil {
			t.Errorf("ParseEmail(%q) accepted", s)
		}
	}
	l, d, err := ParseEmail(" Jan+tag@Mail.Example.TEST. ")
	if err != nil || l != "Jan+tag" || d != "mail.example.test" {
		t.Fatalf("got %q %q %v", l, d, err)
	}
	if _, err := (&Discoverer{}).Discover(context.Background(), "nope"); !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("Discover: %v", err)
	}
}

func TestProviderAndDefault(t *testing.T) {
	for host, want := range map[string]string{
		"imap.gmail.com": ProviderGmail, "imap.google.com": ProviderGmail, "outlook.office365.com": ProviderMicrosoft,
		"imap-mail.outlook.com": ProviderMicrosoft, "imap.example.test": ProviderGeneric, "gmail.com.evil.test": ProviderGeneric,
	} {
		if got := ProviderOf(host); got != want {
			t.Errorf("ProviderOf(%s) = %s, want %s", host, got, want)
		}
	}
	r, err := Default("jan@silo.test", Server{"mail.silo.test", 993, TLSImplicit}, Server{"mail.silo.test", 587, TLSStartTLS}, LoginLocalpart)
	if err != nil || r.Source != SourceDefault || r.Username != "jan" || r.LoginFormat != LoginLocalpart || r.IMAP == nil || r.SMTP.Port != 587 {
		t.Fatalf("Default = %+v, %v", r, err)
	}
}

func TestBetterSourceCancelsSlowerOnes(t *testing.T) {
	w := newWorld(t)
	w.serveFile("ispdb.test", "/v1.1/example.test", "ispdb-example.test.xml")
	w.dns.hosts["autoconfig.example.test"] = ipAutoconfig
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	w.mux["autoconfig.example.test"] = func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
	d := w.discoverer()
	d.StepTimeout = 2 * time.Second
	start := time.Now()
	r := run(t, d, "jan@example.test")
	if el := time.Since(start); el > time.Second || r.Source != SourceISPDB {
		t.Fatalf("took %v, result %+v", el, r)
	}
	if a := source(t, r, SourceAutoconfig); a.OK || !strings.Contains(a.Detail, "skipped") {
		t.Fatalf("autoconfig attempt %+v", a)
	}
}
