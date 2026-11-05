// Package mailview is the read side of the mailbox API: account resolution,
// the thread list (keyset pagination, plan 4.7), the thread with its
// messages, and the content of messages (sanitised HTML, raw .eml, attachment
// bytes) with on-demand download of bodies that are not in the mirror yet.
//
// It only reads; writes stay in internal/mail (labels, threads) and
// internal/ops (operations).
package mailview

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/blob"
	"github.com/silopost/silo-server/internal/ops"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

// Errors of the package.
var (
	// ErrNotFound: unknown id, or an object of another user (plan 5.3: 404).
	ErrNotFound = errors.New("mailview: not found")
	// ErrInvalid: malformed cursor or parameters.
	ErrInvalid = errors.New("mailview: invalid request")
	// ErrContentNotAvailable: the body is not in the mirror and the mail
	// server could not deliver it in time (503 content_not_available).
	ErrContentNotAvailable = errors.New("mailview: content not available")
)

// BodyFetcher downloads a message body from the mail server on demand;
// *sync.Manager implements it.
type BodyFetcher interface {
	FetchBody(ctx context.Context, accountID, messageID int64) error
}

// Default and maximum page size of the thread list.
const (
	DefaultLimit = 50
	MaxLimit     = 100
)

// View reads the mailbox of users.
type View struct {
	pool    *pgxpool.Pool
	q       *sqlcgen.Queries
	blobs   blob.BlobStore
	fetcher BodyFetcher
	// FetchWait bounds the wait for an on-demand download (plan 5.1: 10 s).
	FetchWait time.Duration
	// SearchTimeout is the statement_timeout of a search (plan 3.12: 3 s).
	SearchTimeout time.Duration
	// thumbsGenerated counts thumbnails decoded and encoded (not served
	// from the cache).
	thumbsGenerated atomic.Int64
}

// New returns a View. blobs and fetcher may be nil (content endpoints then
// answer from the mirror only).
func New(pool *pgxpool.Pool, blobs blob.BlobStore, fetcher BodyFetcher) *View {
	return &View{pool: pool, q: sqlcgen.New(pool), blobs: blobs, fetcher: fetcher, FetchWait: 10 * time.Second, SearchTimeout: DefaultSearchTimeout}
}

