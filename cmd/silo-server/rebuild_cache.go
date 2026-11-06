package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/store"
	silosync "github.com/silopost/silo-server/internal/sync"
)

// rebuildCacheCmd implements `silo-server rebuild-cache --account ID|--all
// [--yes]` (plan 2.4, 3.3): the same as POST /accounts/{id}/resync {mode:
// rebuild}, for an administrator (also after TRUNCATE of the cache tables).
// It records the request and queues the job; the running server (or the
// next serve) wipes the cache of the account and runs the initial sync from
// scratch. Refused while a send is in flight or due, or operations wait for
// write-back.
func rebuildCacheCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rebuild-cache", flag.ContinueOnError)
	accountID := fs.Int64("account", 0, "account id")
	all := fs.Bool("all", false, "every account that is not disabled")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*accountID > 0) == *all {
		return errors.New("give --account ID or --all")
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if cfg.DatabaseURL == "" {
		return errors.New("SILO_DATABASE_URL is required")
	}
	if !*yes {
		what := fmt.Sprintf("account %d", *accountID)
		if *all {
			what = "every account"
		}
		fmt.Printf("Drop the cached mail of %s and download it again from the IMAP server? [y/N] ", what)
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
	return runRebuildCache(ctx, pool, *accountID, *all, os.Stdout, os.Stderr)
}

// runRebuildCache requests the rebuild of one account, or with all of every
// account that is not disabled; all goes on past an account that is refused
// and fails at the end.
func runRebuildCache(ctx context.Context, pool *pgxpool.Pool, accountID int64, all bool, out, errOut io.Writer) error {
	ids := []int64{accountID}
	if all {
		rows, err := pool.Query(ctx, `SELECT id FROM accounts WHERE sync_state <> 'disabled' ORDER BY id`)
		if err != nil {
			return err
		}
		if ids, err = pgx.CollectRows(rows, pgx.RowTo[int64]); err != nil {
			return err
		}
	}
	failed := 0
	for _, id := range ids {
		err := silosync.RebuildAccount(ctx, pool, id)
		switch {
		case err == nil:
			fmt.Fprintf(out, "rebuild of account %d queued; silo-server serve performs it (sync-status shows the initial sync)\n", id)
			continue
		case errors.Is(err, silosync.ErrNotFound):
			err = fmt.Errorf("account %d not found", id)
		case errors.Is(err, silosync.ErrRebuildBlocked):
			err = fmt.Errorf("account %d: %v: wait until sends in flight and pending operations are done", id, err)
		}
		if !all {
			return err
		}
		fmt.Fprintln(errOut, err)
		failed++
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d accounts not queued", failed, len(ids))
	}
	return nil
}
