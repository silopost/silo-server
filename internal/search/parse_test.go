package search

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type parserCase struct {
	q    string
	ast  string // AST.String()
	sql  string // ToSQL where
	args string // argString(args)
	excl bool   // default Spam/Trash exclusion on (IncludeSpamTrash false)
	err  bool   // ErrInvalidQuery
}

var testNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func testScope() Scope {
	warsaw, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		panic(err)
	}
	labels := map[string][]int64{"faktury": {20}, "work projects": {21}, "a b": {22, 23}, "moje faktury": {24}}
	return Scope{
		UserID: 1, AccountID: 2,
		SystemLabels: map[string]int64{"INBOX": 1, "SENT": 2, "DRAFT": 3, "TRASH": 4, "SPAM": 5, "ARCHIVE": 6,
			"SNOOZED": 7, "STARRED": 8, "IMPORTANT": 9, "SCHEDULED": 10},
		ResolveLabel: func(name string) []int64 { return labels[name] },
		Location:     warsaw, Now: testNow, ArgOffset: 2,
	}
}

func argString(args []any) string {
	parts := make([]string, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case time.Time:
			parts[i] = v.UTC().Format(time.RFC3339)
		default:
			parts[i] = fmt.Sprint(v)
		}
	}
	return strings.Join(parts, " | ")
}

const (
	notSpamTrash = "NOT EXISTS (SELECT 1 FROM message_labels ml WHERE ml.message_id = m.id AND ml.label_id = ANY($%d::bigint[]))"
	labelSQL     = "EXISTS (SELECT 1 FROM message_labels ml WHERE ml.message_id = m.id AND ml.label_id = ANY($%d::bigint[]))"
	tsv          = "m.search_tsv @@ $%d::text::tsquery"
)

func f(format string, n ...any) string { return fmt.Sprintf(format, n...) }

