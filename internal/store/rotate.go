package store

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

// RotateAccountSecrets re-encrypts every accounts.secret_enc that was not sealed
// with the keyring's active key. Each account is handled in its own transaction
// holding a row lock, so it is safe to run next to a live server and to repeat
// (a second run changes nothing). It returns the number of rewritten accounts.
func RotateAccountSecrets(ctx context.Context, pool *pgxpool.Pool, ring *secrets.Keyring) (int, error) {
	ids, err := sqlcgen.New(pool).ListAccountIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list accounts: %w", err)
	}
	rotated := 0
	for _, id := range ids {
		changed, err := rotateAccount(ctx, pool, ring, id)
		if err != nil {
			return rotated, fmt.Errorf("account %d: %w", id, err)
		}
		if changed {
			rotated++
		}
	}
	return rotated, nil
}

func rotateAccount(ctx context.Context, pool *pgxpool.Pool, ring *secrets.Keyring, id int64) (bool, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	blob, err := q.LockAccountSecret(ctx, id)
	if err != nil {
		return false, err
	}
	if !ring.NeedsRotation(blob) {
		return false, nil
	}
	aad := []byte(strconv.FormatInt(id, 10))
	plain, err := ring.Decrypt(blob, aad)
	if err != nil {
		return false, err
	}
	sealed, err := ring.Encrypt(plain, aad)
	if err != nil {
		return false, err
	}
	if err := q.UpdateAccountSecret(ctx, sqlcgen.UpdateAccountSecretParams{ID: id, SecretEnc: sealed}); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
