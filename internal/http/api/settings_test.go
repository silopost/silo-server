package api_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/mail"
)

func TestSettings(t *testing.T) {
	m := newMailEnv(t)
	ctx := context.Background()
	patch := func(body map[string]any, client string) (resp, api.Settings) {
		t.Helper()
		var s api.Settings
		r := serve(m.web, m.as(req{method: "PATCH", path: "/settings", body: body, headers: map[string]string{"X-Silo-Client": client}}))
		if r.Code == http.StatusOK {
			r.json(&s)
		}
		return r, s
	}
	get := func() api.Settings {
		t.Helper()
		var s api.Settings
		r := serve(m.web, m.as(req{method: "GET", path: "/settings"}))
		if r.Code != http.StatusOK {
			t.Fatalf("get settings: %d %s", r.Code, r.Body)
		}
		r.json(&s)
		return s
	}

	def := get()
	if def.Density != "default" || def.UndoSendSeconds != 5 || def.Timezone != "UTC" || !def.KeyboardShortcutsEnabled ||
		!def.ConversationView || def.DesktopNotifications || def.UpdatedAt != nil || def.ActiveAccountID == nil || def.Language != "en" {
		t.Fatalf("defaults: %+v", def)
	}

	t.Run("validation", func(t *testing.T) {
		for _, c := range []struct {
			name string
			body map[string]any
			code string
		}{
			{"density", map[string]any{"density": "huge"}, api.CodeInvalidSetting},
			{"undoSendSeconds", map[string]any{"undoSendSeconds": 7}, api.CodeInvalidSetting},
			{"undoSendSeconds 0", map[string]any{"undoSendSeconds": 0}, api.CodeInvalidSetting},
			{"signature too long", map[string]any{"signature": strings.Repeat("x", api.MaxSignatureBytes+1)}, api.CodeInvalidSetting},
			{"timezone", map[string]any{"timezone": "Mars/Olympus"}, api.CodeInvalidTimezone},
			{"timezone empty", map[string]any{"timezone": ""}, api.CodeInvalidTimezone},
			{"timezone Local", map[string]any{"timezone": "Local"}, api.CodeInvalidTimezone},
			{"activeAccountId unknown", map[string]any{"activeAccountId": "acc_zzzz"}, api.CodeInvalidSetting},
			{"activeAccountId garbage", map[string]any{"activeAccountId": "x"}, api.CodeInvalidSetting},
			{"language unknown", map[string]any{"language": "de"}, api.CodeInvalidSetting},
			{"language region", map[string]any{"language": "en-US"}, api.CodeInvalidSetting},
			{"language empty", map[string]any{"language": ""}, api.CodeInvalidSetting},
		} {
			if r, _ := patch(c.body, ""); r.Code != http.StatusUnprocessableEntity || r.code() != c.code {
				t.Errorf("%s: %d %s", c.name, r.Code, r.Body)
			}
		}
		if r, _ := patch(map[string]any{"density": 3}, ""); r.Code != http.StatusBadRequest {
			t.Errorf("wrong type: %d %s", r.Code, r.Body)
		}
		if r := serve(m.web, req{method: "PATCH", path: "/settings", body: map[string]any{"density": "compact"}, cookie: m.l.cookie}); r.Code != http.StatusForbidden {
			t.Errorf("without CSRF: %d", r.Code)
		}
		if s := get(); s.UpdatedAt != nil || s.Density != "default" {
			t.Errorf("refused patches changed the settings: %+v", s)
		}
	})

	// Another user's account is refused like an unknown one (422, not 404 as
	// the phase 5 draft of this endpoint had it), and nothing of the request
	// is written, its valid fields included.
	t.Run("activeAccountId of another user", func(t *testing.T) {
		var otherUser, otherAcc int64
		if err := m.pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ('settings-other@silo.test') RETURNING id`).Scan(&otherUser); err != nil {
			t.Fatal(err)
		}
		if err := m.pool.QueryRow(ctx, `
INSERT INTO accounts (user_id, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, secret_enc)
VALUES ($1, 'settings-other@silo.test', 'h', 1, 'none', 'h', 25, 'none', 'other', '\x00') RETURNING id`, otherUser).Scan(&otherAcc); err != nil {
			t.Fatal(err)
		}
		foreign := id36("acc_", otherAcc)
		r, _ := patch(map[string]any{"activeAccountId": foreign, "density": "compact"}, "")
		ru, _ := patch(map[string]any{"activeAccountId": "acc_zzzz", "density": "compact"}, "")
		if r.Code != http.StatusUnprocessableEntity || r.code() != api.CodeInvalidSetting || r.Body.String() != ru.Body.String() {
			t.Errorf("foreign activeAccountId: %d %s (unknown account: %s)", r.Code, r.Body, ru.Body)
		}
		if s := get(); s.Density != "default" || s.ActiveAccountID == nil || *s.ActiveAccountID != m.l.body.Accounts[0].ID {
			t.Errorf("refused patch changed the settings: %+v", s)
		}
		if n := m.count(`SELECT count(*) FROM users WHERE id = $1 AND settings ? 'active_account_id'`, otherUser); n != 0 {
			t.Error("the other user's settings changed")
		}
	})

	t.Run("round trip", func(t *testing.T) {
		before := time.Now().Add(-time.Second)
		r, s := patch(map[string]any{
			"density": "compact", "undoSendSeconds": 20, "timezone": "Europe/Warsaw", "keyboardShortcutsEnabled": false,
			"conversationView": false, "desktopNotifications": true,
			"signature":       `<p onclick="x()">Jan <b>K</b><script>alert(1)</script></p>`,
			"activeAccountId": m.l.body.Accounts[0].ID,
		}, "tab-set")
		if r.Code != http.StatusOK || s.Density != "compact" || s.UndoSendSeconds != 20 || s.Timezone != "Europe/Warsaw" ||
			s.KeyboardShortcutsEnabled || s.ConversationView || !s.DesktopNotifications || s.UpdatedAt == nil || s.UpdatedAt.Before(before) ||
			s.ActiveAccountID == nil || *s.ActiveAccountID != m.l.body.Accounts[0].ID {
			t.Fatalf("patch: %d %s", r.Code, r.Body)
		}
		if strings.Contains(s.Signature, "script") || strings.Contains(s.Signature, "onclick") || !strings.Contains(s.Signature, "<b>K</b>") {
			t.Errorf("signature not sanitized: %q", s.Signature)
		}
		// Only the fields sent change.
		r, s2 := patch(map[string]any{"density": "comfortable"}, "")
		if r.Code != http.StatusOK || s2.Density != "comfortable" || s2.UndoSendSeconds != 20 || s2.Timezone != "Europe/Warsaw" || s2.Signature != s.Signature {
			t.Fatalf("partial patch: %d %s", r.Code, r.Body)
		}
		if g := get(); g.Density != "comfortable" || g.UpdatedAt == nil || !g.UpdatedAt.Equal(*s2.UpdatedAt) {
			t.Errorf("GET /settings = %+v", g)
		}
		var sb api.SessionBody
		rs := serve(m.web, m.as(req{method: "GET", path: "/session"}))
		rs.json(&sb)
		if sb.Settings.Density != "comfortable" || sb.Settings.Timezone != "Europe/Warsaw" || !sb.Settings.DesktopNotifications || sb.Settings.UpdatedAt == nil {
			t.Errorf("GET /session settings = %+v", sb.Settings)
		}
		if n := m.count(`SELECT count(*) FROM events WHERE type = 'settings.updated' AND payload->>'origin' = 'tab-set' AND payload ? 'updatedAt'`); n != 1 {
			t.Errorf("settings.updated events with origin = %d", n)
		}
		// null clears activeAccountId (the first account is the default again).
		if r, s := patch(map[string]any{"activeAccountId": nil}, ""); r.Code != http.StatusOK || s.ActiveAccountID == nil {
			t.Errorf("clear activeAccountId: %d %s", r.Code, r.Body)
		}
		if n := m.count(`SELECT count(*) FROM users WHERE settings ? 'active_account_id'`); n != 0 {
			t.Error("active_account_id still stored after null")
		}
		// An empty signature is allowed.
		if r, s := patch(map[string]any{"signature": ""}, ""); r.Code != http.StatusOK || s.Signature != "" {
			t.Errorf("empty signature: %d %s", r.Code, r.Body)
		}
	})

	t.Run("language", func(t *testing.T) {
		r, s := patch(map[string]any{"language": "pl"}, "")
		if r.Code != http.StatusOK || s.Language != "pl" {
			t.Fatalf("patch language: %d %s", r.Code, r.Body)
		}
		if g := get(); g.Language != "pl" {
			t.Errorf("GET /settings language = %q", g.Language)
		}
		var sb api.SessionBody
		serve(m.web, m.as(req{method: "GET", path: "/session"})).json(&sb)
		if sb.Settings.Language != "pl" {
			t.Errorf("GET /session language = %q", sb.Settings.Language)
		}
		// Other patches keep it; an unknown stored value reads as the default.
		if r, s := patch(map[string]any{"density": "compact"}, ""); r.Code != http.StatusOK || s.Language != "pl" {
			t.Errorf("partial patch dropped language: %d %s", r.Code, r.Body)
		}
		if _, err := m.pool.Exec(ctx, `UPDATE users SET settings = settings || '{"language":"xx"}'::jsonb`); err != nil {
			t.Fatal(err)
		}
		if g := get(); g.Language != "en" {
			t.Errorf("unsupported stored language = %q, want en", g.Language)
		}
		if r, s := patch(map[string]any{"language": "en"}, ""); r.Code != http.StatusOK || s.Language != "en" {
			t.Errorf("back to en: %d %s", r.Code, r.Body)
		}
	})

	t.Run("undoSendSeconds sets the undo window of a send", func(t *testing.T) {
		if r, _ := patch(map[string]any{"undoSendSeconds": 30}, ""); r.Code != http.StatusOK {
			t.Fatal(r.Body)
		}
		var d api.Draft
		r := serve(m.web, m.as(req{method: "POST", path: "/drafts", body: map[string]any{}}))
		r.json(&d)
		body := recipients([]string{"x@silo.test"}, nil, nil)
		body["version"] = d.Version
		if r = serve(m.web, m.as(req{method: "PUT", path: "/drafts/" + d.ID, body: body})); r.Code != http.StatusOK {
			t.Fatalf("save: %d %s", r.Code, r.Body)
		}
		var out api.SendResult
		start := time.Now()
		r = serve(m.web, m.as(req{method: "POST", path: "/drafts/" + d.ID + "/send", body: map[string]any{}}))
		r.json(&out)
		if r.Code != http.StatusOK && r.Code != http.StatusAccepted {
			t.Fatalf("send: %d %s", r.Code, r.Body)
		}
		if w := out.UndoUntil.Sub(start); w < 29*time.Second || w > 32*time.Second {
			t.Errorf("undo window = %s, want 30 s", w)
		}
	})

	t.Run("timezone moves before: and the snooze presets", func(t *testing.T) {
		// 23:30 UTC on 4 June is already 5 June in Warsaw.
		at := time.Date(2024, 6, 4, 23, 30, 0, 0, time.UTC)
		err := pgx.BeginFunc(ctx, m.pool, func(tx pgx.Tx) error {
			_, err := mail.IngestBatch(ctx, tx, m.accID, m.inbox, []mail.FetchedMessage{{UID: 90, Size: 100, InternalDate: at,
				Header: []byte("From: tz@silo.test\r\nSubject: Midnight\r\nMessage-ID: <tz1@silo.test>\r\n\r\n")}})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		hits := func() int {
			t.Helper()
			var l api.ThreadList
			r := serve(m.web, m.as(req{method: "GET", path: "/threads?q=" + url.QueryEscape("from:tz@silo.test before:2024/06/05")}))
			r.json(&l)
			return len(l.Threads)
		}
		patch(map[string]any{"timezone": "UTC"}, "")
		if n := hits(); n != 1 {
			t.Errorf("before:2024/06/05 in UTC: %d threads, want 1", n)
		}
		patch(map[string]any{"timezone": "Europe/Warsaw"}, "")
		if n := hits(); n != 0 {
			t.Errorf("before:2024/06/05 in Europe/Warsaw: %d threads, want 0", n)
		}
		var p api.SnoozePresets
		serve(m.web, m.as(req{method: "GET", path: "/snooze/presets?now=2026-03-02T10:00:00Z"})).json(&p)
		if p.Timezone != "Europe/Warsaw" {
			t.Errorf("presets time zone = %q", p.Timezone)
		}
		for _, x := range p.Presets {
			if x.Key == "tomorrow" && !x.Until.Equal(time.Date(2026, 3, 3, 8, 0, 0, 0, mustLoc(t, "Europe/Warsaw"))) {
				t.Errorf("tomorrow = %s, want 08:00 Warsaw", x.Until)
			}
		}
	})
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
