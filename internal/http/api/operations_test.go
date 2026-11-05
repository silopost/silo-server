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

	// A worker claimed the job: too late, the client applies the inverse.
	op = archive()
	if _, err := m.pool.Exec(context.Background(), `UPDATE jobs SET locked_until = now() + interval '1 minute' WHERE group_id = $1::uuid`, *op.OperationID); err != nil {
		t.Fatal(err)
	}
	r = serve(m.web, m.as(req{method: "POST", path: "/operations/" + *op.OperationID + "/undo"}))
	if r.Code != http.StatusConflict || r.code() != api.CodeUndoExpired {
		t.Errorf("undo of a claimed job: %d %s", r.Code, r.Body)
	}

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
