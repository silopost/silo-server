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

func TestThreadModifyEndpoints(t *testing.T) {
	e := newEnv(t)
	h := e.web
	ctx := context.Background()
	l := e.login(h, imaptest.User, imaptest.Pass)
	as := func(c req) req { c.cookie, c.csrf = l.cookie, l.csrf; return c }
	accID, _ := strconv.ParseInt(strings.TrimPrefix(l.body.Accounts[0].ID, "acc_"), 36, 64)

	var inbox, threadID int64
	if err := e.pool.QueryRow(ctx, `
INSERT INTO mailbox_folders (account_id, user_id, name, name_raw, role, uidvalidity)
SELECT id, user_id, 'INBOX', 'INBOX', 'inbox', 1 FROM accounts WHERE id = $1 RETURNING id`, accID).Scan(&inbox); err != nil {
		t.Fatal(err)
	}
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		res, err := mail.IngestBatch(ctx, tx, accID, inbox, []mail.FetchedMessage{{
			UID: 1, Size: 100, InternalDate: time.Now(),
			Header: []byte("From: a@silo.test\r\nSubject: Hello\r\nMessage-ID: <x1@silo.test>\r\n\r\n"),
		}})
		if err == nil {
			threadID = res.Messages[0].ThreadID
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	tid := "t_" + strconv.FormatInt(threadID, 36)

	var out api.OperationResult
	r := serve(h, as(req{method: "POST", path: "/threads/modify", headers: map[string]string{"X-Silo-Client": "tab-1"},
		body: map[string]any{"ids": []string{tid}, "addLabelIds": []string{"STARRED"}, "removeLabelIds": []string{"UNREAD"}}}))
	r.json(&out)
	if r.Code != http.StatusOK || len(out.Threads) != 1 || out.OperationID == nil || out.UndoUntil != nil ||
		!out.Threads[0].PendingSync || !slices.Contains(out.Threads[0].LabelIDs, "STARRED") || slices.Contains(out.Threads[0].LabelIDs, "UNREAD") {
		t.Fatalf("star: %d %s", r.Code, r.Body)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE type = 'thread.updated' AND payload->>'origin' = 'tab-1'`); n == 0 {
		t.Error("no event with origin")
	}
	r = serve(h, as(req{method: "POST", path: "/threads/modify", body: map[string]any{"ids": []string{tid}, "removeLabelIds": []string{"INBOX"}}}))
	r.json(&out)
	if r.Code != http.StatusOK || out.UndoUntil == nil || slices.Contains(out.Threads[0].LabelIDs, "INBOX") || !slices.Contains(out.Threads[0].LabelIDs, "ARCHIVE") {
		t.Fatalf("archive: %d %s", r.Code, r.Body)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE account_id = $1 AND kind = 'imap_op' AND run_at > now()`, accID); n != 1 {
		t.Errorf("delayed folder jobs = %d", n)
	}
	for name, c := range map[string]req{
		"archive label": {method: "POST", path: "/threads/modify", body: map[string]any{"ids": []string{tid}, "addLabelIds": []string{"ARCHIVE"}}},
		"sent label":    {method: "POST", path: "/threads/modify", body: map[string]any{"ids": []string{tid}, "removeLabelIds": []string{"SENT"}}},
		"bad id":        {method: "POST", path: "/threads/modify", body: map[string]any{"ids": []string{"x"}, "addLabelIds": []string{"STARRED"}}},
		"delete":        {method: "POST", path: "/threads/delete", body: map[string]any{"ids": []string{tid}}},
	} {
		if r := serve(h, as(c)); r.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, r.Code, r.Body)
		}
	}
	if r := serve(h, as(req{method: "POST", path: "/threads/trash", body: map[string]any{"ids": []string{"t_zzzz"}}})); r.Code != http.StatusNotFound {
		t.Errorf("unknown thread: %d", r.Code)
	}
	if r := serve(h, as(req{method: "POST", path: "/messages/modify", body: map[string]any{"ids": []string{"m_1"}, "addLabelIds": []string{"STARRED"}}})); r.Code != http.StatusOK && r.Code != http.StatusNotFound {
		t.Errorf("messages/modify: %d %s", r.Code, r.Body)
	}
	if r := serve(h, req{method: "POST", path: "/threads/trash", body: map[string]any{"ids": []string{tid}}}); r.Code != http.StatusUnauthorized {
		t.Errorf("without session: %d", r.Code)
	}
}
