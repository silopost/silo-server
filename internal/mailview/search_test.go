package mailview_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"mime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/blob"
	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/mailview"
	"github.com/silopost/silo-server/internal/search"
	"github.com/silopost/silo-server/internal/store"
	"github.com/silopost/silo-server/internal/testutil/pgtest"
)

// corpus is a generated mailbox for the search tests.
type corpus struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	uid, acc int64
	folders  map[string]int64
	n        int
}

var (
	senders = []mail.Address{
		{Name: "Jan Kowalski", Email: "jan.kowalski@firma.pl"}, {Name: "Anna Nowak", Email: "anna@nowak.pl"},
		{Name: "Łukasz Żółtowski", Email: "lukasz@zolto.pl"}, {Name: "Zofia Wiśniewska", Email: "zofia@example.com"},
		{Name: "Newsletter", Email: "news@lists.example.com"}, {Name: "", Email: "noreply@bank.pl"},
		{Name: "Grzegorz Brzęczyszczykiewicz", Email: "grzegorz@chrzaszczyrzewoszyce.pl"}, {Name: "Me", Email: "me@silo.test"},
	}
	recipients = []mail.Address{
		{Name: "Me", Email: "me@silo.test"}, {Name: "Piotr Zieliński", Email: "piotr@x.pl"}, {Name: "Ewa", Email: "ewa@firma.pl"},
	}
	subjectWords = []string{"Faktura", "oferta", "Q4", "spotkanie", "Łódź", "raport", "umowa", "najmu", "zażółć",
		"gęślą", "jaźń", "newsletter", "przypomnienie", "Re:", "pilne", "projekt", "Alfa", "2024", "budżet"}
	bodyWords = []string{"cześć", "dzień", "dobry", "w", "załączniku", "przesyłam", "fakturę", "za", "listopad", "umowa",
		"najmu", "lokalu", "spotkanie", "jutro", "o", "10:00", "pozdrawiam", "kod", "ABC-123", "e-mail", "żółw",
		"strasse", "Straße", "naïve", "café", "budżet", "projekt", "alfa", "beta", "1.5", "100%"}
	fileNames = []string{"faktura_2024-11.pdf", "Raport Q4.xlsx", "zdjęcie.JPG", "umowa najmu.docx", "logo.png"}
	folderSet = []struct{ name, role string }{
		{"INBOX", "inbox"}, {"Sent", "sent"}, {"Trash", "trash"}, {"Junk", "junk"}, {"Archive", "archive"},
		{"Faktury", "user"}, {"Snoozed", "snoozed"},
	}
)

func pick[T any](r *rand.Rand, l []T) T { return l[r.Intn(len(l))] }

func words(r *rand.Rand, l []string, n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = pick(r, l)
	}
	return strings.Join(out, " ")
}

func addrHeader(l []mail.Address) string {
	parts := make([]string, len(l))
	for i, a := range l {
		if a.Name == "" {
			parts[i] = a.Email
		} else {
			parts[i] = mime.QEncoding.Encode("utf-8", a.Name) + " <" + a.Email + ">"
		}
	}
	return strings.Join(parts, ", ")
}

