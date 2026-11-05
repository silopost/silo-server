package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// newPostgres starts a throwaway postgres:17 container and returns its URL.
//
// Integration tests are skipped with -short and when Docker is unavailable,
// unless SILO_REQUIRE_DOCKER=true (set in CI), in which case they fail.
func newPostgres(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped with -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx, "postgres:17",
		tcpostgres.WithDatabase("silo"),
		tcpostgres.WithUsername("silo"),
		tcpostgres.WithPassword("silo"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if os.Getenv("SILO_REQUIRE_DOCKER") == "true" {
			t.Fatalf("start postgres container: %v", err)
		}
		t.Skipf("docker unavailable: %v", err)
	}
	testcontainers.CleanupContainer(t, ctr)

	url, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return url
}