// ResolveAccount returns accountID when it belongs to the user, or the active
// account (users.settings.active_account_id, else the first account) when
// accountID is 0.
func (v *View) ResolveAccount(ctx context.Context, userID, accountID int64) (int64, error) {
	if accountID != 0 {
		var ok bool
		if err := v.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM accounts WHERE id = $1 AND user_id = $2)`, accountID, userID).Scan(&ok); err != nil {
			return 0, err
		}
		if !ok {
			return 0, ErrNotFound
		}
		return accountID, nil
	}
	var id int64
	err := v.pool.QueryRow(ctx, `
SELECT a.id FROM accounts a JOIN users u ON u.id = a.user_id
WHERE u.id = $1
ORDER BY (a.id = NULLIF(u.settings->>'active_account_id', '')::bigint) DESC, a.position, a.id LIMIT 1`, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// labelKeys maps the label ids of an account to their system key ("" for user
// and folder labels).
func (v *View) labelKeys(ctx context.Context, db interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, accountID int64) (map[int64]string, error) {
	rows, err := db.Query(ctx, `SELECT id, COALESCE(system_key, '') FROM labels WHERE account_id = $1`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, err
		}
		out[id] = key
	}
	return out, rows.Err()
}

// ListParams selects a page of threads.
type ListParams struct {
	UserID    int64
	AccountID int64
	// LabelID is the label to list; 0 lists "All mail" of the account.
	LabelID int64
	// Query is a search query (internal/search); with one, the page lists
	// the threads with a matching message (LabelID narrows it like in:).
	Query  string
	Cursor string
	Limit  int
}

// ThreadPage is one page of the list.
type ThreadPage struct {
	Threads            []ops.Thread
	NextCursor         *string
	ResultSizeEstimate int
	// Approximate: the search hit its time limit; the page is empty.
	Approximate bool
	// IndexingDone is set for a search: the share of messages whose body is
	// indexed (IndexingDone).
	IndexingDone *float64
}

type threadRow struct {
	ID, AccountID  int64
	Subject        string
	Snippet        string
	LastMessageAt  time.Time
	MessageCount   int
	UnreadCount    int
	HasAttachments bool
	Participants   []byte
	SnoozedUntil   *time.Time
	PendingOps     int
	SortAt         time.Time
	LabelIDs       []int64
}

func encodeCursor(sortAt time.Time, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(sortAt.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(id, 10)))
}

// decodeCursor returns the position after which the next page starts; an
// empty cursor is the position before the first row.
func decodeCursor(c string) (time.Time, int64, bool, error) {
	if c == "" {
		return time.Time{}, 0, true, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("%w: bad cursor", ErrInvalid)
	}
	ts, ids, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, 0, false, fmt.Errorf("%w: bad cursor", ErrInvalid)
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	id, err2 := strconv.ParseInt(ids, 10, 64)
	if err != nil || err2 != nil || id <= 0 {
		return time.Time{}, 0, false, fmt.Errorf("%w: bad cursor", ErrInvalid)
	}
	return t, id, false, nil
}

func tstz(t time.Time, first bool) pgtype.Timestamptz {
	if first {
		return pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

const maxID = int64(1<<63 - 1)

// ListThreads returns a page of the label (or "All mail") ordered by
// sort_at DESC, id DESC, as the plan 4.7 query does.
func (v *View) ListThreads(ctx context.Context, p ListParams) (*ThreadPage, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	if strings.TrimSpace(p.Query) != "" {
		return v.searchList(ctx, p, limit)
	}
	at, id, first, err := decodeCursor(p.Cursor)
	if err != nil {
		return nil, err
	}
	curID := id
	if first {
		curID = maxID
	}
	var rows []threadRow
	switch {
	case p.LabelID != 0:
		var rs []sqlcgen.ListThreadsByLabelRow
		rs, err = v.q.ListThreadsByLabel(ctx, sqlcgen.ListThreadsByLabelParams{
			UserID: p.UserID, LabelID: p.LabelID, CursorSortAt: tstz(at, first), CursorThreadID: curID, PageLimit: int32(limit + 1),
		})
		for _, r := range rs {
			rows = append(rows, threadRow{
				ID: r.ID, AccountID: r.AccountID, Subject: r.Subject, Snippet: r.Snippet, LastMessageAt: r.LastMessageAt.Time,
				MessageCount: int(r.MessageCount), UnreadCount: int(r.UnreadCount), HasAttachments: r.HasAttachments,
				Participants: r.Participants, SnoozedUntil: ptrTime(r.SnoozedUntil), PendingOps: int(r.PendingOps),
				SortAt: r.SortAt.Time, LabelIDs: r.LabelIds,
			})
		}
	default:
		var rs []sqlcgen.ListThreadsAllRow
		rs, err = v.q.ListThreadsAll(ctx, sqlcgen.ListThreadsAllParams{
			AccountID: p.AccountID, UserID: p.UserID, CursorSortAt: tstz(at, first), CursorThreadID: curID, PageLimit: int32(limit + 1),
		})
		for _, r := range rs {
			rows = append(rows, threadRow{
				ID: r.ID, AccountID: r.AccountID, Subject: r.Subject, Snippet: r.Snippet, LastMessageAt: r.LastMessageAt.Time,
				MessageCount: int(r.MessageCount), UnreadCount: int(r.UnreadCount), HasAttachments: r.HasAttachments,
				Participants: r.Participants, SnoozedUntil: ptrTime(r.SnoozedUntil), PendingOps: int(r.PendingOps),
				SortAt: r.SortAt.Time, LabelIDs: r.LabelIds,
			})
		}
	}
	if err != nil {
		return nil, err
	}
	page := &ThreadPage{Threads: []ops.Thread{}}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		c := encodeCursor(last.SortAt, last.ID)
		page.NextCursor = &c
	}
	keys, err := v.labelKeys(ctx, v.pool, p.AccountID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		page.Threads = append(page.Threads, threadOf(r, keys))
	}
	if err := ops.FillThreadExtras(ctx, v.pool, page.Threads); err != nil {
		return nil, err
	}
	page.ResultSizeEstimate, err = v.estimate(ctx, p)
	if err != nil {
		return nil, err
	}
	return page, nil
}

// estimate is the number of threads of the view: the exact count of the label
// (an index-only scan), the thread count of the account for "All mail".
func (v *View) estimate(ctx context.Context, p ListParams) (int, error) {
	var n int
	if p.LabelID != 0 {
		err := v.pool.QueryRow(ctx, `SELECT count(*)::int FROM thread_labels WHERE user_id = $1 AND label_id = $2 AND NOT hidden`, p.UserID, p.LabelID).Scan(&n)
		return n, err
	}
	err := v.pool.QueryRow(ctx, `SELECT count(*)::int FROM threads WHERE account_id = $1 AND user_id = $2`, p.AccountID, p.UserID).Scan(&n)
	return n, err
}

func ptrTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

func threadOf(r threadRow, keys map[int64]string) ops.Thread {
	t := ops.Thread{
		ID: r.ID, AccountID: r.AccountID, Subject: r.Subject, Snippet: r.Snippet, LastMessageAt: r.LastMessageAt,
		MessageCount: r.MessageCount, UnreadCount: r.UnreadCount, HasAttachments: r.HasAttachments,
		SnoozedUntil: r.SnoozedUntil, PendingSync: r.PendingOps > 0,
		SystemLabels: []string{}, LabelIDs: []int64{}, Participants: []ops.Participant{},
	}
	for _, id := range r.LabelIDs {
		if key := keys[id]; key != "" {
			t.SystemLabels = append(t.SystemLabels, key)
		} else {
			t.LabelIDs = append(t.LabelIDs, id)
		}
	}
	if r.UnreadCount > 0 {
		t.SystemLabels = append(t.SystemLabels, ops.LabelUnread)
	}
	_ = json.Unmarshal(r.Participants, &t.Participants)
	return t
}

// UnreadThreads counts the threads of the account with unread mail outside
// Trash and Spam (the counter of the pseudo label UNREAD).
func (v *View) UnreadThreads(ctx context.Context, userID, accountID int64) (int, error) {
	var n int
	err := v.pool.QueryRow(ctx, `
SELECT count(*)::int FROM threads t
WHERE t.account_id = $1 AND t.user_id = $2 AND t.unread_count > 0
  AND EXISTS (SELECT 1 FROM thread_labels x JOIN labels l ON l.id = x.label_id WHERE x.thread_id = t.id AND NOT x.hidden
              AND l.system_key IS DISTINCT FROM 'TRASH' AND l.system_key IS DISTINCT FROM 'SPAM')`, accountID, userID).Scan(&n)
	return n, err
}
