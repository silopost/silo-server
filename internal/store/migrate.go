package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5:// driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/migrations"
)

// migrateLockKey is the pg_advisory_lock key serializing migrations across
// concurrently starting silo-server instances.
const migrateLockKey int64 = 0x53494c4f4d4947 // "SILOMIG"

// MigrateUp applies all pending migrations. It is a no-op when up to date.
func MigrateUp(ctx context.Context, url string) error {
	return withMigrator(ctx, url, func(m *migrate.Migrate) error { return m.Up() })
}

// MigrateDown reverts every migration, leaving only schema_migrations (version 0).
func MigrateDown(ctx context.Context, url string) error {
	return withMigrator(ctx, url, func(m *migrate.Migrate) error { return m.Down() })
}

// MigrateVersion reports the current schema version; version 0 means none applied.
func MigrateVersion(ctx context.Context, url string) (version uint, dirty bool, err error) {
	err = withMigrator(ctx, url, func(m *migrate.Migrate) error {
		var verr error
		version, dirty, verr = m.Version()
		if errors.Is(verr, migrate.ErrNilVersion) {
			return nil
		}
		return verr
	})
	return version, dirty, err
}

// withMigrator runs fn under the migration advisory lock, treating
// "no change" as success.
func withMigrator(ctx context.Context, url string, fn func(*migrate.Migrate) error) error {
	// The lock lives on its own connection for the whole run; it is released
	// when the connection closes, even if this process dies mid-migration.
	lockConn, err := pgx.Connect(ctx, url)
	if err != nil {
		return fmt.Errorf("connect for migration lock: %w", err)
	}
	defer lockConn.Close(context.WithoutCancel(ctx))
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}

	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("open migrations: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, migrateURL(url))
	if err != nil {
		return fmt.Errorf("init migrate: %w", err)
	}
	defer m.Close()

	if err := fn(m); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// migrateURL rewrites a postgres:// URL to the scheme of the golang-migrate pgx/v5 driver.
func migrateURL(url string) string {
	for _, p := range []string{"postgres://", "postgresql://"} {
		if strings.HasPrefix(url, p) {
			return "pgx5://" + strings.TrimPrefix(url, p)
		}
	}
	return url
}