func newCorpus(t *testing.T) *corpus {
	t.Helper()
	ctx := context.Background()
	url := pgtest.New(t)
	if err := store.MigrateUp(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	c := &corpus{ctx: ctx, pool: pool, folders: map[string]int64{}}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(pool.QueryRow(ctx, `INSERT INTO users (email, settings) VALUES ('me@silo.test', '{"timezone":"Europe/Warsaw"}') RETURNING id`).Scan(&c.uid))
	must(pool.QueryRow(ctx, `INSERT INTO accounts (user_id, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, secret_enc)
VALUES ($1, 'me@silo.test', 'h', 1, 'none', 'h', 1, 'none', 'me', '\x00') RETURNING id`, c.uid).Scan(&c.acc))
	for _, f := range folderSet {
		var id int64
		must(pool.QueryRow(ctx, `INSERT INTO mailbox_folders (account_id, user_id, name, name_raw, role, uidvalidity) VALUES ($1, $2, $3, $3, $4, 1) RETURNING id`,
			c.acc, c.uid, f.name, f.role).Scan(&id))
		c.folders[f.name] = id
	}
	return c
}

// generate adds n messages: varied senders, Polish subjects and bodies,
// attachments known from BODYSTRUCTURE, list ids, flags, dates (some at the
// edges of days in Europe/Warsaw) and sizes; about 60% get their body.
func (c *corpus) generate(t *testing.T, n int, seed int64) {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	blobs, err := blob.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	edges := []time.Time{
		time.Date(2024, 1, 30, 22, 59, 59, 0, time.UTC), time.Date(2024, 1, 30, 23, 0, 0, 0, time.UTC),
		time.Date(2024, 6, 4, 21, 59, 0, 0, time.UTC), time.Date(2024, 6, 4, 22, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 3, 11, 0, 0, 0, time.UTC), time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC),
	}
	type bodyJob struct {
		id  int64
		raw []byte
	}
	var bodies []bodyJob
	for i := 0; i < n; i++ {
		c.n++
		k := c.n
		from := pick(r, senders)
		to := []mail.Address{pick(r, recipients)}
		var cc []mail.Address
		if r.Intn(3) == 0 {
			cc = append(cc, pick(r, recipients), pick(r, senders))
		}
		subject := words(r, subjectWords, 1+r.Intn(4))
		body := words(r, bodyWords, 3+r.Intn(25))
		date := base.Add(time.Duration(r.Int63n(int64(1035 * 24 * time.Hour))))
		if k <= len(edges) {
			date = edges[k-1]
		}
		var atts []string
		if r.Intn(3) == 0 {
			atts = append(atts, pick(r, fileNames))
			if r.Intn(3) == 0 {
				atts = append(atts, pick(r, fileNames))
			}
		}
		folder := pick(r, folderSet).name
		var flags []string
		if r.Intn(2) == 0 {
			flags = append(flags, string(imap.FlagSeen))
		}
		if r.Intn(5) == 0 {
			flags = append(flags, string(imap.FlagFlagged))
		}
		if r.Intn(8) == 0 {
			flags = append(flags, "$Important")
		}
		var h strings.Builder
		fmt.Fprintf(&h, "From: %s\r\nTo: %s\r\n", addrHeader([]mail.Address{from}), addrHeader(to))
		if len(cc) > 0 {
			fmt.Fprintf(&h, "Cc: %s\r\n", addrHeader(cc))
		}
		fmt.Fprintf(&h, "Subject: %s\r\nMessage-ID: <gen%d@silo.test>\r\nDate: %s\r\nMIME-Version: 1.0\r\n",
			mime.QEncoding.Encode("utf-8", subject), k, date.Format(time.RFC1123Z))
		if from.Email == "news@lists.example.com" {
			h.WriteString("List-Id: Newsletter <news.lists.example.com>\r\n")
		}
		var bs imap.BodyStructure
		var raw strings.Builder
		if len(atts) == 0 {
			h.WriteString("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
			raw.WriteString(h.String() + body + "\r\n")
			bs = &imap.BodyStructureSinglePart{Type: "text", Subtype: "plain", Params: map[string]string{"charset": "utf-8"}, Encoding: "8bit", Size: uint32(len(body))}
		} else {
			h.WriteString("Content-Type: multipart/mixed; boundary=\"b\"\r\n\r\n")
			raw.WriteString(h.String() + "--b\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + body + "\r\n")
			mp := &imap.BodyStructureMultiPart{Subtype: "mixed", Children: []imap.BodyStructure{
				&imap.BodyStructureSinglePart{Type: "text", Subtype: "plain", Params: map[string]string{"charset": "utf-8"}, Encoding: "8bit", Size: uint32(len(body))},
			}}
			for _, fn := range atts {
				data := base64.StdEncoding.EncodeToString([]byte("data of " + fn))
				fmt.Fprintf(&raw, "--b\r\nContent-Type: application/octet-stream; name=\"%s\"\r\nContent-Disposition: attachment; filename=\"%s\"\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n",
					mime.QEncoding.Encode("utf-8", fn), mime.QEncoding.Encode("utf-8", fn), data)
				mp.Children = append(mp.Children, &imap.BodyStructureSinglePart{Type: "application", Subtype: "octet-stream",
					Params: map[string]string{"name": fn}, Encoding: "base64", Size: uint32(len(data)),
					Extended: &imap.BodyStructureSinglePartExt{Disposition: &imap.BodyStructureDisposition{Value: "attachment", Params: map[string]string{"filename": fn}}}})
			}
			raw.WriteString("--b--\r\n")
			bs = mp
		}
		size := int64(1000 + r.Intn(200_000))
		if r.Intn(6) == 0 {
			size = int64(1+r.Intn(12)) << 20
		}
		header := []byte(h.String())
		err := pgx.BeginFunc(c.ctx, c.pool, func(tx pgx.Tx) error {
			res, err := mail.IngestBatch(c.ctx, tx, c.acc, c.folders[folder], []mail.FetchedMessage{{
				UID: uint32(k), Flags: flags, InternalDate: date, Size: size, Header: header, BodyStructure: bs,
			}})
			if err == nil && r.Intn(10) < 6 {
				bodies = append(bodies, bodyJob{id: res.Messages[0].MessageID, raw: []byte(raw.String())})
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range bodies {
		if err := pgx.BeginFunc(c.ctx, c.pool, func(tx pgx.Tx) error {
			_, err := mail.IngestBody(c.ctx, tx, blobs, b.id, b.raw)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	// User labels (local only) and Bcc of a few messages (as a sent draft has).
	for _, name := range []string{"Projekt Alfa", "Work/Projects", "pilne"} {
		if _, err := c.pool.Exec(c.ctx, `INSERT INTO labels (user_id, account_id, name, type) VALUES ($1, $2, $3, 'user') ON CONFLICT DO NOTHING`, c.uid, c.acc, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.pool.Exec(c.ctx, `
INSERT INTO message_labels (message_id, label_id, user_id)
SELECT m.id, l.id, m.user_id FROM messages m JOIN labels l ON l.account_id = m.account_id AND l.type = 'user'
WHERE m.account_id = $1 AND (m.id * 7 + l.id * 3) % 5 = 0
ON CONFLICT DO NOTHING`, c.acc); err != nil {
		t.Fatal(err)
	}
	if _, err := c.pool.Exec(c.ctx, `UPDATE messages SET bcc_addrs = '[{"name":"Ukryty Odbiorca","email":"ukryty@x.pl"}]' WHERE account_id = $1 AND id % 9 = 0`, c.acc); err != nil {
		t.Fatal(err)
	}
}

// views loads every message of the account as the matcher sees it.
func (c *corpus) views(t *testing.T) map[int64]search.MessageView {
	t.Helper()
	rows, err := c.pool.Query(c.ctx, `
SELECT m.id, m.subject, m.from_text, m.recipients_text, m.cc_addrs, m.bcc_addrs, COALESCE(m.list_id, ''), m.message_id_hdr,
       m.internal_date, m.size_bytes, m.has_attachments, m.is_unread, m.body_text,
       ARRAY(SELECT a.filename FROM attachments a WHERE a.message_id = m.id ORDER BY a.id)::text[],
       ARRAY(SELECT l.system_key FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE ml.message_id = m.id AND l.type = 'system')::text[],
       ARRAY(SELECT l.name FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE ml.message_id = m.id AND l.type = 'folder')::text[],
       ARRAY(SELECT l.name FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE ml.message_id = m.id AND l.type = 'user')::text[]
FROM messages m WHERE m.account_id = $1 AND m.orphaned_at IS NULL`, c.acc)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]search.MessageView{}
	for rows.Next() {
		var id int64
		var v search.MessageView
		var cc, bcc []byte
		if err := rows.Scan(&id, &v.Subject, &v.FromText, &v.RecipientsText, &cc, &bcc, &v.ListID, &v.MessageIDHdr, &v.Date, &v.Size,
			&v.HasAttachments, &v.Unread, &v.Body, &v.Attachments, &v.SystemLabels, &v.FolderLabels, &v.UserLabels); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(cc, &v.Cc)
		_ = json.Unmarshal(bcc, &v.Bcc)
		out[id] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (c *corpus) sqlIDs(t *testing.T, a search.AST, s search.Scope) []int64 {
	t.Helper()
	s.ArgOffset = 2
	where, args, err := search.ToSQL(a, s)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := c.pool.Query(c.ctx, `SELECT m.id FROM messages m WHERE m.user_id = $1 AND m.account_id = $2 AND m.orphaned_at IS NULL AND `+where+` ORDER BY m.id`,
		append([]any{c.uid, c.acc}, args...)...)
	if err != nil {
		t.Fatalf("%s: %v\n%s", a, err, where)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// propertyQueries cover every operator; SQL and the matcher must agree on
// each of them over the corpus.
var propertyQueries = []string{
	"faktura", "Faktura", "fakt", "faktur", "zażółć", "zazolc", "Łódź", "lodz", "żółw", "strasse", "Straße", "naive", "cafe",
	`"umowa najmu"`, `"najmu umowa"`, `"dzień dobry"`, "abc-123", "abc", "1.5", "e-mail", "100%",
	"faktura OR raport", "faktura | spotkanie", "{oferta raport umowa}", "-faktura", "-{faktura oferta}", "faktura -raport",
	"(faktura OR oferta) -pilne", "faktura AND budżet",
	"from:jan", "from:kowalski", "from:ŁUKASZ", "from:\"Anna Nowak\"", "from:(jan OR zofia)", "-from:me@silo.test",
	"to:piotr", "to:me", "cc:ewa", "cc:\"Piotr Zieliński\"", "bcc:ukryty", "-bcc:ukryty", "deliveredto:firma.pl",
	"subject:faktura", "subject:(oferta q4)", "subject:\"umowa najmu\"", "subject:zazolc", "-subject:re",
	"has:attachment", "-has:attachment", "has:attachment -zdjecie", "has:userlabels", "has:nouserlabels",
	"filename:pdf", "filename:jpg", "filename:\"umowa najmu\"", "filename:zdjęcie", "filename:raport", "zdjecie", "docx",
	"label:pilne", "label:projekt-alfa", "label:work-projects", "label:work/projects", "label:faktury", "label:nieznana",
	"in:inbox", "in:sent", "in:archive", "in:spam", "in:trash", "-in:trash", "in:anywhere", "in:anywhere faktura",
	"in:faktury", "in:snoozed", "label:unread", "is:unread", "is:read", "is:starred", "is:important", "is:snoozed",
	"before:2024/01/31", "after:2024/01/31", "before:2024-06-05", "after:2024/6/5", "older:2025/01/01", "newer:2026/01/01",
	"older_than:2d", "newer_than:2d", "older_than:1y", "newer_than:6m", "before:2024/13/40",
	"larger:5M", "smaller:100K", "larger:1G", "larger:50000 smaller:150000", "larger:big",
	"list:lists.example.com", "-list:example", "rfc822msgid:<gen7@silo.test>", "rfc822msgid:gen8@silo.test",
	"from:jan has:attachment is:unread", "{from:anna to:piotr} -in:inbox", "foo:bar", "@@@", "jan.kowalski@firma.pl",
}

// TestSearchSQLMatchesMatcher is the DoD property test of phase 4 (plan
// 3.12/3.11): over a generated corpus, the ids SQL finds for a query are the
// ids the in-memory matcher accepts.
func TestSearchSQLMatchesMatcher(t *testing.T) {
	c := newCorpus(t)
	c.generate(t, 240, 42)
	v := mailview.New(c.pool, nil, nil)
	scope, err := v.SearchScope(c.ctx, c.uid, c.acc)
	if err != nil {
		t.Fatal(err)
	}
	if scope.Location.String() != "Europe/Warsaw" {
		t.Fatalf("location = %v", scope.Location)
	}
	scope.Now = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	views := c.views(t)
	if len(views) < 200 {
		t.Fatalf("corpus has %d messages", len(views))
	}
	m := search.Matcher{Location: scope.Location, Now: scope.Now}
	nontrivial := 0
	for _, q := range propertyQueries {
		a, err := search.Parse(q)
		if err != nil {
			t.Fatalf("Parse(%q): %v", q, err)
		}
		got := c.sqlIDs(t, a, scope)
		var want []int64
		for id, mv := range views {
			if m.Match(a, mv) {
				want = append(want, id)
			}
		}
		slices.Sort(want)
		if !slices.Equal(got, want) {
			only := func(a, b []int64) []int64 {
				var out []int64
				for _, x := range a {
					if !slices.Contains(b, x) {
						out = append(out, x)
					}
				}
				return out
			}
			t.Errorf("%q: SQL %d ids, matcher %d; only SQL %v, only matcher %v", q, len(got), len(want), only(got, want), only(want, got))
		}
		t.Logf("%-40q %d", q, len(got))
		if len(got) > 0 && len(got) < len(views) {
			nontrivial++
		}
	}
	if len(propertyQueries) < 40 || nontrivial < 60 {
		t.Errorf("%d queries, %d with a non-trivial result", len(propertyQueries), nontrivial)
	}

	// has:attachment finds messages whose body is not downloaded.
	n := 0
	if err := c.pool.QueryRow(c.ctx, `SELECT count(*) FROM messages WHERE account_id = $1 AND body_state = 'headers' AND has_attachments`, c.acc).Scan(&n); err != nil || n == 0 {
		t.Fatalf("no header-only message with an attachment (%v)", err)
	}
	ids := c.sqlIDs(t, search.MustParse("has:attachment in:anywhere"), scope)
	var headerOnly int
	if err := c.pool.QueryRow(c.ctx, `SELECT count(*) FROM messages WHERE id = ANY($1) AND body_state = 'headers'`, ids).Scan(&headerOnly); err != nil || headerOnly != n {
		t.Errorf("has:attachment found %d of %d header-only messages with attachments (%v)", headerOnly, n, err)
	}
}

// Rows indexed by an older tokenizer are re-indexed by mail.ReindexSearch.
func TestReindexSearch(t *testing.T) {
	c := newCorpus(t)
	c.generate(t, 30, 7)
	before := c.sqlIDs(t, search.MustParse("in:anywhere faktura OR from:jan OR zazolc"), search.Scope{})
	if _, err := c.pool.Exec(c.ctx, `UPDATE messages SET search_tsv = '', search_ver = 0`); err != nil {
		t.Fatal(err)
	}
	if got := c.sqlIDs(t, search.MustParse("in:anywhere faktura OR zazolc"), search.Scope{}); len(got) != 0 {
		t.Fatalf("cleared index still matches %v", got)
	}
	if err := mail.ReindexSearch(c.ctx, c.pool, nil); err != nil {
		t.Fatal(err)
	}
	if after := c.sqlIDs(t, search.MustParse("in:anywhere faktura OR from:jan OR zazolc"), search.Scope{}); !slices.Equal(before, after) || len(after) == 0 {
		t.Errorf("after reindex %v, before %v", after, before)
	}
	var stale int
	if err := c.pool.QueryRow(c.ctx, `SELECT count(*) FROM messages WHERE search_ver < $1`, mail.SearchVersion).Scan(&stale); err != nil || stale != 0 {
		t.Errorf("stale rows = %d (%v)", stale, err)
	}
}
