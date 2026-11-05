package mail_test

import (
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	silomail "github.com/silopost/silo-server/internal/mail"
)

func (e *env) one(from, to, subject, id string, offset time.Duration) []byte {
	return simple(from, to, subject, id, baseTime.Add(offset))
}

func (e *env) messageIDByHdr(id string) int64 {
	e.t.Helper()
	var mid int64
	must(e.t, e.pool.QueryRow(e.ctx, `SELECT id FROM messages WHERE account_id = $1 AND message_id_hdr = $2 ORDER BY id LIMIT 1`, e.accountID, id).Scan(&mid))
	return mid
}

func TestDeriveSystemLabelsRolesAndFlags(t *testing.T) {
	cases := []struct {
		folder string
		flags  []string
		want   []string
		unread bool
	}{
		{"INBOX", nil, []string{"INBOX"}, true},
		{"INBOX", []string{`\Seen`}, []string{"INBOX"}, false},
		{"INBOX", []string{`\Seen`, `\Flagged`}, []string{"INBOX", "STARRED"}, false},
		{"INBOX", []string{`\Seen`, "$Important"}, []string{"IMPORTANT", "INBOX"}, false},
		{"Sent", []string{`\Seen`}, []string{"SENT"}, false},
		{"Drafts", []string{`\Seen`, `\Draft`}, []string{"DRAFT"}, false},
		{"Trash", []string{`\Seen`}, []string{"TRASH"}, false},
		{"Junk", nil, []string{"SPAM"}, true},
		{"Archive", []string{`\Seen`}, []string{"ARCHIVE"}, false},
		{"Faktury", []string{`\Seen`}, []string{"Faktury"}, false},
		{"INBOX", []string{`\Seen`, `\Answered`, "$Forwarded", "$MDNSent", "$Junk", "NonJunk"}, []string{"INBOX"}, false}, // no labels from convention flags
	}
	e := newEnv(t)
	for i, c := range cases {
		id := "derive" + string(rune('a'+i)) + "@example.com"
		e.ingest(c.folder, e.fetched(c.folder, e.one("a@example.com", "dev@silo.test", "Derive "+id, id, time.Duration(i)*time.Hour), c.flags...))
		mid := e.messageIDByHdr(id)
		got := e.messageLabels(mid)
		if !slices.Equal(got, c.want) {
			t.Errorf("case %d (%s %v): labels = %v, want %v", i, c.folder, c.flags, got, c.want)
		}
		var unread bool
		must(t, e.pool.QueryRow(e.ctx, `SELECT is_unread FROM messages WHERE id = $1`, mid).Scan(&unread))
		if unread != c.unread {
			t.Errorf("case %d: is_unread = %v, want %v", i, unread, c.unread)
		}
	}
	// The user folder got a folder label linked from mailbox_folders.
	if n := e.count(`SELECT count(*) FROM labels l JOIN mailbox_folders f ON f.label_id = l.id WHERE l.type = 'folder' AND f.account_id = $1`, e.accountID); n != 1 {
		t.Errorf("folder labels = %d, want 1", n)
	}
	e.reconcileClean()
}

func TestSeenFlagChangeUpdatesUnread(t *testing.T) {
	e := newEnv(t)
	m := e.fetched("INBOX", e.one("a@example.com", "dev@silo.test", "Flags", "flags1@example.com", 0))
	e.ingest("INBOX", m)
	mid := e.messageIDByHdr("flags1@example.com")
	unread := func() bool {
		var u bool
		must(t, e.pool.QueryRow(e.ctx, `SELECT unread_count > 0 FROM thread_labels tl JOIN messages m ON m.thread_id = tl.thread_id WHERE m.id = $1 AND tl.label_id = $2`, mid, e.labelID("INBOX")).Scan(&u))
		return u
	}
	if !unread() {
		t.Fatal("new message should be unread in INBOX")
	}
	must(t, e.tx(func(tx pgx.Tx) error {
		return silomail.ApplyFlags(e.ctx, tx, e.folders["INBOX"], m.UID, []string{`\Seen`, `\Flagged`}, 7)
	}))
	if unread() {
		t.Fatal("thread still unread after \\Seen")
	}
	if got := e.messageLabels(mid); !slices.Equal(got, []string{"INBOX", "STARRED"}) {
		t.Fatalf("labels = %v", got)
	}
	var modseq int64
	must(t, e.pool.QueryRow(e.ctx, `SELECT modseq FROM message_locations WHERE folder_id = $1 AND uid = $2`, e.folders["INBOX"], m.UID).Scan(&modseq))
	if modseq != 7 {
		t.Fatalf("modseq = %d", modseq)
	}
	// Unknown UIDs are reported back to the caller.
	var res *silomail.FlagsResult
	must(t, e.tx(func(tx pgx.Tx) (err error) {
		res, err = silomail.ApplyFlagsBatch(e.ctx, tx, e.folders["INBOX"], []silomail.FlagUpdate{{UID: 999, Flags: []string{`\Seen`}}, {UID: m.UID, Flags: []string{`\Flagged`, `\Seen`}, ModSeq: 7}})
		return err
	}))
	if !slices.Equal(res.Unknown, []uint32{999}) || res.Changed != 0 {
		t.Fatalf("result = %+v", res)
	}
	// Unseen again.
	must(t, e.tx(func(tx pgx.Tx) error { return silomail.ApplyFlags(e.ctx, tx, e.folders["INBOX"], m.UID, nil, 8) }))
	if !unread() {
		t.Fatal("thread should be unread again")
	}
	if got := e.messageLabels(mid); !slices.Equal(got, []string{"INBOX"}) {
		t.Fatalf("labels = %v", got)
	}
	e.reconcileClean()
}

