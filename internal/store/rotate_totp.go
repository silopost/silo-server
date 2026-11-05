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

// RotateUserTOTPSecrets re-encrypts every users.totp_secret_enc not sealed with
// the active key (AAD = users.id), one row lock and transaction per user, like
// RotateAccountSecrets. It returns the number of rewritten users.
func RotateUserTOTPSecrets(ctx context.Context, pool *pgxpool.Pool, ring *secrets.Keyring) (int, error) {
	ids, err := sqlcgen.New(pool).ListUserTOTPSecrets(ctx)
	if err != nil {
		return 0, fmt.Errorf("list users: %w", err)
	}
	rotated := 0
	for _, id := range ids {
		changed, err := rotateUserTOTP(ctx, pool, ring, id)
		if err != nil {
			return rotated, fmt.Errorf("user %d: %w", id, err)
		}
		if changed {
			rotated++
		}
	}
	return rotated, nil
}

func rotateUserTOTP(ctx context.Context, pool *pgxpool.Pool, ring *secrets.Keyring, id int64) (bool, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	blob, err := q.LockUserTOTPSecret(ctx, id)
	if err != nil {
		return false, err
	}
	if blob == nil || !ring.NeedsRotation(blob) {
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
	if err := q.UpdateUserTOTPSecret(ctx, sqlcgen.UpdateUserTOTPSecretParams{ID: id, TotpSecretEnc: sealed}); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
