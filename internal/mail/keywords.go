package mail

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// ignoredKeywords are conventional or system keywords that never become user
// labels (plan 3.7). Compared case-insensitively.
var ignoredKeywords = map[string]bool{
	"$forwarded": true, "$mdnsent": true, "$junk": true, "$notjunk": true,
	"nonjunk": true, "$important": true, "$silosnoozed": true, "$phishing": true,
}

// thunderbirdLabels maps Thunderbird's $label1..$label5 tags to label names.
var thunderbirdLabels = map[string]string{
	"$label1": "Ważne", "$label2": "Praca", "$label3": "Osobiste",
	"$label4": "Do zrobienia", "$label5": "Później",
}

// IsLabelKeyword reports whether an IMAP flag may be imported as a user label:
// it must be a keyword (no leading backslash) and not a system/convention one.
func IsLabelKeyword(flag string) bool {
	if flag == "" || strings.HasPrefix(flag, `\`) {
		return false
	}
	return !ignoredKeywords[strings.ToLower(flag)]
}

var slugReplacer = strings.NewReplacer(
	"ł", "l", "Ł", "L", "ø", "o", "Ø", "O", "đ", "d", "Đ", "D", "ß", "ss",
	"æ", "ae", "Æ", "AE", "œ", "oe", "Œ", "OE", "þ", "th", "Þ", "Th",
)

// KeywordSlug turns a label name into an ASCII IMAP keyword atom
// ("Praca/Projekty" -> "Praca_Projekty", "Zażółć" -> "Zazolc"). Characters
// forbidden in an atom ( ) { % * " \ ] and control characters are dropped,
// separators become "_". An empty result becomes "label".
func KeywordSlug(name string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(slugReplacer.Replace(name)) {
		switch {
		case unicode.Is(unicode.Mn, r):
			// combining accent from the NFD decomposition
		case r == '/' || r == 0x5c || unicode.IsSpace(r) || r == '|':
			b.WriteByte('_')
		case r > 0x20 && r < 0x7f && !strings.ContainsRune(`(){}%*"]`, r):
			b.WriteRune(r)
		}
	}
	s := strings.Trim(b.String(), "_")
	if s == "" {
		return "label"
	}
	return s
}

// UniqueKeyword builds prefix+KeywordSlug(name) and, when taken reports a
// collision (case-insensitive on the caller's side), appends _2, _3, ...
func UniqueKeyword(prefix, name string, taken func(keyword string) bool) string {
	base := prefix + KeywordSlug(name)
	kw := base
	for i := 2; taken(kw); i++ {
		kw = base + "_" + strconv.Itoa(i)
	}
	return kw
}

// AssignKeyword picks a free keyword for a new label name on the account,
// honouring accounts.keyword_prefix and existing labels.imap_keyword values.
func AssignKeyword(ctx context.Context, db DB, accountID int64, name string) (string, error) {
	var prefix string
	if err := db.QueryRow(ctx, `SELECT keyword_prefix FROM accounts WHERE id = $1`, accountID).Scan(&prefix); err != nil {
		return "", fmt.Errorf("mail: load keyword prefix: %w", err)
	}
	rows, err := db.Query(ctx, `SELECT lower(imap_keyword) FROM labels WHERE account_id = $1 AND imap_keyword IS NOT NULL`, accountID)
	if err != nil {
		return "", fmt.Errorf("mail: load keywords: %w", err)
	}
	defer rows.Close()
	used := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return "", err
		}
		used[k] = true
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return UniqueKeyword(prefix, name, func(k string) bool { return used[strings.ToLower(k)] }), nil
}
