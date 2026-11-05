package api_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

func TestSyncEndpoints(t *testing.T) {
	e := newEnv(t)
	h := e.web
	ctx := context.Background()
	l := e.login(h, imaptest.User, imaptest.Pass)
	as := func(c req) req { c.cookie, c.csrf = l.cookie, l.csrf; return c }
	accID := l.body.Accounts[0].ID
	id, _ := strconv.ParseInt(strings.TrimPrefix(accID, "acc_"), 36, 64)

	// Folders as discovery would leave them.
	fid := map[string]string{}
	for _, f := range []struct{ name, role string }{{"INBOX", "inbox"}, {"Sent", "sent"}, {"Elementy", "user"}, {"Archiwum", "user"}} {
		var n int64
		if err := e.pool.QueryRow(ctx, `
INSERT INTO mailbox_folders (account_id, user_id, name, name_raw, role, uidvalidity, messages_count)
SELECT id, user_id, $2, $2, $3, 1, 3 FROM accounts WHERE id = $1 RETURNING id`, id, f.name, f.role).Scan(&n); err != nil {
			t.Fatal(err)
		}
		fid[f.name] = "f_" + strconv.FormatInt(n, 36)
	}

	var st api.SyncStatus
	r := serve(h, as(req{method: "GET", path: "/accounts/" + accID + "/sync-status"}))
	r.json(&st)
	if r.Code != http.StatusOK || st.State != "connecting" || len(st.Folders) != 4 || len(st.MissingRoles) != 1 || st.MissingRoles[0] != "archive" ||
		st.InitialSync.Phase != "headers" || st.Folders[0].Name != "INBOX" {
		t.Fatalf("sync-status: %d %s", r.Code, r.Body)
	}

	var fl struct{ Folders []api.Folder }
	r = serve(h, as(req{method: "GET", path: "/accounts/" + accID + "/folders"}))
	r.json(&fl)
	if r.Code != http.StatusOK || len(fl.Folders) != 4 {
		t.Fatalf("folders: %d %s", r.Code, r.Body)
	}

	put := func(changes ...map[string]any) resp {
		return serve(h, as(req{method: "PUT", path: "/accounts/" + accID + "/folders", body: map[string]any{"folders": changes}}))
	}
	for name, body := range map[string]map[string]any{
		"second sent":   {"id": fid["Elementy"], "role": "sent"},
		"inbox role":    {"id": fid["INBOX"], "role": "user"},
		"inbox ignored": {"id": fid["INBOX"], "syncMode": "ignore"},
		"to inbox":      {"id": fid["Elementy"], "role": "inbox"},
		"unknown":       {"id": "f_zzzz", "role": "archive"},
	} {
		if r := put(body); r.Code != http.StatusUnprocessableEntity && r.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, r.Code, r.Body)
		}
	}
	// Archiwum becomes archive, Sent is moved to Elementy, Archiwum... one call.
	r = put(map[string]any{"id": fid["Archiwum"], "role": "archive"},
		map[string]any{"id": fid["Sent"], "role": "user", "syncMode": "ignore"},
		map[string]any{"id": fid["Elementy"], "role": "sent"})
	r.json(&fl)
	if r.Code != http.StatusOK {
		t.Fatalf("put: %d %s", r.Code, r.Body)
	}
	roles := map[string]string{}
	for _, f := range fl.Folders {
		roles[f.Name] = f.Role + "/" + f.SyncMode
	}
	if roles["Archiwum"] != "archive/sync" || roles["Elementy"] != "sent/sync" || roles["Sent"] != "user/ignore" {
		t.Errorf("roles after put: %v", roles)
	}
	if n := e.count(`SELECT count(*) FROM accounts WHERE id = $1 AND folder_mapping->>'archive' = 'Archiwum' AND folder_mapping->>'sent' = 'Elementy' AND folder_mapping->>'drafts' = ''`, id); n != 1 {
		t.Error("folder_mapping not stored")
	}
	if n := e.count(`SELECT count(*) FROM labels WHERE account_id = $1 AND type = 'folder' AND name = 'Sent'`, id); n != 1 {
		t.Error("Sent, now a user folder, has no folder label")
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE account_id = $1 AND kind = 'account.resync'`, id); n != 1 {
		t.Errorf("resync jobs after put = %d", n)
	}
	r = serve(h, as(req{method: "GET", path: "/accounts/" + accID + "/sync-status"}))
	r.json(&st)
	if len(st.MissingRoles) != 0 {
		t.Errorf("missing roles after mapping archive: %v", st.MissingRoles)
	}

	// Resync.
	if r := serve(h, as(req{method: "POST", path: "/accounts/" + accID + "/resync", body: map[string]string{"mode": "full"}})); r.Code != http.StatusAccepted {
		t.Errorf("resync full: %d %s", r.Code, r.Body)
	}
	if r := serve(h, as(req{method: "POST", path: "/accounts/" + accID + "/resync", body: map[string]string{}})); r.Code != http.StatusAccepted {
		t.Errorf("resync default: %d %s", r.Code, r.Body)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE account_id = $1 AND kind = 'account.resync' AND payload->>'mode' IN ('full','incremental')`, id); n != 3 {
		t.Errorf("resync jobs = %d", n)
	}
	r = serve(h, as(req{method: "POST", path: "/accounts/" + accID + "/resync", body: map[string]string{"mode": "rebuild"}}))
	if r.Code != http.StatusAccepted || e.count(`SELECT count(*) FROM account_rebuilds WHERE account_id = $1 AND started_at IS NULL`, id) != 1 ||
		e.count(`SELECT count(*) FROM jobs WHERE account_id = $1 AND kind = 'account.resync' AND payload->>'mode' = 'rebuild'`, id) != 1 {
		t.Errorf("rebuild: %d %s", r.Code, r.Body)
	}
	// An operation waiting for write-back blocks the rebuild.
	e.exec(`INSERT INTO jobs (kind, account_id, payload) VALUES ('imap_op', $1, '{"op":"flags"}')`, id)
	r = serve(h, as(req{method: "POST", path: "/accounts/" + accID + "/resync", body: map[string]string{"mode": "rebuild"}}))
	if r.Code != http.StatusConflict || r.code() != api.CodeRebuildBlocked {
		t.Errorf("rebuild with a pending operation: %d %s", r.Code, r.Body)
	}
	e.exec(`DELETE FROM jobs WHERE account_id = $1 AND kind = 'imap_op'`, id)
	for _, c := range []req{
		{method: "POST", path: "/accounts/acc_zzzz/resync", body: map[string]string{"mode": "full"}},
		{method: "GET", path: "/accounts/acc_zzzz/folders"},
		{method: "PUT", path: "/accounts/acc_zzzz/folders", body: map[string]any{"folders": []any{}}},
		{method: "GET", path: "/accounts/acc_zzzz/sync-status"},
	} {
		if r := serve(h, as(c)); r.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d", c.method, c.path, r.Code)
		}
	}
	if r := serve(h, req{method: "GET", path: "/accounts/" + accID + "/folders"}); r.Code != http.StatusUnauthorized {
		t.Errorf("folders without session: %d", r.Code)
	}
	_ = auth.JobAccountReconnect
}
