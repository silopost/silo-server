package mail_test

import (
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// seedDir is the dev corpus of the silo repository (sibling checkout).
const seedDir = "../../../silo/seed/mail"

type seedEntry struct {
	file, folder, date string
	flags              []string
}

// loadSeed reads manifest.yaml with a tiny line parser (no yaml dependency).
func loadSeed(t *testing.T) []seedEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(seedDir, "manifest.yaml"))
	if err != nil {
		t.Skipf("seed corpus not available: %v", err)
	}
	var out []seedEntry
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		val := func(key string) string {
			v := strings.TrimSpace(strings.TrimPrefix(line, key))
			return strings.ReplaceAll(strings.Trim(v, `"`), `\\`, `\`)
		}
		switch {
		case strings.HasPrefix(line, "- file:"):
			out = append(out, seedEntry{file: val("- file:")})
		case len(out) == 0:
		case strings.HasPrefix(line, "folder:"):
			out[len(out)-1].folder = val("folder:")
		case strings.HasPrefix(line, "flags:"):
			out[len(out)-1].flags = strings.Fields(val("flags:"))
		case strings.HasPrefix(line, "date:"):
			out[len(out)-1].date = val("date:")
		}
	}
	if len(out) < 60 {
		t.Fatalf("seed manifest parsed to %d entries", len(out))
	}
	return out
}

// TestSeedCorpus ingests the dev seed mailbox in the manifest order and in
// shuffled orders: the thread partition must not depend on the order, and the
// derived data must reconcile.
func TestSeedCorpus(t *testing.T) {
	entries := loadSeed(t)
	files := map[string][]byte{}
	for _, en := range entries {
		b, err := os.ReadFile(filepath.Join(seedDir, en.file))
		must(t, err)
		files[en.file] = b
	}
	run := func(order []seedEntry) (partition [][]string, threads int) {
		e := newEnv(t)
		for _, en := range order {
			m := e.fetched(en.folder, files[en.file], en.flags...)
			if d, err := time.Parse("02-Jan-2006 15:04:05 -0700", en.date); err == nil {
				m.InternalDate = d
			}
			e.ingest(en.folder, m)
		}
		e.reconcileClean()
		rows, err := e.pool.Query(e.ctx, `SELECT thread_id, size_bytes, message_id_hdr FROM messages WHERE account_id = $1`, e.accountID)
		must(t, err)
		defer rows.Close()
		groups := map[int64][]string{}
		for rows.Next() {
			var tid, size int64
			var id string
			must(t, rows.Scan(&tid, &size, &id))
			groups[tid] = append(groups[tid], id+":"+string(rune('0'+size%10)))
		}
		must(t, rows.Err())
		return normalize(groups), len(groups)
	}
	want, threads := run(entries)
	t.Logf("seed: %d messages in %d threads", len(entries), threads)

	// The first conversation of the manifest (Plan sprintu 47, files 0001-0006).
	found := false
	for _, g := range want {
		if slices.ContainsFunc(g, func(s string) bool { return strings.HasPrefix(s, "seed-t1a@example.com") }) {
			found = true
			if len(g) != 6 {
				t.Errorf("Plan sprintu 47 thread has %d messages (%v), want 6", len(g), g)
			}
		}
	}
	if !found {
		t.Error("conversation seed-t1a not found")
	}

	rng := rand.New(rand.NewSource(7))
	for i := range 5 {
		order := slices.Clone(entries)
		rng.Shuffle(len(order), func(a, b int) { order[a], order[b] = order[b], order[a] })
		got, _ := run(order)
		if !slices.EqualFunc(got, want, slices.Equal) {
			t.Fatalf("shuffle %d produced a different partition:\n got %v\nwant %v", i, got, want)
		}
	}
}
