package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/silopost/silo-server/migrations"
)

var (
	createTableRe = regexp.MustCompile(`(?is)CREATE TABLE (\w+) \((.*?)\n\);`)
	queryTableRe  = regexp.MustCompile(`(?i)\b(?:from|join|into|update)\s+(\w+)`)
	userIDColRe   = regexp.MustCompile(`(?m)^\s*user_id\s`)
)

// tenantTables returns the tables of the initial migration that have a user_id column.
func tenantTables(t *testing.T) map[string]bool {
	t.Helper()
	sql, err := migrations.FS.ReadFile("000001_init.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, m := range createTableRe.FindAllStringSubmatch(string(sql), -1) {
		if userIDColRe.MatchString(m[2]) {
			out[m[1]] = true
		}
	}
	if !out["accounts"] || !out["messages"] || out["blobs"] {
		t.Fatalf("tenant table detection is broken: %v", out)
	}
	return out
}

// TestQueriesCarryUserID is the tenancy contract (plan 3.2): every query that
// touches a table with a user_id column must mention user_id. A query that is
// legitimately cross-tenant (e.g. a background worker) must say so with a
// "-- tenancy: exempt <reason>" comment inside the query.
func TestQueriesCarryUserID(t *testing.T) {
	tenant := tenantTables(t)
	files, err := filepath.Glob("queries/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no query files found: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// Each query starts with a "-- name:" line.
		for _, q := range strings.Split(string(b), "-- name:")[1:] {
			for _, table := range tenancyViolations(tenant, q) {
				t.Errorf("%s: query %s touches %s but never mentions user_id", f, strings.Fields(q)[0], table)
			}
		}
	}
}

// tenancyViolations returns the tenant tables a query touches without mentioning user_id.
func tenancyViolations(tenant map[string]bool, query string) []string {
	if strings.Contains(query, "-- tenancy: exempt") || strings.Contains(query, "user_id") {
		return nil
	}
	var out []string
	for _, m := range queryTableRe.FindAllStringSubmatch(query, -1) {
		if tenant[strings.ToLower(m[1])] {
			out = append(out, m[1])
		}
	}
	return out
}

func TestTenancyViolations(t *testing.T) {
	tenant := map[string]bool{"threads": true}
	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"missing user_id", "GetThread :one\nSELECT * FROM threads WHERE id = $1;", 1},
		{"join counts", "X :many\nSELECT * FROM blobs JOIN threads ON true;", 1},
		{"has user_id", "X :one\nSELECT * FROM threads WHERE id = $1 AND user_id = $2;", 0},
		{"non-tenant table", "X :one\nSELECT * FROM blobs WHERE sha256 = $1;", 0},
		{"explicit exemption", "X :many\n-- tenancy: exempt background worker\nSELECT * FROM threads;", 0},
	}
	for _, tt := range tests {
		if got := tenancyViolations(tenant, tt.query); len(got) != tt.want {
			t.Errorf("%s: got %v, want %d violations", tt.name, got, tt.want)
		}
	}
}
