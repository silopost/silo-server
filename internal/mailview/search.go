package mailview

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/ops"
	"github.com/silopost/silo-server/internal/search"
)

// Search limits (plan 3.12).
const (
	// MaxSearchHits is the number of threads a search returns at most
	// (newest first).
	MaxSearchHits = 2000
	// DefaultSearchTimeout is the statement_timeout of a search; past it the
	// answer is empty and approximate.
	DefaultSearchTimeout = 3 * time.Second
	// MaxSearchHistory is the number of queries kept per account.
	MaxSearchHistory = 50
	// SuggestLimit is the number of suggestions of each kind.
	SuggestLimit = 5
)

// SearchScope is the search.Scope of an account: its labels (system keys and
// names), and the user's time zone (users.settings.timezone, UTC without
// one).
func (v *View) SearchScope(ctx context.Context, userID, accountID int64) (search.Scope, error) {
	return LoadSearchScope(ctx, v.pool, userID, accountID)
}

// LoadSearchScope is SearchScope over any connection (filters use it for
// the matcher and for POST /filters/{id}/apply).
func LoadSearchScope(ctx context.Context, db mail.DB, userID, accountID int64) (search.Scope, error) {
	s := search.Scope{UserID: userID, AccountID: accountID, SystemLabels: map[string]int64{}, Location: time.UTC}
	rows, err := db.Query(ctx, `SELECT id, name, COALESCE(system_key, '') FROM labels WHERE account_id = $1 AND user_id = $2`, accountID, userID)
	if err != nil {
		return s, err
	}
	names := map[string][]int64{}
	for rows.Next() {
		var id int64
		var name, key string
		if err := rows.Scan(&id, &name, &key); err != nil {
			rows.Close()
			return s, err
		}
		if key != "" {
			s.SystemLabels[key] = id
		}
		n := search.NormalizeLabel(name)
		names[n] = append(names[n], id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return s, err
	}
	s.ResolveLabel = func(name string) []int64 { return names[name] }
	var tz string
	if err := db.QueryRow(ctx, `SELECT COALESCE(settings->>'timezone', '') FROM users WHERE id = $1`, userID).Scan(&tz); err != nil {
		return s, err
	}
	if tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			s.Location = loc
		}
	}
	return s, nil
}

