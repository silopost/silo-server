// Package smtptest provides a minimal in-process SMTP server for tests of the
// connection check: it greets, answers EHLO and AUTH PLAIN against one
// fixed credential pair and quits. It accepts no mail and offers no STARTTLS.
package smtptest

import (
	"bufio"
	"encoding/base64"
	"net"
	"strings"
	"testing"
)

// Start listens on 127.0.0.1 and returns the host and port. The server stops
// when the test ends.
func Start(t *testing.T, user, pass string) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(c, user, pass)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

func serve(c net.Conn, user, pass string) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
	read := func() (string, bool) {
		l, err := r.ReadString('\n')
		return strings.TrimRight(l, "\r\n"), err == nil
	}
	say("220 smtptest ESMTP")
	for {
		line, ok := read()
		if !ok {
			return
		}
		cmd := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250-smtptest")
			say("250 AUTH PLAIN")
		case strings.HasPrefix(cmd, "AUTH PLAIN"):
			arg := strings.TrimSpace(line[len("AUTH PLAIN"):])
			if arg == "" {
				say("334 ")
				if arg, ok = read(); !ok {
					return
				}
			}
			raw, _ := base64.StdEncoding.DecodeString(arg)
			parts := strings.Split(string(raw), "\x00")
			if len(parts) == 3 && parts[1] == user && parts[2] == pass {
				say("235 2.7.0 Authentication successful")
			} else {
				say("535 5.7.8 Authentication failed")
			}
		case strings.HasPrefix(cmd, "QUIT"):
			say("221 bye")
			return
		default:
			say("502 5.5.2 command not implemented")
		}
	}
}
