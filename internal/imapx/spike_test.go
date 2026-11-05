package imapx_test

// Phase 1 spike: what github.com/emersion/go-imap/v2 (v2.0.0-beta.8) gives us
// against a real Dovecot 2.4.5, and what imapx has to add. Findings are
// logged (run with -v) and condensed in silo/docs/adr/0006-go-imap-v2-spike.md.
// Assertions pin the facts the ADR relies on, so a library upgrade that changes
// them fails here first.

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/silopost/silo-server/internal/imapx"
	"github.com/silopost/silo-server/internal/testutil/imaptest"
)

type spikeConn struct {
	*imapx.Client
	c *imapclient.Client
}

func connect(t *testing.T, addr, user, pass string, h *imapclient.UnilateralDataHandler) *spikeConn {
	t.Helper()
	host, port := hostPort(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var opts []imapx.Option
	if h != nil {
		opts = append(opts, imapx.WithUnilateralDataHandler(h))
	}
	cl, err := imapx.Dial(ctx, host, port, imapx.TLSNone, false, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	if err := cl.Login(ctx, user, pass); err != nil {
		t.Fatal(err)
	}
	return &spikeConn{Client: cl, c: cl.IMAP()}
}

func rfc822(subject string) []byte {
	return fmt.Appendf(nil, "From: a@silo.test\r\nTo: silo@silo.test\r\nSubject: %s\r\nMessage-ID: <%s@silo.test>\r\nDate: Mon, 02 Jan 2006 15:04:05 +0000\r\n\r\nbody of %s\r\n", subject, strings.ReplaceAll(subject, " ", "-"), subject)
}

func (s *spikeConn) append(t *testing.T, mailbox, subject string, flags []imap.Flag, when time.Time) *imap.AppendData {
	t.Helper()
	b := rfc822(subject)
	cmd := s.c.Append(mailbox, int64(len(b)), &imap.AppendOptions{Flags: flags, Time: when})
	if _, err := cmd.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := cmd.Wait()
	if err != nil {
		t.Fatalf("APPEND %s: %v", mailbox, err)
	}
	return data
}

func (s *spikeConn) uidSearchAll(t *testing.T) []imap.UID {
	t.Helper()
	data, err := s.c.UIDSearch(&imap.SearchCriteria{}, &imap.SearchOptions{ReturnAll: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	set, ok := data.All.(imap.UIDSet)
	if !ok {
		return nil
	}
	uids, _ := set.Nums()
	return uids
}

// raw is a bare-socket IMAP session used where go-imap cannot express the command.
type raw struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
	n int
}

func dialRaw(t *testing.T, addr, user, pass string) *raw {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	r := &raw{t: t, c: c, r: bufio.NewReader(c)}
	if _, err := r.r.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	r.cmd("LOGIN " + user + " " + pass)
	return r
}

// cmd sends a command and returns all untagged lines plus the tagged status line.
func (r *raw) cmd(cmd string) (untagged []string, status string) {
	r.t.Helper()
	r.n++
	tag := fmt.Sprintf("r%d", r.n)
	_ = r.c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := r.c.Write([]byte(tag + " " + cmd + "\r\n")); err != nil {
		r.t.Fatal(err)
	}
	for {
		l, err := r.r.ReadString('\n')
		if err != nil {
			r.t.Fatalf("%s: %v", cmd, err)
		}
		l = strings.TrimRight(l, "\r\n")
		if strings.HasPrefix(l, tag+" ") {
			return untagged, l
		}
		untagged = append(untagged, l)
	}
}

func TestSpikeFull(t *testing.T) {
	addr, user, pass := imaptest.Start(t, imaptest.Full)
	ctx := context.Background()

	var (
		uidValidity uint32
		inboxUIDs   []imap.UID
		modseq1     uint64
	)
	s := connect(t, addr, user, pass, nil)

	t.Run("capabilities_and_enable", func(t *testing.T) {
		caps, err := s.Capabilities(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"QRESYNC", "CONDSTORE", "MOVE", "UIDPLUS", "IDLE", "SPECIAL-USE", "LIST-STATUS", "ESEARCH", "BINARY", "NOTIFY", "COMPRESS=DEFLATE", "UTF8=ACCEPT"} {
			t.Logf("  %-18s %v", name, caps.Has(name))
		}
		t.Logf("  OBJECTID (RFC 8474) %v, CREATE-SPECIAL-USE %v, IMAP4rev2 %v", caps.HasObjectID(), caps.HasCreateSpecialUse(), caps.Has("IMAP4rev2"))
		if !caps.HasQRESYNC() || !caps.HasMove() || !caps.HasUIDPlus() || !caps.HasIdle() || !caps.HasSpecialUse() {
			t.Fatal("DoD capabilities missing")
		}

		// FINDING: Enable refuses anything but IMAP4rev2/UTF8/METADATA, so
		// ENABLE QRESYNC (needed for VANISHED) cannot be sent via imapclient.
		_, err = s.c.Enable(imap.CapQResync).Wait()
		t.Logf("  ENABLE QRESYNC via imapclient: %v", err)
		if err == nil {
			t.Error("imapclient now allows ENABLE QRESYNC: revisit ADR 0006")
		}
		// CONDSTORE is enabled implicitly by SELECT (CONDSTORE) / FETCH MODSEQ.
		_, err = s.c.Enable(imap.CapCondStore).Wait()
		t.Logf("  ENABLE CONDSTORE via imapclient: %v", err)
	})

	t.Run("list_special_use_and_create", func(t *testing.T) {
		list, err := s.c.List("", "*", &imap.ListOptions{ReturnSpecialUse: true, ReturnChildren: true}).Collect()
		if err != nil {
			t.Fatal(err)
		}
		roles := map[string][]imap.MailboxAttr{}
		for _, l := range list {
			roles[l.Mailbox] = l.Attrs
			t.Logf("  LIST %-8s delim=%q attrs=%v", l.Mailbox, l.Delim, l.Attrs)
		}
		if !slices.Contains(roles["Sent"], imap.MailboxAttrSent) {
			t.Errorf("Sent role missing: %v", roles["Sent"])
		}

		// SELECT-style special-use filter, as one command.
		sel, err := s.c.List("", "*", &imap.ListOptions{SelectSpecialUse: true, ReturnSpecialUse: true}).Collect()
		t.Logf("  LIST (SPECIAL-USE) RETURN (SPECIAL-USE): %d mailboxes, err=%v", len(sel), err)

		// CREATE ... USE (\Archive) although CREATE-SPECIAL-USE is not advertised.
		err = s.c.Create("Archive", &imap.CreateOptions{SpecialUse: []imap.MailboxAttr{imap.MailboxAttrArchive}}).Wait()
		t.Logf("  CREATE Archive USE (\\Archive): %v", err)
		if err == nil {
			list, _ = s.c.List("", "Archive", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
			for _, l := range list {
				t.Logf("  LIST Archive attrs=%v (Dovecot accepts USE but ignores it without server-side config)", l.Attrs)
			}
		}
		_ = s.c.Subscribe("Archive").Wait()
		if err := s.c.Create("Projects/2026", nil).Wait(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("append_select_status", func(t *testing.T) {
		base := time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC)
		var last *imap.AppendData
		for i := 1; i <= 5; i++ {
			flags := []imap.Flag{imap.FlagSeen}
			if i%2 == 0 {
				flags = append(flags, imap.FlagFlagged)
			}
			last = s.append(t, "INBOX", fmt.Sprintf("msg %d", i), flags, base.Add(time.Duration(i)*time.Hour))
			inboxUIDs = append(inboxUIDs, last.UID)
			t.Logf("  APPENDUID uidvalidity=%d uid=%d", last.UIDValidity, last.UID)
		}

		sel, err := s.c.Select("INBOX", &imap.SelectOptions{CondStore: true}).Wait()
		if err != nil {
			t.Fatal(err)
		}
		uidValidity, modseq1 = sel.UIDValidity, sel.HighestModSeq
		t.Logf("  SELECT (CONDSTORE): exists=%d uidnext=%d uidvalidity=%d highestmodseq=%d permanentflags=%v",
			sel.NumMessages, sel.UIDNext, sel.UIDValidity, sel.HighestModSeq, sel.PermanentFlags)
		if sel.NumMessages != 5 || sel.HighestModSeq == 0 || sel.UIDValidity != last.UIDValidity {
			t.Errorf("unexpected select data: %+v", sel)
		}
		if !slices.Contains(sel.PermanentFlags, imap.FlagWildcard) {
			t.Errorf("PERMANENTFLAGS lacks \\*: %v", sel.PermanentFlags)
		}

		// INTERNALDATE round trip.
		msgs, err := s.c.Fetch(imap.UIDSetNum(inboxUIDs[0]), &imap.FetchOptions{UID: true, Flags: true, InternalDate: true, RFC822Size: true}).Collect()
		if err != nil || len(msgs) != 1 {
			t.Fatalf("fetch: %v %d", err, len(msgs))
		}
		t.Logf("  FETCH uid=%d flags=%v internaldate=%s size=%d", msgs[0].UID, msgs[0].Flags, msgs[0].InternalDate.UTC(), msgs[0].RFC822Size)
		if !msgs[0].InternalDate.Equal(base.Add(time.Hour)) {
			t.Errorf("INTERNALDATE not preserved: %s", msgs[0].InternalDate)
		}

		status, err := s.c.Status("INBOX", &imap.StatusOptions{NumMessages: true, UIDNext: true, UIDValidity: true, HighestModSeq: true}).Wait()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("  STATUS INBOX: messages=%d uidnext=%d uidvalidity=%d highestmodseq=%d", *status.NumMessages, status.UIDNext, status.UIDValidity, status.HighestModSeq)
		if status.HighestModSeq != sel.HighestModSeq {
			t.Errorf("STATUS HIGHESTMODSEQ %d != SELECT %d", status.HighestModSeq, sel.HighestModSeq)
		}

		// LIST-STATUS: discovery of all folders in one command (plan 3.5.3).
		ls, err := s.c.List("", "*", &imap.ListOptions{
			ReturnSpecialUse: true,
			ReturnStatus:     &imap.StatusOptions{NumMessages: true, UIDNext: true, UIDValidity: true, HighestModSeq: true},
		}).Collect()
		if err != nil {
			t.Fatal(err)
		}
		withStatus := 0
		for _, l := range ls {
			if l.Status != nil {
				withStatus++
				t.Logf("  LIST-STATUS %-14s uidvalidity=%d uidnext=%d highestmodseq=%d", l.Mailbox, l.Status.UIDValidity, l.Status.UIDNext, l.Status.HighestModSeq)
			}
		}
		if withStatus == 0 {
			t.Error("LIST ... RETURN (STATUS ...) returned no status")
		}
	})

	t.Run("keywords", func(t *testing.T) {
		set := imap.UIDSetNum(inboxUIDs[0], inboxUIDs[1])
		fcmd := s.c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{"$SiloSnoozed", "Praca"}}, nil)
		msgs, err := fcmd.Collect()
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			t.Logf("  STORE +FLAGS reply uid=%d flags=%v", m.UID, m.Flags)
		}
		got, _ := s.c.Fetch(imap.UIDSetNum(inboxUIDs[0]), &imap.FetchOptions{UID: true, Flags: true}).Collect()
		if !slices.Contains(got[0].Flags, "$SiloSnoozed") || !slices.Contains(got[0].Flags, "Praca") {
			t.Errorf("keywords not stored: %v", got[0].Flags)
		}
		// Keyword case: IMAP keywords are case-insensitive, servers keep the first spelling.
		sel := s.c.Mailbox()
		t.Logf("  SelectedMailbox flags after keywords=%v permanent=%v", sel.Flags, sel.PermanentFlags)
	})

	t.Run("fetch_changedsince_and_modseq", func(t *testing.T) {
		before, err := s.c.Select("INBOX", &imap.SelectOptions{CondStore: true}).Wait()
		if err != nil {
			t.Fatal(err)
		}
		// Change flags of UID inboxUIDs[2] only.
		if _, err := s.c.Store(imap.UIDSetNum(inboxUIDs[2]), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagAnswered}}, nil).Collect(); err != nil {
			t.Fatal(err)
		}
		var all imap.UIDSet
		all.AddRange(1, 0) // 1:*
		msgs, err := s.c.Fetch(all, &imap.FetchOptions{UID: true, Flags: true, ModSeq: true, ChangedSince: before.HighestModSeq}).Collect()
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			t.Logf("  FETCH CHANGEDSINCE %d -> uid=%d flags=%v modseq=%d", before.HighestModSeq, m.UID, m.Flags, m.ModSeq)
		}
		if len(msgs) != 1 || msgs[0].UID != inboxUIDs[2] || msgs[0].ModSeq <= before.HighestModSeq {
			t.Errorf("CHANGEDSINCE returned %d messages", len(msgs))
		}
		// UNCHANGEDSINCE (optimistic write-back) exists too:
		t.Log("  imap.StoreOptions.UnchangedSince is available for conditional STORE")
	})

	t.Run("search_esearch", func(t *testing.T) {
		uids := s.uidSearchAll(t)
		t.Logf("  UID SEARCH ALL RETURN (ALL) -> %v", uids)
		if len(uids) != 5 {
			t.Errorf("want 5 uids, got %v", uids)
		}
		since, err := s.c.UIDSearch(&imap.SearchCriteria{Since: time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)}, &imap.SearchOptions{ReturnAll: true, ReturnCount: true}).Wait()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("  UID SEARCH SINCE ... count=%d", since.Count)
	})

	t.Run("expunge_without_qresync", func(t *testing.T) {
		// Second connection selected on INBOX sees expunges as plain EXPUNGE
		// (sequence numbers), because QRESYNC cannot be enabled.
		var mu sync.Mutex
		var expunged []uint32
		other := connect(t, addr, user, pass, &imapclient.UnilateralDataHandler{
			Expunge: func(seq uint32) { mu.Lock(); expunged = append(expunged, seq); mu.Unlock() },
		})
		if _, err := other.c.Select("INBOX", &imap.SelectOptions{CondStore: true}).Wait(); err != nil {
			t.Fatal(err)
		}

		// s deletes the 4th message: STORE \Deleted + UID EXPUNGE (UIDPLUS).
		victim := inboxUIDs[3]
		if _, err := s.c.Store(imap.UIDSetNum(victim), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Collect(); err != nil {
			t.Fatal(err)
		}
		ex := s.c.UIDExpunge(imap.UIDSetNum(victim))
		var seqs []uint32
		for seq := ex.Next(); seq != 0; seq = ex.Next() {
			seqs = append(seqs, seq)
		}
		if err := ex.Close(); err != nil {
			t.Fatal(err)
		}
		t.Logf("  UID EXPUNGE reply: EXPUNGE seq=%v", seqs)

		if err := other.c.Noop().Wait(); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		t.Logf("  other connection, unilateral Expunge handler: seq=%v", expunged)
		mu.Unlock()
		if !slices.Equal(seqs, []uint32{4}) {
			t.Errorf("EXPUNGE seq = %v", seqs)
		}
		// Detection of deletions without VANISHED: UID SEARCH ALL diff.
		now := s.uidSearchAll(t)
		t.Logf("  UID SEARCH ALL after expunge -> %v (diff = deleted)", now)
		if slices.Contains(now, victim) || len(now) != 4 {
			t.Errorf("unexpected uid set %v", now)
		}
	})

	t.Run("qresync_server_side_and_client_gap", func(t *testing.T) {
		// Folder QR: four messages, remember (uidvalidity, highestmodseq), then
		// delete two, change flags of one, append one, as another client would.
		if err := s.c.Create("QR", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		var uids []imap.UID
		for i := 1; i <= 4; i++ {
			uids = append(uids, s.append(t, "QR", fmt.Sprintf("qr %d", i), nil, time.Time{}).UID)
		}
		sel, err := s.c.Select("QR", &imap.SelectOptions{CondStore: true}).Wait()
		if err != nil {
			t.Fatal(err)
		}
		savedValidity, savedModSeq := sel.UIDValidity, sel.HighestModSeq
		set := imap.UIDSetNum(uids[1], uids[2])
		if _, err := s.c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Collect(); err != nil {
			t.Fatal(err)
		}
		ex := s.c.UIDExpunge(set)
		for ex.Next() != 0 {
		}
		_ = ex.Close()
		if _, err := s.c.Store(imap.UIDSetNum(uids[0]), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}, nil).Collect(); err != nil {
			t.Fatal(err)
		}
		newUID := s.append(t, "QR", "qr 5", nil, time.Time{}).UID
		_ = s.c.Unselect().Wait()

		// Server side works: ENABLE QRESYNC + SELECT (QRESYNC (...)) on a raw socket.
		r := dialRaw(t, addr, user, pass)
		en, st := r.cmd("ENABLE QRESYNC")
		t.Logf("  raw ENABLE QRESYNC -> %v / %s", en, st)
		lines, st := r.cmd(fmt.Sprintf("SELECT QR (QRESYNC (%d %d))", savedValidity, savedModSeq))
		for _, l := range lines {
			t.Logf("  raw SELECT QRESYNC: %s", l)
		}
		t.Logf("  raw SELECT QRESYNC: %s", st)
		joined := strings.Join(lines, "\n")
		wantVanished := fmt.Sprintf("* VANISHED (EARLIER) %d:%d", uids[1], uids[2])
		if !strings.Contains(joined, wantVanished) {
			t.Errorf("no %q in SELECT QRESYNC reply", wantVanished)
		}
		if !strings.Contains(joined, fmt.Sprintf("UID %d", uids[0])) || !strings.Contains(joined, "MODSEQ") {
			t.Errorf("flag change of uid %d not reported with MODSEQ", uids[0])
		}
		if strings.Contains(joined, fmt.Sprintf("UID %d", newUID)) {
			t.Logf("  note: new uid %d also returned in the QRESYNC FETCH list", newUID)
		}

		// Client side gap: the same SELECT cannot be written with imapclient
		// (SelectOptions has only ReadOnly and CondStore) ...
		t.Log("  imap.SelectOptions fields: ReadOnly, CondStore (no QRESYNC parameters)")
		// ... and an unexpected VANISHED kills the connection (see next test).
		// CONDSTORE-only path on the same data, natively supported:
		sel2, err := s.c.Select("QR", &imap.SelectOptions{CondStore: true}).Wait()
		if err != nil {
			t.Fatal(err)
		}
		var all imap.UIDSet
		all.AddRange(1, 0)
		ch, err := s.c.Fetch(all, &imap.FetchOptions{UID: true, Flags: true, ModSeq: true, ChangedSince: savedModSeq}).Collect()
		if err != nil {
			t.Fatal(err)
		}
		var changed []imap.UID
		for _, m := range ch {
			changed = append(changed, m.UID)
		}
		present := s.uidSearchAll(t)
		t.Logf("  CONDSTORE path: highestmodseq %d -> %d, CHANGEDSINCE uids=%v, UID SEARCH ALL=%v (deleted = local minus this)", savedModSeq, sel2.HighestModSeq, changed, present)
		if !slices.Equal(present, []imap.UID{uids[0], uids[3], newUID}) {
			t.Errorf("present = %v", present)
		}
		_ = s.c.Unselect().Wait()
	})

	t.Run("move_copyuid", func(t *testing.T) {
		if _, err := s.c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		uid := inboxUIDs[4]
		data, err := s.c.Move(imap.UIDSetNum(uid), "Projects/2026").Wait()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("  MOVE COPYUID: uidvalidity=%d src=%v dst=%v", data.UIDValidity, data.SourceUIDs, data.DestUIDs)
		if data.DestUIDs == nil || data.UIDValidity == 0 {
			t.Fatalf("COPYUID missing: %+v", data)
		}
		if slices.Contains(s.uidSearchAll(t), uid) {
			t.Error("message still in source")
		}
		// COPY: COPYUID too.
		cp, err := s.c.Copy(imap.UIDSetNum(inboxUIDs[0]), "Projects/2026").Wait()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("  COPY COPYUID: uidvalidity=%d src=%v dst=%v", cp.UIDValidity, cp.SourceUIDs, cp.DestUIDs)
	})

	t.Run("idle_unilateral", func(t *testing.T) {
		var mu sync.Mutex
		var events []string
		exists := make(chan uint32, 8)
		flagsSeen := make(chan uint32, 8)
		idler := connect(t, addr, user, pass, &imapclient.UnilateralDataHandler{
			Mailbox: func(d *imapclient.UnilateralDataMailbox) {
				if d.NumMessages != nil {
					mu.Lock()
					events = append(events, fmt.Sprintf("EXISTS %d", *d.NumMessages))
					mu.Unlock()
					exists <- *d.NumMessages
				}
			},
			Fetch: func(m *imapclient.FetchMessageData) {
				buf, err := m.Collect()
				if err != nil {
					return
				}
				mu.Lock()
				events = append(events, fmt.Sprintf("FETCH seq=%d uid=%d flags=%v modseq=%d", buf.SeqNum, buf.UID, buf.Flags, buf.ModSeq))
				mu.Unlock()
				flagsSeen <- uint32(buf.UID)
			},
			Expunge: func(seq uint32) {
				mu.Lock()
				events = append(events, fmt.Sprintf("EXPUNGE %d", seq))
				mu.Unlock()
			},
		})
		sel, err := idler.c.Select("INBOX", &imap.SelectOptions{CondStore: true}).Wait()
		if err != nil {
			t.Fatal(err)
		}
		idle, err := idler.c.Idle()
		if err != nil {
			t.Fatal(err)
		}
		t0 := time.Now()
		s.append(t, "INBOX", "idle new", nil, time.Time{}) // from the other connection
		select {
		case n := <-exists:
			t.Logf("  IDLE: EXISTS %d after %s (was %d)", n, time.Since(t0).Round(time.Millisecond), sel.NumMessages)
			if n != sel.NumMessages+1 {
				t.Errorf("EXISTS = %d", n)
			}
		case <-time.After(5 * time.Second):
			t.Error("no EXISTS during IDLE")
		}
		// Flag change by another client during IDLE -> unilateral FETCH.
		if _, err := s.c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		if _, err := s.c.Store(imap.UIDSetNum(inboxUIDs[0]), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDraft}}, nil).Collect(); err != nil {
			t.Fatal(err)
		}
		select {
		case uid := <-flagsSeen:
			t.Logf("  IDLE: unilateral FETCH for uid %d after %s", uid, time.Since(t0).Round(time.Millisecond))
		case <-time.After(5 * time.Second):
			t.Error("no unilateral FETCH during IDLE")
		}
		if err := idle.Close(); err != nil {
			t.Fatal(err)
		}
		if err := idle.Wait(); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		t.Logf("  IDLE events: %v", events)
		mu.Unlock()
		// The connection is usable again after IDLE.
		if err := idler.c.Noop().Wait(); err != nil {
			t.Errorf("NOOP after IDLE: %v", err)
		}
		t.Log("  go-imap restarts IDLE itself every 28 min (plan says 25: needs own timer or accept)")
	})

	_ = modseq1
	_ = uidValidity
}

