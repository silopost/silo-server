package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

// TestGeneratedQueries runs the sqlc queries against a migrated database,
// including tenant isolation of the account lookups.
func TestGeneratedQueries(t *testing.T) {
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

	alice, err := q.InsertUser(ctx, sqlcgen.InsertUserParams{Email: "Alice@Example.com", DisplayName: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := q.InsertUser(ctx, sqlcgen.InsertUserParams{Email: "bob@example.com"})
	if err != nil {
		t.Fatal(err)
	}

	// email is citext: lookup is case-insensitive.
	got, err := q.GetUserByEmail(ctx, "alice@example.com")
	if err != nil || got.ID != alice.ID {
		t.Fatalf("GetUserByEmail = %+v, %v", got, err)
	}
	if got, err = q.GetUserByID(ctx, bob.ID); err != nil || got.Email != "bob@example.com" {
		t.Fatalf("GetUserByID = %+v, %v", got, err)
	}

	acc, err := q.InsertAccount(ctx, sqlcgen.InsertAccountParams{
		UserID: alice.ID, Email: "alice@example.com", Provider: "generic",
		ImapHost: "imap.example.com", ImapPort: 993, ImapTls: "implicit",
		SmtpHost: "smtp.example.com", SmtpPort: 587, SmtpTls: "starttls",
		Username: "alice", AuthType: "password", SecretEnc: []byte("sealed"),
	})
	if err != nil {
		t.Fatal(err)
	}

	list, err := q.ListAccountsByUser(ctx, alice.ID)
	if err != nil || len(list) != 1 || list[0].ID != acc.ID {
		t.Fatalf("ListAccountsByUser(alice) = %+v, %v", list, err)
	}
	if list, err = q.ListAccountsByUser(ctx, bob.ID); err != nil || len(list) != 0 {
		t.Fatalf("ListAccountsByUser(bob) = %+v, %v", list, err)
	}
	if n, err := q.CountAccountsByUser(ctx, alice.ID); err != nil || n != 1 {
		t.Fatalf("CountAccountsByUser = %d, %v", n, err)
	}
	if _, err := q.GetAccount(ctx, sqlcgen.GetAccountParams{ID: acc.ID, UserID: alice.ID}); err != nil {
		t.Fatalf("GetAccount(owner): %v", err)
	}
	// Another tenant must not see the account.
	if _, err := q.GetAccount(ctx, sqlcgen.GetAccountParams{ID: acc.ID, UserID: bob.ID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetAccount(other user) err = %v, want no rows", err)
	}
}
