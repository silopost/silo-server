// Package totp implements time-based one-time passwords (RFC 6238 over the
// HOTP algorithm of RFC 4226) as used by authenticator apps: HMAC-SHA1, 30 s
// steps, 6 digits. It also makes the single-use backup codes of the second
// factor. It stores nothing; callers keep the secret encrypted and remember
// the last accepted step (replay protection).
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // HMAC-SHA1 is what RFC 6238 and authenticator apps use
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Parameters fixed by Silo (and advertised in the otpauth URL).
const (
	SecretBytes = 20               // 160-bit secret, as recommended by RFC 4226
	Period      = 30 * time.Second // time step
	Digits      = 6
	// Skew is how many steps before and after the current one are accepted.
	Skew = 1

	// BackupCodeCount and BackupCodeLen shape a set of backup codes: 10 codes
	// of 10 base32 characters (50 bits each).
	BackupCodeCount = 10
	BackupCodeLen   = 10
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret returns a random secret.
func NewSecret() ([]byte, error) {
	b := make([]byte, SecretBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("totp: random secret: %w", err)
	}
	return b, nil
}

// EncodeSecret is the base32 form (no padding) users type into their app.
func EncodeSecret(secret []byte) string { return b32.EncodeToString(secret) }

// URL is the otpauth:// URI encoded in the setup QR code:
// otpauth://totp/<issuer>:<account>?secret=...&issuer=...&algorithm=SHA1&digits=6&period=30
func URL(issuer, account string, secret []byte) string {
	return "otpauth://totp/" + url.PathEscape(issuer) + ":" + url.PathEscape(account) +
		"?secret=" + EncodeSecret(secret) + "&issuer=" + url.QueryEscape(issuer) +
		"&algorithm=SHA1&digits=" + strconv.Itoa(Digits) + "&period=" + strconv.Itoa(int(Period/time.Second))
}

// Step is the time step containing t.
func Step(t time.Time) int64 { return t.Unix() / int64(Period/time.Second) }

// hotp is RFC 4226 section 5.3 with the given number of digits.
func hotp(secret []byte, counter uint64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for range digits {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, bin%mod)
}

// Code is the code of step.
func Code(secret []byte, step int64) string { return hotp(secret, uint64(step), Digits) }

// Verify checks code against the steps around now (±Skew) that are later than
// lastStep, and returns the matching step, which the caller must store as the
// new lastStep. Codes of lastStep or earlier are refused (replay).
func Verify(secret []byte, code string, now time.Time, lastStep int64) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != Digits {
		return 0, false
	}
	cur := Step(now)
	var match int64
	found := false
	for s := cur - Skew; s <= cur+Skew; s++ {
		// Compare every candidate, so the time taken does not depend on which matches.
		eq := subtle.ConstantTimeCompare([]byte(Code(secret, s)), []byte(code)) == 1
		if eq && s > lastStep && !found {
			match, found = s, true
		}
	}
	return match, found
}

// NewBackupCodes returns a fresh set of backup codes (shown to the user once)
// and their hashes (stored).
func NewBackupCodes() (codes, hashes []string, err error) {
	const chunk = 7 // 56 random bits, at least 10 base32 characters
	raw := make([]byte, BackupCodeCount*chunk)
	if _, err := rand.Read(raw); err != nil {
		return nil, nil, fmt.Errorf("totp: random backup codes: %w", err)
	}
	for i := range BackupCodeCount {
		c := b32.EncodeToString(raw[i*chunk : (i+1)*chunk])[:BackupCodeLen]
		codes = append(codes, c)
		hashes = append(hashes, HashBackupCode(c))
	}
	return codes, hashes, nil
}

// NormalizeBackupCode makes typing forgiving: case, spaces and dashes do not matter.
func NormalizeBackupCode(code string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-', '\t':
			return -1
		}
		if r >= 'a' && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}, code)
}

// HashBackupCode is the stored form of a backup code (sha256 hex of the
// normalized code). Backup codes carry 50 random bits, so a plain hash is enough.
func HashBackupCode(code string) string {
	sum := sha256.Sum256([]byte(NormalizeBackupCode(code)))
	return hex.EncodeToString(sum[:])
}

// IsBackupCodeShape reports whether s looks like a backup code rather than a
// TOTP code.
func IsBackupCodeShape(s string) bool {
	n := NormalizeBackupCode(s)
	if len(n) != BackupCodeLen {
		return false
	}
	for _, r := range n {
		if (r < 'A' || r > 'Z') && (r < '2' || r > '7') {
			return false
		}
	}
	return true
}
