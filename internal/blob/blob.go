// Package blob stores immutable, content-addressed byte streams (raw messages,
// attachments, draft uploads). Reference counting lives in the database
// (blobs.ref_count, see package store); this package only manages the bytes.
package blob

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned by Open when no blob has the given hash.
var ErrNotFound = errors.New("blob: not found")

// BlobStore is the storage backend selected by SILO_BLOB_BACKEND.
type BlobStore interface {
	// Put stores r and returns its lower-case hex SHA-256 and size. It is
	// atomic: readers never see a partial blob. Putting content that already
	// exists is a no-op that returns the same hash.
	Put(ctx context.Context, r io.Reader) (sha256 string, size int64, err error)
	// Open returns the blob, or an error matching ErrNotFound.
	Open(ctx context.Context, sha256 string) (io.ReadSeekCloser, error)
	// Delete removes the blob. Deleting a missing blob is not an error.
	Delete(ctx context.Context, sha256 string) error
}
