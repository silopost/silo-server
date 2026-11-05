package mail_test

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	silomail "github.com/silopost/silo-server/internal/mail"
)

// TestRandomOperationsReconcile applies random sequences of server-side
// changes (new messages, duplicates, moves, flag changes, keyword changes,
// expunges, orphan purges) and requires Reconcile to find no drift.
func TestRandomOperationsReconcile(t *testing.T) {
	folders := []string{"INBOX", "Sent", "Trash", "Junk", "Archive", "Faktury", "Projekty"}
	flagSets := [][]string{nil, {`\Seen`}, {`\Seen`, `\Flagged`}, {`\Flagged`}, {`\Seen`, "$Important"}, {"Praca"}, {`\Seen`, "Praca", "$label2"}, {`\Draft`, `\Seen`}}
	for seed := int64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			e := newEnv(t)
			// A pool of related messages: three conversations of four messages.
			var pool [][]byte
			for c := 0; c < 3; c++ {
				prev := ""
				for i := 0; i < 4; i++ {
					id := fmt.Sprintf("p%d-%d@example.com", c, i)
					extra := []string{}
					subj := fmt.Sprintf("Rozmowa %d", c)
					if prev != "" {
						extra = append(extra, "In-Reply-To", "<"+prev+">", "References", "<"+prev+">")
						subj = "Re: " + subj
					}
					from := "anna@example.com"
					if i%2 == 1 {
						from = "dev@silo.test"
					}
					pool = append(pool, simple(from, "bob@example.com", subj, id, baseTime.Add(time.Duration(c*100+i)*time.Hour), extra...))
					prev = id
				}
			}
			type locKey struct {
				folder string
				uid    uint32
			}
			locs := func() []locKey {
				rows, err := e.pool.Query(e.ctx, `SELECT f.name, l.uid FROM message_locations l JOIN mailbox_folders f ON f.id = l.folder_id WHERE l.account_id = $1 ORDER BY f.name, l.uid`, e.accountID)
				must(t, err)
				defer rows.Close()
				var out []locKey
				for rows.Next() {
					var k locKey
					var uid int64
					must(t, rows.Scan(&k.folder, &uid))
					k.uid = uint32(uid)
					out = append(out, k)
				}
				return out
			}
			for step := 0; step < 120; step++ {
				switch op := rng.Intn(10); {
				case op < 4: // a message (possibly a known one) appears in a folder
					raw := pool[rng.Intn(len(pool))]
					f := folders[rng.Intn(len(folders))]
					e.ingest(f, e.fetched(f, raw, flagSets[rng.Intn(len(flagSets))]...))
				case op < 7: // flag change by another client
					l := locs()
					if len(l) == 0 {
						continue
					}
					k := l[rng.Intn(len(l))]
					fl := flagSets[rng.Intn(len(flagSets))]
					must(t, e.tx(func(tx pgx.Tx) error {
						return silomail.ApplyFlags(e.ctx, tx, e.folders[k.folder], k.uid, fl, uint64(step+10))
					}))
				case op < 9: // expunge
					l := locs()
					if len(l) == 0 {
						continue
					}
					k := l[rng.Intn(len(l))]
					must(t, e.tx(func(tx pgx.Tx) error {
						_, err := silomail.LocationVanished(e.ctx, tx, e.folders[k.folder], []uint32{k.uid})
						return err
					}))
				default: // orphan gc
					must(t, e.tx(func(tx pgx.Tx) error {
						_, err := silomail.PurgeOrphans(e.ctx, tx, e.accountID, time.Now().Add(time.Minute))
						return err
					}))
				}
				if step%15 == 0 {
					e.reconcileClean()
				}
			}
			e.reconcileClean()
		})
	}
}
