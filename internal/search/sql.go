package search

import (
	"strconv"
	"strings"
	"time"
)

// Scope is what ToSQL needs besides the query.
type Scope struct {
	UserID, AccountID int64
	// SystemLabels maps system keys (INBOX, SPAM, ...) to label ids of the
	// account.
	SystemLabels map[string]int64
	// ResolveLabel returns the ids of the labels of the account (folder and
	// user labels included) whose NormalizeLabel(name) equals name; nil
	// resolves nothing (label:x is then always false).
	ResolveLabel func(name string) []int64
	// Location is the user's time zone for before:/after: (UTC when nil).
	Location *time.Location
	// Now is the reference time of older_than:/newer_than: (time.Now when
	// zero).
	Now time.Time
	// IncludeSpamTrash turns off the default exclusion of messages in Spam
	// and Trash (also off when the query has in:anywhere, in:spam or
	// in:trash).
	IncludeSpamTrash bool
	// ArgOffset is the number of parameters before the fragment: the first
	// one ToSQL adds is $ArgOffset+1.
	ArgOffset int
}

func (s Scope) loc() *time.Location {
	if s.Location == nil {
		return time.UTC
	}
	return s.Location
}

func now(t time.Time) time.Time {
	if t.IsZero() {
		t = time.Now()
	}
	// Postgres keeps microseconds; compare at that precision on both sides.
	return t.Truncate(time.Microsecond)
}

// ToSQL builds the WHERE condition of the query over messages aliased m
// (plan 3.12): values are always parameters, numbered from
// s.ArgOffset+1. The condition does not restrict the user, account or
// orphaned_at; the caller adds them. It includes the default exclusion of
// Spam and Trash. The error is reserved; the current builder does not fail.
func ToSQL(a AST, s Scope) (string, []any, error) {
	b := &sqlBuilder{s: s, now: now(s.Now)}
	var parts []string
	if a.Root != nil {
		parts = append(parts, b.node(a.Root))
	}
	if !s.IncludeSpamTrash && !MentionsSpamTrash(a) {
		var ids []int64
		for _, k := range []string{"SPAM", "TRASH"} {
			if id, ok := s.SystemLabels[k]; ok {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			parts = append(parts, "NOT EXISTS (SELECT 1 FROM message_labels ml WHERE ml.message_id = m.id AND ml.label_id = ANY("+b.arg(ids)+"::bigint[]))")
		}
	}
	if len(parts) == 0 {
		return "TRUE", nil, nil
	}
	return strings.Join(parts, " AND "), b.args, nil
}

// MentionsSpamTrash reports whether the query names Spam, Trash or
// in:anywhere anywhere (negated too), which turns off their default
// exclusion, as in Gmail.
func MentionsSpamTrash(a AST) bool {
	found := false
	Walk(a.Root, func(n Node) {
		t, ok := n.(Term)
		if !ok {
			return
		}
		t = t.normalized()
		if t.Key != "in" && t.Key != "label" {
			return
		}
		sys, _, anywhere, _ := labelTarget(t.Key, t.Value)
		if anywhere || sys == "SPAM" || sys == "TRASH" {
			found = true
		}
	})
	return found
}

type sqlBuilder struct {
	s    Scope
	now  time.Time
	args []any
}

func (b *sqlBuilder) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(b.s.ArgOffset+len(b.args))
}

func (b *sqlBuilder) node(n Node) string {
	switch x := n.(type) {
	case And:
		parts := make([]string, len(x.Nodes))
		for i, c := range x.Nodes {
			parts[i] = b.node(c)
		}
		return "(" + strings.Join(parts, " AND ") + ")"
	case Or:
		parts := make([]string, len(x.Nodes))
		for i, c := range x.Nodes {
			parts[i] = b.node(c)
		}
		return "(" + strings.Join(parts, " OR ") + ")"
	case Not:
		return "NOT " + b.node(x.Node)
	case Term:
		return b.term(x.normalized())
	}
	return "TRUE"
}

// likeArg is the ILIKE pattern of a substring match.
func likeArg(v string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(v) + "%"
}

func (b *sqlBuilder) fts(value string, phrase, subjectOnly bool) string {
	toks := Tokens(value)
	if len(toks) == 0 {
		return "TRUE"
	}
	return "m.search_tsv @@ " + b.arg(tsQuery(toks, !phrase, subjectOnly)) + "::text::tsquery"
}

func (b *sqlBuilder) labelIDs(ids []int64) string {
	if len(ids) == 0 {
		return "FALSE"
	}
	return "EXISTS (SELECT 1 FROM message_labels ml WHERE ml.message_id = m.id AND ml.label_id = ANY(" + b.arg(ids) + "::bigint[]))"
}

func (b *sqlBuilder) system(key string) string {
	id, ok := b.s.SystemLabels[key]
	if !ok {
		return "FALSE"
	}
	return b.labelIDs([]int64{id})
}

func (b *sqlBuilder) addrs(col, v string) string {
	return "EXISTS (SELECT 1 FROM jsonb_array_elements(m." + col + ") e WHERE concat_ws(' ', e->>'name', e->>'email') ILIKE " + b.arg(likeArg(v)) + ")"
}

const userLabelExists = "EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE ml.message_id = m.id AND l.type = 'user')"

func (b *sqlBuilder) term(t Term) string {
	v := t.Value
	switch t.Key {
	case "":
		return b.fts(v, t.Phrase, false)
	case "subject":
		return b.fts(v, t.Phrase, true)
	case "from":
		return "m.from_text ILIKE " + b.arg(likeArg(v))
	case "to", "deliveredto":
		return "m.recipients_text ILIKE " + b.arg(likeArg(v))
	case "cc":
		return b.addrs("cc_addrs", v)
	case "bcc":
		return b.addrs("bcc_addrs", v)
	case "list":
		return "COALESCE(m.list_id, '') ILIKE " + b.arg(likeArg(v))
	case "filename":
		return "EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = m.id AND a.filename ILIKE " + b.arg(likeArg(v)) + ")"
	case "rfc822msgid":
		return "m.message_id_hdr = " + b.arg(v)
	case "has":
		switch v {
		case "attachment":
			return "m.has_attachments"
		case "userlabels":
			return userLabelExists
		case "nouserlabels":
			return "NOT " + userLabelExists
		}
	case "is":
		switch v {
		case "unread":
			return "m.is_unread"
		case "read":
			return "NOT m.is_unread"
		default:
			return b.system(strings.ToUpper(v))
		}
	case "in", "label":
		sys, unread, anywhere, name := labelTarget(t.Key, v)
		switch {
		case anywhere:
			return "TRUE"
		case unread:
			return "m.is_unread"
		case sys != "":
			return b.system(sys)
		}
		var ids []int64
		if b.s.ResolveLabel != nil {
			ids = b.s.ResolveLabel(name)
		}
		return b.labelIDs(ids)
	case "before", "after":
		d, _ := parseDate(v, b.s.loc())
		if t.Key == "before" {
			return "m.internal_date < " + b.arg(d)
		}
		return "m.internal_date >= " + b.arg(d)
	case "older_than", "newer_than":
		c, _ := parseAge(v, b.now)
		if t.Key == "older_than" {
			return "m.internal_date < " + b.arg(c)
		}
		return "m.internal_date > " + b.arg(c)
	case "larger", "smaller":
		n, _ := parseSize(v)
		if t.Key == "larger" {
			return "m.size_bytes > " + b.arg(n)
		}
		return "m.size_bytes < " + b.arg(n)
	}
	return "FALSE"
}
