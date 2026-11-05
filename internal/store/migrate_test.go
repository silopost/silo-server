package store

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// appTables is every table created by 000001_init (schema_migrations excluded).
var appTables = []string{
	"accounts", "attachments", "blobs", "contacts", "drafts", "events", "filters",
	"jobs", "labels", "mailbox_folders", "message_labels", "message_locations",
	"message_references", "messages", "op_messages", "outbound", "search_history", "sessions",
	"snooze_items", "snoozes", "sync_cursors", "thread_labels", "threads", "users",
}

func listStrings(t *testing.T, pool *pgxpool.Pool, query string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func tables(t *testing.T, pool *pgxpool.Pool) []string {
	return listStrings(t, pool, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name <> 'schema_migrations' ORDER BY 1`)
}

func extensions(t *testing.T, pool *pgxpool.Pool) []string {
	return listStrings(t, pool, `SELECT extname FROM pg_extension WHERE extname IN ('citext','pg_trgm','unaccent') ORDER BY 1`)
}

func TestMigrateRoundTrip(t *testing.T) {
	ctx := context.Background()
	url := newPostgres(t)
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if v, _, err := MigrateVersion(ctx, url); err != nil || v != 0 {
		t.Fatalf("fresh db: version=%d err=%v", v, err)
	}

	if err := MigrateUp(ctx, url); err != nil {
		t.Fatalf("up: %v", err)
	}
	if got := tables(t, pool); !slices.Equal(got, appTables) {
		t.Fatalf("tables after up:\n got %v\nwant %v", got, appTables)
	}
	if got := extensions(t, pool); len(got) != 3 {
		t.Fatalf("extensions after up = %v", got)
	}
	if v, dirty, err := MigrateVersion(ctx, url); err != nil || v != 1 || dirty {
		t.Fatalf("after up: version=%d dirty=%v err=%v", v, dirty, err)
	}

	// Up again is a no-op.
	if err := MigrateUp(ctx, url); err != nil {
		t.Fatalf("second up: %v", err)
	}

	if err := MigrateDown(ctx, url); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := tables(t, pool); len(got) != 0 {
		t.Fatalf("tables left after down: %v", got)
	}
	if got := extensions(t, pool); len(got) != 0 {
		t.Fatalf("extensions left after down: %v", got)
	}
	if v, _, err := MigrateVersion(ctx, url); err != nil || v != 0 {
		t.Fatalf("after down: version=%d err=%v", v, err)
	}

	if err := MigrateUp(ctx, url); err != nil {
		t.Fatalf("up after down: %v", err)
	}
	if got := tables(t, pool); !slices.Equal(got, appTables) {
		t.Fatalf("tables after re-up:\n got %v\nwant %v", got, appTables)
	}
}

// Concurrent instances starting at once must serialize on the advisory lock.
func TestMigrateConcurrent(t *testing.T) {
	ctx := context.Background()
	url := newPostgres(t)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = MigrateUp(ctx, url)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("runner %d: %v", i, err)
		}
	}
	if v, dirty, err := MigrateVersion(ctx, url); err != nil || v != 1 || dirty {
		t.Fatalf("version=%d dirty=%v err=%v", v, dirty, err)
	}
}
