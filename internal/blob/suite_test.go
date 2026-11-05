package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

// The cases below are the BlobStore contract, shared by every backend:
// count returns how many blobs the backend holds (files, objects).

func suiteRoundTrip(t *testing.T, s BlobStore) (hash string, data []byte) {
	t.Helper()
	ctx := context.Background()
	data = bytes.Repeat([]byte("From: a@example.com\r\n"), 1000)
	hash, size, err := s.Put(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if hash != hex.EncodeToString(sum[:]) || size != int64(len(data)) {
		t.Fatalf("hash/size = %s/%d", hash, size)
	}
	f, err := s.Open(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Seek(21, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	if !bytes.Equal(got, data[21:]) {
		t.Fatal("content mismatch after seek")
	}
	return hash, data
}

// suiteHash pins the hash of a known input (echo -n abc | sha256sum).
func suiteHash(t *testing.T, s BlobStore) {
	t.Helper()
	h, size, err := s.Put(context.Background(), strings.NewReader("abc"))
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if err != nil || h != want || size != 3 {
		t.Fatalf("got %s %d %v", h, size, err)
	}
}

func suiteDedup(t *testing.T, s BlobStore, count func(t *testing.T) int) string {
	t.Helper()
	ctx := context.Background()
	h1, _, err := s.Put(ctx, strings.NewReader("same"))
	if err != nil {
		t.Fatal(err)
	}
	h2, size, err := s.Put(ctx, strings.NewReader("same"))
	if err != nil || h2 != h1 || size != 4 {
		t.Fatalf("second put: %s %d %v", h2, size, err)
	}
	if n := count(t); n != 1 {
		t.Errorf("%d blobs, want 1", n)
	}
	return h1
}

func suiteNotFoundAndInvalid(t *testing.T, s BlobStore) {
	t.Helper()
	ctx := context.Background()
	_, err := s.Open(ctx, strings.Repeat("a", 64))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	for _, bad := range []string{"", "../../etc/passwd", strings.Repeat("A", 64)} {
		if _, err := s.Open(ctx, bad); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Open(%q) = %v, want invalid-hash error", bad, err)
		}
	}
}

func suiteDelete(t *testing.T, s BlobStore) {
	t.Helper()
	ctx := context.Background()
	h, _, _ := s.Put(ctx, strings.NewReader("x"))
	if err := s.Delete(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx, h); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := s.Delete(ctx, h); err != nil {
		t.Fatalf("deleting a missing blob: %v", err)
	}
}

type failingReader struct{ n int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n == 0 {
		return 0, errors.New("boom")
	}
	f.n--
	return copy(p, "chunk"), nil
}

func suitePutErrorLeavesNothing(t *testing.T, s BlobStore, count func(t *testing.T) int) {
	t.Helper()
	if _, _, err := s.Put(context.Background(), &failingReader{n: 3}); err == nil {
		t.Fatal("expected error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.Put(ctx, strings.NewReader("data")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled put: %v", err)
	}
	if n := count(t); n != 0 {
		t.Errorf("%d blobs left behind", n)
	}
}
