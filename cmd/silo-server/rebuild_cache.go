package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/store"
	silosync "github.com/silopost/silo-server/internal/sync"
)

// rebuildCacheCmd implements `silo-server rebuild-cache --account ID [--yes]`
// (plan 2.4, 3.3): the same as POST /accounts/{id}/resync {mode: rebuild},
// for an administrator (also after TRUNCATE of the cache tables). It records
// the request and queues the job; the running server (or the next serve)
// wipes the cache of the account and runs the initial sync from scratch.
// Refused while a send is queued or operations wait for write-back.
func rebuildCacheCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rebuild-cache", flag.ContinueOnError)
	accountID := fs.Int64("account", 0, "account id (required)")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *accountID <= 0 {
		return errors.New("--account ID is required")
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if cfg.DatabaseURL == "" {
		return errors.New("SILO_DATABASE_URL is required")
	}
	if !*yes {
		fmt.Printf("Drop the cached mail of account %d and download it again from the IMAP server? [y/N] ", *accountID)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errors.New("aborted")
		}
	}
	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	switch err := silosync.RebuildAccount(ctx, pool, *accountID); {
	case errors.Is(err, silosync.ErrNotFound):
		return fmt.Errorf("account %d not found", *accountID)
	case errors.Is(err, silosync.ErrRebuildBlocked):
		return fmt.Errorf("%v: wait until queued sends and pending operations are done", err)
	case err != nil:
		return err
	}
	fmt.Printf("rebuild of account %d queued; silo-server serve performs it (sync-status shows the initial sync)\n", *accountID)
	return nil
}
