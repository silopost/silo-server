package api_test

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

// mailEnv is a logged-in user with an INBOX folder in the mirror (no sync
// engine runs: jobs stay queued).
type mailEnv struct {
	*env
	l     login
	as    func(req) req
	accID int64
	inbox int64
}

func newMailEnv(t *testing.T) *mailEnv {
	e := newEnv(t)
	m := &mailEnv{env: e, l: e.login(e.web, imaptest.User, imaptest.Pass)}
	m.as = func(c req) req { c.cookie, c.csrf = m.l.cookie, m.l.csrf; return c }
	m.accID, _ = strconv.ParseInt(strings.TrimPrefix(m.l.body.Accounts[0].ID, "acc_"), 36, 64)
	if err := e.pool.QueryRow(context.Background(), `
INSERT INTO mailbox_folders (account_id, user_id, name, name_raw, role, uidvalidity)
SELECT id, user_id, 'INBOX', 'INBOX', 'inbox', 1 FROM accounts WHERE id = $1 RETURNING id`, m.accID).Scan(&m.inbox); err != nil {
		t.Fatal(err)
	}
	return m
}

// ingest puts one message into INBOX and returns its message and thread ids.
func (m *mailEnv) ingest(uid uint32, header string) (msgID, threadID int64) {
	m.t.Helper()
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, m.pool, func(tx pgx.Tx) error {
		res, err := mail.IngestBatch(ctx, tx, m.accID, m.inbox, []mail.FetchedMessage{{
			UID: uid, Size: 100, InternalDate: time.Now().Add(-time.Hour), Header: []byte(header),
		}})
		if err == nil {
			msgID, threadID = res.Messages[0].MessageID, res.Messages[0].ThreadID
		}
		return err
	})
	if err != nil {
		m.t.Fatal(err)
	}
	return msgID, threadID
}

func id36(prefix string, id int64) string { return prefix + strconv.FormatInt(id, 36) }

