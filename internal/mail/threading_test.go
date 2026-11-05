package mail_test

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	silomail "github.com/silopost/silo-server/internal/mail"
)

// scenario is testdata/threading/<name>/expected.json.
type scenario struct {
	Description string              `json:"description"`
	Shuffle     bool                `json:"shuffle"`
	Order       []string            `json:"order"`   // ingest order of the canonical run (default: sorted)
	Folders     map[string]string   `json:"folders"` // file -> folder (default INBOX)
	Also        map[string][]string `json:"also"`    // file -> additional folders holding a copy
	Threads     [][]string          `json:"threads"` // expected partition
	Messages    int                 `json:"messages"`
	Locations   int                 `json:"locations"`
	Synthetic   []string            `json:"synthetic"`
	Subjects    map[string]string   `json:"subjects"`
	FromNames   map[string]string   `json:"from_names"`
	// ThreadLabels: labels (name) that the thread of the file must carry.
	ThreadLabels map[string][]string `json:"thread_labels"`

	dir   string
	files map[string][]byte
}

func loadScenarios(t *testing.T) []*scenario {
	t.Helper()
	dirs, err := filepath.Glob("testdata/threading/*")
	must(t, err)
	var out []*scenario
	for _, d := range dirs {
		raw, err := os.ReadFile(filepath.Join(d, "expected.json"))
		if err != nil {
			continue
		}
		s := &scenario{dir: d, files: map[string][]byte{}}
		must(t, json.Unmarshal(raw, s))
		emls, _ := filepath.Glob(filepath.Join(d, "*.eml"))
		for _, f := range emls {
			b, err := os.ReadFile(f)
			must(t, err)
			s.files[filepath.Base(f)] = b
		}
		out = append(out, s)
	}
	return out
}

func (s *scenario) names() []string {
	var names []string
	for n := range s.files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (s *scenario) folder(file string) string {
	if f, ok := s.Folders[file]; ok {
		return f
	}
	return "INBOX"
}

// item is one (file, folder) location to ingest.
type item struct{ file, folder string }

func (s *scenario) items(order []string) []item {
	var out []item
	for _, f := range order {
		out = append(out, item{f, s.folder(f)})
		for _, extra := range s.Also[f] {
			out = append(out, item{f, extra})
		}
	}
	return out
}

// run ingests the scenario into a fresh account, one message per transaction
// in the given order, and returns the account env.
func (s *scenario) run(t *testing.T, order []item) *env {
	e := newEnv(t)
	for _, it := range order {
		e.ingest(it.folder, e.fetched(it.folder, s.files[it.file]))
	}
	return e
}

// partition returns the threads of the account as sorted lists of file names.
func (s *scenario) partition(t *testing.T, e *env) [][]string {
	key := map[string]string{} // dedupe_key -> file
	for f, raw := range s.files {
		ph := silomail.ParseHeader(headerOf(raw), int64(len(raw)))
		key[fmt.Sprintf("%s:%d", ph.MessageID, len(raw))] = f
	}
	rows, err := e.pool.Query(e.ctx, `SELECT thread_id, dedupe_key FROM messages WHERE account_id = $1`, e.accountID)
	must(t, err)
	defer rows.Close()
	groups := map[int64][]string{}
	for rows.Next() {
		var tid int64
		var dk string
		must(t, rows.Scan(&tid, &dk))
		f, ok := key[dk]
		if !ok {
			t.Fatalf("unknown message %q in database", dk)
		}
		groups[tid] = append(groups[tid], f)
	}
	must(t, rows.Err())
	return normalize(groups)
}

func headerOf(raw []byte) []byte {
	if i := strings.Index(string(raw), "\r\n\r\n"); i >= 0 {
		return raw[:i+4]
	}
	return raw
}

func normalize[K comparable](groups map[K][]string) [][]string {
	var out [][]string
	for _, g := range groups {
		g = slices.Clone(g)
		sort.Strings(g)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return strings.Join(out[i], ",") < strings.Join(out[j], ",") })
	return out
}

func normalizeExpected(threads [][]string) [][]string {
	m := map[int][]string{}
	for i, g := range threads {
		m[i] = g
	}
	return normalize(m)
}

