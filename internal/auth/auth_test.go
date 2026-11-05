package auth

import (
	"strings"
	"testing"
	"time"

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

func TestLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLimiter(Limit{Burst: 2, Every: 10 * time.Second}, func() time.Time { return now })

	for i := 0; i < 2; i++ {
		if ok, _ := l.allow("k"); !ok {
			t.Fatalf("request %d denied within burst", i+1)
		}
	}
	ok, wait := l.allow("k")
	if ok || wait <= 0 || wait > 10*time.Second {
		t.Fatalf("third request: ok=%v wait=%v, want denied with wait in (0,10s]", ok, wait)
	}
	if ok, _ := l.allow("other"); !ok {
		t.Error("keys must not share a bucket")
	}
	now = now.Add(10 * time.Second)
	if ok, _ := l.allow("k"); !ok {
		t.Error("token not refilled after Every")
	}
	if ok, _ := l.allow("k"); ok {
		t.Error("only one token should have been refilled")
	}
	// Idle, fully refilled buckets are swept.
	now = now.Add(time.Hour)
	l.allow("trigger")
	if len(l.buckets) != 1 {
		t.Errorf("buckets after sweep = %d, want 1", len(l.buckets))
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
