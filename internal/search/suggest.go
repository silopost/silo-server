package search

import (
	"strings"
	"unicode"
)

// Operator is a suggestion of an operator for the search box.
type Operator struct {
	// Text is what the token becomes ("has:attachment", or "from:" waiting
	// for a value).
	Text string
	// Label describes it.
	Label string
}

// Operators are the operator suggestions, in the order they are offered.
var Operators = []Operator{
	{"from:", "Sender"}, {"to:", "Recipient"}, {"subject:", "Words in the subject"},
	{"has:attachment", "Has an attachment"}, {"is:unread", "Unread"}, {"is:read", "Read"},
	{"is:starred", "Starred"}, {"is:important", "Important"}, {"is:snoozed", "Snoozed"},
	{"in:inbox", "In Inbox"}, {"in:sent", "In Sent"}, {"in:drafts", "In Drafts"}, {"in:archive", "In Archive"},
	{"in:spam", "In Spam"}, {"in:trash", "In Trash"}, {"in:anywhere", "Anywhere, Spam and Trash included"},
	{"label:", "With the label"}, {"cc:", "Cc recipient"}, {"bcc:", "Bcc recipient"},
	{"filename:", "Attachment name or type"}, {"before:", "Before a date (YYYY/MM/DD)"},
	{"after:", "After a date (YYYY/MM/DD)"}, {"older_than:", "Older than (1d, 2m, 1y)"},
	{"newer_than:", "Newer than (1d, 2m, 1y)"}, {"larger:", "Larger than (5M, 100K)"},
	{"smaller:", "Smaller than (5M, 100K)"}, {"has:userlabels", "With a user label"},
	{"has:nouserlabels", "Without a user label"}, {"list:", "Mailing list"},
	{"deliveredto:", "Delivered to"}, {"rfc822msgid:", "Message-ID"},
}

// SplitLast splits a query being typed into the text before its last token
// and the last token (empty when the query ends with a space). A quoted
// phrase still open is the last token.
func SplitLast(q string) (head, last string) {
	inQuote := false
	start := 0
	for i, r := range q {
		switch {
		case r == '"':
			inQuote = !inQuote
		case !inQuote && (unicode.IsSpace(r) || r == '(' || r == ')' || r == '{' || r == '}'):
			start = i + len(string(r))
		}
	}
	return q[:start], q[start:]
}

// MatchOperators returns the operators the token is a prefix of
// (case-insensitive), at most limit; none for an empty token or a token
// that is already a complete operator.
func MatchOperators(token string, limit int) []Operator {
	t := strings.ToLower(strings.TrimPrefix(token, "-"))
	if t == "" {
		return nil
	}
	var out []Operator
	for _, o := range Operators {
		if strings.HasPrefix(o.Text, t) && o.Text != t && len(out) < limit {
			out = append(out, o)
		}
	}
	return out
}

// LabelToken is the label: value of a label name: spaces become "-", which
// ResolveLabel treats as a space, so the token needs no quotes; names with
// brackets or quotes are quoted instead.
func LabelToken(name string) string {
	if strings.ContainsAny(name, "(){}|\"") {
		return `"` + strings.ReplaceAll(name, `"`, "") + `"`
	}
	return strings.Join(strings.Fields(name), "-")
}