// A message that moves INBOX -> Faktury (UID vanishes, new UID appears later)
// keeps its id, thread and user labels.
func TestMoveKeepsIdentityAndUserLabels(t *testing.T) {
	e := newEnv(t)
	raw := e.one("a@example.com", "dev@silo.test", "Przeniesienie", "move1@example.com", 0)
	in := e.fetched("INBOX", raw, `\Seen`, "Praca")
	e.ingest("INBOX", in)
	mid := e.messageIDByHdr("move1@example.com")
	var threadID int64
	must(t, e.pool.QueryRow(e.ctx, `SELECT thread_id FROM messages WHERE id = $1`, mid).Scan(&threadID))
	if got := e.messageLabels(mid); !slices.Equal(got, []string{"INBOX", "Praca"}) {
		t.Fatalf("labels before = %v", got)
	}
	// A local-only label added by the user must survive too.
	var userLabel int64
	must(t, e.pool.QueryRow(e.ctx, `INSERT INTO labels (user_id, account_id, name, type) VALUES ($1, $2, 'Lokalna', 'user') RETURNING id`, e.userID, e.accountID).Scan(&userLabel))
	_, err := e.pool.Exec(e.ctx, `INSERT INTO message_labels (message_id, label_id, user_id) VALUES ($1, $2, $3)`, mid, userLabel, e.userID)
	must(t, err)

	// 1. the location vanishes
	var vr *silomail.VanishedResult
	must(t, e.tx(func(tx pgx.Tx) (err error) {
		vr, err = silomail.LocationVanished(e.ctx, tx, e.folders["INBOX"], []uint32{in.UID})
		return err
	}))
	if vr.Removed != 1 || vr.Orphaned != 1 {
		t.Fatalf("vanished = %+v", vr)
	}
	if got := e.messageLabels(mid); !slices.Equal(got, []string{"Lokalna", "Praca"}) {
		t.Fatalf("labels while orphaned = %v (system labels must go, user labels stay)", got)
	}
	if n := e.count(`SELECT count(*) FROM thread_labels WHERE thread_id = $1`, threadID); n != 0 {
		t.Fatalf("orphaned message still in %d thread_labels", n)
	}
	if n := e.count(`SELECT message_count FROM threads WHERE id = $1`, threadID); n != 0 {
		t.Fatalf("thread message_count = %d", n)
	}
	e.reconcileClean()

	// 2. it shows up in Faktury under a new UID (matched by dedupe_key)
	moved := e.fetched("Faktury", raw, `\Seen`, "Praca")
	res := e.ingest("Faktury", moved)
	if got := res.Messages[0]; got.MessageID != mid || got.ThreadID != threadID || got.Outcome != silomail.OutcomeMoved {
		t.Fatalf("result = %+v, want message %d thread %d moved", got, mid, threadID)
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE account_id = $1`, e.accountID); n != 1 {
		t.Fatalf("messages = %d", n)
	}
	if got := e.messageLabels(mid); !slices.Equal(got, []string{"Faktury", "Lokalna", "Praca"}) {
		t.Fatalf("labels after move = %v", got)
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE id = $1 AND orphaned_at IS NULL`, mid); n != 1 {
		t.Fatal("orphaned_at not cleared")
	}
	e.reconcileClean()
}

