package mail_test

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net/mail"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	silomail "github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/store"
	"github.com/silopost/silo-server/internal/testutil/pgtest"
)

// One Postgres container is shared by all tests of the package; every test
// works on its own user and account.
var (
	testPool    *pgxpool.Pool
	dockerError error
	envSeq      atomic.Int64
)

func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if !testing.Short() {
		ctx := context.Background()
		url, terminate, err := pgtest.Run(ctx)
		if err != nil {
			dockerError = err
		} else {
			defer terminate()
			if err := store.MigrateUp(ctx, url); err != nil {
				fmt.Fprintln(os.Stderr, "migrate:", err)
				return 1
			}
			if testPool, err = store.Connect(ctx, url); err != nil {
				fmt.Fprintln(os.Stderr, "connect:", err)
				return 1
			}
			defer testPool.Close()
		}
	}
	return m.Run()
}

// env is a fresh user with one account and a standard set of folders.
type env struct {
	t         *testing.T
	ctx       context.Context
	pool      *pgxpool.Pool
	userID    int64
	accountID int64
	email     string
	folders   map[string]int64
	nextUID   map[string]uint32
}

var folderRoles = []struct{ name, role string }{
	{"INBOX", "inbox"}, {"Sent", "sent"}, {"Drafts", "drafts"}, {"Trash", "trash"},
	{"Junk", "junk"}, {"Archive", "archive"}, {"Faktury", "user"}, {"Projekty", "user"},
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped with -short")
	}
	if testPool == nil {
		if os.Getenv("SILO_REQUIRE_DOCKER") == "true" {
			t.Fatalf("postgres container unavailable: %v", dockerError)
		}
		t.Skipf("docker unavailable: %v", dockerError)
	}
	n := envSeq.Add(1)
	e := &env{t: t, ctx: context.Background(), pool: testPool, folders: map[string]int64{}, nextUID: map[string]uint32{},
		email: "dev@silo.test"}
	must(t, e.pool.QueryRow(e.ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`, fmt.Sprintf("user%d@silo.test", n)).Scan(&e.userID))
	must(t, e.pool.QueryRow(e.ctx, `
INSERT INTO accounts (user_id, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, secret_enc)
VALUES ($1, $2, 'imap.test', 993, 'implicit', 'smtp.test', 465, 'implicit', $3, '\x00') RETURNING id`,
		e.userID, e.email, fmt.Sprintf("acc%d", n)).Scan(&e.accountID))
	for _, f := range folderRoles {
		var id int64
		must(t, e.pool.QueryRow(e.ctx, `
INSERT INTO mailbox_folders (account_id, user_id, name, name_raw, role, uidvalidity) VALUES ($1, $2, $3, $3, $4, 1) RETURNING id`,
			e.accountID, e.userID, f.name, f.role).Scan(&id))
		e.folders[f.name] = id
	}
	must(t, e.tx(func(tx pgx.Tx) error { return silomail.EnsureSystemLabels(e.ctx, tx, e.accountID) }))
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// tx runs fn in a transaction and commits it (rolls back on error).
func (e *env) tx(fn func(tx pgx.Tx) error) error {
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(e.ctx)
		return err
	}
	return tx.Commit(e.ctx)
}

// uid hands out increasing UIDs per folder.
func (e *env) uid(folder string) uint32 {
	e.nextUID[folder]++
	return e.nextUID[folder]
}

// ingest ingests messages into a folder in one transaction.
func (e *env) ingest(folder string, msgs ...silomail.FetchedMessage) *silomail.IngestResult {
	e.t.Helper()
	var res *silomail.IngestResult
	must(e.t, e.tx(func(tx pgx.Tx) error {
		var err error
		res, err = silomail.IngestBatch(e.ctx, tx, e.accountID, e.folders[folder], msgs)
		return err
	}))
	return res
}

// fetched builds the FETCH data of a raw message.
func (e *env) fetched(folder string, raw []byte, flags ...string) silomail.FetchedMessage {
	hdr := raw
	body := []byte(nil)
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		hdr, body = raw[:i+4], raw[i+4:]
	} else if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		hdr, body = raw[:i+2], raw[i+2:]
	}
	var date time.Time
	if m, err := mail.ReadMessage(bytes.NewReader(raw)); err == nil {
		date, _ = m.Header.Date()
	}
	if len(body) > 300 {
		body = body[:300]
	}
	return silomail.FetchedMessage{
		UID: e.uid(folder), Flags: flags, InternalDate: date, Size: int64(len(raw)),
		Header: hdr, TextSnippet: body,
	}
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	must(e.t, e.pool.QueryRow(e.ctx, query, args...).Scan(&n))
	return n
}

func (e *env) labelID(key string) int64 {
	e.t.Helper()
	var id int64
	must(e.t, e.pool.QueryRow(e.ctx, `SELECT id FROM labels WHERE account_id = $1 AND system_key = $2`, e.accountID, key).Scan(&id))
	return id
}

// messageLabels returns the names of all labels of a message, sorted.
func (e *env) messageLabels(msgID int64) []string {
	e.t.Helper()
	return e.names(`SELECT l.name FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE ml.message_id = $1 ORDER BY l.name`, msgID)
}

func (e *env) names(query string, args ...any) []string {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, query, args...)
	must(e.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		must(e.t, rows.Scan(&s))
		out = append(out, s)
	}
	must(e.t, rows.Err())
	return out
}

// reconcile fails the test if Reconcile reports anything.
func (e *env) reconcileClean() {
	e.t.Helper()
	ds, err := silomail.Reconcile(e.ctx, e.pool, e.accountID)
	must(e.t, err)
	for _, d := range ds {
		e.t.Errorf("reconcile: %s", d)
	}
}

// eml builds a simple text message.
func eml(headers map[string]string, order []string, body string) []byte {
	var b bytes.Buffer
	for _, k := range order {
		fmt.Fprintf(&b, "%s: %s\r\n", k, headers[k])
	}
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", body)
	return b.Bytes()
}

// simple builds a message with the usual headers.
func simple(from, to, subject, id string, date time.Time, extra ...string) []byte {
	h := map[string]string{
		"From": from, "To": to, "Subject": subject,
		"Date": date.UTC().Format(time.RFC1123Z),
	}
	order := []string{"From", "To", "Subject", "Date"}
	if id != "" {
		h["Message-ID"] = "<" + id + ">"
		order = append(order, "Message-ID")
	}
	for i := 0; i+1 < len(extra); i += 2 {
		h[extra[i]] = extra[i+1]
		order = append(order, extra[i])
	}
	return eml(h, order, "Hello "+id)
}

var baseTime = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
