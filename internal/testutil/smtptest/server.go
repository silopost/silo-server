package smtptest

import (
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// Message is a message accepted by a Server: the envelope and the DATA bytes.
type Message struct {
	From string
	To   []string
	Data []byte
}

// Server is an in-process submission server (go-smtp) for tests of sending:
// AUTH PLAIN against one credential pair, then MAIL / RCPT / DATA. Replies
// can be made to fail per recipient or for DATA. It offers no STARTTLS.
type Server struct {
	Host string
	Port int

	user, pass string
	mu         sync.Mutex
	msgs       []Message
	rcptCode   map[string]int
	dataCode   int
	// xoauth2 accepts AUTH XOAUTH2 of xoUser when it returns true for the
	// bearer token (nil: XOAUTH2 not offered).
	xoauth2 func(token string) bool
	xoUser  string
}

// StartServer listens on 127.0.0.1 and accepts mail until the test ends.
// maxSize > 0 is announced as SIZE.
func StartServer(t *testing.T, user, pass string, maxSize int64) *Server {
	t.Helper()
	s := &Server{user: user, pass: pass, rcptCode: map[string]int{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := smtp.NewServer(smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) { return &session{s: s}, nil }))
	srv.Domain = "smtptest"
	srv.AllowInsecureAuth = true
	srv.MaxMessageBytes = maxSize
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	s.Host, s.Port = "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
	return s
}

// RejectRcpt makes RCPT TO addr fail with code (4xx or 5xx); 0 accepts it.
func (s *Server) RejectRcpt(addr string, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rcptCode[strings.ToLower(addr)] = code
}

// RejectData makes the reply after DATA fail with code; 0 accepts messages.
func (s *Server) RejectData(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dataCode = code
}

// Messages returns the accepted messages.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.msgs...)
}

func reply(code int, msg string) error {
	return &smtp.SMTPError{Code: code, EnhancedCode: smtp.EnhancedCode{code / 100, 0, 0}, Message: msg}
}

type session struct {
	s    *Server
	auth bool
	from string
	to   []string
}

func (x *session) AuthMechanisms() []string {
	x.s.mu.Lock()
	defer x.s.mu.Unlock()
	if x.s.xoauth2 != nil {
		return []string{sasl.Plain, xoauth2Mech}
	}
	return []string{sasl.Plain}
}

func (x *session) Auth(mech string) (sasl.Server, error) {
	if mech == xoauth2Mech {
		return &xoauth2Server{x: x}, nil
	}
	return sasl.NewPlainServer(func(_, user, pass string) error {
		if user != x.s.user || pass != x.s.pass {
			return smtp.ErrAuthFailed
		}
		x.auth = true
		return nil
	}), nil
}

func (x *session) Mail(from string, _ *smtp.MailOptions) error {
	if !x.auth {
		return smtp.ErrAuthRequired
	}
	x.from = from
	return nil
}

func (x *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	x.s.mu.Lock()
	code := x.s.rcptCode[strings.ToLower(to)]
	x.s.mu.Unlock()
	if code != 0 {
		return reply(code, "recipient refused")
	}
	x.to = append(x.to, to)
	return nil
}

func (x *session) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	x.s.mu.Lock()
	defer x.s.mu.Unlock()
	if x.s.dataCode != 0 {
		return reply(x.s.dataCode, "message refused")
	}
	if len(x.to) == 0 {
		return errors.New("no recipients")
	}
	x.s.msgs = append(x.s.msgs, Message{From: x.from, To: append([]string(nil), x.to...), Data: data})
	return nil
}

func (x *session) Reset() { x.from, x.to = "", nil }

func (x *session) Logout() error { return nil }

const xoauth2Mech = "XOAUTH2"

// AcceptXOAuth2 makes the server offer AUTH XOAUTH2 (besides PLAIN) and
// accept user with any bearer token for which valid returns true.
func (s *Server) AcceptXOAuth2(user string, valid func(token string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.xoUser, s.xoauth2 = user, valid
}

// xoauth2Server is the server side of XOAUTH2 as Gmail runs it: the initial
// response is "user=<u>^Aauth=Bearer <token>^A^A"; a refused one gets a
// 334 challenge with a base64 JSON error, then 535 after the client's
// (empty) answer.
type xoauth2Server struct {
	x      *session
	failed bool
}

func (a *xoauth2Server) Next(resp []byte) ([]byte, bool, error) {
	if a.failed {
		return nil, false, reply(535, "5.7.8 Username and Password not accepted")
	}
	if resp == nil {
		return []byte{}, false, nil // ask for the initial response
	}
	a.x.s.mu.Lock()
	user, valid := a.x.s.xoUser, a.x.s.xoauth2
	a.x.s.mu.Unlock()
	fields := strings.Split(string(resp), "\x01")
	var u, tok string
	for _, f := range fields {
		if v, ok := strings.CutPrefix(f, "user="); ok {
			u = v
		}
		if v, ok := strings.CutPrefix(f, "auth=Bearer "); ok {
			tok = v
		}
	}
	ok := len(fields) == 4 && fields[2] == "" && fields[3] == "" && u == user && valid(tok)
	if ok {
		a.x.auth = true
		return nil, true, nil
	}
	a.failed = true
	return []byte(`{"status":"400","schemes":"Bearer","scope":"https://mail.google.com/"}`), false, nil
}