func TestDuplicateLocationUnionOfLabels(t *testing.T) {
	e := newEnv(t)
	raw := e.one("a@example.com", "dev@silo.test", "Dwa foldery", "dup1@example.com", 0)
	e.ingest("INBOX", e.fetched("INBOX", raw))
	res := e.ingest("Archive", e.fetched("Archive", raw, `\Seen`))
	if res.Messages[0].Outcome != silomail.OutcomeDuplicate {
		t.Fatalf("outcome = %v", res.Messages[0].Outcome)
	}
	mid := e.messageIDByHdr("dup1@example.com")
	if got := e.messageLabels(mid); !slices.Equal(got, []string{"ARCHIVE", "INBOX"}) {
		t.Fatalf("labels = %v", got)
	}
	// Unread while the INBOX copy is unseen; vanishing of it leaves ARCHIVE.
	var unread bool
	must(t, e.pool.QueryRow(e.ctx, `SELECT is_unread FROM messages WHERE id = $1`, mid).Scan(&unread))
	if !unread {
		t.Fatal("unseen copy should make the message unread")
	}
	must(t, e.tx(func(tx pgx.Tx) error {
		_, err := silomail.LocationVanished(e.ctx, tx, e.folders["INBOX"], []uint32{1})
		return err
	}))
	if got := e.messageLabels(mid); !slices.Equal(got, []string{"ARCHIVE"}) {
		t.Fatalf("labels = %v", got)
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE id = $1 AND orphaned_at IS NULL`, mid); n != 1 {
		t.Fatal("message with a remaining location must not be orphaned")
	}
	e.reconcileClean()
}

func TestKnownMessageIDAndEmailID(t *testing.T) {
	e := newEnv(t)
	raw := e.one("a@example.com", "dev@silo.test", "Matcher", "match1@example.com", 0)
	first := e.fetched("INBOX", raw)
	first.EmailID = "email-1"
	e.ingest("INBOX", first)
	mid := e.messageIDByHdr("match1@example.com")
	// Different bytes (hence size and dedupe_key) but the same EMAILID: same message.
	other := e.one("a@example.com", "dev@silo.test", "Matcher", "match1@example.com", 0)
	other = append(other, "extra line\r\n"...)
	f := e.fetched("Archive", other)
	f.EmailID = "email-1"
	res := e.ingest("Archive", f)
	if res.Messages[0].MessageID != mid || res.Messages[0].Outcome != silomail.OutcomeDuplicate {
		t.Fatalf("emailid match = %+v", res.Messages[0])
	}
	// Our own COPYUID-style operation: known message id for an unrelated blob.
	g := e.fetched("Faktury", e.one("x@example.com", "dev@silo.test", "Whatever", "other@example.com", 0))
	g.KnownMessageID = mid
	res = e.ingest("Faktury", g)
	if res.Messages[0].MessageID != mid {
		t.Fatalf("known id match = %+v", res.Messages[0])
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE account_id = $1`, e.accountID); n != 1 {
		t.Fatalf("messages = %d", n)
	}
	e.reconcileClean()
}

