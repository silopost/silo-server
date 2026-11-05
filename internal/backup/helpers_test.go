package backup_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/backup"
	"github.com/silopost/silo-server/internal/blob"
	"github.com/silopost/silo-server/internal/drafts"
	"github.com/silopost/silo-server/internal/imapx"
	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/mailview"
	"github.com/silopost/silo-server/internal/ops"
	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/store"
	silosync "github.com/silopost/silo-server/internal/sync"
	"github.com/silopost/silo-server/internal/testutil/pgtest"
	"github.com/silopost/silo-server/internal/worker"
)

var testKey = bytes.Repeat([]byte("k"), 32)

func keyring(t *testing.T) *secrets.Keyring {
	t.Helper()
	ring, err := secrets.NewKeyring(testKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

// newDB starts a Postgres with the schema migrated.
func newDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := pgtest.New(t)
	ctx := context.Background()
	if err := store.MigrateUp(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// stack is the part of the server the tests need: job worker, sync engine,
// drafts, write-back and the metadata_restore job over one database.
type stack struct {
	t     *testing.T
	pool  *pgxpool.Pool
	ring  *secrets.Keyring
	blobs blob.BlobStore
	ops   *ops.Service
	dr    *drafts.Service
	view  *mailview.View
	sync  *silosync.Service
	stop  func()
}

func newBlobs(t *testing.T) blob.BlobStore {
	t.Helper()
	b, err := blob.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func startStack(t *testing.T, pool *pgxpool.Pool, blobs blob.BlobStore) *stack {
	t.Helper()
	ring := keyring(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	jobs := worker.New(worker.Options{Pool: pool, Logger: logger, Concurrency: 4, PollInterval: 100 * time.Millisecond})
	mgr := silosync.NewManager(silosync.Deps{
		Pool: pool, Keys: ring, Blobs: blobs, Logger: logger,
		Settings: silosync.Settings{MaxConnections: 3, InitialConcurrency: 2, PollInterval: time.Minute, BackfillRate: 50 << 20,
			MaxMessageBytes: 50 << 20, InsecureTLS: true},
		Backoff: []time.Duration{100 * time.Millisecond, 200 * time.Millisecond},
	})
	mgr.RegisterJobs(jobs)
	view := mailview.New(pool, blobs, mgr)
	dr := drafts.New(drafts.Options{Pool: pool, Blobs: blobs, MaxUploadBytes: 10 << 20, SyncDelay: 200 * time.Millisecond,
		Keys: ring, InsecureTransport: true, Logger: logger,
		OpenRaw: func(ctx context.Context, userID, messageID int64) (io.ReadCloser, error) {
			r, err := view.OpenRaw(ctx, userID, messageID)
			if err != nil {
				return nil, err
			}
			return r.Body, nil
		}})
	dr.RegisterJobs(jobs)
	backup.RegisterJobs(jobs, backup.RestoreOptions{Pool: pool, Logger: logger, RetryEvery: 300 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	go func() { _ = jobs.Run(ctx); done <- struct{}{} }()
	go func() { _ = mgr.Run(ctx); done <- struct{}{} }()
	var once stdsync.Once
	s := &stack{t: t, pool: pool, ring: ring, blobs: blobs, ops: ops.NewService(pool, 0), dr: dr, view: view,
		sync: silosync.NewService(pool)}
	s.stop = func() {
		once.Do(func() {
			cancel()
			<-done
			<-done
		})
	}
	t.Cleanup(s.stop)
	return s
}

func (s *stack) count(query string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		s.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (s *stack) id(query string, args ...any) int64 {
	s.t.Helper()
	var n int64
	if err := s.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		s.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (s *stack) exec(query string, args ...any) {
	s.t.Helper()
	if _, err := s.pool.Exec(context.Background(), query, args...); err != nil {
		s.t.Fatalf("%s: %v", query, err)
	}
}

// addAccount creates (or reuses) a user and adds an account on the test
// Dovecot, as POST /accounts does (sealed secret, initial_sync job).
func (s *stack) addAccount(email, addr, user, pass string) (userID, accountID int64) {
	s.t.Helper()
	ctx := context.Background()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
INSERT INTO users (email, display_name) VALUES ($1, $2) ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email RETURNING id`,
			email, email).Scan(&userID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
INSERT INTO accounts (user_id, email, display_name, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, secret_enc, sent_append)
VALUES ($1, $2, 'Konto', $3, $4, 'none', $3, 2525, 'none', $5, '\x', true) RETURNING id`, userID, user+"@silo.test", host, port, user).
			Scan(&accountID); err != nil {
			return err
		}
		sealed, err := s.ring.Encrypt([]byte(pass), []byte(strconv.FormatInt(accountID, 10)))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE accounts SET secret_enc = $2 WHERE id = $1`, accountID, sealed); err != nil {
			return err
		}
		_, err = worker.Enqueue(ctx, tx, worker.NewJob{Kind: auth.JobAccountInitialSync, AccountID: &accountID})
		return err
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return userID, accountID
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	start := time.Now()
	for time.Since(start) < timeout {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// waitSettled waits until every account finished its initial sync, is live,
// and no write-back, draft copy or restore work is left.
func (s *stack) waitSettled(what string) {
	s.t.Helper()
	waitFor(s.t, 2*time.Minute, what, func() bool {
		return s.count(`SELECT count(*) FROM accounts WHERE initial_sync_done_at IS NULL OR sync_state <> 'live'`) == 0 &&
			s.count(`SELECT count(*) FROM jobs WHERE status IN ('pending', 'running') AND kind <> 'send'`) == 0 &&
			s.count(`SELECT count(*) FROM metadata_restores`) == 0
	})
}

func (s *stack) msgID(accountID int64, hdr string) int64 {
	s.t.Helper()
	return s.id(`SELECT id FROM messages WHERE account_id = $1 AND message_id_hdr = $2 ORDER BY id LIMIT 1`, accountID, hdr)
}

// userLabels is the set user|Message-ID|label of every user-label
// assignment.
func (s *stack) userLabels() []string {
	s.t.Helper()
	return s.strings(`
SELECT DISTINCT u.email::text || '|' || m.message_id_hdr || '|' || l.name
FROM message_labels ml JOIN labels l ON l.id = ml.label_id JOIN messages m ON m.id = ml.message_id
JOIN users u ON u.id = l.user_id
WHERE l.type = 'user' ORDER BY 1`)
}

// threadPartition is the split of the account's messages into threads: one
// sorted Message-ID list per thread, sorted.
func (s *stack) threadPartition(accountID int64) []string {
	s.t.Helper()
	return s.strings(`
SELECT string_agg(message_id_hdr, ',' ORDER BY message_id_hdr) FROM messages
WHERE account_id = $1 AND orphaned_at IS NULL GROUP BY thread_id ORDER BY 1`, accountID)
}

func (s *stack) strings(query string, args ...any) []string {
	s.t.Helper()
	rows, err := s.pool.Query(context.Background(), query, args...)
	if err != nil {
		s.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		s.t.Fatal(err)
	}
	return out
}

// inbox lists the Inbox of an account as GET /threads?labelId=INBOX shows
// it, with label ids replaced by names.
func (s *stack) inbox(userID, accountID int64) []string {
	s.t.Helper()
	ctx := context.Background()
	inboxID := s.id(`SELECT id FROM labels WHERE account_id = $1 AND system_key = 'INBOX'`, accountID)
	page, err := s.view.ListThreads(ctx, mailview.ListParams{UserID: userID, AccountID: accountID, LabelID: inboxID, Limit: 100})
	if err != nil {
		s.t.Fatal(err)
	}
	var out []string
	for _, th := range page.Threads {
		names := slices.Clone(th.SystemLabels)
		for _, id := range th.LabelIDs {
			var name string
			if err := s.pool.QueryRow(ctx, `SELECT name FROM labels WHERE id = $1`, id).Scan(&name); err != nil {
				s.t.Fatal(err)
			}
			names = append(names, name)
		}
		sort.Strings(names)
		out = append(out, fmt.Sprintf("%s|%s|%d|%s", th.Subject, th.LastMessageAt.UTC().Format(time.RFC3339), th.MessageCount, strings.Join(names, ",")))
	}
	return out
}

// imapUser is a direct IMAP session to a test mailbox.
func imapUser(t *testing.T, addr, user, pass string) *imapclient.Client {
	t.Helper()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cl, err := imapx.Dial(ctx, host, port, imapx.TLSNone, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	if err := cl.Login(ctx, user, pass); err != nil {
		t.Fatal(err)
	}
	return cl.IMAP()
}

// appendMsg APPENDs a plain message; inReplyTo (optional) also becomes
// References. age dates it in the past (deterministic INTERNALDATE).
func appendMsg(t *testing.T, c *imapclient.Client, folder, msgID, subject, inReplyTo string, age time.Duration) {
	t.Helper()
	date := time.Now().Add(-age).Truncate(time.Second)
	hdr := fmt.Sprintf("From: Ala <ala@example.com>\r\nTo: silo@silo.test\r\nSubject: %s\r\nMessage-ID: <%s>\r\nDate: %s\r\n",
		subject, msgID, date.Format(time.RFC1123Z))
	if inReplyTo != "" {
		hdr += fmt.Sprintf("In-Reply-To: <%s>\r\nReferences: <%s>\r\n", inReplyTo, inReplyTo)
	}
	raw := []byte(hdr + "MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nBody of " + subject + "\r\n")
	cmd := c.Append(folder, int64(len(raw)), &imap.AppendOptions{Time: date})
	if _, err := cmd.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.Wait(); err != nil {
		t.Fatalf("APPEND %s: %v", folder, err)
	}
}

// labelLocal adds a user label without keyword (local_only) to messages,
// as the only record of the assignment (server without keywords).
func (s *stack) labelLocal(userID, accountID int64, name string, msgIDs ...int64) {
	s.t.Helper()
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var id int64
		if err := tx.QueryRow(ctx, `
INSERT INTO labels (user_id, account_id, name, type, keyword_state, color_bg) VALUES ($1, $2, $3, 'user', 'local_only', '#00ff00')
ON CONFLICT DO NOTHING RETURNING id`, userID, accountID, name).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO message_labels (message_id, label_id, user_id) SELECT unnest($1::bigint[]), $2, $3`,
			msgIDs, id, userID); err != nil {
			return err
		}
		_, err := mail.RefreshMessages(ctx, tx, accountID, msgIDs, "test")
		return err
	})
	if err != nil {
		s.t.Fatal(err)
	}
}

// labelMirrored creates a user label through the API path and puts it on
// messages (keyword written to the server by the write-back).
func (s *stack) labelMirrored(userID, accountID int64, name string, msgIDs ...int64) int64 {
	s.t.Helper()
	ctx := context.Background()
	l, err := s.ops.CreateLabel(ctx, ops.CreateLabel{UserID: userID, AccountID: accountID, Name: name, Type: "user"})
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.ops.ModifyLabels(ctx, ops.Modify{UserID: userID, MessageIDs: msgIDs, Add: []ops.LabelRef{{ID: l.ID}}}); err != nil {
		s.t.Fatal(err)
	}
	return l.ID
}

// snooze snoozes the thread of a message directly in the tables (the snooze
// API belongs to phase 4).
func (s *stack) snooze(userID, accountID, msgID int64, wake time.Time) {
	s.t.Helper()
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var thread, id int64
		if err := tx.QueryRow(ctx, `SELECT thread_id FROM messages WHERE id = $1`, msgID).Scan(&thread); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO snoozes (user_id, account_id, thread_id, wake_at) VALUES ($1, $2, $3, $4) RETURNING id`,
			userID, accountID, thread, wake).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO snooze_items (snooze_id, message_id, source_folder_id)
SELECT $1, $2, (SELECT f.id FROM mailbox_folders f WHERE f.account_id = $3 AND f.role = 'inbox')`, id, msgID, accountID); err != nil {
			return err
		}
		return mail.RecomputeThread(ctx, tx, thread)
	})
	if err != nil {
		s.t.Fatal(err)
	}
}

// snoozes lists user|wake_at|Message-IDs of every snooze.
func (s *stack) snoozes() []string {
	s.t.Helper()
	return s.strings(`
SELECT u.email::text || '|' || to_char(sn.wake_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS') || '|' ||
       string_agg(m.message_id_hdr, ',' ORDER BY m.message_id_hdr) || '|' || COALESCE(max(f.name), '') || '|' ||
       (t.snoozed_until = sn.wake_at)::text
FROM snoozes sn JOIN users u ON u.id = sn.user_id JOIN snooze_items si ON si.snooze_id = sn.id
JOIN messages m ON m.id = si.message_id JOIN threads t ON t.id = sn.thread_id
LEFT JOIN mailbox_folders f ON f.id = si.source_folder_id
GROUP BY u.email, sn.id, t.snoozed_until ORDER BY 1`)
}
