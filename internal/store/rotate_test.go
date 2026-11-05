package store

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

func TestRotateAccountSecrets(t *testing.T) {
	const n = 1000
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

	oldKey, newKey := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	oldRing, err := secrets.NewKeyring(oldKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.NewKeyring(newKey, oldKey)
	if err != nil {
		t.Fatal(err)
	}

	user, err := q.InsertUser(ctx, sqlcgen.InsertUserParams{Email: "u@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	// Insert with a placeholder, then seal using the generated id as AAD.
	for i := range n {
		acc, err := q.InsertAccount(ctx, sqlcgen.InsertAccountParams{
			UserID: user.ID, Email: "a@example.com", Provider: "generic",
			ImapHost: "imap.example.com", ImapPort: 993, ImapTls: "implicit",
			SmtpHost: "smtp.example.com", SmtpPort: 587, SmtpTls: "starttls",
			Username: fmt.Sprintf("user%d", i), AuthType: "password", SecretEnc: []byte("x"),
		})
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := oldRing.Encrypt([]byte("pw-"+strconv.FormatInt(acc.ID, 10)), []byte(strconv.FormatInt(acc.ID, 10)))
		if err != nil {
			t.Fatal(err)
		}
		if err := q.UpdateAccountSecret(ctx, sqlcgen.UpdateAccountSecretParams{ID: acc.ID, SecretEnc: sealed}); err != nil {
			t.Fatal(err)
		}
	}

	start := time.Now()
	got, err := RotateAccountSecrets(ctx, pool, ring)
	if err != nil || got != n {
		t.Fatalf("first run: rotated=%d err=%v", got, err)
	}
	t.Logf("rotated %d secrets in %s", n, time.Since(start))

	checkAll := func() map[int64][]byte {
		rows, err := pool.Query(ctx, `SELECT id, secret_enc FROM accounts`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[int64][]byte{}
		for rows.Next() {
			var id int64
			var blob []byte
			if err := rows.Scan(&id, &blob); err != nil {
				t.Fatal(err)
			}
			out[id] = blob
		}
		return out
	}
	after := checkAll()
	if len(after) != n {
		t.Fatalf("got %d accounts", len(after))
	}
	for id, blob := range after {
		if ring.NeedsRotation(blob) {
			t.Fatalf("account %d still on old key", id)
		}
		idStr := strconv.FormatInt(id, 10)
		plain, err := ring.Decrypt(blob, []byte(idStr))
		if err != nil || string(plain) != "pw-"+idStr {
			t.Fatalf("account %d: %q, %v", id, plain, err)
		}
	}

	got, err = RotateAccountSecrets(ctx, pool, ring)
	if err != nil || got != 0 {
		t.Fatalf("second run: rotated=%d err=%v", got, err)
	}
	again := checkAll()
	for id, blob := range after {
		if !bytes.Equal(blob, again[id]) {
			t.Fatalf("account %d changed on idempotent run", id)
		}
	}
}