// IndexingDone is the share of the account's messages (orphans aside) whose
// body is in the mirror and indexed (body_state full or local): 1 once the
// backfill is over.
func (v *View) IndexingDone(ctx context.Context, accountID int64) (float64, error) {
	var pending int
	if err := v.pool.QueryRow(ctx, `
SELECT count(*) FROM messages WHERE account_id = $1 AND body_state IN ('headers', 'deferred') AND orphaned_at IS NULL`,
		accountID).Scan(&pending); err != nil {
		return 0, err
	}
	if pending == 0 {
		return 1, nil
	}
	var total int
	if err := v.pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE account_id = $1 AND orphaned_at IS NULL`, accountID).Scan(&total); err != nil {
		return 0, err
	}
	if total == 0 {
		return 1, nil
	}
	return math.Floor(float64(total-pending)/float64(total)*1000) / 1000, nil
}

// isTimeout reports a statement cancelled by statement_timeout.
func isTimeout(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "57014"
}

// searchList is ListThreads with a query (plan 3.12): the threads with a
// matching message, newest first, at most MaxSearchHits, under
// statement_timeout.
func (v *View) searchList(ctx context.Context, p ListParams, limit int) (*ThreadPage, error) {
	q := strings.TrimSpace(p.Query)
	ast, err := search.Parse(q)
	if err != nil {
		return nil, err
	}
	at, id, first, err := decodeCursor(p.Cursor)
	if err != nil {
		return nil, err
	}
	if first {
		id = maxID
	}
	scope, err := v.SearchScope(ctx, p.UserID, p.AccountID)
	if err != nil {
		return nil, err
	}
	keys, err := v.labelKeys(ctx, v.pool, p.AccountID)
	if err != nil {
		return nil, err
	}
	if k := keys[p.LabelID]; p.LabelID != 0 && (k == "SPAM" || k == "TRASH") {
		scope.IncludeSpamTrash = true
	}
	scope.ArgOffset = 2
	where, args, err := search.ToSQL(ast, scope)
	if err != nil {
		return nil, err
	}
	args = append([]any{p.UserID, p.AccountID}, args...)
	next := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	labelCond := ""
	if p.LabelID != 0 {
		labelCond = " AND EXISTS (SELECT 1 FROM message_labels lm WHERE lm.message_id = m.id AND lm.label_id = " + next(p.LabelID) + ")"
	}
	sql := `
WITH hits AS MATERIALIZED (
  SELECT t.id, t.sort_at FROM threads t
  WHERE t.user_id = $1 AND t.account_id = $2 AND EXISTS (
    SELECT 1 FROM messages m
    WHERE m.thread_id = t.id AND m.account_id = $2 AND m.orphaned_at IS NULL AND ` + where + labelCond + `)
  ORDER BY t.sort_at DESC, t.id DESC LIMIT ` + strconv.Itoa(MaxSearchHits) + `
)
SELECT t.id, t.account_id, t.subject, t.snippet, t.last_message_at, t.message_count, t.unread_count, t.has_attachments,
       t.participants, t.snoozed_until, t.pending_ops, hits.sort_at,
       ARRAY(SELECT x.label_id FROM thread_labels x WHERE x.thread_id = t.id AND NOT x.hidden ORDER BY x.label_id)::bigint[],
       (SELECT count(*) FROM hits)::int
FROM hits JOIN threads t ON t.id = hits.id
WHERE (hits.sort_at, hits.id) < (` + next(tstz(at, first)) + `::timestamptz, ` + next(id) + `::bigint)
ORDER BY hits.sort_at DESC, hits.id DESC
LIMIT ` + next(limit+1)

	page := &ThreadPage{Threads: []ops.Thread{}}
	var rows []threadRow
	total := 0
	timeout := v.SearchTimeout
	if timeout <= 0 {
		timeout = DefaultSearchTimeout
	}
	err = pgx.BeginFunc(ctx, v.pool, func(tx pgx.Tx) error {
		// Custom plans: the selectivity of a word or pattern decides between
		// walking threads newest first and a bitmap scan of the GIN index; a
		// cached generic plan would pick one for every query.
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d; SET LOCAL plan_cache_mode = force_custom_plan", max(timeout.Milliseconds(), 1))); err != nil {
			return err
		}
		rs, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var r threadRow
			if err := rs.Scan(&r.ID, &r.AccountID, &r.Subject, &r.Snippet, &r.LastMessageAt, &r.MessageCount, &r.UnreadCount,
				&r.HasAttachments, &r.Participants, &r.SnoozedUntil, &r.PendingOps, &r.SortAt, &r.LabelIDs, &total); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return rs.Err()
	})
	switch {
	case isTimeout(err) && ctx.Err() == nil:
		// Too slow: an empty, approximate answer rather than an error.
		rows, total, page.Approximate = nil, 0, true
	case err != nil:
		return nil, err
	}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		c := encodeCursor(last.SortAt, last.ID)
		page.NextCursor = &c
	}
	for _, r := range rows {
		page.Threads = append(page.Threads, threadOf(r, keys))
	}
	if err := ops.FillThreadExtras(ctx, v.pool, page.Threads); err != nil {
		return nil, err
	}
	page.ResultSizeEstimate = total
	done, err := v.IndexingDone(ctx, p.AccountID)
	if err != nil {
		return nil, err
	}
	page.IndexingDone = &done
	if p.Cursor == "" {
		if err := v.RecordSearch(ctx, p.UserID, p.AccountID, q); err != nil {
			return nil, err
		}
	}
	return page, nil
}

// RecordSearch saves a query in the search history of the account (the
// query as typed, trimmed): a repeated query moves to the top; only the
// MaxSearchHistory most recent are kept.
func (v *View) RecordSearch(ctx context.Context, userID, accountID int64, q string) error {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil
	}
	return pgx.BeginFunc(ctx, v.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO search_history (user_id, account_id, query, used_at) VALUES ($1, $2, $3, clock_timestamp())
ON CONFLICT (account_id, query) DO UPDATE SET used_at = clock_timestamp()`, userID, accountID, q); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
DELETE FROM search_history WHERE account_id = $1 AND id NOT IN (
  SELECT id FROM search_history WHERE account_id = $1 ORDER BY used_at DESC, id DESC LIMIT $2)`, accountID, MaxSearchHistory)
		return err
	})
}

// DeleteSearchHistory removes one query (exact text) from the history of the
// account, or the whole history when query is "". It returns the number of
// entries removed.
func (v *View) DeleteSearchHistory(ctx context.Context, userID, accountID int64, query string) (int64, error) {
	var tag pgconn.CommandTag
	var err error
	if query == "" {
		tag, err = v.pool.Exec(ctx, `DELETE FROM search_history WHERE user_id = $1 AND account_id = $2`, userID, accountID)
	} else {
		tag, err = v.pool.Exec(ctx, `DELETE FROM search_history WHERE user_id = $1 AND account_id = $2 AND query = $3`, userID, accountID, strings.TrimSpace(query))
	}
	return tag.RowsAffected(), err
}

// Suggestion is a suggestion of the search box. Text is the whole query
// after accepting it; Label is what to show.
type Suggestion struct {
	Kind  string // history, contact, label, operator
	Text  string
	Label string
}

// Suggest returns suggestions for a query being typed (plan 3.12, 5.1):
// recent queries starting with it, then for its last token contacts
// (from:<email>, or the key typed: to:, cc:, bcc:), user and folder labels
// (label:<name>, or in:<name>) and operators the token is a prefix of. At
// most SuggestLimit of each kind; an empty query gets the recent queries.
func (v *View) Suggest(ctx context.Context, userID, accountID int64, q string) ([]Suggestion, error) {
	out := []Suggestion{}
	q = strings.TrimLeft(q, " \t")
	like := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(q)) + "%"
	rows, err := v.pool.Query(ctx, `
