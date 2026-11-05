// Package sync is the IMAP synchronisation engine (plan 3.5): it keeps the
// Postgres mirror of every active account in step with its IMAP server.
//
// Structure:
//
//   - Manager: one per process. Registry of AccountWorkers, started for every
//     account that is not disabled when serve starts (and for new accounts as
//     soon as a job of theirs is inserted), stopped when the account is
//     disabled or deleted. It registers the account.* and orphan_gc job
//     handlers; jobs only wake workers (see Manager).
//   - AccountWorker: the supervisor goroutine of one account. It runs the
//     state machine connecting -> discovering -> initial_sync -> live ->
//     backoff (plan 3.5.2), the account's job loop, and a session while
//     connected.
//   - session: the IMAP connections of one connected period. The "sync"
//     connection is owned by the supervisor goroutine, which runs a small
//     scheduler (incremental work > new INBOX bodies > header backfill > body
//     backfill); the "idle" connection runs on its own goroutine, keeps
//     SELECT INBOX + IDLE and only signals "INBOX changed" (ADR 0006: the
//     unilateral FETCH has no UID).
//
// There is no QRESYNC (ADR 0006): incremental sync uses SELECT (CONDSTORE) +
// UID FETCH CHANGEDSINCE + UID SEARCH ALL set difference, or full flag
// comparison without CONDSTORE.
//
// All database writes go through internal/mail (messages, locations, labels,
// threads, events); this package writes mailbox_folders, sync_cursors and the
// sync columns of accounts.
package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/blob"
	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/worker"
)

// Account sync states (accounts.sync_state, plan 3.5.2). "syncing" from the
// plan diagram is not persisted: it is the live state with work in progress.
const (
	StateConnecting  = "connecting"
	StateDiscovering = "discovering"
	StateInitialSync = "initial_sync"
	StateLive        = "live"
	StateBackoff     = "backoff"
	StateAuthFailed  = "auth_failed"
	StateDisabled    = "disabled"
)

// Event types written by the engine (plan 5.4).
const (
	EventAccountStatus   = "account.status"
	EventAccountError    = "account.error"
	EventAccountProgress = "account.sync_progress"
)

// Error codes of account.error.
const (
	ErrCodeAuthFailed  = "auth_failed"
	ErrCodeTLS         = "tls_error"
	ErrCodeUnreachable = "unreachable"
	ErrCodeQuota       = "quota"
)

// Job kinds handled by the engine. account.initial_sync and account.reconnect
// are enqueued by internal/auth and internal/accounts.
const (
	JobInitialSync = auth.JobAccountInitialSync
	JobReconnect   = auth.JobAccountReconnect
	JobResync      = "account.resync"
	JobOrphanGC    = "orphan_gc"
)

// Resync modes of POST /accounts/{id}/resync.
const (
	ResyncIncremental = "incremental"
	ResyncFull        = "full"
	ResyncRebuild     = "rebuild"
)

// Tunables from the plan that are not configurable.
const (
	headerBatch      = 200                 // UID FETCH batch of phases A/B and new messages
	flagsBatch       = 1000                // FETCH FLAGS batch without CONDSTORE
	recentWindow     = 30 * 24 * time.Hour // phase A
	inboxPollNoIdle  = 60 * time.Second    // INBOX polling without IDLE
	fullFlagsMaxAge  = 15 * time.Minute    // no CONDSTORE: skip unchanged folders for this long
	bigFolder        = 50000               // no CONDSTORE: above this, full flag compare hourly
	bigFolderWindow  = 5000                // ... and the newest UIDs on every tick
	bigFolderFullAge = time.Hour
	commandTimeout   = 60 * time.Second
	progressEvery    = time.Second
	orphanMinAge     = 24 * time.Hour
)

// DefaultKeywordFolderLimit: a Dovecot maildir folder keeps 26 keywords (letters
// a-z in the file names); Dovecot 2.4 accepts more without an error but keeps
// them only in its index, so they are lost when the index is rebuilt.
const DefaultKeywordFolderLimit = 26

// DefaultBackoff is the reconnect schedule of plan 3.5.2 (jitter is added).
var DefaultBackoff = []time.Duration{5 * time.Second, 15 * time.Second, time.Minute, 5 * time.Minute}

// Settings are the engine parameters from internal/config.
type Settings struct {
	MaxConnections     int           // SILO_SYNC_MAX_CONNECTIONS_PER_ACCOUNT (>= 2: sync + idle)
	InitialConcurrency int           // SILO_SYNC_INITIAL_CONCURRENCY
	PollInterval       time.Duration // SILO_SYNC_POLL_INTERVAL
	BackfillRate       int64         // SILO_SYNC_BACKFILL_RATE, bytes per second
	MaxMessageBytes    int64         // SILO_MIRROR_MAX_MESSAGE_BYTES
	QuotaPerAccount    int64         // SILO_MIRROR_QUOTA_PER_ACCOUNT, 0 = unlimited
	InsecureTLS        bool          // SILO_ALLOW_INSECURE_TRANSPORT: skip certificate checks
	// KeywordFolderLimit is the number of distinct keywords a folder can
	// keep (SILO_SYNC_KEYWORD_FOLDER_LIMIT; 0 = default 26, the Dovecot maildir
	// limit; < 0 = no limit). A label keyword that would exceed it is not
	// stored: the label is local in that folder (plan 3.7).
	KeywordFolderLimit int
}

func (s Settings) withDefaults() Settings {
	if s.MaxConnections < 1 {
		s.MaxConnections = 3
	}
	if s.InitialConcurrency < 1 {
		s.InitialConcurrency = 2
	}
	if s.PollInterval <= 0 {
		s.PollInterval = 5 * time.Minute
	}
	if s.BackfillRate <= 0 {
		s.BackfillRate = 5 << 20
	}
	if s.MaxMessageBytes <= 0 {
		s.MaxMessageBytes = 50 << 20
	}
	if s.KeywordFolderLimit == 0 {
		s.KeywordFolderLimit = DefaultKeywordFolderLimit
	}
	return s
}

// Hooks are test seams; all optional.
type Hooks struct {
	// AfterBatch is called after every committed batch of the initial sync
	// (phase "headers_recent", "headers" or "bodies") with the batch size.
	AfterBatch func(phase string, n int)
	// Now replaces time.Now for the phase A window and the orphan gc.
	Now func() time.Time
	// StoreError, when it returns an error, replaces the server's answer to
	// UID STORE +FLAGS of keywords in a folder (a NO the test server never
	// sends).
	StoreError func(folder string, keywords []string) error
}

// Deps are the dependencies shared by all workers.
type Deps struct {
	Pool     *pgxpool.Pool
	Keys     *secrets.Keyring
	Blobs    blob.BlobStore
	Logger   *slog.Logger
	Settings Settings
	// Jobs runs the per-account job loop inside each AccountWorker; nil in
	// tests that drive workers directly.
	Jobs *worker.Worker
	// Backoff overrides DefaultBackoff (tests).
	Backoff []time.Duration
	Hooks   Hooks
}

func (d *Deps) now() time.Time {
	if d.Hooks.Now != nil {
		return d.Hooks.Now()
	}
	return time.Now()
}

func (d *Deps) log() *slog.Logger {
	if d.Logger == nil {
		return slog.Default()
	}
	return d.Logger
}

// inTx runs fn in a transaction. Database work is not interrupted by shutdown:
// a batch fetched from IMAP is committed (plan: graceful shutdown finishes the
// current transaction), with its own deadline.
func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	return pgx.BeginFunc(ctx, pool, fn)
}
