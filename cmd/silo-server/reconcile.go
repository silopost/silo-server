package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/store"
)

// reconcileCmd implements `silo-server reconcile [--account ID]`: it compares
// the derived mail data (thread aggregates, thread_labels, system labels,
// is_unread) with their sources and prints every difference. It changes
// nothing and exits with an error when drift is found.
func reconcileCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	accountID := fs.Int64("account", 0, "check only this account id (default: all accounts)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if cfg.DatabaseURL == "" {
		return errors.New("SILO_DATABASE_URL is required")
	}
	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	var ids []int64
	if *accountID != 0 {
		ids = []int64{*accountID}
	} else {
		rows, err := pool.Query(ctx, `SELECT id FROM accounts ORDER BY id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	total := 0
	for _, id := range ids {
		ds, err := mail.Reconcile(ctx, pool, id)
		if err != nil {
			return fmt.Errorf("account %d: %w", id, err)
		}
		for _, d := range ds {
			fmt.Printf("account %d: %s\n", id, d)
		}
		total += len(ds)
	}
	fmt.Printf("checked %d accounts, %d discrepancies\n", len(ids), total)
	if total > 0 {
		return fmt.Errorf("%d discrepancies found", total)
	}
	return nil
}
