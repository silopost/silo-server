// Package filters keeps the user's filters (plan 3.11) and runs them: the
// criteria are a query in the search syntax (internal/search, the same AST as
// the search box and its SQL builder); the actions are label changes run as
// write-back operations (internal/ops) without the undo window, and a
// forward through the compose path (internal/drafts).
//
// Two jobs run filters on the server only (plan 2.6):
//   - filter_eval: queued by ingest for new INBOX messages received after the
//     account went live (accounts.filters_since); the enabled filters of the
//     account are evaluated in memory (search.Matcher) in position order and
//     the actions of every matching filter are applied together;
//   - filter_apply: POST /filters/{id}/apply, the same AST as SQL
//     (search.ToSQL) over the existing messages of the account, in batches.
package filters

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/ops"
	"github.com/silopost/silo-server/internal/search"
)

// Limits of a filter.
const (
	MaxNameLength     = 100
	MaxCriteriaLength = 1000
	MaxLabels         = 20
	// MaxFilters is the number of filters an account may have.
	MaxFilters = 500
)

// Errors of the package.
var (
	ErrNotFound = errors.New("filters: not found")
	// ErrInvalid: the filter breaks a rule (no action, archive with trash,
	// a label that is not a user label of the account, ...).
	ErrInvalid = errors.New("filters: invalid filter")
	// ErrInvalidLabel: addLabelIds names a label that is not a user label of
	// the filter's account.
	ErrInvalidLabel = errors.New("filters: addLabelIds must name user labels of the account")
)

// Actions are what a filter does with a matching message (plan 3.11).
type Actions struct {
	// Archive: skip the inbox (-INBOX, a move to Archive).
	Archive bool `json:"archive,omitempty"`
	// MarkRead: \Seen.
	MarkRead bool `json:"markRead,omitempty"`
	// Star: \Flagged.
	Star bool `json:"star,omitempty"`
	// Important: the IMPORTANT label ($Important).
	Important bool `json:"important,omitempty"`
	// AddLabelIDs are user labels of the account (keywords on the server).
	AddLabelIDs []int64 `json:"addLabelIds,omitempty"`
	// Trash: move to Trash.
	Trash bool `json:"trash,omitempty"`
	// ForwardTo: send a copy to this address ("" none).
	ForwardTo string `json:"forwardTo,omitempty"`
}

// Empty reports whether no action is set.
func (a Actions) Empty() bool {
	return !a.Archive && !a.MarkRead && !a.Star && !a.Important && len(a.AddLabelIDs) == 0 && !a.Trash && a.ForwardTo == ""
}

// merge adds the actions of b (filters matching the same message act
// together, as in Gmail; trash wins over archive).
func (a Actions) merge(b Actions) Actions {
	a.Archive, a.MarkRead, a.Star = a.Archive || b.Archive, a.MarkRead || b.MarkRead, a.Star || b.Star
	a.Important, a.Trash = a.Important || b.Important, a.Trash || b.Trash
	for _, id := range b.AddLabelIDs {
		if !slices.Contains(a.AddLabelIDs, id) {
			a.AddLabelIDs = append(a.AddLabelIDs, id)
		}
	}
	if a.Trash {
		a.Archive = false
	}
	return a
}

// labelChange is the change of the actions in the Gmail label model.
func (a Actions) labelChange() (add, remove []ops.LabelRef) {
	if a.Archive && !a.Trash {
		remove = append(remove, ops.LabelRef{System: "INBOX"})
	}
	if a.MarkRead {
		remove = append(remove, ops.LabelRef{System: ops.LabelUnread})
	}
	if a.Star {
		add = append(add, ops.LabelRef{System: "STARRED"})
	}
	if a.Important {
		add = append(add, ops.LabelRef{System: "IMPORTANT"})
	}
	for _, id := range a.AddLabelIDs {
		add = append(add, ops.LabelRef{ID: id})
	}
	if a.Trash {
		add = append(add, ops.LabelRef{System: "TRASH"})
	}
	return add, remove
}

