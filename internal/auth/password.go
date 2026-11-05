package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (64 MiB, one pass, four lanes: within the OWASP
// recommendations for argon2id).
const (
	argonMemoryKiB = 64 * 1024
	argonTime      = 1
	argonThreads   = 4
	argonKeyLen    = 32
	argonSaltLen   = 16
)

// maxConcurrentHashes bounds the memory argon2 can take at once.
const maxConcurrentHashes = 8

// hasher computes and verifies argon2id hashes in PHC string format.
type hasher struct {
	sem chan struct{}
}

func newHasher() *hasher { return &hasher{sem: make(chan struct{}, maxConcurrentHashes)} }

func (h *hasher) hash(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: salt: %w", err)
	}
	h.sem <- struct{}{}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	<-h.sem
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// verify reports whether password matches the PHC-encoded hash. A malformed
// hash never matches.
func (h *hasher) verify(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var mem, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &p); err != nil {
		return false
	}
	// Refuse parameters far above ours: a hash in the database is trusted, but
	// this keeps a corrupted row from allocating gigabytes.
	if mem == 0 || mem > 4*argonMemoryKiB || t == 0 || t > 10 || p == 0 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := enc.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	h.sem <- struct{}{}
	got := argon2.IDKey([]byte(password), salt, t, mem, p, uint32(len(want)))
	<-h.sem
	return subtle.ConstantTimeCompare(got, want) == 1
}

// randomToken returns n random bytes as unpadded base64url.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("auth: random token")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
