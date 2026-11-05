package mail

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/search"
)

// SearchVersion is the version of the search_tsv tokenizer (messages.
// search_ver); rows indexed with an older one are re-indexed by
// ReindexSearch. Raising it needs a migration that moves the partial index
// messages_search_stale to the new value.
const SearchVersion = 1

// searchDocs loads what the index of the messages is built from.
const searchDocsSQL = `
SELECT m.id, m.subject, m.from_text, m.recipients_text, m.body_text,
       ARRAY(SELECT a.filename FROM attachments a WHERE a.message_id = m.id ORDER BY a.id)::text[]
FROM messages m`

func writeSearch(ctx context.Context, db DB, rows pgx.Rows) error {
	var ids []int64
	var tsvs []string
	for rows.Next() {
		var id int64
		var v search.MessageView
		if err := rows.Scan(&id, &v.Subject, &v.FromText, &v.RecipientsText, &v.Body, &v.Attachments); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
		tsvs = append(tsvs, v.Index().TSVector())
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := db.Exec(ctx, `
UPDATE messages m SET search_tsv = v.tsv::tsvector, search_ver = $3
FROM unnest($1::bigint[], $2::text[]) AS v(id, tsv)
WHERE m.id = v.id`, ids, tsvs, SearchVersion)
	return err
}

// refreshSearch rebuilds search_tsv of the messages from their current
// columns with the search tokenizer (plan 3.12: subject A, people and
// attachment names B, the first 512 KB of the body C).
func refreshSearch(ctx context.Context, db DB, msgIDs []int64) error {
	if len(msgIDs) == 0 {
		return nil
	}
	rows, err := db.Query(ctx, searchDocsSQL+` WHERE m.id = ANY($1)`, msgIDs)
	if err == nil {
		err = writeSearch(ctx, db, rows)
	}
	if err != nil {
		return fmt.Errorf("mail: refresh search_tsv: %w", err)
	}
	return nil
}

// RefreshSearch is refreshSearch for callers outside the package (tests and
// tools that change indexed columns directly).
func RefreshSearch(ctx context.Context, db DB, msgIDs []int64) error {
	return refreshSearch(ctx, db, msgIDs)
}

// ReindexSearch re-indexes, in batches, the messages whose search_tsv was
// built by an older tokenizer (search_ver < SearchVersion), until none is
// left or ctx ends. Rows are locked while they are indexed, so a concurrent
// body download (which indexes the row itself) is never overwritten with
// older text. Run it in the background at start-up.
func ReindexSearch(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	total := 0
	start := time.Now()
	for ctx.Err() == nil {
		n := 0
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, searchDocsSQL+`
WHERE m.id IN (SELECT id FROM messages WHERE search_ver < $1 ORDER BY id LIMIT 500 FOR UPDATE SKIP LOCKED)`, SearchVersion)
			if err != nil {
				return err
			}
			var cnt pgx.Rows = &countingRows{Rows: rows, n: &n}
			return writeSearch(ctx, tx, cnt)
		})
		if err != nil {
			return fmt.Errorf("mail: reindex search: %w", err)
		}
		total += n
		if n == 0 {
			break
		}
	}
	if total > 0 && log != nil {
		log.Info("search index rebuilt", "messages", total, "took", time.Since(start).Round(time.Millisecond))
	}
	return ctx.Err()
}

type countingRows struct {
	pgx.Rows
	n *int
}

func (r *countingRows) Next() bool {
	ok := r.Rows.Next()
	if ok {
		*r.n++
	}
	return ok
}
