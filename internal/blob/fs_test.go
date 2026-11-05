package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) (*FSStore, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "blobs")
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestPutOpenRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, dir := newStore(t)
	data := bytes.Repeat([]byte("From: a@example.com\r\n"), 1000)

	hash, size, err := s.Put(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if hash != hex.EncodeToString(sum[:]) || size != int64(len(data)) {
		t.Fatalf("hash/size = %s/%d", hash, size)
	}
	if _, err := os.Stat(filepath.Join(dir, hash[:2], hash[2:4], hash)); err != nil {
		t.Fatalf("layout: %v", err)
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
}

// TestHashMatchesSha256sum pins the hash of a known input (echo -n abc | sha256sum).
func TestHashMatchesSha256sum(t *testing.T) {
	s, _ := newStore(t)
	h, size, err := s.Put(context.Background(), strings.NewReader("abc"))
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if err != nil || h != want || size != 3 {
		t.Fatalf("got %s %d %v", h, size, err)
	}
}

func TestPutDedup(t *testing.T) {
	ctx := context.Background()
	s, dir := newStore(t)
	h1, _, err := s.Put(ctx, strings.NewReader("same"))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, h1[:2], h1[2:4], h1)
	before, _ := os.Stat(p)
	h2, size, err := s.Put(ctx, strings.NewReader("same"))
	if err != nil || h2 != h1 || size != 4 {
		t.Fatalf("second put: %s %d %v", h2, size, err)
	}
	after, _ := os.Stat(p)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("existing blob was overwritten")
	}
	if n := countFiles(t, dir); n != 1 {
		t.Errorf("%d files, want 1", n)
	}
}

func TestOpenNotFoundAndInvalid(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
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

func TestDelete(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
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

func TestPutErrorLeavesNothing(t *testing.T) {
	s, dir := newStore(t)
	if _, _, err := s.Put(context.Background(), &failingReader{n: 3}); err == nil {
		t.Fatal("expected error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.Put(ctx, strings.NewReader("data")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled put: %v", err)
	}
	if n := countFiles(t, dir); n != 0 {
		t.Errorf("%d files left behind", n)
	}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}
