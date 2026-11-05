package sync_test

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/silopost/silo-server/internal/ops"
	silosync "github.com/silopost/silo-server/internal/sync"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

// storeKeyword changes a keyword of one message as another client
// (Thunderbird) does: UID STORE +FLAGS / -FLAGS on its own connection.
func (e *opsEnv) storeKeyword(folder, hdr string, op imap.StoreFlagsOp, kw string) {
	e.t.Helper()
	_, err := e.other.Select(folder, nil).Wait()
	must(e.t, err)
	uid := uidOf(e.t, e.other, "<"+hdr+">")
	_, err = e.other.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: op, Silent: true, Flags: []imap.Flag{imap.Flag(kw)}}, nil).Collect()
	must(e.t, err)
}

// flagsIn returns the flags of a message in one folder as another client sees them.
func (e *opsEnv) flagsIn(folder, hdr string) []string {
	e.t.Helper()
	_, err := e.other.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	must(e.t, err)
	uid := uidOf(e.t, e.other, "<"+hdr+">")
	bufs, err := e.other.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{Flags: true, UID: true}).Collect()
	must(e.t, err)
	var out []string
	for _, f := range bufs[0].Flags {
		out = append(out, string(f))
	}
	return out
}

// userLabel returns id, keyword and keyword_state of the user label with this name.
func (e *opsEnv) userLabel(name string) (id int64, kw, state string, ok bool) {
	var k *string
	err := testPool.QueryRow(context.Background(), `
SELECT id, imap_keyword, keyword_state FROM labels WHERE account_id = $1 AND type = 'user' AND name = $2`, e.a.id, name).Scan(&id, &k, &state)
	if k != nil {
		kw = *k
	}
	return id, kw, state, err == nil
}

func (e *opsEnv) createLabel(name string, parent *int64, color *string) int64 {
	e.t.Helper()
	l, err := e.svc.CreateLabel(context.Background(), ops.CreateLabel{UserID: e.a.userID, AccountID: e.a.id, Name: name, Type: "user", ParentID: parent, ColorBG: color})
	must(e.t, err)
	return l.ID
}