func TestUndoOperation(t *testing.T) {
	m := newMailEnv(t)
	_, threadID := m.ingest(1, "From: a@silo.test\r\nSubject: Hello\r\nMessage-ID: <u1@silo.test>\r\n\r\n")
	tid := id36("t_", threadID)
	archive := func() api.OperationResult {
		t.Helper()
		var out api.OperationResult
		r := serve(m.web, m.as(req{method: "POST", path: "/threads/modify", body: map[string]any{"ids": []string{tid}, "removeLabelIds": []string{"INBOX"}}}))
		r.json(&out)
		if r.Code != http.StatusOK || out.OperationID == nil || out.UndoUntil == nil || !out.UndoUntil.After(time.Now()) {
			t.Fatalf("archive: %d %s", r.Code, r.Body)
		}
		return out
	}

	op := archive()
	var out api.OperationResult
	r := serve(m.web, m.as(req{method: "POST", path: "/operations/" + *op.OperationID + "/undo", headers: map[string]string{"X-Silo-Client": "tab-u"}}))
	r.json(&out)
	if r.Code != http.StatusOK || len(out.Threads) != 1 || !slices.Contains(out.Threads[0].LabelIDs, "INBOX") ||
		slices.Contains(out.Threads[0].LabelIDs, "ARCHIVE") || out.Threads[0].PendingSync {
		t.Fatalf("undo: %d %s", r.Code, r.Body)
	}
	if n := m.count(`SELECT count(*) FROM jobs WHERE account_id = $1 AND kind = 'imap_op'`, m.accID); n != 0 {
		t.Errorf("jobs left after undo: %d", n)
	}
	if n := m.count(`SELECT count(*) FROM events WHERE type = 'thread.updated' AND payload->>'origin' = 'tab-u'`); n == 0 {
		t.Error("no thread.updated with the origin of the undo")
	}
	if r := serve(m.web, m.as(req{method: "POST", path: "/operations/" + *op.OperationID + "/undo"})); r.Code != http.StatusNotFound {
		t.Errorf("second undo: %d %s", r.Code, r.Body)
	}

	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := m.pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	undo := func(id, client string) (resp, api.OperationResult) {
		t.Helper()
		var out api.OperationResult
		r := serve(m.web, m.as(req{method: "POST", path: "/operations/" + id + "/undo", headers: map[string]string{"X-Silo-Client": client}}))
		if r.Code == http.StatusOK {
			r.json(&out)
		}
		return r, out
	}
	folder := func(role string) int64 {
		t.Helper()
		var id int64
		if err := m.pool.QueryRow(ctx, `
INSERT INTO mailbox_folders (account_id, user_id, name, name_raw, role, uidvalidity)
SELECT id, user_id, $2, $2, $2, 1 FROM accounts WHERE id = $1 RETURNING id`, m.accID, role).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// executed simulates the worker: the message is in folderID on the
	// server and the jobs of the operation are gone.
	executed := func(op string, folderID int64) {
		t.Helper()
		exec(`UPDATE message_locations SET folder_id = $2 WHERE message_id IN (SELECT id FROM messages WHERE thread_id = $1)`, threadID, folderID)
		exec(`DELETE FROM jobs WHERE group_id = $1::uuid`, op)
	}

	// After the job ran (Archive was created by it), the server applies the
	// inverse as a new operation.
	op = archive()
	executed(*op.OperationID, folder("archive"))
	r, out = undo(*op.OperationID, "tab-v")
	if r.Code != http.StatusOK || len(out.Threads) != 1 || !slices.Contains(out.Threads[0].LabelIDs, "INBOX") ||
		slices.Contains(out.Threads[0].LabelIDs, "ARCHIVE") || !out.Threads[0].PendingSync ||
		out.OperationID == nil || *out.OperationID == *op.OperationID || out.UndoUntil == nil {
		t.Fatalf("undo after the job ran: %d %s", r.Code, r.Body)
	}
	if n := m.count(`SELECT count(*) FROM jobs WHERE group_id = $1::uuid AND payload->>'op' = 'move' AND (payload->>'toFolderId')::bigint = $2 AND run_at > now()`,
		*out.OperationID, m.inbox); n != 1 {
		t.Errorf("inverse move jobs = %d", n)
	}
	if n := m.count(`SELECT count(*) FROM events WHERE type = 'thread.updated' AND payload->>'origin' = 'tab-v'`); n == 0 {
		t.Error("no thread.updated with the origin of the late undo")
	}
	if r, _ := undo(*op.OperationID, ""); r.Code != http.StatusNotFound {
		t.Errorf("second late undo: %d %s", r.Code, r.Body)
	}
	// The undo is an operation: undoing it inside its window cancels it.
	back := *out.OperationID
	r, out = undo(back, "")
	if r.Code != http.StatusOK || !slices.Contains(out.Threads[0].LabelIDs, "ARCHIVE") || slices.Contains(out.Threads[0].LabelIDs, "INBOX") ||
		out.OperationID != nil || out.Threads[0].PendingSync {
		t.Fatalf("undo of the undo: %d %s", r.Code, r.Body)
	}
	if n := m.count(`SELECT count(*) FROM jobs WHERE account_id = $1 AND kind = 'imap_op'`, m.accID); n != 0 {
		t.Errorf("jobs left after undoing the undo: %d", n)
	}

	// A user label mirrored as a keyword: the flags job was claimed at once;
	// the undo takes the label off again.
	exec(`UPDATE accounts SET keywords_supported = true WHERE id = $1`, m.accID)
	var labelID int64
	if err := m.pool.QueryRow(ctx, `INSERT INTO labels (user_id, account_id, name, type) SELECT user_id, id, 'Projekt', 'user' FROM accounts WHERE id = $1 RETURNING id`, m.accID).Scan(&labelID); err != nil {
		t.Fatal(err)
	}
	lid := id36("l_", labelID)
	var lop api.OperationResult
	r = serve(m.web, m.as(req{method: "POST", path: "/threads/modify", body: map[string]any{"ids": []string{tid}, "addLabelIds": []string{lid}}}))
	r.json(&lop)
	if r.Code != http.StatusOK || lop.OperationID == nil || !slices.Contains(lop.Threads[0].LabelIDs, lid) {
		t.Fatalf("add label: %d %s", r.Code, r.Body)
	}
	exec(`UPDATE jobs SET locked_until = now() + interval '1 minute' WHERE group_id = $1::uuid`, *lop.OperationID)
	r, out = undo(*lop.OperationID, "")
	if r.Code != http.StatusOK || slices.Contains(out.Threads[0].LabelIDs, lid) || out.OperationID == nil || out.UndoUntil != nil {
		t.Fatalf("undo of the label after its job was claimed: %d %s", r.Code, r.Body)
	}
	if n := m.count(`SELECT count(*) FROM message_labels WHERE label_id = $1`, labelID); n != 0 {
		t.Errorf("label still on %d messages", n)
	}
	if n := m.count(`SELECT count(*) FROM jobs WHERE group_id = $1::uuid AND payload->>'op' = 'flags' AND payload->'remove' ? 'Projekt'`, *out.OperationID); n != 1 {
		t.Errorf("inverse keyword jobs = %d", n)
	}

	// Outside the retention: too late.
	exec(`DELETE FROM jobs WHERE account_id = $1`, m.accID)
	exec(`UPDATE operations SET created_at = now() - interval '25 hours' WHERE id = $1::uuid`, *out.OperationID)
	if r, _ := undo(*out.OperationID, ""); r.Code != http.StatusConflict || r.code() != api.CodeUndoExpired {
		t.Errorf("undo after the retention: %d %s", r.Code, r.Body)
	}

	// A permanent delete can be undone only inside its window.
	exec(`UPDATE message_locations SET folder_id = $2 WHERE message_id IN (SELECT id FROM messages WHERE thread_id = $1)`, threadID, folder("trash"))
	var dop api.OperationResult
	r = serve(m.web, m.as(req{method: "POST", path: "/threads/delete", body: map[string]any{"ids": []string{tid}}}))
	r.json(&dop)
	if r.Code != http.StatusOK || dop.OperationID == nil {
		t.Fatalf("delete: %d %s", r.Code, r.Body)
	}
	exec(`UPDATE jobs SET locked_until = now() + interval '1 minute' WHERE group_id = $1::uuid`, *dop.OperationID)
	if r, _ := undo(*dop.OperationID, ""); r.Code != http.StatusConflict || r.code() != api.CodeNotUndoable {
		t.Errorf("undo of a permanent delete: %d %s", r.Code, r.Body)
	}
	op = dop

	other := newUserSession(t, m.env, "other-undo@silo.test")
	r = serve(m.web, req{method: "POST", path: "/operations/" + *op.OperationID + "/undo", cookie: other.cookie, csrf: other.csrf})
	if r.Code != http.StatusNotFound {
		t.Errorf("undo of another user's operation: %d %s", r.Code, r.Body)
	}
	for _, id := range []string{"op_x", "00000000-0000-0000-0000-000000000000"} {
		if r := serve(m.web, m.as(req{method: "POST", path: "/operations/" + id + "/undo"})); r.Code != http.StatusNotFound {
			t.Errorf("undo %s: %d", id, r.Code)
		}
	}
	if r := serve(m.web, req{method: "POST", path: "/operations/" + *op.OperationID + "/undo"}); r.Code != http.StatusUnauthorized {
		t.Errorf("without session: %d", r.Code)
	}
}
