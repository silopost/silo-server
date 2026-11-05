package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/store"
)

// rotateSecretsCmd implements `silo-server rotate-secrets`: it re-encrypts every
// account secret sealed with SILO_SECRET_KEY_PREVIOUS using SILO_SECRET_KEY.
// Running it again is a no-op.
func rotateSecretsCmd(ctx context.Context, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if cfg.DatabaseURL == "" {
		return errors.New("SILO_DATABASE_URL is required")
	}
	if cfg.SecretKey == "" || cfg.SecretKeyPrevious == "" {
		return errors.New("rotate-secrets needs SILO_SECRET_KEY (new) and SILO_SECRET_KEY_PREVIOUS (old)")
	}
	current, previous, err := cfg.SecretKeys()
	if err != nil {
		return err
	}
	ring, err := secrets.NewKeyring(current, previous)
	if err != nil {
		return err
	}
	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	n, err := store.RotateAccountSecrets(ctx, pool, ring)
	if err != nil {
		return fmt.Errorf("rotated %d accounts before failing: %w", n, err)
	}
	fmt.Printf("re-encrypted secrets of %d accounts (active key id %s)\n", n, ring.ActiveKeyID())
	return nil
}
