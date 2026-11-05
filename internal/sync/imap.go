package sync

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/silopost/silo-server/internal/imapx"
	"github.com/silopost/silo-server/internal/mail"
)

// conn is one IMAP connection of a session with the capabilities it saw.
type conn struct {
	cl       *imapx.Client
	caps     imapx.Capabilities
	selected string
	sel      *imap.SelectData
}

func (c *conn) imap() *imapclient.Client { return c.cl.IMAP() }

// await runs one command with the default timeout.
func (c *conn) await(ctx context.Context, timeout time.Duration, fn func() error) error {
	if timeout <= 0 {
		timeout = commandTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := c.cl.Await(ctx, fn)
	if errors.Is(err, context.DeadlineExceeded) {
		// imapx closed the connection; the session cannot go on.
		return errors.Join(imapx.ErrUnavailable, err)
	}
	return err
}

// selectFolder SELECTs a mailbox (with CONDSTORE when available) and keeps
// the result.
func (c *conn) selectFolder(ctx context.Context, name string) (*imap.SelectData, error) {
	var opts *imap.SelectOptions
	if c.caps.HasCondstore() {
		opts = &imap.SelectOptions{CondStore: true}
	}
	c.selected, c.sel = "", nil
	var data *imap.SelectData
	err := c.await(ctx, 0, func() error {
		var err error
		data, err = c.imap().Select(name, opts).Wait()
		return err
	})
	if err != nil {
		return nil, err
	}
	c.selected, c.sel = name, data
	return data, nil
}

// searchUIDs runs UID SEARCH (ESEARCH RETURN (ALL) when available) and
// returns the UIDs in ascending order.
func (c *conn) searchUIDs(ctx context.Context, criteria *imap.SearchCriteria) ([]uint32, error) {
	var opts *imap.SearchOptions
	if c.caps.HasESearch() {
		opts = &imap.SearchOptions{ReturnAll: true}
	}
	var data *imap.SearchData
	err := c.await(ctx, 0, func() error {
		var err error
		data, err = c.imap().UIDSearch(criteria, opts).Wait()
		return err
	})
	if err != nil {
		return nil, err
	}
	all := data.AllUIDs()
	out := make([]uint32, len(all))
	for i, u := range all {
		out[i] = uint32(u)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// uidRange is the criteria for UIDs in [lo, hi] (hi 0 = *).
func uidRange(lo, hi uint32) *imap.SearchCriteria {
	return &imap.SearchCriteria{UID: []imap.UIDSet{{imap.UIDRange{Start: imap.UID(lo), Stop: imap.UID(hi)}}}}
}

func uidSet(uids []uint32) imap.UIDSet {
	var s imap.UIDSet
	for _, u := range uids {
		s.AddNum(imap.UID(u))
	}
	return s
}

var (
	sectionHeader = &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
	sectionText   = &imap.FetchItemBodySection{Specifier: imap.PartSpecifierText, Peek: true, Partial: &imap.SectionPartial{Offset: 0, Size: 4096}}
	sectionFull   = &imap.FetchItemBodySection{Peek: true}
)

// fetchHeaders fetches the ingest items of plan 3.5.3 for the UIDs. EMAILID
// is not requested: go-imap/v2 beta.8 cannot express it (ADR 0006), so the
// matcher relies on dedupe_key.
func (c *conn) fetchHeaders(ctx context.Context, uids []uint32) ([]mail.FetchedMessage, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	opts := &imap.FetchOptions{
		UID: true, Flags: true, InternalDate: true, RFC822Size: true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection:   []*imap.FetchItemBodySection{sectionHeader, sectionText},
		ModSeq:        c.caps.HasCondstore(),
	}
	var bufs []*imapclient.FetchMessageBuffer
	err := c.await(ctx, 0, func() error {
		var err error
		bufs, err = c.imap().Fetch(uidSet(uids), opts).Collect()
		return err
	})
	if err != nil {
		return nil, err
	}
	want := map[uint32]bool{}
	for _, u := range uids {
		want[u] = true
	}
	out := make([]mail.FetchedMessage, 0, len(bufs))
	for _, b := range bufs {
		uid := uint32(b.UID)
		if !want[uid] { // "n:*" quirks and unrelated responses
			continue
		}
		out = append(out, mail.FetchedMessage{
			UID: uid, Flags: flagStrings(b.Flags), InternalDate: b.InternalDate, Size: b.RFC822Size,
			ModSeq: b.ModSeq, Header: b.FindBodySection(sectionHeader), TextSnippet: b.FindBodySection(sectionText),
			BodyStructure: b.BodyStructure,
		})
	}
	// Newest first inside a batch, as the plan orders the backfill.
	slices.SortFunc(out, func(a, b mail.FetchedMessage) int { return int(int64(b.UID) - int64(a.UID)) })
	return out, nil
}

// fetchFlags fetches FLAGS (and MODSEQ) for a UID set; changedSince > 0 adds
// CHANGEDSINCE.
func (c *conn) fetchFlags(ctx context.Context, set imap.UIDSet, changedSince uint64) ([]mail.FlagUpdate, error) {
	opts := &imap.FetchOptions{UID: true, Flags: true, ModSeq: c.caps.HasCondstore(), ChangedSince: changedSince}
	var bufs []*imapclient.FetchMessageBuffer
	err := c.await(ctx, 0, func() error {
		var err error
		bufs, err = c.imap().Fetch(set, opts).Collect()
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]mail.FlagUpdate, 0, len(bufs))
	for _, b := range bufs {
		if b.UID == 0 {
			continue
		}
		out = append(out, mail.FlagUpdate{UID: uint32(b.UID), Flags: flagStrings(b.Flags), ModSeq: b.ModSeq})
	}
	return out, nil
}

// fetchBodies fetches BODY.PEEK[] of the UIDs; the timeout grows with the
// expected size (plan 3.5.4).
func (c *conn) fetchBodies(ctx context.Context, uids []uint32, expectBytes int64) (map[uint32][]byte, error) {
	timeout := commandTimeout + time.Duration(expectBytes/(256<<10))*time.Second
	var bufs []*imapclient.FetchMessageBuffer
	err := c.await(ctx, timeout, func() error {
		var err error
		bufs, err = c.imap().Fetch(uidSet(uids), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{sectionFull}}).Collect()
		return err
	})
	if err != nil {
		return nil, err
	}
	out := map[uint32][]byte{}
	for _, b := range bufs {
		if raw := b.FindBodySection(sectionFull); raw != nil && b.UID != 0 {
			out[uint32(b.UID)] = raw
		}
	}
	return out, nil
}

// flagStrings converts flags, dropping \Recent (session-only, IMAP4rev1).
func flagStrings(flags []imap.Flag) []string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		if strings.EqualFold(string(f), `\Recent`) {
			continue
		}
		out = append(out, string(f))
	}
	return out
}

// isCommandError reports a tagged NO/BAD for one command: the folder is
// marked as failing and the session goes on (plan 3.5.4).
func isCommandError(err error) bool {
	var ie *imap.Error
	return errors.As(err, &ie) && (ie.Type == imap.StatusResponseTypeNo || ie.Type == imap.StatusResponseTypeBad)
}

// isQuotaError reports NO [OVERQUOTA] / [LIMIT].
func isQuotaError(err error) bool {
	var ie *imap.Error
	return errors.As(err, &ie) && (ie.Code == imap.ResponseCodeOverQuota || ie.Code == imap.ResponseCodeLimit)
}
