package sync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
)

// Errors of the on-demand operations.
var (
	// ErrUnavailable: the account has no connected session (server down, auth
	// failed, worker not running).
	ErrUnavailable = errors.New("sync: mail server not connected")
	// ErrNoContent: the message has no location on the server any more.
	ErrNoContent = errors.New("sync: message is not on the server")
)

// runRequest executes one request on connection c: a queued operation or an
// on-demand function.
func (s *session) runRequest(ctx context.Context, c *conn, r opRequest) error {
	if r.fn != nil {
		return r.fn(ctx, s, c)
	}
	return s.execOp(ctx, c, r.job)
}

// do runs fn on the account's ops connection (the shared sync connection when
// the pool has only two), ahead of write-back and sync work: this is the
// priority "content on demand" of plan 3.5.1. It waits for a connected session
// until ctx ends.
func (w *AccountWorker) do(ctx context.Context, fn func(ctx context.Context, s *session, c *conn) error) error {
	var s *session
	for {
		w.mu.Lock()
		s = w.live
		w.mu.Unlock()
		if s != nil {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	req := opRequest{fn: fn, done: make(chan error, 1)}
	select {
	case s.opsReq <- req:
	case <-s.ended:
		return ErrUnavailable
	case <-ctx.Done():
		return fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
	}
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
	}
}

// FetchBody downloads the full message (BODY.PEEK[]) of a message whose body
// is not in the mirror yet and stores it with mail.IngestBody. It returns
// when the content is stored, or with ErrUnavailable / ErrNoContent / the
// server's error. Used by GET /messages/{id}/html and /attachments/{id}.
func (m *Manager) FetchBody(ctx context.Context, accountID, messageID int64) error {
	w := m.Worker(accountID)
	if w == nil {
		return ErrUnavailable
	}
	return w.do(ctx, func(ctx context.Context, s *session, c *conn) error {
		return s.fetchBodyNow(ctx, c, messageID)
	})
}

func (s *session) fetchBodyNow(ctx context.Context, c *conn, messageID int64) error {
	defer func() { c.selected = "" }()
	var name string
	var uid int64
	var size int64
	err := s.d.Pool.QueryRow(ctx, `
SELECT f.name, l.uid, m.size_bytes FROM message_locations l
JOIN mailbox_folders f ON f.id = l.folder_id AND f.selectable
JOIN messages m ON m.id = l.message_id
WHERE l.message_id = $1 AND m.account_id = $2 AND NOT l.stale AND NOT (l.flags @> ARRAY['\Deleted'])
ORDER BY l.folder_id LIMIT 1`, messageID, s.acc.id).Scan(&name, &uid, &size)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %w", errOpPermanent, ErrNoContent)
	}
	if err != nil {
		return err
	}
	if err := c.ensure(ctx, name); err != nil {
		return err
	}
	bodies, err := c.fetchBodies(ctx, []uint32{uint32(uid)}, size)
	if err != nil {
		return err
	}
	raw, ok := bodies[uint32(uid)]
	if !ok {
		return fmt.Errorf("%w: %w", errOpPermanent, ErrNoContent)
	}
	return inTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if err := mail.LockAccount(ctx, tx, s.acc.id); err != nil {
			return err
		}
		_, err := mail.IngestBody(ctx, tx, s.d.Blobs, messageID, raw)
		return err
	})
}
