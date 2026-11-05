// Package imapx is a thin layer over github.com/emersion/go-imap/v2: dialing
// with timeouts and TLS modes, login with error classification, and parsed
// server capabilities. Sync logic lives elsewhere.
package imapx

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
)

// TLSMode selects how the connection is secured.
type TLSMode string

const (
	TLSImplicit TLSMode = "implicit" // TLS from the first byte (port 993)
	TLSStartTLS TLSMode = "starttls" // plaintext greeting, then STARTTLS (port 143)
	TLSNone     TLSMode = "none"     // no encryption; only for tests and trusted networks
)

// DefaultDialTimeout bounds TCP connect plus TLS handshake plus greeting.
const DefaultDialTimeout = 15 * time.Second

// DefaultCommandTimeout bounds commands issued through the wait helpers here.
const DefaultCommandTimeout = 60 * time.Second

// Option customizes Dial.
type Option func(*dialOptions)

type dialOptions struct {
	imap        imapclient.Options
	dialTimeout time.Duration
	dial        func(ctx context.Context, network, addr string) (net.Conn, error)
}

// WithUnilateralDataHandler installs a handler for EXISTS, EXPUNGE, FETCH and
// mailbox updates sent outside a command (notably during IDLE).
func WithUnilateralDataHandler(h *imapclient.UnilateralDataHandler) Option {
	return func(o *dialOptions) { o.imap.UnilateralDataHandler = h }
}

// WithDebugWriter tees raw protocol traffic (including credentials!) to w.
func WithDebugWriter(w io.Writer) Option {
	return func(o *dialOptions) { o.imap.DebugWriter = w }
}

// WithDialTimeout overrides DefaultDialTimeout.
func WithDialTimeout(d time.Duration) Option {
	return func(o *dialOptions) { o.dialTimeout = d }
}

// WithDialContext replaces the TCP dial (net.Dialer) with dial, e.g. the
// anti-SSRF dialer of netx. TLS still verifies the host name given to Dial.
func WithDialContext(dial func(ctx context.Context, network, addr string) (net.Conn, error)) Option {
	return func(o *dialOptions) { o.dial = dial }
}

// Client is a connected IMAP client. Use IMAP() for commands go-imap supports.
type Client struct {
	c *imapclient.Client
}

// Dial connects, secures the connection per mode and waits for the greeting.
// Network, TLS and timeout failures wrap ErrUnavailable.
func Dial(ctx context.Context, host string, port int, mode TLSMode, insecureSkipVerify bool, opts ...Option) (*Client, error) {
	o := dialOptions{dialTimeout: DefaultDialTimeout}
	for _, opt := range opts {
		opt(&o)
	}
	ctx, cancel := context.WithTimeout(ctx, o.dialTimeout)
	defer cancel()

	tlsCfg := &tls.Config{ServerName: host, InsecureSkipVerify: insecureSkipVerify} //nolint:gosec // opt-in per account
	o.imap.TLSConfig = tlsCfg
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	dial := o.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, wrapNetError(err)
	}
	// The deadline covers handshake and greeting; it is cleared on success.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	var c *imapclient.Client
	switch mode {
	case TLSImplicit:
		cfg := tlsCfg.Clone()
		cfg.NextProtos = []string{"imap"}
		tc := tls.Client(conn, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, wrapNetError(err)
		}
		c = imapclient.New(tc, &o.imap)
	case TLSStartTLS:
		c, err = imapclient.NewStartTLS(conn, &o.imap)
		if err != nil {
			return nil, wrapNetError(err)
		}
	case TLSNone:
		c = imapclient.New(conn, &o.imap)
	default:
		conn.Close()
		return nil, fmt.Errorf("imapx: unknown TLS mode %q", mode)
	}

	if err := waitGreeting(ctx, c); err != nil {
		c.Close()
		return nil, wrapNetError(err)
	}
	_ = conn.SetDeadline(time.Time{})
	return &Client{c: c}, nil
}

func waitGreeting(ctx context.Context, c *imapclient.Client) error {
	done := make(chan error, 1)
	go func() { done <- c.WaitGreeting() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// IMAP exposes the underlying go-imap client.
func (c *Client) IMAP() *imapclient.Client { return c.c }

// Login authenticates with the LOGIN command. A tagged NO yields
// ErrAuthFailed; any transport failure yields ErrUnavailable.
func (c *Client) Login(ctx context.Context, user, pass string) error {
	ctx, cancel := context.WithTimeout(ctx, DefaultCommandTimeout)
	defer cancel()
	return wrapLoginError(c.await(ctx, c.c.Login(user, pass).Wait))
}

// Capabilities asks the server for its capabilities (they usually grow after
// authentication, so call this after Login).
func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultCommandTimeout)
	defer cancel()
	var caps Capabilities
	err := c.await(ctx, func() error {
		set, err := c.c.Capability().Wait()
		caps = NewCapabilities(set)
		return err
	})
	return caps, wrapNetError(err)
}

// Close drops the connection immediately.
func (c *Client) Close() error { return c.c.Close() }

// Logout sends LOGOUT and closes the connection.
func (c *Client) Logout(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := c.await(ctx, c.c.Logout().Wait)
	_ = c.c.Close()
	return err
}

// Await waits for a command with a deadline from ctx. go-imap commands have no
// timeouts of their own, so on expiry the connection is closed (it is not
// reusable afterwards). Transport errors wrap ErrUnavailable.
//
//	err := cl.Await(ctx, cl.IMAP().Noop().Wait)
func (c *Client) Await(ctx context.Context, wait func() error) error {
	return wrapNetError(c.await(ctx, wait))
}

func (c *Client) await(ctx context.Context, wait func() error) error {
	done := make(chan error, 1)
	go func() { done <- wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = c.c.Close()
		return ctx.Err()
	}
}
