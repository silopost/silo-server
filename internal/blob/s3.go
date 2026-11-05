package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Options configure an S3Store (SILO_BLOB_S3_*).
type S3Options struct {
	// Endpoint is host:port, or a URL whose scheme (http/https) overrides
	// UseSSL.
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string // default us-east-1
	// Prefix is prepended to every object key ("silo" -> silo/ab/cd/<sha>).
	Prefix string
	UseSSL bool
	// PathStyle addresses the bucket in the path (MinIO) instead of the
	// host name (virtual-hosted style).
	PathStyle bool
	// TempDir holds the spool file of Put (default: the OS temp dir).
	TempDir string
}

// S3Store keeps blobs as objects <prefix>/ab/cd/<sha256> in an
// S3-compatible bucket, with the semantics of FSStore.
type S3Store struct {
	cl     *minio.Client
	bucket string
	prefix string
	tmp    string
}

var _ BlobStore = (*S3Store)(nil)

// NewS3Store connects to the bucket and checks that it exists (it is not
// created: a missing bucket is a configuration error).
func NewS3Store(ctx context.Context, o S3Options) (*S3Store, error) {
	if o.Endpoint == "" || o.Bucket == "" {
		return nil, errors.New("blob: s3 endpoint and bucket are required")
	}
	endpoint, secure := o.Endpoint, o.UseSSL
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("blob: s3 endpoint %q: not a URL", o.Endpoint)
		}
		switch u.Scheme {
		case "https":
			secure = true
		case "http":
			secure = false
		default:
			return nil, fmt.Errorf("blob: s3 endpoint %q: scheme must be http or https", o.Endpoint)
		}
		endpoint = u.Host
	}
	region := o.Region
	if region == "" {
		region = "us-east-1"
	}
	lookup := minio.BucketLookupDNS
	if o.PathStyle {
		lookup = minio.BucketLookupPath
	}
	cl, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(o.AccessKey, o.SecretKey, ""),
		Secure:       secure,
		Region:       region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("blob: s3 client: %w", err)
	}
	ok, err := cl.BucketExists(ctx, o.Bucket)
	if err != nil {
		return nil, fmt.Errorf("blob: s3 bucket %q at %s: %w", o.Bucket, endpoint, err)
	}
	if !ok {
		return nil, fmt.Errorf("blob: s3 bucket %q does not exist at %s (create it first)", o.Bucket, endpoint)
	}
	prefix := strings.Trim(o.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &S3Store{cl: cl, bucket: o.Bucket, prefix: prefix, tmp: o.TempDir}, nil
}

// key maps a hash to its object key; like FSStore.path it accepts only 64
// lower-case hex characters.
func (s *S3Store) key(hash string) (string, error) {
	if len(hash) != 64 {
		return "", fmt.Errorf("blob: invalid hash %q", hash)
	}
	for _, c := range hash {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("blob: invalid hash %q", hash)
		}
	}
	return s.prefix + hash[:2] + "/" + hash[2:4] + "/" + hash, nil
}

func isNoSuchKey(err error) bool {
	r := minio.ToErrorResponse(err)
	return r.Code == "NoSuchKey" || r.StatusCode == http.StatusNotFound
}

// Put spools r to a temporary file while hashing it (the key is known only
// at the end), then uploads it unless the object exists already.
func (s *S3Store) Put(ctx context.Context, r io.Reader) (string, int64, error) {
	tmp, err := os.CreateTemp(s.tmp, "silo-s3-*")
	if err != nil {
		return "", 0, fmt.Errorf("blob: create temp: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), &ctxReader{ctx: ctx, r: r})
	if err != nil {
		return "", 0, fmt.Errorf("blob: write: %w", err)
	}
	hash := hex.EncodeToString(h.Sum(nil))
	key, _ := s.key(hash)
	if _, err := s.cl.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err == nil {
		return hash, size, nil // dedup: content is immutable
	} else if !isNoSuchKey(err) {
		return "", 0, fmt.Errorf("blob: s3 stat: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	if _, err := s.cl.PutObject(ctx, s.bucket, key, tmp, size, minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		return "", 0, fmt.Errorf("blob: s3 put: %w", err)
	}
	return hash, size, nil
}

// Open returns the object; it supports Seek (ranged reads).
func (s *S3Store) Open(ctx context.Context, hash string) (io.ReadSeekCloser, error) {
	key, err := s.key(hash)
	if err != nil {
		return nil, err
	}
	obj, err := s.cl.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("blob: s3 get: %w", err)
	}
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if isNoSuchKey(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, hash)
		}
		return nil, fmt.Errorf("blob: s3 get: %w", err)
	}
	return obj, nil
}

// Delete removes the object; a missing object is fine.
func (s *S3Store) Delete(ctx context.Context, hash string) error {
	key, err := s.key(hash)
	if err != nil {
		return err
	}
	if err := s.cl.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil && !isNoSuchKey(err) {
		return fmt.Errorf("blob: s3 delete: %w", err)
	}
	return nil
}
