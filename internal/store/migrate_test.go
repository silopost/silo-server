package store

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

// latestVersion is the number of the newest migration (000050_local_auth_totp; phase 4 has 000006..000009).
const latestVersion = 50

// appTables is every table created by the migrations (schema_migrations excluded).
var appTables = []string{
	"account_rebuilds", "accounts", "attachments", "blobs", "contacts", "drafts", "events", "filters",
	"jobs", "label_folder_state", "labels", "mailbox_folders", "message_labels", "message_locations",
	"message_references", "messages", "op_messages", "operations", "outbound", "search_history", "sessions",
	"snooze_items", "snoozes", "sync_cursors", "thread_labels", "threads", "thumbnails", "users",
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
	if v, dirty, err := MigrateVersion(ctx, url); err != nil || v != latestVersion || dirty {
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
	if v, dirty, err := MigrateVersion(ctx, url); err != nil || v != latestVersion || dirty {
		t.Fatalf("version=%d dirty=%v err=%v", v, dirty, err)
	}
}

// 000005 turns sent_append on for generic accounts that existed before it.
func TestMigrateSentAppendGeneric(t *testing.T) {
	ctx := context.Background()
	url := newPostgres(t)
	if err := withMigrator(ctx, url, func(m *migrate.Migrate) error { return m.Migrate(4) }); err != nil {
		t.Fatal(err)
	}
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `
INSERT INTO users (email) VALUES ('u@x.test');
INSERT INTO accounts (user_id, email, provider, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, secret_enc, sent_append)
VALUES (1, 'g@x.test', 'generic', 'h', 993, 'implicit', 'h', 465, 'implicit', 'g', '\x00', false),
       (1, 'm@x.test', 'gmail', 'h', 993, 'implicit', 'h', 465, 'implicit', 'm', '\x00', false)`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUp(ctx, url); err != nil {
		t.Fatal(err)
	}
	if got := listStrings(t, pool, `SELECT provider || '=' || sent_append::text FROM accounts ORDER BY id`); !slices.Equal(got, []string{"generic=true", "gmail=false"}) {
		t.Errorf("after up: %v", got)
	}
	// Down is a no-op for the data.
	if err := withMigrator(ctx, url, func(m *migrate.Migrate) error { return m.Migrate(4) }); err != nil {
		t.Fatal(err)
	}
	if got := listStrings(t, pool, `SELECT provider || '=' || sent_append::text FROM accounts ORDER BY id`); !slices.Equal(got, []string{"generic=true", "gmail=false"}) {
		t.Errorf("after down: %v", got)
	}
}