// Filter is a stored filter.
type Filter struct {
	ID, UserID, AccountID int64
	Name                  string
	// Criteria is the query as the user wrote it.
	Criteria  string
	Actions   Actions
	Enabled   bool
	Position  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Normalized is the criteria as the parser understood them (AST.String()).
func (f Filter) Normalized() string {
	a, err := search.Parse(f.Criteria)
	if err != nil {
		return ""
	}
	return a.String()
}

// Input is a filter to create or the full replacement of one (PUT).
type Input struct {
	AccountID int64 // create only; PUT keeps the account
	Name      string
	Criteria  string
	Actions   Actions
	Enabled   bool
	// Position is the 0-based place in the account's list; nil appends (create)
	// or keeps the place (PUT). Others shift to make room.
	Position *int
}

// BodyFetcher downloads the body of a message on demand (sync.Manager).
type BodyFetcher interface {
	FetchBody(ctx context.Context, accountID, messageID int64) error
}

// Forwarder sends a copy of a message to an address (drafts.Service).
type Forwarder interface {
	Forward(ctx context.Context, userID, messageID int64, to, origin string) (int64, error)
}

// Options configure a Service.
type Options struct {
	Pool *pgxpool.Pool
	Ops  *ops.Service
	// Bodies fetches bodies of new messages a filter needs (nil: evaluate on
	// what is there).
	Bodies BodyFetcher
	// Forward runs forwardTo (nil: forwardTo is refused as unsupported).
	Forward Forwarder
	// BodyWait is how long filter_eval waits for the body of a new message
	// (plan 3.11: 10 s) before evaluating on the headers.
	BodyWait time.Duration
	// ApplyBatch is the number of messages per operation of filter_apply
	// (500).
	ApplyBatch int
	Logger     *slog.Logger
}

// Service manages and runs filters.
type Service struct {
	o   Options
	log *slog.Logger
}

// DefaultBodyWait is BodyWait when not set.
const DefaultBodyWait = 10 * time.Second

// DefaultApplyBatch is ApplyBatch when not set.
const DefaultApplyBatch = 500

// New returns a Service.
func New(o Options) *Service {
	if o.BodyWait <= 0 {
		o.BodyWait = DefaultBodyWait
	}
	if o.ApplyBatch <= 0 {
		o.ApplyBatch = DefaultApplyBatch
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{o: o, log: log}
}

// CanForward reports whether forwardTo is supported.
func (s *Service) CanForward() bool { return s.o.Forward != nil }

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// ParseCriteria parses the criteria of a filter: a query of the search
// syntax (search.ErrInvalidQuery as in the search box) that restricts
// something (not empty, not only in:anywhere).
func ParseCriteria(q string) (search.AST, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return search.AST{}, invalid("criteria are empty")
	}
	if utf8.RuneCountInString(q) > MaxCriteriaLength {
		return search.AST{}, invalid("criteria are longer than %d characters", MaxCriteriaLength)
	}
	a, err := search.Parse(q)
	if err != nil {
		return a, err
	}
	if a.Empty() {
		return a, invalid("criteria match every message")
	}
	if t, ok := a.Root.(search.Term); ok && t.Key == "in" && strings.EqualFold(t.Value, "anywhere") {
		return a, invalid("criteria match every message")
	}
	return a, nil
}

// validate checks a filter against the rules of plan 3.11 and the account
// (labels); it normalizes in.
func (s *Service) validate(ctx context.Context, tx pgx.Tx, userID, accountID int64, in *Input) error {
	in.Name = strings.TrimSpace(in.Name)
	if utf8.RuneCountInString(in.Name) > MaxNameLength {
		return invalid("name is longer than %d characters", MaxNameLength)
	}
	in.Criteria = strings.TrimSpace(in.Criteria)
	if _, err := ParseCriteria(in.Criteria); err != nil {
		return err
	}
	a := &in.Actions
	if a.Empty() {
		return invalid("a filter needs at least one action")
	}
	if a.Archive && a.Trash {
		return invalid("archive and trash cannot be combined")
	}
	if a.ForwardTo != "" {
		if s.o.Forward == nil {
			return ErrUnsupported
		}
		addr, err := mail.ParseAddress(strings.TrimSpace(a.ForwardTo))
		if err != nil || addr.Name != "" || strings.ContainsAny(addr.Address, " <>,;\"\r\n") {
			return invalid("forwardTo is not an e-mail address")
		}
		a.ForwardTo = strings.ToLower(addr.Address)
	}
	slices.Sort(a.AddLabelIDs)
	a.AddLabelIDs = slices.Compact(a.AddLabelIDs)
	if len(a.AddLabelIDs) > MaxLabels {
		return invalid("at most %d labels", MaxLabels)
	}
	if len(a.AddLabelIDs) > 0 {
		var n int
		if err := tx.QueryRow(ctx, `
SELECT count(*) FROM labels WHERE id = ANY($1) AND user_id = $2 AND account_id = $3 AND type = 'user'`,
			a.AddLabelIDs, userID, accountID).Scan(&n); err != nil {
			return err
		}
		if n != len(a.AddLabelIDs) {
			return ErrInvalidLabel
		}
	}
	return nil
}

// ErrUnsupported is returned for forwardTo without a forwarder (never in silo-server).
var ErrUnsupported = errors.New("filters: action not supported")

const filterCols = `id, user_id, account_id, name, criteria, actions, enabled, position, created_at, updated_at`

func scanFilter(row pgx.Row) (*Filter, error) {
	f := &Filter{}
	var actions Actions
	err := row.Scan(&f.ID, &f.UserID, &f.AccountID, &f.Name, &f.Criteria, &actions, &f.Enabled, &f.Position, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f.Actions = actions
	return f, nil
}

// List returns the filters of an account in position order.
func (s *Service) List(ctx context.Context, userID, accountID int64) ([]Filter, error) {
	return list(ctx, s.o.Pool, userID, accountID, false)
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func list(ctx context.Context, db querier, userID, accountID int64, enabledOnly bool) ([]Filter, error) {
	rows, err := db.Query(ctx, `SELECT `+filterCols+` FROM filters WHERE user_id = $1 AND account_id = $2 AND (enabled OR NOT $3)
ORDER BY position, id`, userID, accountID, enabledOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Filter{}
	for rows.Next() {
		f, err := scanFilter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// Get returns one filter of the user.
func (s *Service) Get(ctx context.Context, userID, id int64) (*Filter, error) {
	return scanFilter(s.o.Pool.QueryRow(ctx, `SELECT `+filterCols+` FROM filters WHERE id = $1 AND user_id = $2`, id, userID))
}

// lockAccount serializes changes to the filter list of an account and checks
// that the account is the user's.
func lockAccount(ctx context.Context, tx pgx.Tx, userID, accountID int64) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM accounts WHERE id = $1 AND user_id = $2 FOR UPDATE`, accountID, userID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// place moves filter id to position pos (clamped) of the account's list and
// renumbers the list 0..n-1.
func place(ctx context.Context, tx pgx.Tx, accountID, id int64, pos *int) error {
	rows, err := tx.Query(ctx, `SELECT id FROM filters WHERE account_id = $1 ORDER BY position, id`, accountID)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	if pos != nil {
		i := slices.Index(ids, id)
		ids = slices.Delete(ids, i, i+1)
		p := min(max(*pos, 0), len(ids))
		ids = slices.Insert(ids, p, id)
	}
	_, err = tx.Exec(ctx, `
UPDATE filters f SET position = o.n - 1 FROM unnest($1::bigint[]) WITH ORDINALITY AS o(id, n)
WHERE f.id = o.id AND f.position <> o.n - 1`, ids)
	return err
}

// Create stores a new filter (at the end of the list unless Position).
func (s *Service) Create(ctx context.Context, userID int64, in Input) (*Filter, error) {
	var id int64
	err := pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		if err := lockAccount(ctx, tx, userID, in.AccountID); err != nil {
			return err
		}
		if err := s.validate(ctx, tx, userID, in.AccountID, &in); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM filters WHERE account_id = $1`, in.AccountID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxFilters {
			return invalid("an account can have at most %d filters", MaxFilters)
		}
		if err := tx.QueryRow(ctx, `
INSERT INTO filters (user_id, account_id, name, criteria, actions, enabled, position)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`, userID, in.AccountID, in.Name, in.Criteria, in.Actions, in.Enabled, n).Scan(&id); err != nil {
			return err
		}
		return place(ctx, tx, in.AccountID, id, in.Position)
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, userID, id)
}

// Update replaces a filter (PUT); the account stays.
func (s *Service) Update(ctx context.Context, userID, id int64, in Input) (*Filter, error) {
	f, err := s.Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	err = pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		if err := lockAccount(ctx, tx, userID, f.AccountID); err != nil {
			return err
		}
		if err := s.validate(ctx, tx, userID, f.AccountID, &in); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
UPDATE filters SET name = $3, criteria = $4, actions = $5, enabled = $6, updated_at = now()
WHERE id = $1 AND user_id = $2`, id, userID, in.Name, in.Criteria, in.Actions, in.Enabled)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return place(ctx, tx, f.AccountID, id, in.Position)
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, userID, id)
}

// Delete removes a filter; the others close the gap.
func (s *Service) Delete(ctx context.Context, userID, id int64) error {
	f, err := s.Get(ctx, userID, id)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		if err := lockAccount(ctx, tx, userID, f.AccountID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM filters WHERE id = $1 AND user_id = $2`, id, userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return place(ctx, tx, f.AccountID, 0, nil)
	})
}
