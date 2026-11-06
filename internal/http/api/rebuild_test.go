package api_test

import (
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

// TestResyncRebuild: POST /accounts/{id}/resync {mode: rebuild} drops the
// mirror and the account syncs again from scratch (state back to live, the
// same messages); with write-back pending it is 409 rebuild_blocked.
func TestResyncRebuild(t *testing.T) {
	s := newFullStack(t, nil)
	addr := net.JoinHostPort(s.cfg.DefaultIMAPHost, strconv.Itoa(s.cfg.DefaultIMAPPort))
	c := imapUser(t, addr, imaptest.User, imaptest.Pass)
	for i := range 3 {
		appendMsg(t, c, "INBOX", "rb"+strconv.Itoa(i)+"@a.test", "Ala <ala@a.test>", "silo@silo.test", "Odbudowa "+strconv.Itoa(i))
	}
	l := s.login(s.h, imaptest.User, imaptest.Pass)
	accs := s.waitLive(t, l, 1)
	accID := accs[0].ID
	id := idOf("acc_", accID)
	waitFor(t, 30*time.Second, "3 messages", func() bool {
		return s.count(`SELECT count(*) FROM messages WHERE account_id = $1`, id) == 3
	})
	before := s.count(`SELECT max(id) FROM messages WHERE account_id = $1`, id)

	// A pending write-back operation blocks the rebuild.
	s.count(`WITH j AS (INSERT INTO jobs (kind, account_id, run_at) VALUES ('imap_op', $1, now() + interval '1 hour') RETURNING 1) SELECT count(*) FROM j`, id)
	r := serve(s.h, as(l, req{method: "POST", path: "/accounts/" + accID + "/resync", body: map[string]string{"mode": "rebuild"}}))
	if r.Code != http.StatusConflict || r.code() != api.CodeRebuildBlocked {
		t.Fatalf("rebuild with pending ops: %d %s", r.Code, r.Body)
	}
	s.count(`WITH d AS (DELETE FROM jobs WHERE account_id = $1 AND kind = 'imap_op' RETURNING 1) SELECT count(*) FROM d`, id)

	r = serve(s.h, as(l, req{method: "POST", path: "/accounts/" + accID + "/resync", body: map[string]string{"mode": "rebuild"}}))
	if r.Code != http.StatusAccepted {
		t.Fatalf("rebuild: %d %s", r.Code, r.Body)
	}
	var st api.SyncStatus
	waitFor(t, 90*time.Second, "rebuilt and live", func() bool {
		serve(s.h, as(l, req{method: "GET", path: "/accounts/" + accID + "/sync-status"})).json(&st)
		return st.State == "live" && st.InitialSyncDoneAt != nil &&
			s.count(`SELECT count(*) FROM messages WHERE account_id = $1 AND id > $2`, id, before) == 3
	})
	if n := s.count(`SELECT count(*) FROM messages WHERE account_id = $1`, id); n != 3 {
		t.Errorf("messages after rebuild = %d, want 3 (old rows dropped, new ones synced)", n)
	}
	if n := s.count(`SELECT count(*) FROM threads WHERE account_id = $1`, id); n != 3 {
		t.Errorf("threads after rebuild = %d", n)
	}
}
