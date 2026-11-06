// Command upgradecheck drives scripts/upgrade-test.sh: it talks to a running
// silo-server over the public REST API only (so the same build works against
// the previous release and the new one) and to the Dovecot of the test.
//
//	upgradecheck seed   -base URL -imap HOST:PORT -state FILE
//	upgradecheck verify -base URL -state FILE [-expect-version V]
//
// seed appends four messages to the INBOX of the test mailbox, signs in
// (creating the user and account), waits for the sync to be live, creates a
// user label and puts it on one thread; when the release has the phase 4 API
// (v0.4.0+) it also creates a filter and snoozes another thread for an hour.
// It saves the normalised INBOX and SNOOZED listings and the filter.
// verify signs in with the same password, waits for the sync, and compares the
// listings (with the label; the snoozed thread still SNOOZED), the filter and
// the server version.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	user     = "silo"
	password = "silopass"
	domain   = "silo.test"
	messages = 4 // one thread each; one is snoozed when the release has snooze
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: upgradecheck seed|verify [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	base := fs.String("base", "http://127.0.0.1:8080", "server URL")
	imapAddr := fs.String("imap", "", "IMAP host:port of the test mailbox (seed)")
	state := fs.String("state", "upgrade-state.json", "file with the seeded state")
	expect := fs.String("expect-version", "", "serverVersion GET /meta must report (verify)")
	timeout := fs.Duration("timeout", 3*time.Minute, "overall time limit")
	_ = fs.Parse(os.Args[2:])

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c := newClient(*base)
	var err error
	switch os.Args[1] {
	case "seed":
		err = seed(ctx, c, *imapAddr, *state)
	case "verify":
		err = verify(ctx, c, *state, *expect)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "upgradecheck:", err)
		os.Exit(1)
	}
}

// Snapshot is what seed saves and verify compares.
type Snapshot struct {
	ServerVersion string   `json:"serverVersion"`
	LabelID       string   `json:"labelId"`
	Inbox         []Thread `json:"inbox"`
	// Phase 4 data, when the old release has filters and snooze (v0.4.0+).
	FilterID       string   `json:"filterId,omitempty"`
	FilterCriteria string   `json:"filterCriteria,omitempty"`
	Snoozed        []Thread `json:"snoozed,omitempty"`
}

// Thread is the normalised part of a thread of the list: fields that must not
// change across an upgrade.
type Thread struct {
	ID           string     `json:"id"`
	Subject      string     `json:"subject"`
	MessageCount int        `json:"messageCount"`
	UnreadCount  int        `json:"unreadCount"`
	LabelIDs     []string   `json:"labelIds"`
	SnoozedUntil *time.Time `json:"snoozedUntil,omitempty"`
}

const filterCriteria = "from:jan@firma.pl subject:upgrade"