func TestThreadingCorpus(t *testing.T) {
	scenarios := loadScenarios(t)
	if len(scenarios) < 20 {
		t.Fatalf("only %d scenarios in testdata/threading, want at least 20", len(scenarios))
	}
	for _, s := range scenarios {
		t.Run(filepath.Base(s.dir), func(t *testing.T) {
			want := normalizeExpected(s.Threads)
			order := s.Order
			if order == nil {
				order = s.names()
			}
			e := s.run(t, s.items(order))
			if got := s.partition(t, e); !slices.EqualFunc(got, want, slices.Equal) {
				t.Fatalf("%s\npartition = %v\nwant        %v", s.Description, got, want)
			}
			s.checkExtras(t, e)
			e.reconcileClean()
		})
	}
}

// TestThreadingShuffle ingests every scenario in 10 random orders (fixed
// seed) and requires the same partition each time (DoD of phase 2).
func TestThreadingShuffle(t *testing.T) {
	for _, s := range loadScenarios(t) {
		if !s.Shuffle {
			continue
		}
		t.Run(filepath.Base(s.dir), func(t *testing.T) {
			want := normalizeExpected(s.Threads)
			rng := rand.New(rand.NewSource(42))
			for i := range 10 {
				items := s.items(s.names())
				rng.Shuffle(len(items), func(a, b int) { items[a], items[b] = items[b], items[a] })
				e := s.run(t, items)
				if got := s.partition(t, e); !slices.EqualFunc(got, want, slices.Equal) {
					t.Fatalf("shuffle %d order %v:\npartition = %v\nwant        %v", i, items, got, want)
				}
				if i == 0 {
					e.reconcileClean()
				}
			}
		})
	}
}

// TestThreadingBatched ingests each scenario with one IngestBatch call per
// folder (the way initial sync does) in reverse order.
func TestThreadingBatched(t *testing.T) {
	for _, s := range loadScenarios(t) {
		t.Run(filepath.Base(s.dir), func(t *testing.T) {
			e := newEnv(t)
			items := s.items(s.names())
			slices.Reverse(items)
			byFolder := map[string][]silomail.FetchedMessage{}
			var folders []string
			for _, it := range items {
				if _, ok := byFolder[it.folder]; !ok {
					folders = append(folders, it.folder)
				}
				byFolder[it.folder] = append(byFolder[it.folder], e.fetched(it.folder, s.files[it.file]))
			}
			for _, f := range folders {
				e.ingest(f, byFolder[f]...)
			}
			if got, want := s.partition(t, e), normalizeExpected(s.Threads); !slices.EqualFunc(got, want, slices.Equal) {
				t.Fatalf("batched: partition = %v want %v", got, want)
			}
			e.reconcileClean()
		})
	}
}

func (s *scenario) checkExtras(t *testing.T, e *env) {
	t.Helper()
	if s.Messages > 0 {
		if n := e.count(`SELECT count(*) FROM messages WHERE account_id = $1`, e.accountID); n != s.Messages {
			t.Errorf("messages = %d, want %d", n, s.Messages)
		}
	}
	if s.Locations > 0 {
		if n := e.count(`SELECT count(*) FROM message_locations WHERE account_id = $1`, e.accountID); n != s.Locations {
			t.Errorf("locations = %d, want %d", n, s.Locations)
		}
	}
	for _, f := range s.Synthetic {
		raw := s.files[f]
		ph := silomail.ParseHeader(headerOf(raw), int64(len(raw)))
		if !ph.SyntheticID || !strings.HasSuffix(ph.MessageID, "@silo.invalid") {
			t.Errorf("%s: message id %q is not synthetic", f, ph.MessageID)
		}
		if n := e.count(`SELECT count(*) FROM messages WHERE account_id = $1 AND message_id_hdr = $2`, e.accountID, ph.MessageID); n != 1 {
			t.Errorf("%s: %d rows with the synthetic id", f, n)
		}
	}
	for f, subj := range s.Subjects {
		raw := s.files[f]
		ph := silomail.ParseHeader(headerOf(raw), int64(len(raw)))
		if n := e.count(`SELECT count(*) FROM messages WHERE account_id = $1 AND message_id_hdr = $2 AND subject = $3`, e.accountID, ph.MessageID, subj); n != 1 {
			t.Errorf("%s: subject %q not stored decoded", f, subj)
		}
	}
	for f, name := range s.FromNames {
		raw := s.files[f]
		ph := silomail.ParseHeader(headerOf(raw), int64(len(raw)))
		if n := e.count(`SELECT count(*) FROM messages WHERE account_id = $1 AND message_id_hdr = $2 AND from_name = $3`, e.accountID, ph.MessageID, name); n != 1 {
			t.Errorf("%s: from_name %q not stored decoded", f, name)
		}
	}
	for f, labels := range s.ThreadLabels {
		raw := s.files[f]
		ph := silomail.ParseHeader(headerOf(raw), int64(len(raw)))
		got := e.names(`
SELECT l.name FROM thread_labels tl JOIN labels l ON l.id = tl.label_id
JOIN messages m ON m.thread_id = tl.thread_id
WHERE m.account_id = $1 AND m.message_id_hdr = $2 ORDER BY l.name`, e.accountID, ph.MessageID)
		for _, want := range labels {
			if !slices.Contains(got, want) {
				t.Errorf("%s: thread labels %v miss %q", f, got, want)
			}
		}
	}
}

