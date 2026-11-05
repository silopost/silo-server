package store

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

func TestBlobRefCounting(t *testing.T) {
	ctx := context.Background()
	url := newPostgres(t)
	if err := MigrateUp(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := sqlcgen.New(pool)

	sum := bytes.Repeat([]byte{7}, 32)
	refCount := func() int32 {
		var n int32
		if err := pool.QueryRow(ctx, `SELECT ref_count FROM blobs WHERE sha256 = $1`, sum).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	up := sqlcgen.UpsertBlobRefParams{Sha256: sum, SizeBytes: 10, Kind: "mirror"}
	for range 2 {
		if err := q.UpsertBlobRef(ctx, up); err != nil {
			t.Fatal(err)
		}
	}
	if n := refCount(); n != 2 {
		t.Fatalf("ref_count = %d, want 2", n)
	}
	for range 3 { // the third release must not go below zero
		if err := q.ReleaseBlobRef(ctx, sum); err != nil {
			t.Fatal(err)
		}
	}
	if n := refCount(); n != 0 {
		t.Fatalf("ref_count = %d, want 0", n)
	}

	old := func(d time.Duration) sqlcgen.ListUnreferencedBlobsParams {
		return sqlcgen.ListUnreferencedBlobsParams{CreatedAt: pgtype.Timestamptz{Time: time.Now().Add(d), Valid: true}, Limit: 10}
	}
	if rows, err := q.ListUnreferencedBlobs(ctx, old(-time.Hour)); err != nil || len(rows) != 0 {
		t.Fatalf("fresh blob listed: %v %v", rows, err)
	}
	if rows, err := q.ListUnreferencedBlobs(ctx, old(time.Hour)); err != nil || len(rows) != 1 {
		t.Fatalf("unreferenced blob not listed: %v %v", rows, err)
	}
}
