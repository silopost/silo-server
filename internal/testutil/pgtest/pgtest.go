// Package pgtest starts throwaway Postgres containers for integration tests.
package pgtest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// New starts a postgres:17 container and returns its connection URL.
//
// Integration tests are skipped with -short and when Docker is unavailable,
// unless SILO_REQUIRE_DOCKER=true (set in CI), in which case they fail.
func New(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped with -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	url, terminate, err := Run(ctx)
	if err != nil {
		if os.Getenv("SILO_REQUIRE_DOCKER") == "true" {
			t.Fatalf("start postgres container: %v", err)
		}
		t.Skipf("docker unavailable: %v", err)
	}
	t.Cleanup(terminate)
	return url
}

// Run starts a postgres:17 container without a *testing.T, for packages that
// share one container across all tests from TestMain. The returned function
// terminates the container.
func Run(ctx context.Context) (url string, terminate func(), err error) {
	ctr, err := tcpostgres.Run(ctx, "postgres:17",
		tcpostgres.WithDatabase("silo"),
		tcpostgres.WithUsername("silo"),
		tcpostgres.WithPassword("silo"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return "", nil, err
	}
	terminate = func() { _ = testcontainers.TerminateContainer(ctr) }
	url, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		terminate()
		return "", nil, err
	}
	return url, terminate, nil
}
