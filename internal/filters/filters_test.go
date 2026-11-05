package filters

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/silopost/silo-server/internal/ops"
	"github.com/silopost/silo-server/internal/search"
)

func TestParseCriteria(t *testing.T) {
	for _, c := range []struct {
		q    string
		want error
	}{
		{"from:newsletter@x.com has:attachment", nil},
		{"  subject:(faktura) -in:spam ", nil},
		{"", ErrInvalid},
		{"   ", ErrInvalid},
		{"in:anywhere", ErrInvalid},
		{"IN:Anywhere", ErrInvalid},
		{"in:anywhere faktura", nil},
		{"a)", search.ErrInvalidQuery},
	} {
		_, err := ParseCriteria(c.q)
		if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("ParseCriteria(%q) = %v, want %v", c.q, err, c.want)
		}
	}
}

func TestNeedsBody(t *testing.T) {
	for q, want := range map[string]bool{
		"from:jan":                  false,
		"subject:faktura":           false,
		"faktura":                   true,
		`from:jan "umowa najmu"`:    true,
		"from:jan -(to:x OR pilne)": true,
		"has:attachment larger:1M":  false,
	} {
		if got := NeedsBody(search.MustParse(q)); got != want {
			t.Errorf("NeedsBody(%q) = %v", q, got)
		}
	}
}

func TestActionsMergeAndLabels(t *testing.T) {
	a := Actions{Archive: true, AddLabelIDs: []int64{3}}.merge(Actions{MarkRead: true, AddLabelIDs: []int64{3, 5}, ForwardTo: "x@y.pl"})
	if !a.Archive || !a.MarkRead || !slices.Equal(a.AddLabelIDs, []int64{3, 5}) {
		t.Fatalf("merge = %+v", a)
	}
	add, remove := a.labelChange()
	if !slices.Equal(remove, []ops.LabelRef{{System: "INBOX"}, {System: ops.LabelUnread}}) ||
		!slices.Equal(add, []ops.LabelRef{{ID: 3}, {ID: 5}}) {
		t.Errorf("labelChange = %v / %v", add, remove)
	}
	// Trash from another filter wins over archive.
	b := a.merge(Actions{Trash: true, Star: true, Important: true})
	add, remove = b.labelChange()
	if b.Archive || slices.Contains(remove, ops.LabelRef{System: "INBOX"}) ||
		!slices.Contains(add, ops.LabelRef{System: "TRASH"}) || !slices.Contains(add, ops.LabelRef{System: "STARRED"}) ||
		!slices.Contains(add, ops.LabelRef{System: "IMPORTANT"}) {
		t.Errorf("trash merge: %+v -> %v / %v", b, add, remove)
	}
	if !(Actions{}).Empty() || (Actions{ForwardTo: "a@b.c"}).Empty() {
		t.Error("Empty")
	}
}

func TestMatchCombinesFilters(t *testing.T) {
	c := func(id int64, q string, a Actions) compiled {
		return compiled{f: Filter{ID: id, Criteria: q, Actions: a}, ast: search.MustParse(q)}
	}
	fs := []compiled{
		c(1, "from:newsletter", Actions{Archive: true, ForwardTo: "a@x.pl"}),
		c(2, "subject:faktura", Actions{AddLabelIDs: []int64{7}, ForwardTo: "a@x.pl"}),
		c(3, "umowa", Actions{Star: true}),
	}
	v := search.MessageView{Subject: "Faktura 12", FromText: "News <newsletter@shop.pl>", Date: time.Now(),
		SystemLabels: []string{"INBOX"}, Body: "w załączniku"}
	acts, fwd, ok := match(search.Matcher{}, fs, v)
	if !ok || !acts.Archive || !slices.Equal(acts.AddLabelIDs, []int64{7}) || acts.Star || !slices.Equal(fwd, []string{"a@x.pl"}) {
		t.Errorf("match = %+v %v %v", acts, fwd, ok)
	}
	// A body word matches once the body is there.
	v.Body = "Umowa najmu"
	if acts, _, _ = match(search.Matcher{}, fs, v); !acts.Star {
		t.Error("body word not matched")
	}
	if _, _, ok := match(search.Matcher{}, fs, search.MessageView{Subject: "x"}); ok {
		t.Error("unrelated message matched")
	}
}
