package accounts

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/silopost/silo-server/internal/imapx"
)

// probeTimeout bounds each protocol check as a whole.
const probeTimeout = 30 * time.Second

// Step is the outcome of one stage of a connection test.
type Step struct {
	Step   string `json:"step" doc:"Stage name: connect, tls, starttls, ehlo, login, auth, capability, list."`
	OK     bool   `json:"ok"`
	MS     int64  `json:"ms" doc:"Duration of the stage in milliseconds."`
	Detail string `json:"detail,omitempty" doc:"Server capabilities or the reason the stage failed."`
}

// ProtocolResult is the outcome of testing IMAP or SMTP.
type ProtocolResult struct {
	OK    bool   `json:"ok"`
	Steps []Step `json:"steps"`
}

// TestResult is the outcome of testing both protocols.
type TestResult struct {
	IMAP ProtocolResult `json:"imap"`
	SMTP ProtocolResult `json:"smtp"`
}

// OK reports whether both protocols passed.
func (r TestResult) OK() bool { return r.IMAP.OK && r.SMTP.OK }

// endpoint is a server to test.
type endpoint struct {
	Host     string
	Port     int
	TLS      string
	Username string
	Password string
	Insecure bool // skip certificate verification (SILO_ALLOW_INSECURE_TRANSPORT)
}

// steps records stages in order and stops at the first failure.
type steps struct {
	list   []Step
	failed bool
}

func (s *steps) run(name string, fn func() (string, error)) bool {
	if s.failed {
		return false
	}
	start := time.Now()
	detail, err := fn()
	st := Step{Step: name, OK: err == nil, MS: time.Since(start).Milliseconds(), Detail: detail}
	if err != nil {
		st.Detail = err.Error()
		s.failed = true
	}
	s.list = append(s.list, st)
	return err == nil
}

func (s *steps) result() ProtocolResult {
	return ProtocolResult{OK: !s.failed && len(s.list) > 0, Steps: s.list}
}

// probeIMAP checks connect, TLS, LOGIN, CAPABILITY and LIST.
func probeIMAP(ctx context.Context, e endpoint) ProtocolResult {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var st steps
	var cl *imapx.Client
	defer func() {
		if cl != nil {
			_ = cl.Close()
		}
	}()

	st.run("connect", func() (string, error) { return "", tcpProbe(ctx, e.Host, e.Port) })
	st.run("tls", func() (string, error) {
		var err error
		cl, err = imapx.Dial(ctx, e.Host, e.Port, imapx.TLSMode(e.TLS), e.Insecure)
		if err != nil {
			return "", err
		}
		if e.TLS == "none" {
			return "no encryption", nil
		}
		return e.TLS, nil
	})
	st.run("login", func() (string, error) {
		if err := cl.Login(ctx, e.Username, e.Password); err != nil {
			if errors.Is(err, imapx.ErrAuthFailed) {
				return "", fmt.Errorf("authentication failed: %w", unwrapIMAP(err))
			}
			return "", err
		}
		return "", nil
	})
	st.run("capability", func() (string, error) {
		caps, err := cl.Capabilities(ctx)
		if err != nil {
			return "", err
		}
		return strings.Join(caps.Names(), " "), nil
	})
	st.run("list", func() (string, error) {
		var n int
		err := cl.Await(ctx, func() error {
			boxes, err := cl.IMAP().List("", "*", &imap.ListOptions{}).Collect()
			n = len(boxes)
			return err
		})
		if err != nil {
			return "", err
		}
		return strconv.Itoa(n) + " mailboxes", nil
	})
	return st.result()
}

// unwrapIMAP returns the server's own message for a rejected login.
func unwrapIMAP(err error) error {
	var ie *imap.Error
	if errors.As(err, &ie) {
		return errors.New(strings.TrimSpace(ie.Text))
	}
	return errors.New("rejected by server")
}

// probeSMTP checks connect, EHLO, STARTTLS and AUTH. It never sends a message.
func probeSMTP(ctx context.Context, e endpoint) ProtocolResult {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var st steps
	var conn net.Conn
	var c *smtp.Client
	defer func() {
		if c != nil {
			_ = c.Close()
		} else if conn != nil {
			_ = conn.Close()
		}
	}()
	tlsCfg := &tls.Config{ServerName: e.Host, InsecureSkipVerify: e.Insecure} //nolint:gosec // opt-in via SILO_ALLOW_INSECURE_TRANSPORT
	addr := net.JoinHostPort(e.Host, strconv.Itoa(e.Port))

	st.run("connect", func() (string, error) {
		var d net.Dialer
		var err error
		conn, err = d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return "", err
		}
		if dl, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(dl)
		}
		if e.TLS == "implicit" {
			tc := tls.Client(conn, tlsCfg)
			if err := tc.HandshakeContext(ctx); err != nil {
				return "", err
			}
			conn = tc
		}
		c, err = smtp.NewClient(conn, e.Host)
		if err != nil {
			return "", err
		}
		return "", nil
	})
	st.run("ehlo", func() (string, error) {
		if err := c.Hello("silo.invalid"); err != nil {
			return "", err
		}
		return "", nil
	})
	if e.TLS == "starttls" {
		st.run("starttls", func() (string, error) {
			if ok, _ := c.Extension("STARTTLS"); !ok {
				return "", errors.New("server does not offer STARTTLS")
			}
			return "", c.StartTLS(tlsCfg)
		})
	}
	st.run("auth", func() (string, error) {
		ok, mechs := c.Extension("AUTH")
		if !ok {
			return "", errors.New("server does not offer AUTH")
		}
		var a smtp.Auth
		switch {
		case hasMech(mechs, "PLAIN"):
			a = plainAuth{user: e.Username, pass: e.Password}
		case hasMech(mechs, "LOGIN"):
			a = loginAuth{user: e.Username, pass: e.Password}
		default:
			return "", fmt.Errorf("no supported AUTH mechanism (server offers %s)", mechs)
		}
		return "", c.Auth(a)
	})
	if !st.failed {
		_ = c.Quit()
		c = nil
	}
	return st.result()
}

func hasMech(mechs, want string) bool {
	for _, m := range strings.Fields(mechs) {
		if strings.EqualFold(m, want) {
			return true
		}
	}
	return false
}

func tcpProbe(ctx context.Context, host string, port int) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return conn.Close()
}

// plainAuth is AUTH PLAIN without net/smtp's refusal to run unencrypted: the
// transport policy is decided by the instance configuration.
type plainAuth struct{ user, pass string }

func (a plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}

func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected server challenge")
	}
	return nil, nil
}

// loginAuth is the legacy AUTH LOGIN mechanism.
type loginAuth struct{ user, pass string }

func (a loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }

func (a loginAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(challenge))) {
	case "username:":
		return []byte(a.user), nil
	case "password:":
		return []byte(a.pass), nil
	}
	return nil, errors.New("unexpected server challenge")
}
