package auth

import (
	"strings"
	"testing"

	"github.com/silopost/silo-server/internal/config"
)

func TestPasswordHash(t *testing.T) {
	h := newHasher()
	enc, err := h.hash("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=65536,t=1,p=4$") {
		t.Fatalf("unexpected encoding %q", enc)
	}
	if !h.verify("s3cret", enc) {
		t.Error("correct password rejected")
	}
	if h.verify("S3cret", enc) {
		t.Error("wrong password accepted")
	}
	for _, bad := range []string{"", "plain", "$argon2id$v=19$m=65536,t=1,p=4$x", "$bcrypt$v=19$m=1,t=1,p=1$YQ$YQ"} {
		if h.verify("s3cret", bad) {
			t.Errorf("malformed hash %q accepted", bad)
		}
	}
}

func TestLoginIdentity(t *testing.T) {
	email := New(Options{Config: config.Config{DefaultLoginFormat: config.LoginFormatEmail}})
	local := New(Options{Config: config.Config{DefaultLoginFormat: config.LoginFormatLocalpart, DefaultDomain: "example.com"}})

	tests := []struct {
		name, in   string
		svc        *Service
		addr, user string
		ok         bool
	}{
		{"email full", "a@example.com", email, "a@example.com", "a@example.com", true},
		{"email needs domain", "a", email, "", "", false},
		{"email empty domain", "a@", email, "", "", false},
		{"two ats", "a@b@c", email, "", "", false},
		{"space", "a b@c.d", email, "", "", false},
		{"localpart bare", "jan", local, "jan@example.com", "jan", true},
		{"localpart with domain", "jan@Example.com", local, "jan@example.com", "jan", true},
		{"localpart foreign domain", "jan@evil.com", local, "", "", false},
		{"empty", "", email, "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addr, user, ok := tc.svc.loginIdentity(tc.in)
			if ok != tc.ok || addr != tc.addr || user != tc.user {
				t.Fatalf("loginIdentity(%q) = %q, %q, %v; want %q, %q, %v", tc.in, addr, user, ok, tc.addr, tc.user, tc.ok)
			}
		})
	}
}