// TestKeywordsWithOtherClient is DoD (a) of phase 4 (plan 3.7): a tag set in
// another client becomes a Silo label within 5 s in INBOX (IDLE) and within
// the polling interval elsewhere; a label set in Silo is a keyword the other
// client sees; slug collisions get _2 instead of a guessed mapping; rename
// and delete change the keyword in every folder.
func TestKeywordsWithOtherClient(t *testing.T) {
	ctx := context.Background()
	const poll = 2 * time.Second
	e := newOpsEnv(t, imaptest.Full, 500*time.Millisecond, spec{"INBOX", 4}, spec{"Faktury", 2})
	e.a.deps.Settings.PollInterval = poll
	e.restart()
	e.a.waitState(silosync.StateLive, 20*time.Second)
	time.Sleep(500 * time.Millisecond) // the idle connection is up after live

	t.Run("tag in INBOX becomes a label within 5 s", func(t *testing.T) {
		id, _ := e.thread("m1@silo.test")
		e.storeKeyword("INBOX", "m1@silo.test", imap.StoreFlagsAdd, "Praca")
		d := waitFor(t, 5*time.Second, "label Praca", func() bool { return e.a.labelsOf(id)["Praca"] })
		t.Logf("INBOX tag -> Silo label in %s", d.Round(time.Millisecond))
		if _, kw, state, ok := e.userLabel("Praca"); !ok || kw != "Praca" || state != "mirrored" {
			t.Errorf("label Praca: %q %q %v", kw, state, ok)
		}
		e.storeKeyword("INBOX", "m1@silo.test", imap.StoreFlagsDel, "Praca")
		waitFor(t, 5*time.Second, "label Praca removed", func() bool { return !e.a.labelsOf(id)["Praca"] })
	})

	t.Run("Thunderbird $label1 is Ważne", func(t *testing.T) {
		id, _ := e.thread("m2@silo.test")
		e.storeKeyword("INBOX", "m2@silo.test", imap.StoreFlagsAdd, "$label1")
		waitFor(t, 5*time.Second, "label Ważne", func() bool { return e.a.labelsOf(id)["Ważne"] })
		if _, kw, _, _ := e.userLabel("Ważne"); kw != "$label1" {
			t.Errorf("keyword of Ważne = %q", kw)
		}
	})

	t.Run("tag in another folder within the polling interval", func(t *testing.T) {
		id, _ := e.thread("m5@silo.test")
		e.storeKeyword("Faktury", "m5@silo.test", imap.StoreFlagsAdd, "Klient")
		d := waitFor(t, poll+5*time.Second, "label Klient", func() bool { return e.a.labelsOf(id)["Klient"] })
		t.Logf("Faktury tag -> Silo label in %s (poll %s)", d.Round(time.Millisecond), poll)
	})

	t.Run("Silo label is a keyword for the other client", func(t *testing.T) {
		lid := e.createLabel("Do faktury", nil, nil)
		e.modify("m6@silo.test", []ops.LabelRef{{ID: lid}}, nil)
		waitFor(t, 10*time.Second, "keyword on the server", func() bool { return slices.Contains(e.flagsIn("Faktury", "m6@silo.test"), "Do_faktury") })
	})

	t.Run("slug collision maps explicitly", func(t *testing.T) {
		id3, _ := e.thread("m3@silo.test")
		e.storeKeyword("INBOX", "m3@silo.test", imap.StoreFlagsAdd, "Zazolc")
		waitFor(t, 5*time.Second, "foreign tag imported", func() bool { return e.a.labelsOf(id3)["Zazolc"] })
		lid := e.createLabel("Zażółć", nil, nil)
		if _, kw, _, _ := e.userLabel("Zażółć"); kw != "Zazolc_2" {
			t.Fatalf("keyword of Zażółć = %q, want Zazolc_2", kw)
		}
		id4, _ := e.thread("m4@silo.test")
		e.modify("m4@silo.test", []ops.LabelRef{{ID: lid}}, nil)
		waitFor(t, 10*time.Second, "Zazolc_2 on the server", func() bool { return slices.Contains(e.flagsIn("INBOX", "m4@silo.test"), "Zazolc_2") })
		e.waitNoJobs(5 * time.Second)
		time.Sleep(time.Second) // the sync of the change
		if l := e.a.labelsOf(id4); !l["Zażółć"] || l["Zazolc"] {
			t.Errorf("labels of m4: %v", l)
		}
		if l := e.a.labelsOf(id3); l["Zażółć"] || !l["Zazolc"] {
			t.Errorf("labels of m3: %v", l)
		}

		// A tag known to the server only (PERMANENTFLAGS), never seen on a
		// message by Silo, is not taken over either.
		e.stop()
		e.storeKeyword("INBOX", "m4@silo.test", imap.StoreFlagsAdd, "Obcy")
		e.storeKeyword("INBOX", "m4@silo.test", imap.StoreFlagsDel, "Obcy")
		e.start()
		waitFor(t, 20*time.Second, "INBOX keyword list recorded", func() bool {
			return e.a.int(`SELECT count(*) FROM mailbox_folders WHERE account_id = $1 AND role = 'inbox' AND 'Obcy' = ANY(permanent_flags)`, e.a.id) == 1
		})
		e.createLabel("Obcy", nil, nil)
		if _, kw, _, _ := e.userLabel("Obcy"); kw != "Obcy_2" {
			t.Errorf("keyword of Obcy = %q, want Obcy_2", kw)
		}
	})

	t.Run("rename and delete change the keyword in every folder", func(t *testing.T) {
		lid := e.createLabel("Projekt", nil, nil)
		e.modify("m2@silo.test", []ops.LabelRef{{ID: lid}}, nil)
		e.modify("m5@silo.test", []ops.LabelRef{{ID: lid}}, nil)
		waitFor(t, 10*time.Second, "Projekt in both folders", func() bool {
			return slices.Contains(e.flagsIn("INBOX", "m2@silo.test"), "Projekt") && slices.Contains(e.flagsIn("Faktury", "m5@silo.test"), "Projekt")
		})
		e.waitNoJobs(5 * time.Second)
		name := "Projekt Nowy"
		if _, err := e.svc.UpdateLabel(ctx, ops.UpdateLabel{UserID: e.a.userID, LabelID: lid, Name: &name}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 10*time.Second, "keyword renamed in both folders", func() bool {
			a, b := e.flagsIn("INBOX", "m2@silo.test"), e.flagsIn("Faktury", "m5@silo.test")
			return slices.Contains(a, "Projekt_Nowy") && !slices.Contains(a, "Projekt") && slices.Contains(b, "Projekt_Nowy") && !slices.Contains(b, "Projekt")
		})
		e.waitNoJobs(5 * time.Second)
		id2, _ := e.thread("m2@silo.test")
		if l := e.a.labelsOf(id2); !l["Projekt Nowy"] || l["Projekt"] {
			t.Errorf("labels after rename: %v", l)
		}
		if err := e.svc.DeleteLabel(ctx, e.a.userID, lid, ""); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 10*time.Second, "keyword cleared in both folders", func() bool {
			return !slices.Contains(e.flagsIn("INBOX", "m2@silo.test"), "Projekt_Nowy") && !slices.Contains(e.flagsIn("Faktury", "m5@silo.test"), "Projekt_Nowy")
		})
		e.waitNoJobs(5 * time.Second)
		time.Sleep(time.Second)
		if _, _, _, ok := e.userLabel("Projekt_Nowy"); ok {
			t.Error("the deleted label came back from the server")
		}
	})

	e.waitNoJobs(10 * time.Second)
	e.reconcile()
}

