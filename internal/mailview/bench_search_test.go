package mailview_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/mailview"
	"github.com/silopost/silo-server/internal/search"
	"github.com/silopost/silo-server/internal/store"
	"github.com/silopost/silo-server/internal/testutil/pgtest"
)

// TestSearchBenchmark is the DoD benchmark of phase 4 (plan 3.12): a search
// over 1M messages answers in p95 < 500 ms for one word and for from:. It
// loads synthetic rows directly (search_tsv from to_tsvector over ASCII
// words, which gives the lexemes of the application tokenizer) and is
// skipped unless SILO_BENCH_SEARCH=true; SILO_BENCH_MESSAGES overrides the
// size (1,000,000).
func TestSearchBenchmark(t *testing.T) {
	if os.Getenv("SILO_BENCH_SEARCH") != "true" {
		t.Skip("set SILO_BENCH_SEARCH=true to run the 1M-message search benchmark")
	}
	msgs := 1_000_000
	if v := os.Getenv("SILO_BENCH_MESSAGES"); v != "" {
		fmt.Sscan(v, &msgs)
	}
	threads := msgs / 3
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
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var uid, acc, inbox int64
	must(pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ('bench@silo.test') RETURNING id`).Scan(&uid))
	must(pool.QueryRow(ctx, `INSERT INTO accounts (user_id, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, secret_enc)
VALUES ($1, 'bench@silo.test', 'h', 1, 'none', 'h', 1, 'none', 'u', '\x00') RETURNING id`, uid).Scan(&acc))
	for _, k := range []string{"INBOX", "SPAM", "TRASH"} {
		var id int64
		must(pool.QueryRow(ctx, `INSERT INTO labels (user_id, account_id, name, type, system_key) VALUES ($1, $2, $3, 'system', $3) RETURNING id`, uid, acc, k).Scan(&id))
		if k == "INBOX" {
			inbox = id
		}
	}

	t0 := time.Now()
	_, err = pool.Exec(ctx, `
INSERT INTO threads (user_id, account_id, subject, subject_norm, last_message_at, sort_at, message_count)
SELECT $1::bigint, $2::bigint, 'Subject ' || g, 'subject ' || g, ts, ts, 3
FROM (SELECT g, now() - make_interval(secs => ((g::bigint * 7919) % $3::bigint) * 60) AS ts FROM generate_series(1, $3::int) g) s`, uid, acc, threads)
	must(err)
	// Words w0..w19999 with a skewed choice: low numbers are common.
	_, err = pool.Exec(ctx, `
INSERT INTO messages (user_id, account_id, thread_id, message_id_hdr, dedupe_key, size_bytes, internal_date, subject,
  from_addr, from_text, recipients_text, body_text, body_state, search_tsv, search_ver)
SELECT t.user_id, t.account_id, t.id, 'm' || t.id || '.' || n, 'm' || t.id || '.' || n || ':100', 1000 + (t.id * n) % 100000,
       t.sort_at - make_interval(secs => n), x.subj, 's' || x.s || '@dom' || (x.s % 97) || '.pl',
       'Sender ' || x.s || ' <s' || x.s || '@dom' || (x.s % 97) || '.pl>', 'bench@silo.test', x.body, 'full',
       setweight(to_tsvector('simple', x.subj), 'A') ||
       setweight(to_tsvector('simple', 'Sender ' || x.s || ' s' || x.s || ' dom' || (x.s % 97) || ' pl bench silo test'), 'B') ||
       setweight(to_tsvector('simple', x.body), 'C'), 1
FROM threads t CROSS JOIN generate_series(1, 3) n
CROSS JOIN LATERAL (SELECT (t.id * 31 + n * 7) % 5000 AS s,
  'w' || ((t.id * 13 + n) % 20000 * ((t.id + n) % 20000) / 20000) || ' w' || ((t.id * 17 + n * 3) % 2000) || ' w' || ((t.id + n * 11) % 20000) AS subj,
  (SELECT string_agg('w' || (((t.id * 7 + n * 5 + k * 104729) % 20000) * ((t.id * 3 + k) % 20000) / 20000), ' ') FROM generate_series(1, 30) k) AS body) x
WHERE t.account_id = $1`, acc)
	must(err)
	_, err = pool.Exec(ctx, `INSERT INTO message_labels (message_id, label_id, user_id) SELECT id, $1, user_id FROM messages`, inbox)
	must(err)
	_, err = pool.Exec(ctx, `VACUUM ANALYZE threads`)
	must(err)
	_, err = pool.Exec(ctx, `VACUUM ANALYZE messages`)
	must(err)
	_, err = pool.Exec(ctx, `VACUUM ANALYZE message_labels`)
	must(err)
	var n int
	must(pool.QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&n))
	t.Logf("loaded %d messages in %d threads in %v", n, threads, time.Since(t0).Round(time.Second))

	v := mailview.New(pool, nil, nil)
	run := func(name string, queries []string) {
		var samples []time.Duration
		hits := 0
		slowest := map[string]time.Duration{}
		for i := 0; i < 120; i++ {
			q := queries[i%len(queries)]
			start := time.Now()
			page, err := v.ListThreads(ctx, mailview.ListParams{UserID: uid, AccountID: acc, Query: q, Limit: 50})
			d := time.Since(start)
			must(err)
			if page.Approximate {
				t.Errorf("%s %q: approximate", name, q)
			}
			if i >= len(queries) { // the first round warms the cache
				samples = append(samples, d)
			}
			if d > slowest[q] {
				slowest[q] = d
			}
			hits += page.ResultSizeEstimate
		}
		slices.Sort(samples)
		p50, p95 := samples[len(samples)/2], samples[len(samples)*95/100]
		t.Logf("%-10s %v: p50 %v p95 %v max %v (avg %d hits)", name, queries, p50, p95, samples[len(samples)-1], hits/120)
		t.Logf("%-10s slowest per query: %v", name, slowest)
		if p95 >= 500*time.Millisecond {
			t.Errorf("%s: p95 %v >= 500ms", name, p95)
		}
	}
	if os.Getenv("SILO_BENCH_EXPLAIN") == "true" {
		for _, q := range []string{"w3", "w19999", "from:dom7.pl"} {
			explainSearch(t, ctx, pool, v, uid, acc, q)
		}
	}
	run("word", []string{"w3", "w1500", "w19999", "w42", "w777"})
	run("from:", []string{"from:s1234@", "from:dom7.pl", "from:sender 42", "from:s4999"})
	run("word+from", []string{"w5 from:dom3", "w100 -from:dom1"})
}

// explainSearch logs the plan of the hits CTE of a query.
func explainSearch(t *testing.T, ctx context.Context, pool *pgxpool.Pool, v *mailview.View, uid, acc int64, q string) {
	t.Helper()
	scope, err := v.SearchScope(ctx, uid, acc)
	if err != nil {
		t.Fatal(err)
	}
	scope.ArgOffset = 2
	where, args, _ := search.ToSQL(search.MustParse(q), scope)
	rows, err := pool.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS)
SELECT t.id, t.sort_at FROM threads t
WHERE t.user_id = $1 AND t.account_id = $2 AND EXISTS (
  SELECT 1 FROM messages m WHERE m.thread_id = t.id AND m.account_id = $2 AND m.orphaned_at IS NULL AND `+where+`)
ORDER BY t.sort_at DESC, t.id DESC LIMIT 2000`, append([]any{uid, acc}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	t.Logf("EXPLAIN %q:\n%s", q, strings.Join(plan, "\n"))
}
