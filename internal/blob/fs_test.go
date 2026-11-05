package blob

import (
	"context"
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

func fsCount(dir string) func(t *testing.T) int {
	return func(t *testing.T) int { return countFiles(t, dir) }
}

func TestPutOpenRoundTrip(t *testing.T) {
	s, dir := newStore(t)
	hash, _ := suiteRoundTrip(t, s)
	if _, err := os.Stat(filepath.Join(dir, hash[:2], hash[2:4], hash)); err != nil {
		t.Fatalf("layout: %v", err)
	}
}

// TestHashMatchesSha256sum pins the hash of a known input (echo -n abc | sha256sum).
func TestHashMatchesSha256sum(t *testing.T) {
	s, _ := newStore(t)
	suiteHash(t, s)
}

func TestPutDedup(t *testing.T) {
	s, dir := newStore(t)
	h, _, err := s.Put(context.Background(), strings.NewReader("same"))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, h[:2], h[2:4], h)
	before, _ := os.Stat(p)
	suiteDedup(t, s, fsCount(dir))
	after, _ := os.Stat(p)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("existing blob was overwritten")
	}
}

func TestOpenNotFoundAndInvalid(t *testing.T) {
	s, _ := newStore(t)
	suiteNotFoundAndInvalid(t, s)
}

func TestDelete(t *testing.T) {
	s, _ := newStore(t)
	suiteDelete(t, s)
}

func TestPutErrorLeavesNothing(t *testing.T) {
	s, dir := newStore(t)
	suitePutErrorLeavesNothing(t, s, fsCount(dir))
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
