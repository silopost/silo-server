package store

import (
	"testing"

	"github.com/silopost/silo-server/internal/testutil/pgtest"
)

// newPostgres starts a throwaway postgres:17 container and returns its URL.
func newPostgres(t *testing.T) string {
	t.Helper()
	return pgtest.New(t)
}