func seed(ctx context.Context, c *client, imapAddr, state string) error {
	if err := appendMessages(ctx, imapAddr); err != nil {
		return err
	}
	acc, err := c.login(ctx)
	if err != nil {
		return err
	}
	inbox, err := c.waitList(ctx, acc, "INBOX", messages)
	if err != nil {
		return err
	}
	var label struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, "POST", "/labels", map[string]string{"name": "Upgrade test", "type": "user"}, &label); err != nil {
		return fmt.Errorf("create label: %w", err)
	}
	if err := c.do(ctx, "POST", "/threads/modify", map[string]any{"ids": []string{inbox[0].ID}, "addLabelIds": []string{label.ID}}, nil); err != nil {
		return fmt.Errorf("label thread: %w", err)
	}
	snap := Snapshot{ServerVersion: c.version(ctx), LabelID: label.ID}

	// Phase 4: a filter (new mail only: the listing does not change) and a
	// thread snoozed for an hour (it leaves the INBOX listing).
	var filter struct {
		ID string `json:"id"`
	}
	err = c.do(ctx, "POST", "/filters", map[string]any{"name": "Upgrade test", "criteria": filterCriteria,
		"actions": map[string]any{"star": true}}, &filter)
	inInbox := messages
	switch {
	case errors.Is(err, errNotFound):
		fmt.Println("no filters and snooze in this release: phase 4 data not seeded")
	case err != nil:
		return fmt.Errorf("create filter: %w", err)
	default:
		snap.FilterID, snap.FilterCriteria = filter.ID, filterCriteria
		until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		if err := c.do(ctx, "POST", "/threads/"+inbox[len(inbox)-1].ID+"/snooze", map[string]any{"until": until}, nil); err != nil {
			return fmt.Errorf("snooze thread: %w", err)
		}
		inInbox--
		// The snooze reaches the mail server after the undo window.
		if snap.Snoozed, err = c.waitList(ctx, acc, "SNOOZED", 1); err != nil {
			return err
		}
		if s := snap.Snoozed[0]; s.ID != inbox[len(inbox)-1].ID || s.SnoozedUntil == nil || !s.SnoozedUntil.Equal(until) {
			return fmt.Errorf("snoozed listing: %+v, want %s until %s", snap.Snoozed, inbox[len(inbox)-1].ID, until)
		}
	}
	// Wait for the label to reach the mail server (no pending operation), so
	// the snapshot does not depend on a job left for the next release.
	if snap.Inbox, err = c.waitList(ctx, acc, "INBOX", inInbox); err != nil {
		return err
	}
	if !hasLabel(snap.Inbox, label.ID) {
		return fmt.Errorf("label %s missing from the INBOX listing: %+v", label.ID, snap.Inbox)
	}
	b, _ := json.MarshalIndent(snap, "", "  ")
	fmt.Printf("seeded (server %s):\n%s\n", snap.ServerVersion, b)
	return os.WriteFile(state, b, 0o644)
}

func verify(ctx context.Context, c *client, state, expect string) error {
	b, err := os.ReadFile(state)
	if err != nil {
		return err
	}
	var want Snapshot
	if err := json.Unmarshal(b, &want); err != nil {
		return err
	}
	version := c.version(ctx)
	if expect != "" && version != expect {
		return fmt.Errorf("serverVersion = %q, want %q", version, expect)
	}
	if version == want.ServerVersion {
		return fmt.Errorf("serverVersion %q did not change", version)
	}
	acc, err := c.login(ctx)
	if err != nil {
		return fmt.Errorf("sign in after the upgrade: %w", err)
	}
	got, err := c.waitList(ctx, acc, "INBOX", len(want.Inbox))
	if err != nil {
		return err
	}
	if err := sameList("INBOX", got, want.Inbox); err != nil {
		return err
	}
	if !hasLabel(got, want.LabelID) {
		return fmt.Errorf("label %s lost", want.LabelID)
	}
	extra := ""
	if want.FilterID != "" {
		snoozed, err := c.waitList(ctx, acc, "SNOOZED", len(want.Snoozed))
		if err != nil {
			return err
		}
		if err := sameList("SNOOZED", snoozed, want.Snoozed); err != nil {
			return err
		}
		var filters struct {
			Filters []struct {
				ID       string `json:"id"`
				Criteria string `json:"criteria"`
				Enabled  bool   `json:"enabled"`
				Actions  struct {
					Star bool `json:"star"`
				} `json:"actions"`
			} `json:"filters"`
		}
		if err := c.do(ctx, "GET", "/filters?accountId="+acc, nil, &filters); err != nil {
			return fmt.Errorf("list filters: %w", err)
		}
		found := false
		for _, f := range filters.Filters {
			found = found || (f.ID == want.FilterID && f.Criteria == want.FilterCriteria && f.Enabled && f.Actions.Star)
		}
		if !found {
			return fmt.Errorf("filter %s (%q) lost: %+v", want.FilterID, want.FilterCriteria, filters.Filters)
		}
		extra = fmt.Sprintf(", snoozed thread %s and filter %s", want.Snoozed[0].ID, want.FilterID)
	}
	fmt.Printf("verified: server %s -> %s, %d INBOX threads and label %s unchanged%s\n", want.ServerVersion, version, len(got), want.LabelID, extra)
	return nil
}

func sameList(label string, got, want []Thread) error {
	if reflect.DeepEqual(got, want) {
		return nil
	}
	g, _ := json.MarshalIndent(got, "", "  ")
	w, _ := json.MarshalIndent(want, "", "  ")
	return fmt.Errorf("%s changed across the upgrade:\nbefore %s\nafter  %s", label, w, g)
}

