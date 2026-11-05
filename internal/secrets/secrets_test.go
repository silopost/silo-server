package secrets

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, keyLen) }

func mustKeyring(t *testing.T, active, previous []byte) *Keyring {
	t.Helper()
	k, err := NewKeyring(active, previous)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	k := mustKeyring(t, testKey(1), nil)
	blob, err := k.Encrypt([]byte("hunter2"), []byte("42"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(blob), "v1:"+k.ActiveKeyID()+":") {
		t.Fatalf("unexpected format %q", blob)
	}
	got, err := k.Decrypt(blob, []byte("42"))
	if err != nil || string(got) != "hunter2" {
		t.Fatalf("got %q, %v", got, err)
	}
	other, _ := k.Encrypt([]byte("hunter2"), []byte("42"))
	if bytes.Equal(blob, other) {
		t.Fatal("nonce reused")
	}
}

func TestDecryptFailures(t *testing.T) {
	k := mustKeyring(t, testKey(1), nil)
	blob, _ := k.Encrypt([]byte("secret"), []byte("1"))

	if _, err := k.Decrypt(blob, []byte("2")); err == nil {
		t.Error("wrong AAD accepted")
	}

	parts := strings.Split(string(blob), ":")
	ct := []byte(parts[3])
	if ct[0] == 'A' {
		ct[0] = 'B'
	} else {
		ct[0] = 'A'
	}
	parts[3] = string(ct)
	if _, err := k.Decrypt([]byte(strings.Join(parts, ":")), []byte("1")); err == nil {
		t.Error("tampered ciphertext accepted")
	}

	foreign := mustKeyring(t, testKey(9), nil)
	if _, err := foreign.Decrypt(blob, []byte("1")); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("unknown key id: got %v", err)
	}
	if _, err := k.Decrypt([]byte("garbage"), nil); !errors.Is(err, ErrMalformed) {
		t.Errorf("malformed: got %v", err)
	}
}

func TestPreviousKey(t *testing.T) {
	oldRing := mustKeyring(t, testKey(1), nil)
	old, _ := oldRing.Encrypt([]byte("pw"), []byte("7"))

	ring := mustKeyring(t, testKey(2), testKey(1))
	if !ring.NeedsRotation(old) {
		t.Error("old value should need rotation")
	}
	got, err := ring.Decrypt(old, []byte("7"))
	if err != nil || string(got) != "pw" {
		t.Fatalf("decrypt with previous key: %q, %v", got, err)
	}
	fresh, _ := ring.Encrypt([]byte("pw"), []byte("7"))
	if !strings.HasPrefix(string(fresh), "v1:"+ring.ActiveKeyID()+":") || ring.NeedsRotation(fresh) {
		t.Errorf("Encrypt must use the active key: %q", fresh)
	}
	if ring.NeedsRotation([]byte("garbage")) {
		t.Error("malformed value must not report rotation")
	}
}

func TestBadKeyLength(t *testing.T) {
	if _, err := NewKeyring([]byte("short"), nil); err == nil {
		t.Error("short active key accepted")
	}
	if _, err := NewKeyring(testKey(1), []byte("short")); err == nil {
		t.Error("short previous key accepted")
	}
}
