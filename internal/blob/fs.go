package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// FSStore keeps blobs on a local filesystem as <dir>/ab/cd/abcdef....
type FSStore struct {
	dir string
}

var _ BlobStore = (*FSStore)(nil)

// NewFSStore returns a store rooted at dir, creating it if needed.
func NewFSStore(dir string) (*FSStore, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("blob dir: %w", err)
	}
	return &FSStore{dir: dir}, nil
}

// path maps a hash to its file; it rejects anything that is not 64 lower-case
// hex characters so callers cannot escape the root.
func (s *FSStore) path(hash string) (string, error) {
	if len(hash) != 64 {
		return "", fmt.Errorf("blob: invalid hash %q", hash)
	}
	for _, c := range hash {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("blob: invalid hash %q", hash)
		}
	}
	return filepath.Join(s.dir, hash[:2], hash[2:4], hash), nil
}

// Put streams r into a temporary file, fsyncs it and renames it into place.
func (s *FSStore) Put(ctx context.Context, r io.Reader) (string, int64, error) {
	// The hash is unknown until the stream ends, so the temp file lives in the
	// store root (same filesystem as the final path, hence an atomic rename).
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return "", 0, fmt.Errorf("blob: create temp: %w", err)
	}
	defer func() {
		// After a successful rename the temp name no longer exists.
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), &ctxReader{ctx: ctx, r: r})
	if err != nil {
		return "", 0, fmt.Errorf("blob: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("blob: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("blob: close: %w", err)
	}
	hash := hex.EncodeToString(h.Sum(nil))
	dst, _ := s.path(hash)

	if _, err := os.Stat(dst); err == nil {
		return hash, size, nil // dedup: content is immutable, keep the existing file
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return "", 0, fmt.Errorf("blob: mkdir: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", 0, fmt.Errorf("blob: rename: %w", err)
	}
	syncDir(filepath.Dir(dst))
	return hash, size, nil
}

// Open returns the blob file.
func (s *FSStore) Open(_ context.Context, hash string) (io.ReadSeekCloser, error) {
	p, err := s.path(hash)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, hash)
	}
	if err != nil {
		return nil, fmt.Errorf("blob: open: %w", err)
	}
	return f, nil
}

// Delete removes the blob file; a missing file is fine.
func (s *FSStore) Delete(_ context.Context, hash string) error {
	p, err := s.path(hash)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("blob: delete: %w", err)
	}
	return nil
}

// syncDir makes the rename durable. Directories cannot be synced on Windows,
// so errors are ignored (best effort).
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// ctxReader aborts a copy when the context is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
