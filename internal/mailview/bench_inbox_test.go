package mailview_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/silopost/silo-server/internal/mailview"
	"github.com/silopost/silo-server/internal/store"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
	"github.com/silopost/silo-server/internal/testutil/pgtest"
)

// explainSQL is sqlcgen's ListThreadsByLabel (internal/store/queries/threads.sql)
// with positional parameters; keep them in step.
const explainSQL = `EXPLAIN (ANALYZE, BUFFERS)
SELECT t.id, t.account_id, t.subject, t.snippet, t.last_message_at, t.message_count, t.unread_count,
       t.has_attachments, t.participants, t.snoozed_until, t.pending_ops,
       tl.sort_at,
       ARRAY(SELECT x.label_id FROM thread_labels x WHERE x.thread_id = t.id AND NOT x.hidden ORDER BY x.label_id)::bigint[] AS label_ids
FROM thread_labels tl
JOIN threads t ON t.id = tl.thread_id
WHERE tl.user_id  = $1
  AND tl.label_id = $2
  AND NOT tl.hidden
  AND (tl.sort_at, tl.thread_id) < ($3::timestamptz, $4::bigint)
ORDER BY tl.sort_at DESC, tl.thread_id DESC
LIMIT $5`

// TestInboxListBenchmark is the DoD benchmark of phase 2 (plan 4.7): the inbox
// list over 100k threads / 300k messages must stay under 50 ms at p95 for any
// page depth, with the plan "Index Scan using thread_labels_list". It loads
// the tables directly (no IMAP) and is skipped unless SILO_BENCH_INBOX=true
// (nightly workflow); SILO_BENCH_THREADS overrides the size.
func TestInboxListBenchmark(t *testing.T) {
	if os.Getenv("SILO_BENCH_INBOX") != "true" {
		t.Skip("set SILO_BENCH_INBOX=true to run the 100k-thread benchmark")
	}
	threads := 100_000
	if v := os.Getenv("SILO_BENCH_THREADS"); v != "" {
		if _, err := fmt.Sscan(v, &threads); err != nil {
			t.Fatalf("SILO_BENCH_THREADS: %v", err)
		}
	}
	ctx := context.Background()
	url := pgtest.New(t)
	if err := store.MigrateUp(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var uid, acc, inbox, archive int64
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ('bench@silo.test') RETURNING id`).Scan(&uid))
	must(pool.QueryRow(ctx, `INSERT INTO accounts (user_id, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, secret_enc)
VALUES ($1, 'bench@silo.test', 'h', 1, 'none', 'h', 1, 'none', 'u', '\x00') RETURNING id`, uid).Scan(&acc))
	must(pool.QueryRow(ctx, `INSERT INTO labels (user_id, account_id, name, type, system_key) VALUES ($1, $2, 'Inbox', 'system', 'INBOX') RETURNING id`, uid, acc).Scan(&inbox))
	must(pool.QueryRow(ctx, `INSERT INTO labels (user_id, account_id, name, type, system_key) VALUES ($1, $2, 'Archive', 'system', 'ARCHIVE') RETURNING id`, uid, acc).Scan(&archive))

	t0 := time.Now()
	// Threads, newest first by id order is irrelevant: sort_at is shuffled by a
	// multiplicative step so that ids and dates are not correlated.
	_, err = pool.Exec(ctx, `
INSERT INTO threads (user_id, account_id, subject, subject_norm, last_message_at, sort_at, message_count, unread_count, has_attachments, snippet, participants)
SELECT $1::bigint, $2::bigint, 'Subject ' || g, 'subject ' || g, ts, ts, 3, (g % 4 = 0)::int, g % 7 = 0, 'Snippet of thread ' || g,
       '[{"email":"a@x.pl","name":"A","unread":false},{"email":"me","name":"ja","unread":false}]'::jsonb
FROM (SELECT g, now() - make_interval(secs => ((g::bigint * 7919) % $3::bigint) * 60 + (g % 60)) AS ts FROM generate_series(1, $3::int) g) s`, uid, acc, threads)
	must(err)
	_, err = pool.Exec(ctx, `
INSERT INTO messages (user_id, account_id, thread_id, message_id_hdr, dedupe_key, size_bytes, internal_date, subject, from_addr, body_state)
SELECT t.user_id, t.account_id, t.id, 'm' || t.id || '.' || n, 'm' || t.id || '.' || n || ':100', 100, t.sort_at - make_interval(secs => n), t.subject, 'a@x.pl', 'full'
FROM threads t CROSS JOIN generate_series(1, 3) n`)
	must(err)
	_, err = pool.Exec(ctx, `
INSERT INTO thread_labels (thread_id, label_id, user_id, message_count, unread_count, sort_at)
SELECT id, $1::bigint, user_id, message_count, unread_count, sort_at FROM threads
UNION ALL
SELECT id, $2::bigint, user_id, message_count, unread_count, sort_at FROM threads WHERE id % 2 = 0`, inbox, archive)
	must(err)
	_, err = pool.Exec(ctx, `ANALYZE threads, messages, thread_labels, labels`)
	must(err)
	var msgs int
	must(pool.QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&msgs))
	t.Logf("loaded %d threads, %d messages in %v", threads, msgs, time.Since(t0).Round(time.Millisecond))

	// Cursors at several depths.
	q := sqlcgen.New(pool)
	depths := []int{0, 1, 50, 500, 5_000, 25_000, threads / 2, threads - 100}
	type cur struct {
		at pgtype.Timestamptz
		id int64
	}
	var cursors []cur
	for _, d := range depths {
		c := cur{at: pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}, id: 1<<63 - 1}
		if d > 0 {
			must(pool.QueryRow(ctx, `SELECT sort_at, thread_id FROM thread_labels WHERE user_id = $1 AND label_id = $2 AND NOT hidden
ORDER BY sort_at DESC, thread_id DESC OFFSET $3 LIMIT 1`, uid, inbox, d-1).Scan(&c.at, &c.id))
		}
		cursors = append(cursors, c)
	}

	run := func(c cur) ([]sqlcgen.ListThreadsByLabelRow, time.Duration) {
		start := time.Now()
		rows, err := q.ListThreadsByLabel(ctx, sqlcgen.ListThreadsByLabelParams{
			UserID: uid, LabelID: inbox, CursorSortAt: c.at, CursorThreadID: c.id, PageLimit: 51,
		})
		must(err)
		return rows, time.Since(start)
	}
	for _, c := range cursors { // warm up
		run(c)
	}
	var samples []time.Duration
	for i := 0; i < 300; i++ {
		rows, d := run(cursors[i%len(cursors)])
		samples = append(samples, d)
		if i%len(cursors) < len(cursors)-1 && len(rows) != 51 {
			t.Fatalf("page has %d rows", len(rows))
		}
	}
	slices.Sort(samples)
	p50, p95, p99 := samples[len(samples)*50/100], samples[len(samples)*95/100], samples[len(samples)*99/100]
	t.Logf("ListThreadsByLabel over %d INBOX threads, %d samples at depths %v: p50 %v p95 %v p99 %v max %v",
		threads, len(samples), depths, p50, p95, p99, samples[len(samples)-1])

	// The plan of a deep page.
	deep := cursors[len(cursors)/2+1]
	rows, err := pool.Query(ctx, explainSQL, uid, inbox, deep.at, deep.id, 51)
	must(err)
	var plan []string
	for rows.Next() {
		var line string
		must(rows.Scan(&line))
		plan = append(plan, line)
	}
	rows.Close()
	t.Logf("EXPLAIN (ANALYZE, BUFFERS):\n%s", strings.Join(plan, "\n"))
	if !regexp.MustCompile(`Index (Only )?Scan using thread_labels_list`).MatchString(strings.Join(plan, "\n")) {
		t.Errorf("plan does not use thread_labels_list")
	}
	if p95 >= 50*time.Millisecond {
		t.Errorf("p95 %v >= 50ms", p95)
	}

	// The whole endpoint path (labels lookup, count, mapping).
	v := mailview.New(pool, nil, nil)
	var api []time.Duration
	for i := 0; i < 100; i++ {
		start := time.Now()
		_, err := v.ListThreads(ctx, mailview.ListParams{UserID: uid, AccountID: acc, LabelID: inbox, Limit: 50})
		must(err)
		api = append(api, time.Since(start))
	}
	slices.Sort(api)
	t.Logf("View.ListThreads first page (with resultSizeEstimate count): p50 %v p95 %v", api[50], api[95])
}