// TestThreadingIdempotent re-ingests the same batches: nothing is duplicated.
func TestThreadingIdempotent(t *testing.T) {
	for _, s := range loadScenarios(t) {
		t.Run(filepath.Base(s.dir), func(t *testing.T) {
			e := newEnv(t)
			var batches [][]silomail.FetchedMessage
			var folders []string
			for _, it := range s.items(s.names()) {
				m := e.fetched(it.folder, s.files[it.file], "\\Seen", "Projekt")
				batches = append(batches, []silomail.FetchedMessage{m})
				folders = append(folders, it.folder)
			}
			ingestAll := func() {
				for i, b := range batches {
					e.ingest(folders[i], b...)
				}
			}
			counts := func() [4]int {
				return [4]int{
					e.count(`SELECT count(*) FROM messages WHERE account_id = $1`, e.accountID),
					e.count(`SELECT count(*) FROM message_locations WHERE account_id = $1`, e.accountID),
					e.count(`SELECT count(*) FROM message_labels ml JOIN messages m ON m.id = ml.message_id WHERE m.account_id = $1`, e.accountID),
					e.count(`SELECT count(*) FROM thread_labels tl JOIN threads t ON t.id = tl.thread_id WHERE t.account_id = $1`, e.accountID),
				}
			}
			ingestAll()
			first := counts()
			contacts := e.count(`SELECT COALESCE(sum(received_count + sent_count), 0) FROM contacts WHERE user_id = $1`, e.userID)
			ingestAll()
			ingestAll()
			if second := counts(); second != first {
				t.Fatalf("counts after re-ingest %v, before %v", second, first)
			}
			if c := e.count(`SELECT COALESCE(sum(received_count + sent_count), 0) FROM contacts WHERE user_id = $1`, e.userID); c != contacts {
				t.Fatalf("contact counters changed on re-ingest: %d -> %d", contacts, c)
			}
			if got, want := s.partition(t, e), normalizeExpected(s.Threads); !slices.EqualFunc(got, want, slices.Equal) {
				t.Fatalf("partition after re-ingest = %v want %v", got, want)
			}
			e.reconcileClean()
		})
	}
}

// TestThreadSizeLimit: a thread takes at most 100 messages (plan 4.6); the
// rest of a long chain continues in a second thread.
func TestThreadSizeLimit(t *testing.T) {
	e := newEnv(t)
	const total = 105
	prev := ""
	var msgs []silomail.FetchedMessage
	for i := 1; i <= total; i++ {
		id := fmt.Sprintf("chain%d@example.com", i)
		extra := []string{}
		if prev != "" {
			extra = append(extra, "In-Reply-To", "<"+prev+">")
		}
		subj := "Long chain"
		if i > 1 {
			subj = "Re: Long chain"
		}
		raw := simple("Anna <anna@example.com>", "dev@silo.test", subj, id, baseTime.Add(time.Duration(i)*time.Minute), extra...)
		msgs = append(msgs, e.fetched("INBOX", raw))
		prev = id
	}
	for i := 0; i < len(msgs); i += 35 { // batches of 35, in order
		e.ingest("INBOX", msgs[i:min(i+35, len(msgs))]...)
	}
	rows, err := e.pool.Query(e.ctx, `SELECT message_count FROM threads WHERE account_id = $1 ORDER BY message_count DESC`, e.accountID)
	must(t, err)
	defer rows.Close()
	var sizes []int
	for rows.Next() {
		var n int
		must(t, rows.Scan(&n))
		sizes = append(sizes, n)
	}
	if !slices.Equal(sizes, []int{100, 5}) {
		t.Fatalf("thread sizes = %v, want [100 5]", sizes)
	}
	e.reconcileClean()
}

