package store

import (
	"bytes"
	"context"
	"strconv"
	"testing"

	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

func TestRotateUserTOTPSecrets(t *testing.T) {
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
	oldRing, _ := secrets.NewKeyring(oldKey, nil)
	ring, _ := secrets.NewKeyring(newKey, oldKey)

	withTOTP, err := q.InsertUser(ctx, sqlcgen.InsertUserParams{Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertUser(ctx, sqlcgen.InsertUserParams{Email: "b@example.com"}); err != nil {
		t.Fatal(err)
	}
	aad := []byte(strconv.FormatInt(withTOTP.ID, 10))
	sealed, err := oldRing.Encrypt([]byte("12345678901234567890"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetTOTPSecret(ctx, sqlcgen.SetTOTPSecretParams{ID: withTOTP.ID, TotpSecretEnc: sealed}); err != nil {
		t.Fatal(err)
	}

	if n, err := RotateUserTOTPSecrets(ctx, pool, ring); err != nil || n != 1 {
		t.Fatalf("first run: %d, %v", n, err)
	}
	if n, err := RotateUserTOTPSecrets(ctx, pool, ring); err != nil || n != 0 {
		t.Fatalf("second run: %d, %v", n, err)
	}
	u, err := q.GetUserByID(ctx, withTOTP.ID)
	if err != nil {
		t.Fatal(err)
	}
	newOnly, _ := secrets.NewKeyring(newKey, nil)
	plain, err := newOnly.Decrypt(u.TotpSecretEnc, aad)
	if err != nil || string(plain) != "12345678901234567890" {
		t.Fatalf("decrypt with the new key: %q, %v", plain, err)
	}
}