// TestSpikeVanishedKillsClient shows that go-imap treats VANISHED (RFC 7162) as
// an unknown response and drops the connection, so QRESYNC cannot be enabled
// without patching the library. A scripted server sends it.
func TestSpikeVanishedKillsClient(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		fmt.Fprint(c, "* OK [CAPABILITY IMAP4rev1 QRESYNC CONDSTORE] ready\r\n")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			tag := strings.SplitN(line, " ", 2)[0]
			fmt.Fprintf(c, "* VANISHED (EARLIER) 2:3\r\n%s OK done\r\n", tag)
		}
	}()
	host, port := hostPort(t, l.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cl, err := imapx.Dial(ctx, host, port, imapx.TLSNone, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	err = cl.IMAP().Noop().Wait()
	t.Logf("NOOP answered with VANISHED -> %v", err)
	if err == nil {
		t.Fatal("expected go-imap to fail on VANISHED: revisit ADR 0006")
	}
	select {
	case <-cl.IMAP().Closed():
		t.Log("connection closed by client after the unsupported response")
	case <-time.After(2 * time.Second):
		t.Error("client still open")
	}
}

// Profiles without the extensions: the fallbacks Phase 2 relies on.
func TestSpikeNoCondstoreProfile(t *testing.T) {
	addr, user, pass := imaptest.Start(t, imaptest.NoCondstore)
	s := connect(t, addr, user, pass, nil)
	caps, err := s.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("CAPABILITY: %v", caps.Names())
	if caps.HasCondstore() || caps.HasQRESYNC() || !caps.HasMove() || !caps.HasUIDPlus() {
		t.Fatalf("profile no-condstore wrong: %v", caps.Names())
	}
	// Full-scan path: STATUS (MESSAGES UIDNEXT UIDVALIDITY) without HIGHESTMODSEQ.
	s.append(t, "INBOX", "a", nil, time.Time{})
	st, err := s.c.Status("INBOX", &imap.StatusOptions{NumMessages: true, UIDNext: true, UIDValidity: true}).Wait()
	if err != nil || *st.NumMessages != 1 {
		t.Fatalf("status: %+v %v", st, err)
	}
	if _, err := s.c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.c.Fetch(imap.UIDSetNum(st.UIDNext-1), &imap.FetchOptions{UID: true, Flags: true}).Collect()
	if err != nil || len(msgs) != 1 {
		t.Fatalf("flag scan: %v", err)
	}
}

func TestSpikeNoMoveProfile(t *testing.T) {
	addr, user, pass := imaptest.Start(t, imaptest.NoMove)
	s := connect(t, addr, user, pass, nil)
	caps, err := s.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("CAPABILITY: %v", caps.Names())
	if caps.HasMove() || caps.HasUIDPlus() || !caps.HasCondstore() || !caps.HasQRESYNC() {
		t.Fatalf("profile no-move wrong: %v", caps.Names())
	}
	if err := s.c.Create("Dest", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	// APPEND without UIDPLUS advertised: does Dovecot still return APPENDUID?
	ad := s.append(t, "INBOX", "to move", nil, time.Time{})
	t.Logf("APPENDUID without advertised UIDPLUS: uid=%d uidvalidity=%d (server still sends it)", ad.UID, ad.UIDValidity)
	// A bystander the user marked \Deleted in another client but did not expunge.
	bystander := s.append(t, "INBOX", "bystander", []imap.Flag{imap.FlagDeleted}, time.Time{})
	if _, err := s.c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	// imapclient.Move falls back to UID COPY + UID STORE +FLAGS.SILENT \Deleted + EXPUNGE
	// by itself when MOVE is not advertised; without UIDPLUS it uses plain EXPUNGE,
	// which removes every \Deleted message in the folder (the plan forbids that).
	data, err := s.c.Move(imap.UIDSetNum(ad.UID), "Dest").Wait()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fallback Move result: %+v", data)
	// FINDING: without UIDPLUS the fallback ends in a plain EXPUNGE, which also
	// destroys the bystander. imapx must not use Client.Move on such servers
	// (see ADR 0006); this assertion documents the trap.
	if got := s.uidSearchAll(t); len(got) != 0 {
		t.Errorf("trap not reproduced (remaining %v, bystander %d): revisit ADR 0006", got, bystander.UID)
	} else {
		t.Logf("DANGER: fallback Move expunged the unrelated \\Deleted message %d too", bystander.UID)
	}
	if _, err := s.c.Select("Dest", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if got := s.uidSearchAll(t); len(got) != 1 {
		t.Errorf("dest: %v", got)
	}
}