// TestMergeKeepsOldestAndEmitsEvent checks the merge bookkeeping.
func TestMergeKeepsOldestAndEmitsEvent(t *testing.T) {
	e := newEnv(t)
	b1 := simple("b@example.com", "dev@silo.test", "Re: X", "m-b1@x.com", baseTime.Add(time.Hour), "In-Reply-To", "<m-root@x.com>")
	b2 := simple("c@example.com", "dev@silo.test", "Re: X", "m-b2@x.com", baseTime.Add(30*24*time.Hour), "In-Reply-To", "<m-root@x.com>")
	root := simple("a@example.com", "dev@silo.test", "X", "m-root@x.com", baseTime)
	e.ingest("INBOX", e.fetched("INBOX", b1))
	e.ingest("INBOX", e.fetched("INBOX", b2))
	var t1, t2 int64
	must(t, e.pool.QueryRow(e.ctx, `SELECT thread_id FROM messages WHERE message_id_hdr = 'm-b1@x.com' AND account_id = $1`, e.accountID).Scan(&t1))
	must(t, e.pool.QueryRow(e.ctx, `SELECT thread_id FROM messages WHERE message_id_hdr = 'm-b2@x.com' AND account_id = $1`, e.accountID).Scan(&t2))
	if t1 == t2 {
		t.Fatalf("setup: b1 and b2 should start in separate threads")
	}
	res := e.ingest("INBOX", e.fetched("INBOX", root))
	if res.Messages[0].ThreadID != t1 {
		t.Fatalf("winner = %d, want oldest thread %d", res.Messages[0].ThreadID, t1)
	}
	if n := e.count(`SELECT count(*) FROM threads WHERE account_id = $1`, e.accountID); n != 1 {
		t.Fatalf("threads = %d, want 1", n)
	}
	var payload string
	must(t, e.pool.QueryRow(e.ctx, `SELECT payload::text FROM events WHERE user_id = $1 AND type = 'thread.merged'`, e.userID).Scan(&payload))
	var ev struct {
		FromIDs []int64 `json:"fromIds"`
		IntoID  int64   `json:"intoId"`
	}
	must(t, json.Unmarshal([]byte(payload), &ev))
	if ev.IntoID != t1 || !slices.Equal(ev.FromIDs, []int64{t2}) {
		t.Fatalf("thread.merged = %s, want from [%d] into %d", payload, t2, t1)
	}
	e.reconcileClean()
}

// TestMergeMovesSnooze: snoozes of merged threads are combined (earliest wake_at).
func TestMergeMovesSnooze(t *testing.T) {
	e := newEnv(t)
	b1 := simple("b@example.com", "dev@silo.test", "Re: S", "s-b1@x.com", baseTime.Add(time.Hour), "In-Reply-To", "<s-root@x.com>")
	b2 := simple("c@example.com", "dev@silo.test", "Re: S", "s-b2@x.com", baseTime.Add(30*24*time.Hour), "In-Reply-To", "<s-root@x.com>")
	e.ingest("INBOX", e.fetched("INBOX", b1))
	e.ingest("INBOX", e.fetched("INBOX", b2))
	rows, err := e.pool.Query(e.ctx, `SELECT thread_id FROM messages WHERE account_id = $1 ORDER BY id`, e.accountID)
	must(t, err)
	var ts []int64
	for rows.Next() {
		var id int64
		must(t, rows.Scan(&id))
		ts = append(ts, id)
	}
	rows.Close()
	wake1, wake2 := baseTime.Add(100*time.Hour), baseTime.Add(50*time.Hour)
	for i, w := range []time.Time{wake1, wake2} {
		_, err := e.pool.Exec(e.ctx, `INSERT INTO snoozes (user_id, account_id, thread_id, wake_at) VALUES ($1, $2, $3, $4)`, e.userID, e.accountID, ts[i], w)
		must(t, err)
	}
	e.ingest("INBOX", e.fetched("INBOX", simple("a@example.com", "dev@silo.test", "S", "s-root@x.com", baseTime)))
	var wake time.Time
	var thread int64
	must(t, e.pool.QueryRow(e.ctx, `SELECT thread_id, wake_at FROM snoozes WHERE account_id = $1`, e.accountID).Scan(&thread, &wake))
	if !wake.Equal(wake2) || thread != ts[0] {
		t.Fatalf("snooze = thread %d wake %v, want thread %d wake %v", thread, wake, ts[0], wake2)
	}
	var until time.Time
	must(t, e.pool.QueryRow(e.ctx, `SELECT snoozed_until FROM threads WHERE id = $1`, ts[0]).Scan(&until))
	if !until.Equal(wake2) {
		t.Fatalf("snoozed_until = %v, want %v", until, wake2)
	}
}