var parserCases = []parserCase{
	// Words, phrases, boolean structure.
	{q: "", ast: "", sql: "TRUE", args: ""},
	{q: "   ", ast: "", sql: "TRUE", args: ""},
	{q: "faktura", ast: "faktura", sql: f(tsv, 3), args: "'faktura':*"},
	{q: "Faktura Łódź", ast: "Faktura Łódź", sql: "(" + f(tsv, 3) + " AND " + f(tsv, 4) + ")", args: "'faktura':* | 'lodz':*"},
	{q: `"umowa najmu"`, ast: `"umowa najmu"`, sql: f(tsv, 3), args: "'umowa' <-> 'najmu'"},
	{q: "a OR b", ast: "a OR b", sql: "(" + f(tsv, 3) + " OR " + f(tsv, 4) + ")", args: "'a':* | 'b':*"},
	{q: "a | b", ast: "a OR b", sql: "(" + f(tsv, 3) + " OR " + f(tsv, 4) + ")", args: "'a':* | 'b':*"},
	{q: "a|b", ast: "a OR b", sql: "(" + f(tsv, 3) + " OR " + f(tsv, 4) + ")", args: "'a':* | 'b':*"},
	{q: "a AND b", ast: "a b", sql: "(" + f(tsv, 3) + " AND " + f(tsv, 4) + ")", args: "'a':* | 'b':*"},
	{q: "a or b", ast: "a or b", sql: "(" + f(tsv, 3) + " AND " + f(tsv, 4) + " AND " + f(tsv, 5) + ")", args: "'a':* | 'or':* | 'b':*"},
	{q: "-spam", ast: "-spam", sql: "NOT " + f(tsv, 3), args: "'spam':*"},
	{q: "-(a b)", ast: "-(a b)", sql: "NOT (" + f(tsv, 3) + " AND " + f(tsv, 4) + ")", args: "'a':* | 'b':*"},
	{q: "(a OR b) c", ast: "(a OR b) c", sql: "((" + f(tsv, 3) + " OR " + f(tsv, 4) + ") AND " + f(tsv, 5) + ")", args: "'a':* | 'b':* | 'c':*"},
	{q: "{a b c}", ast: "a OR b OR c", sql: "(" + f(tsv, 3) + " OR " + f(tsv, 4) + " OR " + f(tsv, 5) + ")", args: "'a':* | 'b':* | 'c':*"},
	{q: "-{a b}", ast: "-(a OR b)", sql: "NOT (" + f(tsv, 3) + " OR " + f(tsv, 4) + ")", args: "'a':* | 'b':*"},
	{q: "a AND (b OR c) -d", ast: "a (b OR c) -d", sql: "(" + f(tsv, 3) + " AND (" + f(tsv, 4) + " OR " + f(tsv, 5) + ") AND NOT " + f(tsv, 6) + ")", args: "'a':* | 'b':* | 'c':* | 'd':*"},
	{q: "(a b) OR (c d)", ast: "(a b) OR (c d)", sql: "((" + f(tsv, 3) + " AND " + f(tsv, 4) + ") OR (" + f(tsv, 5) + " AND " + f(tsv, 6) + "))", args: "'a':* | 'b':* | 'c':* | 'd':*"},
	{q: "--a", ast: "--a", sql: "NOT NOT " + f(tsv, 3), args: "'a':*"},
	{q: "- a", ast: "- a", sql: "(TRUE AND " + f(tsv, 3) + ")", args: "'a':*"},
	{q: "OR a", ast: "OR a", sql: "(" + f(tsv, 3) + " AND " + f(tsv, 4) + ")", args: "'or':* | 'a':*"},
	{q: "a OR", ast: "a", sql: f(tsv, 3), args: "'a':*"},
	{q: "a OR OR b", ast: "a OR b", sql: "(" + f(tsv, 3) + " OR " + f(tsv, 4) + ")", args: "'a':* | 'b':*"},
	{q: "jan.kowalski@firma.pl", ast: "jan.kowalski@firma.pl", sql: f(tsv, 3), args: "'jan' <-> 'kowalski' <-> 'firma' <-> 'pl':*"},
	{q: "x-y", ast: "x-y", sql: f(tsv, 3), args: "'x' <-> 'y':*"},
	{q: "@@@", ast: "@@@", sql: "TRUE", args: ""},
	{q: "żółć", ast: "żółć", sql: f(tsv, 3), args: "'zolc':*"},
	{q: "a (b", ast: "a b", sql: "(" + f(tsv, 3) + " AND " + f(tsv, 4) + ")", args: "'a':* | 'b':*"},
	{q: `"open phrase`, ast: `"open phrase"`, sql: f(tsv, 3), args: "'open' <-> 'phrase'"},
	{q: "foo:bar", ast: "foo:bar", sql: f(tsv, 3), args: "'foo' <-> 'bar':*"},
	{q: "from:", ast: "from:", sql: f(tsv, 3), args: "'from':*"},
	// Addresses.
	{q: "from:jan", ast: "from:jan", sql: "m.from_text ILIKE $3", args: "%jan%"},
	{q: "FROM:Jan", ast: "from:Jan", sql: "m.from_text ILIKE $3", args: "%Jan%"},
	{q: "from:jan@firma.pl", ast: "from:jan@firma.pl", sql: "m.from_text ILIKE $3", args: "%jan@firma.pl%"},
	{q: `from:"Jan Kowalski"`, ast: `from:"Jan Kowalski"`, sql: "m.from_text ILIKE $3", args: "%Jan Kowalski%"},
	{q: "from:(jan OR anna)", ast: "from:jan OR from:anna", sql: "(m.from_text ILIKE $3 OR m.from_text ILIKE $4)", args: "%jan% | %anna%"},
	{q: "from:{jan anna}", ast: "from:jan OR from:anna", sql: "(m.from_text ILIKE $3 OR m.from_text ILIKE $4)", args: "%jan% | %anna%"},
	{q: "{from:a to:b}", ast: "from:a OR to:b", sql: "(m.from_text ILIKE $3 OR m.recipients_text ILIKE $4)", args: "%a% | %b%"},
	{q: "from:100%_x", ast: "from:100%_x", sql: "m.from_text ILIKE $3", args: `%100\%\_x%`},
	{q: "to:anna", ast: "to:anna", sql: "m.recipients_text ILIKE $3", args: "%anna%"},
	{q: "cc:piotr", ast: "cc:piotr", sql: "EXISTS (SELECT 1 FROM jsonb_array_elements(m.cc_addrs) e WHERE concat_ws(' ', e->>'name', e->>'email') ILIKE $3)", args: "%piotr%"},
	{q: "bcc:ukryty", ast: "bcc:ukryty", sql: "EXISTS (SELECT 1 FROM jsonb_array_elements(m.bcc_addrs) e WHERE concat_ws(' ', e->>'name', e->>'email') ILIKE $3)", args: "%ukryty%"},
	{q: "deliveredto:me@silo.test", ast: "deliveredto:me@silo.test", sql: "m.recipients_text ILIKE $3", args: "%me@silo.test%"},
	{q: "list:newsletter.example.com", ast: "list:newsletter.example.com", sql: "COALESCE(m.list_id, '') ILIKE $3", args: "%newsletter.example.com%"},
	{q: "rfc822msgid:<abc@x>", ast: "rfc822msgid:abc@x", sql: "m.message_id_hdr = $3", args: "abc@x"},
	// Subject.
	{q: "subject:oferta", ast: "subject:oferta", sql: f(tsv, 3), args: "'oferta':*A"},
	{q: "subject:(oferta q4)", ast: "subject:oferta subject:q4", sql: "(" + f(tsv, 3) + " AND " + f(tsv, 4) + ")", args: "'oferta':*A | 'q4':*A"},
	{q: `subject:"oferta na"`, ast: `subject:"oferta na"`, sql: f(tsv, 3), args: "'oferta':A <-> 'na':A"},
	{q: "subject:(-spam)", ast: "-subject:spam", sql: "NOT " + f(tsv, 3), args: "'spam':*A"},
	{q: `subject:""`, ast: "", sql: "TRUE", args: ""},
	// Labels.
	{q: "label:Faktury", ast: "label:Faktury", sql: f(labelSQL, 3), args: "[20]"},
	{q: "label:work-projects", ast: "label:work-projects", sql: f(labelSQL, 3), args: "[21]"},
	{q: "label:work/projects", ast: "label:work/projects", sql: f(labelSQL, 3), args: "[21]"},
	{q: `label:"Moje faktury"`, ast: `label:"Moje faktury"`, sql: f(labelSQL, 3), args: "[24]"},
	{q: "label:a-b", ast: "label:a-b", sql: f(labelSQL, 3), args: "[22 23]"},
	{q: "label:nieznana", ast: "label:nieznana", sql: "FALSE", args: ""},
	{q: "label:unread", ast: "label:unread", sql: "m.is_unread", args: ""},
	{q: "label:starred", ast: "label:starred", sql: f(labelSQL, 3), args: "[8]"},
	{q: "in:inbox", ast: "in:inbox", sql: f(labelSQL, 3), args: "[1]"},
	{q: "in:sent", ast: "in:sent", sql: f(labelSQL, 3), args: "[2]"},
	{q: "in:drafts", ast: "in:drafts", sql: f(labelSQL, 3), args: "[3]"},
	{q: "in:archive", ast: "in:archive", sql: f(labelSQL, 3), args: "[6]"},
	{q: "in:Faktury", ast: "in:Faktury", sql: f(labelSQL, 3), args: "[20]"},
	{q: "is:unread", ast: "is:unread", sql: "m.is_unread", args: ""},
	{q: "is:READ", ast: "is:read", sql: "NOT m.is_unread", args: ""},
	{q: "is:starred", ast: "is:starred", sql: f(labelSQL, 3), args: "[8]"},
	{q: "is:important", ast: "is:important", sql: f(labelSQL, 3), args: "[9]"},
	{q: "is:snoozed", ast: "is:snoozed", sql: f(labelSQL, 3), args: "[7]"},
	{q: "is:muted", ast: "is:muted", sql: f(tsv, 3), args: "'is' <-> 'muted':*"},
	{q: "has:attachment", ast: "has:attachment", sql: "m.has_attachments", args: ""},
	{q: "has:userlabels", ast: "has:userlabels", sql: userLabelExists, args: ""},
	{q: "has:nouserlabels", ast: "has:nouserlabels", sql: "NOT " + userLabelExists, args: ""},
	{q: "has:drive", ast: "has:drive", sql: f(tsv, 3), args: "'has' <-> 'drive':*"},
	// Spam and Trash.
	{q: "faktura", excl: true, ast: "faktura", sql: f(tsv, 3) + " AND " + f(notSpamTrash, 4), args: "'faktura':* | [5 4]"},
	{q: "", excl: true, ast: "", sql: f(notSpamTrash, 3), args: "[5 4]"},
	{q: "in:anywhere faktura", excl: true, ast: "in:anywhere faktura", sql: "(TRUE AND " + f(tsv, 3) + ")", args: "'faktura':*"},
	{q: "in:spam", excl: true, ast: "in:spam", sql: f(labelSQL, 3), args: "[5]"},
	{q: "in:trash", excl: true, ast: "in:trash", sql: f(labelSQL, 3), args: "[4]"},
	{q: "-in:trash", excl: true, ast: "-in:trash", sql: "NOT " + f(labelSQL, 3), args: "[4]"},
	{q: "from:jan has:attachment -label:spam is:unread", excl: true, ast: "from:jan has:attachment -label:spam is:unread",
		sql: "(m.from_text ILIKE $3 AND m.has_attachments AND NOT " + f(labelSQL, 4) + " AND m.is_unread)", args: "%jan% | [5]"},
	// Dates (Europe/Warsaw: CET in January, CEST in June).
	{q: "before:2024/01/31", ast: "before:2024/01/31", sql: "m.internal_date < $3", args: "2024-01-30T23:00:00Z"},
	{q: "after:2024-01-31", ast: "after:2024-01-31", sql: "m.internal_date >= $3", args: "2024-01-30T23:00:00Z"},
	{q: "older:2024/6/5", ast: "before:2024/6/5", sql: "m.internal_date < $3", args: "2024-06-04T22:00:00Z"},
	{q: "newer:2024/06/05", ast: "after:2024/06/05", sql: "m.internal_date >= $3", args: "2024-06-04T22:00:00Z"},
	{q: "before:2024/13/01", ast: "before:2024/13/01", sql: f(tsv, 3), args: "'before' <-> '2024' <-> '13' <-> '01':*"},
	{q: "before:2024/02/30", ast: "before:2024/02/30", sql: f(tsv, 3), args: "'before' <-> '2024' <-> '02' <-> '30':*"},
	{q: "older_than:2d", ast: "older_than:2d", sql: "m.internal_date < $3", args: "2026-11-03T12:00:00Z"},
	{q: "newer_than:3m", ast: "newer_than:3m", sql: "m.internal_date > $3", args: "2026-08-05T12:00:00Z"},
	{q: "older_than:1Y", ast: "older_than:1Y", sql: "m.internal_date < $3", args: "2025-11-05T12:00:00Z"},
	{q: "newer_than:5x", ast: "newer_than:5x", sql: f(tsv, 3), args: "'newer' <-> 'than' <-> '5x':*"},
	// Sizes and attachments.
	{q: "larger:5M", ast: "larger:5M", sql: "m.size_bytes > $3", args: "5242880"},
	{q: "smaller:100K", ast: "smaller:100K", sql: "m.size_bytes < $3", args: "102400"},
	{q: "larger:1GB", ast: "larger:1GB", sql: "m.size_bytes > $3", args: "1073741824"},
	{q: "larger:12345", ast: "larger:12345", sql: "m.size_bytes > $3", args: "12345"},
	{q: "larger:big", ast: "larger:big", sql: f(tsv, 3), args: "'larger' <-> 'big':*"},
	{q: "filename:pdf", ast: "filename:pdf", sql: "EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = m.id AND a.filename ILIKE $3)", args: "%pdf%"},
	{q: `filename:"raport 2024.pdf"`, ast: `filename:"raport 2024.pdf"`, sql: "EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = m.id AND a.filename ILIKE $3)", args: "%raport 2024.pdf%"},
	// Refused.
	{q: "a)", err: true},
	{q: "(a}", err: true},
	{q: "{a)", err: true},
	{q: "}", err: true},
	{q: strings.Repeat("w ", MaxTerms+1), err: true},
	{q: strings.Repeat("(", MaxDepth+1) + "a", err: true},
}