// TestKeywordFolderLimit: a Dovecot maildir folder keeps 26 keywords (plan
// 3.7). Dovecot 2.4.5 accepts a 27th without an error (it keeps it only in
// its index), so Silo counts the folder's PERMANENTFLAGS: a label keyword
// beyond the limit is not stored, the label becomes local in that folder
// (keywordState partial with the folder in localOnlyFolders), keeps working
// there and is still a keyword in other folders. The 26 keywords keep working.
func TestKeywordFolderLimit(t *testing.T) {
	ctx := context.Background()
	e := newOpsEnv(t, imaptest.Full, 500*time.Millisecond, spec{"INBOX", 3}, spec{"Faktury", 1})
	inbox := e.a.folderID("INBOX")
	_, err := e.other.Select("INBOX", nil).Wait()
	must(t, err)
	uid := uidOf(t, e.other, "<m1@silo.test>")
	var kws []imap.Flag
	for i := 0; i < silosync.DefaultKeywordFolderLimit; i++ {
		kws = append(kws, imap.Flag("kw"+strconv.Itoa(i)))
	}
	_, err = e.other.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: kws}, nil).Collect()
	must(t, err)
	id1, _ := e.thread("m1@silo.test")
	waitFor(t, 10*time.Second, "26 tags imported", func() bool { return len(e.a.labelsOf(id1)) >= 26 })

	lid := e.createLabel("Nadmiar", nil, nil)
	id2, _ := e.thread("m2@silo.test")
	e.modify("m2@silo.test", []ops.LabelRef{{ID: lid}}, sys("UNREAD"))
	e.waitNoJobs(10 * time.Second)
	if e.a.int(`SELECT count(*) FROM label_folder_state WHERE label_id = $1 AND folder_id = $2`, lid, inbox) != 1 {
		t.Fatal("label not local in INBOX")
	}
	if fl := e.flagsIn("INBOX", "m2@silo.test"); slices.Contains(fl, "Nadmiar") || !slices.Contains(fl, `\Seen`) {
		t.Errorf("server flags of m2: %v", fl)
	}
	l, err := e.svc.GetLabel(ctx, e.a.userID, lid)
	must(t, err)
	if l.KeywordState != "partial" || !slices.Equal(l.LocalOnlyFolders, []int64{inbox}) {
		t.Errorf("label state: %s %v", l.KeywordState, l.LocalOnlyFolders)
	}
	if e.a.int(`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'label.changed' AND (payload->>'labelId')::bigint = $2`, e.a.userID, lid) == 0 {
		t.Error("no label.changed")
	}
	if e.a.int(`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'account.error'`, e.a.userID) != 0 {
		t.Error("account.error for a per-folder limit")
	}

	// The label stays on m2 through a full resync (no keyword to judge by).
	e.mgr.Worker(e.a.id).RequestResync(silosync.ResyncFull)
	time.Sleep(2 * time.Second)
	if !e.a.labelsOf(id2)["Nadmiar"] {
		t.Error("label lost after a full resync")
	}

	// One of the 26 keywords still works in INBOX.
	kwLabel, _, _, ok := e.userLabel("kw5")
	if !ok {
		t.Fatal("tag kw5 not imported")
	}
	e.modify("m3@silo.test", []ops.LabelRef{{ID: kwLabel}}, nil)
	waitFor(t, 10*time.Second, "kw5 on m3", func() bool { return slices.Contains(e.flagsIn("INBOX", "m3@silo.test"), "kw5") })

	// In another folder the label is a keyword as usual.
	e.modify("m4@silo.test", []ops.LabelRef{{ID: lid}}, nil)
	waitFor(t, 10*time.Second, "Nadmiar in Faktury", func() bool { return slices.Contains(e.flagsIn("Faktury", "m4@silo.test"), "Nadmiar") })
	e.waitNoJobs(5 * time.Second)
	if e.a.int(`SELECT count(*) FROM label_folder_state WHERE label_id = $1`, lid) != 1 {
		t.Error("Faktury became local too")
	}
	e.reconcile()
}

