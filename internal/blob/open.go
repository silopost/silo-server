package blob

import (
	"context"
	"fmt"

	"github.com/silopost/silo-server/internal/config"
)

// Open returns the blob store selected by SILO_BLOB_BACKEND (fs or s3).
func Open(ctx context.Context, cfg config.Config) (BlobStore, error) {
	switch cfg.BlobBackend {
	case "", "fs":
		return NewFSStore(cfg.BlobFSDir)
	case "s3":
		return NewS3Store(ctx, S3Options{
			Endpoint: cfg.BlobS3Endpoint, Bucket: cfg.BlobS3Bucket,
			AccessKey: cfg.BlobS3AccessKey, SecretKey: cfg.BlobS3SecretKey,
			Region: cfg.BlobS3Region, Prefix: cfg.BlobS3Prefix,
			UseSSL: cfg.BlobS3UseSSL, PathStyle: cfg.BlobS3PathStyle,
		})
	}
	return nil, fmt.Errorf("blob: unknown backend %q (SILO_BLOB_BACKEND must be fs or s3)", cfg.BlobBackend)
}
