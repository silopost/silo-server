package discover

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// guessIMAP and guessSMTP are the host names tried last, in order of
// preference (prefixed to the domain).
var (
	guessIMAP = []Server{{"imap.", 993, TLSImplicit}, {"mail.", 993, TLSImplicit}, {"mail.", 143, TLSStartTLS}}
	guessSMTP = []Server{{"smtp.", 587, TLSStartTLS}, {"mail.", 587, TLSStartTLS}, {"smtp.", 465, TLSImplicit}}
)

// maxGreetingLines bounds the lines read from a server before STARTTLS.
const maxGreetingLines = 50

// guess tries the usual host names of the domain. A candidate counts only
// when a TLS handshake with a certificate valid for its name succeeds (no
// login is attempted). Candidates are tried in parallel; the first working one
// in order of preference wins.
func (d *Discoverer) guess(ctx context.Context, domain string, wantIMAP, wantSMTP bool) found {
	type cand struct {
		srv  Server
		imap bool
		err  error
	}
	var cands []*cand
	add := func(list []Server, imap bool) {
		for _, s := range list {
			s.Host += domain
			cands = append(cands, &cand{srv: s, imap: imap})
		}
	}
	if wantIMAP {
		add(guessIMAP, true)
	}
	if wantSMTP {
		add(guessSMTP, false)
	}
	done := make(chan struct{}, len(cands))
	for _, c := range cands {
		go func() {
			c.err = d.handshake(ctx, c.srv, c.imap)
			done <- struct{}{}
		}()
	}
	for range cands {
		<-done
	}
	var f found
	var fails []string
	for _, c := range cands {
		switch {
		case c.err != nil:
			fails = append(fails, fmt.Sprintf("%s:%d %s", c.srv.Host, c.srv.Port, errText(c.err)))
		case c.imap && f.imap == nil:
			s := c.srv
			f.imap = &s
		case !c.imap && f.smtp == nil:
			s := c.srv
			f.smtp = &s
		}
	}
	f.loginFormat = LoginEmail
	var notes []string
	if f.imap != nil {
		notes = append(notes, "IMAP "+f.imap.String())
	}
	if f.smtp != nil {
		notes = append(notes, "SMTP "+f.smtp.String())
	}
	if !f.ok() {
		return found{err: errors.New(strings.Join(fails, "; "))}
	}
	f.detail = strings.Join(notes, "; ")
	return f
}

// handshake connects to s through the anti-SSRF dialer and completes a TLS
// handshake, after STARTTLS for TLSStartTLS.
func (d *Discoverer) handshake(ctx context.Context, s Server, imap bool) error {
	conn, err := d.dialer.DialContext(ctx, "tcp", net.JoinHostPort(s.Host, strconv.Itoa(s.Port)))
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	if s.TLS == TLSStartTLS {
		br := bufio.NewReaderSize(conn, 4096)
		if imap {
			err = imapStartTLS(conn, br)
		} else {
			err = smtpStartTLS(conn, br)
		}
		if err != nil {
			return err
		}
		if br.Buffered() > 0 {
			return errors.New("data after STARTTLS response")
		}
	}
	tc := tls.Client(conn, &tls.Config{ServerName: s.Host, RootCAs: d.RootCAs, MinVersion: tls.VersionTLS12})
	if err := tc.HandshakeContext(ctx); err != nil {
		var ce *tls.CertificateVerificationError
		if errors.As(err, &ce) {
			return fmt.Errorf("certificate not valid for %s", s.Host)
		}
		return fmt.Errorf("TLS handshake: %w", err)
	}
	return nil
}

func readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return "", errors.New("line too long")
	}
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

func imapStartTLS(conn net.Conn, br *bufio.Reader) error {
	greet, err := readLine(br)
	if err != nil {
		return fmt.Errorf("IMAP greeting: %w", err)
	}
	if !strings.HasPrefix(strings.ToUpper(greet), "* OK") {
		return errors.New("no IMAP greeting")
	}
	if _, err := conn.Write([]byte("s1 STARTTLS\r\n")); err != nil {
		return err
	}
	for range maxGreetingLines {
		line, err := readLine(br)
		if err != nil {
			return fmt.Errorf("IMAP STARTTLS: %w", err)
		}
		if rest, ok := strings.CutPrefix(line, "s1 "); ok {
			if !strings.HasPrefix(strings.ToUpper(rest), "OK") {
				return errors.New("IMAP STARTTLS refused")
			}
			return nil
		}
	}
	return errors.New("IMAP STARTTLS: no response")
}

// smtpReply reads a (multi-line) SMTP reply and returns its code.
func smtpReply(br *bufio.Reader) (int, error) {
	for range maxGreetingLines {
		line, err := readLine(br)
		if err != nil {
			return 0, err
		}
		if len(line) < 3 {
			return 0, errors.New("malformed SMTP reply")
		}
		code, err := strconv.Atoi(line[:3])
		if err != nil {
			return 0, errors.New("malformed SMTP reply")
		}
		if len(line) == 3 || line[3] == ' ' {
			return code, nil
		}
	}
	return 0, errors.New("SMTP reply too long")
}

func smtpStartTLS(conn net.Conn, br *bufio.Reader) error {
	if code, err := smtpReply(br); err != nil || code != 220 {
		return fmt.Errorf("no SMTP greeting (%d, %v)", code, err)
	}
	if _, err := conn.Write([]byte("EHLO silo.invalid\r\n")); err != nil {
		return err
	}
	if code, err := smtpReply(br); err != nil || code != 250 {
		return fmt.Errorf("SMTP EHLO failed (%d, %v)", code, err)
	}
	if _, err := conn.Write([]byte("STARTTLS\r\n")); err != nil {
		return err
	}
	if code, err := smtpReply(br); err != nil || code != 220 {
		return fmt.Errorf("SMTP STARTTLS refused (%d, %v)", code, err)
	}
	return nil
}