SELECT query FROM search_history WHERE user_id = $1 AND account_id = $2 AND lower(query) LIKE $3 AND query <> $4
ORDER BY used_at DESC, id DESC LIMIT $5`, userID, accountID, like, strings.TrimSpace(q), SuggestLimit)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, Suggestion{Kind: "history", Text: s, Label: s})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	head, last := search.SplitLast(q)
	neg := ""
	tok := last
	if strings.HasPrefix(tok, "-") {
		neg, tok = "-", tok[1:]
	}
	if tok == "" || strings.HasPrefix(tok, `"`) {
		return out, nil
	}
	key, val, hasKey := strings.Cut(tok, ":")
	key = strings.ToLower(key)
	if !hasKey {
		key, val = "", tok
	}

	// Contacts.
	if ck := key; val != "" && (ck == "" || ck == "from" || ck == "to" || ck == "cc" || ck == "bcc") {
		if ck == "" {
			ck = "from"
		}
		cs, err := mail.SuggestContacts(ctx, v.pool, userID, val, SuggestLimit)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			label := c.Email
			if c.Name != "" {
				label = c.Name + " <" + c.Email + ">"
			}
			out = append(out, Suggestion{Kind: "contact", Text: head + neg + ck + ":" + c.Email + " ", Label: label})
		}
	}

	// User and folder labels.
	if lk := key; val != "" && (lk == "" || lk == "label" || lk == "in") {
		if lk == "" {
			lk = "label"
		}
		want := search.NormalizeLabel(val)
		rows, err := v.pool.Query(ctx, `
SELECT name FROM labels WHERE account_id = $1 AND user_id = $2 AND type IN ('user', 'folder') ORDER BY lower(name)`, accountID, userID)
		if err != nil {
			return nil, err
		}
		n := 0
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return nil, err
			}
			norm := search.NormalizeLabel(name)
			if n < SuggestLimit && want != "" && (strings.HasPrefix(norm, want) || strings.Contains(norm, " "+want)) {
				out = append(out, Suggestion{Kind: "label", Text: head + neg + lk + ":" + search.LabelToken(name) + " ", Label: name})
				n++
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// Operators.
	for _, o := range search.MatchOperators(tok, SuggestLimit) {
		text := head + neg + o.Text
		if !strings.HasSuffix(o.Text, ":") {
			text += " "
		}
		out = append(out, Suggestion{Kind: "operator", Text: text, Label: o.Label})
	}
	return out, nil
}
