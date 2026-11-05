package totp

import (
	"encoding/base32"
	"net/url"
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA1 rows: secret "12345678901234567890", 8 digits.
func TestRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for _, tt := range []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	} {
		step := Step(time.Unix(tt.unix, 0))
		if got := hotp(secret, uint64(step), 8); got != tt.want {
			t.Errorf("T=%d: hotp8 = %s, want %s", tt.unix, got, tt.want)
		}
		// Six digits are the low six of the same truncated value.
		if got := Code(secret, step); got != tt.want[2:] {
			t.Errorf("T=%d: Code = %s, want %s", tt.unix, got, tt.want[2:])
		}
	}
}

func TestVerifyWindowAndReplay(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1111111111, 0)
	cur := Step(now)
	for _, d := range []int64{-1, 0, 1} {
		if s, ok := Verify(secret, Code(secret, cur+d), now, 0); !ok || s != cur+d {
			t.Errorf("offset %d: Verify = %d, %v", d, s, ok)
		}
	}
	for _, d := range []int64{-2, 2} {
		if _, ok := Verify(secret, Code(secret, cur+d), now, 0); ok {
			t.Errorf("offset %d accepted", d)
		}
	}
	// Replay: the step already used (or an earlier one) is refused.
	if _, ok := Verify(secret, Code(secret, cur), now, cur); ok {
		t.Error("replayed code accepted")
	}
	if _, ok := Verify(secret, Code(secret, cur-1), now, cur); ok {
		t.Error("code older than the last step accepted")
	}
	if s, ok := Verify(secret, Code(secret, cur+1), now, cur); !ok || s != cur+1 {
		t.Errorf("next step refused: %d %v", s, ok)
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := Verify(secret, bad, now, 0); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSecretAndURL(t *testing.T) {
	s, err := NewSecret()
	if err != nil || len(s) != SecretBytes {
		t.Fatalf("NewSecret = %d bytes, %v", len(s), err)
	}
	enc := EncodeSecret(s)
	if len(enc) != 32 || strings.Contains(enc, "=") {
		t.Fatalf("encoded secret %q", enc)
	}
	back, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enc)
	if err != nil || string(back) != string(s) {
		t.Fatal("round trip failed")
	}
	u, err := url.Parse(URL("Silo", "ala@example.com", s))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "otpauth" || u.Host != "totp" || u.Path != "/Silo:ala@example.com" {
		t.Errorf("url %s", u)
	}
	q := u.Query()
	if q.Get("secret") != enc || q.Get("issuer") != "Silo" || q.Get("algorithm") != "SHA1" ||
		q.Get("digits") != "6" || q.Get("period") != "30" {
		t.Errorf("query %v", q)
	}
}

func TestBackupCodes(t *testing.T) {
	codes, hashes, err := NewBackupCodes()
	if err != nil || len(codes) != BackupCodeCount || len(hashes) != BackupCodeCount {
		t.Fatalf("NewBackupCodes: %d %d %v", len(codes), len(hashes), err)
	}
	seen := map[string]bool{}
	for i, c := range codes {
		if len(c) != BackupCodeLen || !IsBackupCodeShape(c) || seen[c] {
			t.Errorf("code %q", c)
		}
		seen[c] = true
		if HashBackupCode(c) != hashes[i] || len(hashes[i]) != 64 {
			t.Errorf("hash of %q", c)
		}
	}
	c := codes[0]
	if HashBackupCode(" "+c[:5]+"-"+strings.ToLower(c[5:])+" ") != hashes[0] {
		t.Error("normalization")
	}
	if IsBackupCodeShape("123456") {
		t.Error("TOTP code looks like a backup code")
	}
}
