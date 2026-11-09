// Package ops is the local side of the write-back (plan 3.6): it turns label
// changes in the Gmail model (ModifyLabels, plan 4.4) into imap_op jobs, in
// the same transaction as the visible change, with the undo window, per
// message ordering and coalescing. The sync engine executes the jobs on the
// account's ops connection (internal/sync).
//
// Representation: a job's payload is the operation (mail.OpPayload: op,
// target, flags); its messages are rows of op_messages with the folder each
// message was in (expectFrom). mail.DeriveSystemLabels overlays the pending
// operations of a message on its server state, so the visible state is
// "server state + pending operations" and converges by itself when a job is
// executed, cancelled (undo = DELETE jobs WHERE group_id) or dies.
package ops

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// JobKind is the kind of write-back jobs (account jobs, run by the account's
// worker only).
const JobKind = "imap_op"

// MaxAttempts of an imap_op job: plan 3.6 retries a NO three times.
const MaxAttempts = 3

// Errors of the package.
var (
	ErrNotFound      = errors.New("ops: not found")
	ErrInvalid       = errors.New("ops: invalid request")
	ErrNotModifiable = errors.New("ops: label cannot be modified directly")
	ErrTooLate       = errors.New("ops: operation already sent to the server")
)

// LabelUnread is the pseudo label of the API for is_unread.
const LabelUnread = "UNREAD"

// LabelRef names a label in a modify request: a system key (INBOX, STARRED,
// ..., or the pseudo label UNREAD) or the id of a user/folder label.
type LabelRef struct {
	System string
	ID     int64
}

// Service runs the operations; it never talks to IMAP.
type Service struct {
	pool       *pgxpool.Pool
	undoWindow time.Duration
}

// NewService returns a Service. undoWindow is SILO_UNDO_WINDOW: the delay of
// folder operations.
func NewService(pool *pgxpool.Pool, undoWindow time.Duration) *Service {
	return &Service{pool: pool, undoWindow: undoWindow}
}

// Result is the answer to a modify, trash, untrash or delete.
type Result struct {
	// OperationID is the group_id of the jobs ("" when nothing had to be sent).
	OperationID string
	// UndoUntil is the end of the undo window of folder operations.
	UndoUntil *time.Time
	ThreadIDs []int64
	Threads   []Thread
}

func (s *Service) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, fn)
}