func hasLabel(threads []Thread, id string) bool {
	for _, t := range threads {
		if slices.Contains(t.LabelIDs, id) {
			return true
		}
	}
	return false
}

// appendMessages stores the messages in the INBOX, retrying until Dovecot
// accepts connections.
func appendMessages(ctx context.Context, addr string) error {
	var cl *imapclient.Client
	for {
		var err error
		if cl, err = imapclient.DialInsecure(addr, nil); err == nil {
			if err = cl.Login(user, password).Wait(); err == nil {
				break
			}
			cl.Close()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("imap %s: %w", addr, err)
		case <-time.After(time.Second):
		}
	}
	defer cl.Close()
	when := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	for i := 1; i <= messages; i++ {
		msg := fmt.Sprintf("From: Jan Kowalski <jan@firma.pl>\r\nTo: %s@%s\r\nSubject: Upgrade message %d\r\n"+
			"Message-ID: <upgrade-%d@silo.test>\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n"+
			"Body of message %d.\r\n", user, domain, i, i, when.Add(time.Duration(i)*time.Hour).Format(time.RFC1123Z), i)
		cmd := cl.Append("INBOX", int64(len(msg)), nil)
		if _, err := io.WriteString(cmd, msg); err != nil {
			return err
		}
		if err := cmd.Close(); err != nil {
			return err
		}
		if _, err := cmd.Wait(); err != nil {
			return fmt.Errorf("append: %w", err)
		}
	}
	return cl.Logout().Wait()
}

// errNotFound wraps 404 answers (an endpoint the release does not have).
var errNotFound = errors.New("404 not found")

type client struct {
	base string
	http *http.Client
	csrf string
}

func newClient(base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{base: strings.TrimRight(base, "/") + "/api/v1", http: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" && method != http.MethodGet {
		req.Header.Set("X-Silo-CSRF", c.csrf)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w: %s", method, path, errNotFound, b)
	}
	if res.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, res.StatusCode, b)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *client) version(ctx context.Context) string {
	var meta struct {
		ServerVersion string `json:"serverVersion"`
	}
	_ = c.do(ctx, "GET", "/meta", nil, &meta)
	return meta.ServerVersion
}

// login signs in and returns the id of the first account.
func (c *client) login(ctx context.Context) (string, error) {
	var s struct {
		CSRFToken string `json:"csrfToken"`
		Accounts  []struct {
			ID string `json:"id"`
		} `json:"accounts"`
	}
	if err := c.do(ctx, "POST", "/session", map[string]string{"email": user, "password": password}, &s); err != nil {
		return "", err
	}
	if len(s.Accounts) == 0 {
		return "", errors.New("session without accounts")
	}
	c.csrf = s.CSRFToken
	return s.Accounts[0].ID, nil
}

// waitList waits until the account is live and the label (INBOX, SNOOZED) lists n threads with
// no pending operation, and returns them normalised.
func (c *client) waitList(ctx context.Context, acc, label string, n int) ([]Thread, error) {
	last := "no answer"
	for {
		var st struct {
			State string `json:"state"`
		}
		var list struct {
			Threads []struct {
				Thread
				PendingSync bool `json:"pendingSync"`
			} `json:"threads"`
		}
		err := c.do(ctx, "GET", "/accounts/"+acc+"/sync-status", nil, &st)
		if err == nil && st.State == "live" {
			err = c.do(ctx, "GET", "/threads?labelId="+label, nil, &list)
		}
		switch {
		case err != nil:
			last = err.Error()
		case st.State != "live":
			last = "state " + st.State
		default:
			pending := false
			out := make([]Thread, 0, len(list.Threads))
			for _, t := range list.Threads {
				pending = pending || t.PendingSync
				slices.Sort(t.LabelIDs)
				out = append(out, t.Thread)
			}
			if len(out) == n && !pending {
				slices.SortFunc(out, func(a, b Thread) int { return strings.Compare(a.ID, b.ID) })
				return out, nil
			}
			last = fmt.Sprintf("%d threads, pending %v", len(out), pending)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for %s (%s): %w", label, last, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
