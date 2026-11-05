package blob

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	// minio/minio is no longer published on Docker Hub (since late 2025);
	// a community build of the same server is the default here.
	// SILO_TEST_MINIO_IMAGE overrides it.
	minioImage = "ghcr.io/coollabsio/minio:RELEASE.2025-10-15T17-29-55Z"
	minioUser  = "silominio"
	minioPass  = "silominiopass"
)

// startMinIO launches a throwaway MinIO and returns its host:port. Skipped
// with -short or without Docker, unless SILO_REQUIRE_DOCKER=true.
func startMinIO(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped with -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        cmp.Or(os.Getenv("SILO_TEST_MINIO_IMAGE"), minioImage),
			Cmd:          []string{"server", "/data"},
			Env:          map[string]string{"MINIO_ROOT_USER": minioUser, "MINIO_ROOT_PASSWORD": minioPass},
			ExposedPorts: []string{"9000/tcp"},
			WaitingFor:   wait.ForHTTP("/minio/health/ready").WithPort("9000/tcp").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if ctr != nil {
		testcontainers.CleanupContainer(t, ctr)
	}
	if err != nil {
		if os.Getenv("SILO_REQUIRE_DOCKER") == "true" {
			t.Fatalf("start minio container: %v", err)
		}
		t.Skipf("docker unavailable: %v", err)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := ctr.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s:%s", host, port.Port())
}

func adminClient(t *testing.T, endpoint string) *minio.Client {
	t.Helper()
	cl, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(minioUser, minioPass, ""), BucketLookup: minio.BucketLookupPath})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

func objectCount(cl *minio.Client, bucket, prefix string) func(t *testing.T) int {
	return func(t *testing.T) int {
		t.Helper()
		n := 0
		for o := range cl.ListObjects(context.Background(), bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
			if o.Err != nil {
				t.Fatal(o.Err)
			}
			n++
		}
		return n
	}
}

// TestS3Store runs the BlobStore contract of fs_test.go against MinIO, with
// and without a key prefix.
func TestS3Store(t *testing.T) {
	endpoint := startMinIO(t)
	admin := adminClient(t, endpoint)
	ctx := context.Background()
	bucketN := 0
	newS3 := func(t *testing.T, prefix string) (*S3Store, func(t *testing.T) int) {
		t.Helper()
		bucketN++
		bucket := fmt.Sprintf("silo-test-%d", bucketN)
		if err := admin.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			t.Fatal(err)
		}
		s, err := NewS3Store(ctx, S3Options{Endpoint: endpoint, Bucket: bucket, AccessKey: minioUser, SecretKey: minioPass,
			Prefix: prefix, UseSSL: false, PathStyle: true, TempDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		return s, objectCount(admin, bucket, "")
	}

	for _, prefix := range []string{"", "/silo/blobs/"} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			t.Run("RoundTrip", func(t *testing.T) {
				s, _ := newS3(t, prefix)
				hash, _ := suiteRoundTrip(t, s)
				want := strings.TrimPrefix(strings.TrimSuffix(prefix, "/")+"/", "/") + hash[:2] + "/" + hash[2:4] + "/" + hash
				if _, err := admin.StatObject(ctx, s.bucket, want, minio.StatObjectOptions{}); err != nil {
					t.Fatalf("layout %s: %v", want, err)
				}
			})
			t.Run("Hash", func(t *testing.T) {
				s, _ := newS3(t, prefix)
				suiteHash(t, s)
			})
			t.Run("Dedup", func(t *testing.T) {
				s, count := newS3(t, prefix)
				suiteDedup(t, s, count)
			})
			t.Run("NotFoundAndInvalid", func(t *testing.T) {
				s, _ := newS3(t, prefix)
				suiteNotFoundAndInvalid(t, s)
			})
			t.Run("Delete", func(t *testing.T) {
				s, _ := newS3(t, prefix)
				suiteDelete(t, s)
			})
			t.Run("PutErrorLeavesNothing", func(t *testing.T) {
				s, count := newS3(t, prefix)
				suitePutErrorLeavesNothing(t, s, count)
			})
		})
	}

	t.Run("MissingBucket", func(t *testing.T) {
		_, err := NewS3Store(ctx, S3Options{Endpoint: "http://" + endpoint, Bucket: "nope", AccessKey: minioUser, SecretKey: minioPass, PathStyle: true})
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("missing bucket: %v", err)
		}
	})
	t.Run("BadCredentials", func(t *testing.T) {
		if _, err := NewS3Store(ctx, S3Options{Endpoint: endpoint, Bucket: "silo-test-1", AccessKey: "x", SecretKey: "yyyyyyyy", PathStyle: true}); err == nil {
			t.Fatal("bad credentials accepted")
		}
	})
}
