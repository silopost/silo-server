// Package secrets encrypts small secrets (IMAP/SMTP passwords, OAuth refresh
// tokens) with AES-256-GCM before they are stored in the database.
//
// A sealed value is the ASCII string
//
//	v1:<key_id>:<nonce>:<ciphertext>
//
// where key_id is the hex of the first 8 bytes of SHA-256(key), and nonce and
// ciphertext (including the GCM tag) are unpadded standard base64 (RawStdEncoding),
// which never contains ':'. The caller-supplied AAD (for accounts: the account id
// as a decimal string) is authenticated but not stored, so a value copied to
// another row fails to decrypt.
package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

const (
	version = "v1"
	keyLen  = 32
)

// ErrUnknownKey means the value was sealed with a key the keyring does not hold.
var ErrUnknownKey = errors.New("secrets: unknown key id")

// ErrMalformed means the value is not in the v1 format.
var ErrMalformed = errors.New("secrets: malformed value")

type key struct {
	id   string
	aead cipher.AEAD
}

// Keyring encrypts with the active key and decrypts with the active or the
// previous key.
type Keyring struct {
	active key
	byID   map[string]key
}

// NewKeyring builds a keyring from the active key and an optional previous key
// (nil or empty: none). Keys must be 32 bytes.
func NewKeyring(active, previous []byte) (*Keyring, error) {
	a, err := newKey(active)
	if err != nil {
		return nil, fmt.Errorf("active key: %w", err)
	}
	k := &Keyring{active: a, byID: map[string]key{a.id: a}}
	if len(previous) > 0 {
		p, err := newKey(previous)
		if err != nil {
			return nil, fmt.Errorf("previous key: %w", err)
		}
		k.byID[p.id] = p
	}
	return k, nil
}

func newKey(raw []byte) (key, error) {
	if len(raw) != keyLen {
		return key{}, fmt.Errorf("must be %d bytes, got %d", keyLen, len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return key{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return key{}, err
	}
	sum := sha256.Sum256(raw)
	return key{id: hex.EncodeToString(sum[:8]), aead: aead}, nil
}

// ActiveKeyID returns the id of the key Encrypt uses.
func (k *Keyring) ActiveKeyID() string { return k.active.id }

// Encrypt seals plaintext with the active key, binding it to aad.
func (k *Keyring) Encrypt(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, k.active.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: nonce: %w", err)
	}
	ct := k.active.aead.Seal(nil, nonce, plaintext, aad)
	enc := base64.RawStdEncoding
	return []byte(version + ":" + k.active.id + ":" + enc.EncodeToString(nonce) + ":" + enc.EncodeToString(ct)), nil
}

// Decrypt opens a value produced by Encrypt. The key is chosen by key_id; an
// unknown id yields ErrUnknownKey.
func (k *Keyring) Decrypt(blob, aad []byte) ([]byte, error) {
	keyID, nonce, ct, err := parse(blob)
	if err != nil {
		return nil, err
	}
	key, ok := k.byID[keyID]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownKey, keyID)
	}
	if len(nonce) != key.aead.NonceSize() {
		return nil, ErrMalformed
	}
	pt, err := key.aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, errors.New("secrets: decryption failed")
	}
	return pt, nil
}

// NeedsRotation reports whether blob was not sealed with the active key.
// Malformed values report false: rotation cannot fix them.
func (k *Keyring) NeedsRotation(blob []byte) bool {
	keyID, _, _, err := parse(blob)
	return err == nil && keyID != k.active.id
}

func parse(blob []byte) (keyID string, nonce, ct []byte, err error) {
	parts := bytes.Split(blob, []byte(":"))
	if len(parts) != 4 || string(parts[0]) != version {
		return "", nil, nil, ErrMalformed
	}
	enc := base64.RawStdEncoding
	if nonce, err = enc.DecodeString(string(parts[2])); err != nil {
		return "", nil, nil, ErrMalformed
	}
	if ct, err = enc.DecodeString(string(parts[3])); err != nil {
		return "", nil, nil, ErrMalformed
	}
	return string(parts[1]), nonce, ct, nil
}