// TestKeywordRejectedInFolder: a server that answers NO to the STORE of a
// keyword in a folder announcing \* (injected: the test Dovecot never
// refuses) makes the label local in that folder only, as the limit does.
func TestKeywordRejectedInFolder(t *testing.T) {
	ctx := context.Background()
	e := newOpsEnv(t, imaptest.Full, 500*time.Millisecond, spec{"INBOX", 2}, spec{"Faktury", 1})
	e.a.deps.Hooks.StoreError = func(folder string, kws []string) error {
		if folder == "Faktury" && slices.Contains(kws, "Odrzucona") {
			return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "Too many keywords"}
		}
		return nil
	}
	e.restart()
	e.a.waitState(silosync.StateLive, 20*time.Second)
	lid := e.createLabel("Odrzucona", nil, nil)
	e.modify("m3@silo.test", []ops.LabelRef{{ID: lid}}, sys("UNREAD"))
	e.modify("m1@silo.test", []ops.LabelRef{{ID: lid}}, nil)
	e.waitNoJobs(10 * time.Second)
	waitFor(t, 10*time.Second, "keyword in INBOX", func() bool { return slices.Contains(e.flagsIn("INBOX", "m1@silo.test"), "Odrzucona") })
	if fl := e.flagsIn("Faktury", "m3@silo.test"); slices.Contains(fl, "Odrzucona") || !slices.Contains(fl, `\Seen`) {
		t.Errorf("Faktury flags: %v", fl)
	}
	l, err := e.svc.GetLabel(ctx, e.a.userID, lid)
	must(t, err)
	if l.KeywordState != "partial" || !slices.Equal(l.LocalOnlyFolders, []int64{e.a.folderID("Faktury")}) {
		t.Errorf("label state: %s %v", l.KeywordState, l.LocalOnlyFolders)
	}
	id3, _ := e.thread("m3@silo.test")
	if !e.a.labelsOf(id3)["Odrzucona"] {
		t.Error("label lost on m3")
	}
	if e.a.int(`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'account.error' AND payload->>'code' = $2`, e.a.userID, silosync.ErrCodeKeywords) != 0 {
		t.Error("account.error for a per-folder rejection")
	}
	e.reconcile()
}