func TestParseToSQL(t *testing.T) {
	if len(parserCases) < 60 {
		t.Fatalf("only %d parser cases", len(parserCases))
	}
	for _, c := range parserCases {
		a, err := Parse(c.q)
		if c.err {
			if !errors.Is(err, ErrInvalidQuery) {
				t.Errorf("Parse(%q): err = %v, want ErrInvalidQuery", c.q, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q): %v", c.q, err)
			continue
		}
		if got := a.String(); got != c.ast {
			t.Errorf("Parse(%q).String() = %q, want %q", c.q, got, c.ast)
		}
		// The normalized text parses to the same AST.
		if b, err := Parse(a.String()); err != nil || b.String() != a.String() {
			t.Errorf("Parse(%q) does not round-trip: %q, %v", a.String(), b.String(), err)
		}
		s := testScope()
		s.IncludeSpamTrash = !c.excl
		where, args, err := ToSQL(a, s)
		if err != nil {
			t.Errorf("ToSQL(%q): %v", c.q, err)
			continue
		}
		if where != c.sql {
			t.Errorf("ToSQL(%q):\n got %s\nwant %s", c.q, where, c.sql)
		}
		if got := argString(args); got != c.args {
			t.Errorf("ToSQL(%q) args = %s, want %s", c.q, got, c.args)
		}
	}
}

func TestTokens(t *testing.T) {
	for in, want := range map[string]string{
		"Zażółć gęślą jaźń":             "zazolc gesla jazn",
		"ŁÓDŹ, Straße & Æsir":           "lodz strasse aesir",
		"jan.kowalski@firma.pl":         "jan kowalski firma pl",
		"Faktura_2024-11 (v2).PDF":      "faktura 2024 11 v2 pdf",
		"naïve café":                    "naive cafe",
		"":                              "",
		strings.Repeat("x", 300) + " y": "y",
	} {
		if got := strings.Join(Tokens(in), " "); got != want {
			t.Errorf("Tokens(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDocLimits(t *testing.T) {
	body := strings.Repeat("word ", 20000) + "tail"
	d := BuildDoc(Fields{Subject: "Hello world", People: []string{"Jan <jan@x.pl>"}, Body: body})
	if n := len(d.lex["word"]); n != maxLexemePos {
		t.Errorf("positions of a frequent lexeme = %d", n)
	}
	if ps := d.lex["tail"]; len(ps) != 1 || ps[0].pos != maxPos {
		t.Errorf("tail positions = %v", ps)
	}
	if !d.match([]string{"hello", "world"}, false, true) || d.match([]string{"jan"}, false, true) || !d.match([]string{"ja"}, true, false) {
		t.Error("subject/prefix matching")
	}
	if got := BuildDoc(Fields{Subject: "b a"}).TSVector(); got != "'a':2A 'b':1A" {
		t.Errorf("TSVector = %s", got)
	}
}

func TestMatcherBasics(t *testing.T) {
	v := MessageView{
		Subject: "Oferta na Q4", FromText: "Jan Kowalski <jan@firma.pl>", RecipientsText: "anna@silo.test",
		Cc: []Address{{Name: "Piotr", Email: "piotr@x.pl"}}, Date: time.Date(2024, 1, 30, 23, 30, 0, 0, time.UTC),
		Size: 6 << 20, HasAttachments: true, Unread: true, SystemLabels: []string{"INBOX", "STARRED"},
		FolderLabels: []string{"Faktury"}, UserLabels: []string{"Work/Projects"}, Attachments: []string{"oferta.pdf"},
		Body: "Cześć, w załączniku przesyłam ofertę.",
	}
	warsaw, _ := time.LoadLocation("Europe/Warsaw")
	m := Matcher{Location: warsaw, Now: testNow}
	for q, want := range map[string]bool{
		"oferta": true, "ofer": true, "subject:oferta": true, "subject:jan": false, "jan": true, "firma.pl": true,
		`"oferta na"`: true, `"na oferta"`: false, "zalaczniku": true, "from:KOWALSKI": true, "cc:piotr": true,
		"bcc:piotr": false, "label:work-projects": true, "label:faktury": true, "in:inbox": true, "is:starred": true,
		"is:read": false, "has:attachment": true, "has:nouserlabels": false, "filename:PDF": true, "larger:5M": true,
		"smaller:5M": false, "after:2024/01/31": true, "before:2024/01/31": false, "older_than:1y": true,
		"-oferta": false, "x OR oferta": true, "{x y}": false, "in:anywhere": true, "label:nieznana": false,
	} {
		if got := m.Match(MustParse(q), v); got != want {
			t.Errorf("Match(%q) = %v, want %v", q, got, want)
		}
	}
	v.SystemLabels = []string{"TRASH"}
	if m.Match(MustParse("oferta"), v) || !m.Match(MustParse("oferta in:trash"), v) || !(Matcher{IncludeSpamTrash: true}).Match(MustParse("oferta"), v) {
		t.Error("default Trash exclusion")
	}
}

func TestSuggestHelpers(t *testing.T) {
	for q, want := range map[string][2]string{
		"":                {"", ""},
		"from:jan has:at": {"from:jan ", "has:at"},
		"a ":              {"a ", ""},
		`x "open ph`:      {"x ", `"open ph`},
		"(a OR b":         {"(a OR ", "b"},
	} {
		h, l := SplitLast(q)
		if h != want[0] || l != want[1] {
			t.Errorf("SplitLast(%q) = %q, %q", q, h, l)
		}
	}
	if ops := MatchOperators("is:u", 5); len(ops) != 1 || ops[0].Text != "is:unread" {
		t.Errorf("MatchOperators(is:u) = %v", ops)
	}
	if ops := MatchOperators("in", 5); len(ops) != 5 {
		t.Errorf("MatchOperators(in) = %v", ops)
	}
	if LabelToken("Moje faktury") != "Moje-faktury" || LabelToken("Faktury (folder)") != `"Faktury (folder)"` {
		t.Error("LabelToken")
	}
}
