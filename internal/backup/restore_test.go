package backup_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/backup"
	"github.com/silopost/silo-server/internal/worker"
)

// TestRestoreGivesUp: Message-IDs that never arrive stay staged with
// last_error; after GiveUp the job is dead (visible to the admin).
func TestRestoreGivesUp(t *testing.T) {
	ctx := context.Background()
	pool := newDB(t)
	s := &stack{t: t, pool: pool}
	uid := s.id(`INSERT INTO users (email) VALUES ('a@x.test') RETURNING id`)
	acc := s.id(`INSERT INTO accounts (user_id, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, secret_enc, initial_sync_done_at)
VALUES ($1, 'a@x.test', 'h', 993, 'implicit', 'h', 465, 'implicit', 'a', '\x00', now()) RETURNING id`, uid)
	s.exec(`INSERT INTO labels (user_id, account_id, name, type) VALUES ($1, $2, 'Projekt', 'user')`, uid, acc)
	s.exec(`INSERT INTO metadata_restores (user_id, account_id, kind, payload) VALUES
($1, $2, 'label', '{"label": "Projekt", "messageIdHdrs": ["gone@test"]}'),
($1, $2, 'label', '{"label": "Usunięta", "messageIdHdrs": ["x@test"]}')`, uid, acc)

	res, err := backup.Restore(ctx, pool, acc)
	if err != nil || res.Done || res.Pending != 1 || !res.InitialSyncDone {
		t.Fatalf("restore: %+v %v", res, err)
	}
	// The row of a deleted label is dropped; the other waits with its error.
	if got := s.strings(`SELECT kind || '|' || attempts || '|' || last_error FROM metadata_restores`); len(got) != 1 || got[0] != "label|1|1 messages not found" {
		t.Errorf("staged rows: %v", got)
	}

	jobs := worker.New(worker.Options{Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PollInterval: 50 * time.Millisecond})
	backup.RegisterJobs(jobs, backup.RestoreOptions{Pool: pool, RetryEvery: 50 * time.Millisecond, GiveUp: 300 * time.Millisecond})
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return backup.EnqueueRestore(ctx, tx, acc, time.Time{}) }); err != nil {
		t.Fatal(err)
	}
	// A second request while one is pending is not queued.
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return backup.EnqueueRestore(ctx, tx, acc, time.Time{}) }); err != nil {
		t.Fatal(err)
	}
	if n := s.count(`SELECT count(*) FROM jobs WHERE kind = $1`, backup.JobRestore); n != 1 {
		t.Fatalf("restore jobs = %d", n)
	}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = jobs.Run(rctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, 20*time.Second, "restore job dead", func() bool {
		return s.count(`SELECT count(*) FROM jobs WHERE kind = $1 AND status = 'dead' AND last_error LIKE 'gave up after%'`, backup.JobRestore) == 1
	})
	if n := s.count(`SELECT count(*) FROM metadata_restores WHERE attempts > 1`); n != 1 {
		t.Error("staged row gone or not retried")
	}
}