func TestUIDReuseReplacesLocation(t *testing.T) {
	e := newEnv(t)
	a := e.fetched("INBOX", e.one("a@example.com", "dev@silo.test", "Pierwsza", "reuse1@example.com", 0))
	e.ingest("INBOX", a)
	b := e.fetched("INBOX", e.one("b@example.com", "dev@silo.test", "Druga", "reuse2@example.com", time.Hour))
	b.UID = a.UID // the server reused the UID for another message
	e.ingest("INBOX", b)
	if n := e.count(`SELECT count(*) FROM message_locations WHERE folder_id = $1`, e.folders["INBOX"]); n != 1 {
		t.Fatalf("locations = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE account_id = $1 AND orphaned_at IS NOT NULL`, e.accountID); n != 1 {
		t.Fatalf("orphaned = %d, want the first message", n)
	}
	e.reconcileClean()
}

func TestKeywordsImportAndRemoval(t *testing.T) {
	e := newEnv(t)
	m := e.fetched("INBOX", e.one("a@example.com", "dev@silo.test", "Tagi", "kw1@example.com", 0),
		`\Seen`, "$label1", "$label3", "Klient_ACME", "$Junk", "$Forwarded", "$SiloSnoozed", "NonJunk", `\Answered`)
	e.ingest("INBOX", m)
	mid := e.messageIDByHdr("kw1@example.com")
	want := []string{"INBOX", "Klient_ACME", "Osobiste", "Ważne"}
	if got := e.messageLabels(mid); !slices.Equal(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	if n := e.count(`SELECT count(*) FROM labels WHERE account_id = $1 AND type = 'user'`, e.accountID); n != 3 {
		t.Fatalf("user labels = %d (ignored keywords must not create labels)", n)
	}
	var kw, state string
	must(t, e.pool.QueryRow(e.ctx, `SELECT imap_keyword, keyword_state FROM labels WHERE account_id = $1 AND name = 'Ważne'`, e.accountID).Scan(&kw, &state))
	if kw != "$label1" || state != "mirrored" {
		t.Fatalf("keyword = %q %q", kw, state)
	}
	// Another client removes the keyword: the label goes away on this message.
	must(t, e.tx(func(tx pgx.Tx) error {
		return silomail.ApplyFlags(e.ctx, tx, e.folders["INBOX"], m.UID, []string{`\Seen`, "$label1"}, 3)
	}))
	if got := e.messageLabels(mid); !slices.Equal(got, []string{"INBOX", "Ważne"}) {
		t.Fatalf("labels after keyword removal = %v", got)
	}
	// A label is created only once per account.
	e.ingest("INBOX", e.fetched("INBOX", e.one("a@example.com", "dev@silo.test", "Tagi 2", "kw2@example.com", time.Hour), "$LABEL1"))
	if n := e.count(`SELECT count(*) FROM labels WHERE account_id = $1 AND type = 'user'`, e.accountID); n != 3 {
		t.Fatalf("user labels = %d, keyword matching must be case-insensitive", n)
	}
	e.reconcileClean()
}

func TestKeywordAdoptsLocalLabelAndPrefix(t *testing.T) {
	e := newEnv(t)
	_, err := e.pool.Exec(e.ctx, `UPDATE accounts SET keyword_prefix = '$Silo.' WHERE id = $1`, e.accountID)
	must(t, err)
	_, err = e.pool.Exec(e.ctx, `INSERT INTO labels (user_id, account_id, name, type) VALUES ($1, $2, 'Faktury 2026', 'user')`, e.userID, e.accountID)
	must(t, err)
	e.ingest("INBOX",
		e.fetched("INBOX", e.one("a@example.com", "dev@silo.test", "A", "pre1@example.com", 0), `$Silo.Faktury 2026`),
		e.fetched("INBOX", e.one("a@example.com", "dev@silo.test", "B", "pre2@example.com", time.Hour), `$Silo.Nowa`))
	if n := e.count(`SELECT count(*) FROM labels WHERE account_id = $1 AND type = 'user'`, e.accountID); n != 2 {
		t.Fatalf("user labels = %d, want the local one adopted plus Nowa", n)
	}
	var kw string
	must(t, e.pool.QueryRow(e.ctx, `SELECT imap_keyword FROM labels WHERE account_id = $1 AND name = 'Nowa'`, e.accountID).Scan(&kw))
	if kw != "$Silo.Nowa" {
		t.Fatalf("keyword = %q", kw)
	}
	e.reconcileClean()
}

func TestAssignKeyword(t *testing.T) {
	e := newEnv(t)
	_, err := e.pool.Exec(e.ctx, `UPDATE accounts SET keyword_prefix = 'X-' WHERE id = $1`, e.accountID)
	must(t, err)
	kw, err := silomail.AssignKeyword(e.ctx, e.pool, e.accountID, "Praca/Projekty")
	must(t, err)
	if kw != "X-Praca_Projekty" {
		t.Fatalf("keyword = %q", kw)
	}
	_, err = e.pool.Exec(e.ctx, `INSERT INTO labels (user_id, account_id, name, type, imap_keyword) VALUES ($1, $2, 'Praca Projekty', 'user', $3)`, e.userID, e.accountID, kw)
	must(t, err)
	kw2, err := silomail.AssignKeyword(e.ctx, e.pool, e.accountID, "Praca_Projekty")
	must(t, err)
	if kw2 != "X-Praca_Projekty_2" {
		t.Fatalf("collision keyword = %q", kw2)
	}
}

func TestEnsureSystemLabelsIdempotent(t *testing.T) {
	e := newEnv(t)
	for range 2 {
		must(t, e.tx(func(tx pgx.Tx) error { return silomail.EnsureSystemLabels(e.ctx, tx, e.accountID) }))
	}
	if n := e.count(`SELECT count(*) FROM labels WHERE account_id = $1 AND type = 'system'`, e.accountID); n != len(silomail.SystemLabelKeys) {
		t.Fatalf("system labels = %d, want %d", n, len(silomail.SystemLabelKeys))
	}
}
