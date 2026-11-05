package imaptest

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

// capabilityOf logs in over raw IMAP and returns the post-login CAPABILITY line.
func capabilityOf(t *testing.T, addr, user, pass string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(c)
	if _, err := r.ReadString('\n'); err != nil { // greeting
		t.Fatal(err)
	}
	send := func(tag, cmd string) (untagged []string) {
		if _, err := c.Write([]byte(tag + " " + cmd + "\r\n")); err != nil {
			t.Fatal(err)
		}
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(l, tag+" ") {
				if !strings.HasPrefix(l, tag+" OK") {
					t.Fatalf("%s: %s", cmd, l)
				}
				return untagged
			}
			untagged = append(untagged, l)
		}
	}
	send("a1", "LOGIN "+user+" "+pass)
	for _, l := range send("a2", "CAPABILITY") {
		if strings.HasPrefix(l, "* CAPABILITY") {
			return l
		}
	}
	t.Fatal("no CAPABILITY response")
	return ""
}

func TestProfiles(t *testing.T) {
	tests := []struct {
		profile Profile
		want    []string
		absent  []string
	}{
		{Full, []string{"CONDSTORE", "QRESYNC", "MOVE", "UIDPLUS", "IDLE", "SPECIAL-USE"}, nil},
		{NoCondstore, []string{"MOVE", "UIDPLUS", "IDLE"}, []string{"CONDSTORE", "QRESYNC"}},
		{NoMove, []string{"CONDSTORE", "QRESYNC", "IDLE"}, []string{"MOVE", "UIDPLUS"}},
	}
	for _, tc := range tests {
		t.Run(string(tc.profile), func(t *testing.T) {
			addr, user, pass := Start(t, tc.profile)
			caps := strings.Fields(capabilityOf(t, addr, user, pass))
			has := func(c string) bool {
				for _, f := range caps {
					if f == c {
						return true
					}
				}
				return false
			}
			for _, c := range tc.want {
				if !has(c) {
					t.Errorf("missing %s in %v", c, caps)
				}
			}
			for _, c := range tc.absent {
				if has(c) {
					t.Errorf("unexpected %s in %v", c, caps)
				}
			}
		})
	}
}
