package search

import (
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // users' time zones on hosts without a zoneinfo database
)

// Keys are the operators of the grammar (plan 3.12). older and newer are
// aliases of before and after.
var Keys = []string{
	"from", "to", "cc", "bcc", "subject", "label", "in", "is", "has", "before", "after", "older", "newer",
	"older_than", "newer_than", "larger", "smaller", "filename", "list", "deliveredto", "rfc822msgid",
}

var keySet = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range Keys {
		m[k] = true
	}
	return m
}()

var keyAlias = map[string]string{"older": "before", "newer": "after"}

// Values of is: and has:.
var (
	isValues  = map[string]bool{"unread": true, "read": true, "starred": true, "important": true, "snoozed": true}
	hasValues = map[string]bool{"attachment": true, "userlabels": true, "nouserlabels": true}
)

// systemAlias maps a normalized in:/label: value to a system label key.
var systemAlias = map[string]string{
	"inbox": "INBOX", "sent": "SENT", "draft": "DRAFT", "drafts": "DRAFT", "spam": "SPAM", "junk": "SPAM",
	"trash": "TRASH", "bin": "TRASH", "archive": "ARCHIVE", "snoozed": "SNOOZED", "starred": "STARRED",
	"important": "IMPORTANT", "scheduled": "SCHEDULED",
}

// labelUnread is the in:/label: value of the UNREAD pseudo label (a flag,
// not a label: ADR 0002).
const labelUnread = "unread"

// NormalizeLabel is the form label names are compared in: lower case, "-",
// "/" and runs of spaces as one space (label:my-label matches "My label"
// and "My/Label").
func NormalizeLabel(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer("-", " ", "/", " ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// labelTarget classifies an in:/label: value: a system key, the UNREAD
// pseudo label, "anywhere" (in: only), or a label name to resolve.
func labelTarget(key, value string) (system string, unread, anywhere bool, name string) {
	n := NormalizeLabel(value)
	if key == "in" && n == "anywhere" {
		return "", false, true, ""
	}
	if n == labelUnread {
		return "", true, false, ""
	}
	if k, ok := systemAlias[n]; ok {
		return k, false, false, ""
	}
	return "", false, false, n
}

// parseDate accepts YYYY/MM/DD and YYYY-MM-DD (one-digit month and day
// too) and returns midnight of that day in loc.
func parseDate(s string, loc *time.Location) (time.Time, bool) {
	sep := "/"
	if strings.Contains(s, "-") {
		sep = "-"
	}
	p := strings.Split(s, sep)
	if len(p) != 3 || len(p[0]) != 4 || len(p[1]) < 1 || len(p[1]) > 2 || len(p[2]) < 1 || len(p[2]) > 2 {
		return time.Time{}, false
	}
	y, e1 := strconv.Atoi(p[0])
	m, e2 := strconv.Atoi(p[1])
	d, e3 := strconv.Atoi(p[2])
	if e1 != nil || e2 != nil || e3 != nil || m < 1 || m > 12 || d < 1 || d > 31 {
		return time.Time{}, false
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, loc)
	if t.Day() != d || int(t.Month()) != m {
		return time.Time{}, false // 2024/02/31
	}
	return t, true
}

// parseAge accepts Nd, Nm and Ny (older_than:, newer_than:) and returns the
// cutoff now - N days/months/years.
func parseAge(s string, now time.Time) (time.Time, bool) {
	if len(s) < 2 {
		return time.Time{}, false
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n < 0 || n > 100_000 || strings.ContainsAny(s[:len(s)-1], "+-") {
		return time.Time{}, false
	}
	switch strings.ToLower(s[len(s)-1:]) {
	case "d":
		return now.AddDate(0, 0, -n), true
	case "m":
		return now.AddDate(0, -n, 0), true
	case "y":
		return now.AddDate(-n, 0, 0), true
	}
	return time.Time{}, false
}

// parseSize accepts bytes (12345) or a binary K/M/G multiple with an
// optional B (100K, 5M, 1GB).
func parseSize(s string) (int64, bool) {
	u := strings.ToUpper(s)
	u = strings.TrimSuffix(u, "B")
	mult := int64(1)
	if u != "" {
		switch u[len(u)-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		}
		if mult > 1 {
			u = u[:len(u)-1]
		}
	}
	if u == "" || strings.ContainsAny(u, "+-") {
		return 0, false
	}
	n, err := strconv.ParseInt(u, 10, 64)
	if err != nil || n < 0 || n > (1<<62)/mult {
		return 0, false
	}
	return n * mult, true
}

// checkTerm normalizes a key:value term, or turns it into a plain word when
// the value is not valid for the key (Gmail treats before:2024/13/40 as a
// word). Dates are checked against UTC; the day is the same in every zone.
func checkTerm(key, value string, phrase bool) Term {
	key = strings.ToLower(key)
	if a, ok := keyAlias[key]; ok {
		key = a
	}
	var ok bool
	switch key {
	case "is":
		value = strings.ToLower(value)
		ok = isValues[value]
	case "has":
		value = strings.ToLower(value)
		ok = hasValues[value]
	case "in", "label":
		ok = NormalizeLabel(value) != ""
	case "before", "after":
		_, ok = parseDate(value, time.UTC)
	case "older_than", "newer_than":
		_, ok = parseAge(value, time.Now())
	case "larger", "smaller":
		_, ok = parseSize(value)
	case "rfc822msgid":
		value = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(value), "<"), ">")
		ok = value != ""
	case "from", "to", "cc", "bcc", "subject", "filename", "list", "deliveredto":
		ok = strings.TrimSpace(value) != ""
	default:
		ok = false
	}
	if !ok {
		if phrase {
			// key:"..." with a bad value: the words of key and value.
			return Term{Value: key + " " + value, Phrase: true}
		}
		return Term{Value: key + ":" + value}
	}
	switch key {
	case "is", "has", "before", "after", "older_than", "newer_than", "larger", "smaller":
		phrase = false
	}
	return Term{Key: key, Value: value, Phrase: phrase}
}

// normalized is the term as Parse would build it: Terms built by hand with an
// invalid value are evaluated as words.
func (t Term) normalized() Term {
	if t.Key == "" {
		return t
	}
	return checkTerm(t.Key, t.Value, t.Phrase)
}
