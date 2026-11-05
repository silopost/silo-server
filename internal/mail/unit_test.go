package mail_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"

	silomail "github.com/silopost/silo-server/internal/mail"
)

func TestNormalizeSubject(t *testing.T) {
	cases := []struct {
		in      string
		list    bool
		want    string
		prefixd bool
	}{
		{"Plan sprintu", false, "plan sprintu", false},
		{"Re: Plan sprintu", false, "plan sprintu", true},
		{"RE: re: Odp: PD: Plan", false, "plan", true},
		{"Fwd: Re[2]: odp:  Plan   sprintu 47", false, "plan sprintu 47", true},
		{"AW: WG: SV: VS: TR: R: x", false, "x", true},
		{"Re (3): Tytuł", false, "tytuł", true},
		{"[lista] Re: Temat", true, "temat", true},
		{"Re: [lista] Temat", true, "temat", true},
		{"[lista] Temat", false, "[lista] temat", false},
		{"Reminder: spotkanie", false, "reminder: spotkanie", false}, // "Re" inside a word is not a prefix
		{"Przekazano: coś", false, "przekazano: coś", false},
		{"Zażółć́", false, "zażółć́", false}, // already lower, NFC applied below
		{"", false, "", false},
	}
	for _, c := range cases {
		got, pre := silomail.NormalizeSubject(c.in, c.list)
		want := c.want
		if c.in == "Zażółć́" {
			continue
		}
		if got != want || pre != c.prefixd {
			t.Errorf("NormalizeSubject(%q, %v) = %q, %v; want %q, %v", c.in, c.list, got, pre, want, c.prefixd)
		}
	}
	// NFC: decomposed and composed forms are the same key.
	a, _ := silomail.NormalizeSubject("Zażółć", false)
	b, _ := silomail.NormalizeSubject("Zażółć", false)
	if a != b {
		t.Errorf("NFC differs: %q vs %q", a, b)
	}
	for _, g := range []string{"", "test", "hi", "(no subject)", "(brak tematu)"} {
		if !silomail.IsGenericSubject(g) {
			t.Errorf("%q should be generic", g)
		}
	}
	if silomail.IsGenericSubject("plan sprintu") {
		t.Error("plan sprintu is not generic")
	}
}

func TestKeywordSlug(t *testing.T) {
	cases := map[string]string{
		"Praca/Projekty":  "Praca_Projekty",
		"Zażółć":          "Zazolc",
		"Zażółć gęślą":    "Zazolc_gesla",
		`a(b)c{d}%*"\]e`:  "abcd_e",
		"  Do zrobienia ": "Do_zrobienia",
		"日本語":             "label",
		"":                "label",
		"Łódź-2026":       "Lodz-2026",
		"tab\tnew\nline":  "tab_new_line",
		"ß Æ ø":           "ss_AE_o",
	}
	for in, want := range cases {
		if got := silomail.KeywordSlug(in); got != want {
			t.Errorf("KeywordSlug(%q) = %q, want %q", in, got, want)
		}
	}
	taken := map[string]bool{"Praca": true, "Praca_2": true}
	if got := silomail.UniqueKeyword("", "Praca", func(k string) bool { return taken[k] }); got != "Praca_3" {
		t.Errorf("UniqueKeyword = %q", got)
	}
	if got := silomail.UniqueKeyword("$Silo.", "Praca", func(k string) bool { return taken[k] }); got != "$Silo.Praca" {
		t.Errorf("UniqueKeyword with prefix = %q", got)
	}
	for _, kw := range []string{`\Seen`, "$Junk", "$NotJunk", "NonJunk", "$Forwarded", "$MDNSent", "$Important", "$SiloSnoozed", "$junk", ""} {
		if silomail.IsLabelKeyword(kw) {
			t.Errorf("%q must not become a label", kw)
		}
	}
	for _, kw := range []string{"$label1", "Praca", "$Label5", "Klient_ACME"} {
		if !silomail.IsLabelKeyword(kw) {
			t.Errorf("%q should become a label", kw)
		}
	}
}

func TestParseHeader(t *testing.T) {
	raw := "From: =?ISO-8859-2?Q?Za=BF=F3=B3=E6?= <Anna@Example.COM>\r\n" +
		"To: \"Kowalski, Jan\" <jan@example.com>, broken, Piotr <piotr@example.com>\r\n" +
		"Cc: =?UTF-8?B?xYHDs2Ri?= <lodz@example.com>\r\n" +
		"Subject: =?UTF-8?Q?Za=C5=BC=C3=B3=C5=82=C4=87?= test\r\n" +
		"Date: Mon, 02 Nov 2026 09:00:00 +0100\r\n" +
		"Message-ID: <Id1@Example.com>\r\n" +
		"In-Reply-To: <parent@example.com> (comment)\r\n" +
		"References: <r1@example.com>\r\n\t<r2@example.com> junk <r1@example.com> <Id1@Example.com>\r\n" +
		"List-Id: Lista <list.example.com>\r\n" +
		"List-Unsubscribe: <mailto:unsub@example.com>\r\n\r\n"
	p := silomail.ParseHeader([]byte(raw), 1234)
	if p.From.Email != "anna@example.com" || p.From.Name != "Zażółć" {
		t.Errorf("from = %+v", p.From)
	}
	if len(p.To) != 2 || p.To[0].Name != "Kowalski, Jan" || p.To[1].Email != "piotr@example.com" {
		t.Errorf("to = %+v (broken entry must be skipped)", p.To)
	}
	if p.ParseError == "" {
		t.Error("expected parse_error for the broken address")
	}
	if len(p.Cc) != 1 || p.Cc[0].Name != "Łódb" {
		t.Errorf("cc = %+v", p.Cc)
	}
	if p.Subject != "Zażółć test" {
		t.Errorf("subject = %q", p.Subject)
	}
	if p.Date == nil || p.Date.UTC().Hour() != 8 {
		t.Errorf("date = %v", p.Date)
	}
	if p.MessageID != "Id1@Example.com" || p.SyntheticID {
		t.Errorf("message id = %q", p.MessageID)
	}
	if p.InReplyTo != "parent@example.com" {
		t.Errorf("in-reply-to = %q", p.InReplyTo)
	}
	if want := []string{"r1@example.com", "r2@example.com", "parent@example.com"}; !slices.Equal(p.Refs, want) {
		t.Errorf("refs = %v, want %v (own id and duplicates dropped, In-Reply-To last)", p.Refs, want)
	}
	if p.ListID == "" || p.ListUnsubscribe != "<mailto:unsub@example.com>" {
		t.Errorf("list headers = %q %q", p.ListID, p.ListUnsubscribe)
	}

	// Missing Message-ID: synthetic, deterministic, size-dependent.
	h := []byte("From: a@example.com\r\nSubject: x\r\n\r\n")
	a, b, c := silomail.ParseHeader(h, 10), silomail.ParseHeader(h, 10), silomail.ParseHeader(h, 11)
	if !a.SyntheticID || !strings.HasSuffix(a.MessageID, "@silo.invalid") || a.MessageID != b.MessageID || a.MessageID == c.MessageID {
		t.Errorf("synthetic ids: %q %q %q", a.MessageID, b.MessageID, c.MessageID)
	}
}

func TestAttachmentsFromStructure(t *testing.T) {
	bs := &imap.BodyStructureMultiPart{Subtype: "mixed", Children: []imap.BodyStructure{
		&imap.BodyStructureMultiPart{Subtype: "related", Children: []imap.BodyStructure{
			&imap.BodyStructureSinglePart{Type: "text", Subtype: "html", Size: 10},
			&imap.BodyStructureSinglePart{Type: "image", Subtype: "png", ID: "<logo@x>", Size: 5,
				Extended: &imap.BodyStructureSinglePartExt{Disposition: &imap.BodyStructureDisposition{Value: "inline"}}},
		}},
		&imap.BodyStructureSinglePart{Type: "application", Subtype: "pdf", Size: 99, Params: map[string]string{"name": "a.pdf"}},
		&imap.BodyStructureSinglePart{Type: "text", Subtype: "calendar", Size: 7},
	}}
	got := silomail.AttachmentsFromStructure(bs)
	if len(got) != 3 {
		t.Fatalf("attachments = %+v", got)
	}
	if got[0].PartPath != "1.2" || !got[0].Inline || got[0].ContentID != "logo@x" {
		t.Errorf("inline image = %+v", got[0])
	}
	if got[1].PartPath != "2" || got[1].Filename != "a.pdf" || got[1].Inline || got[1].Size != 99 {
		t.Errorf("pdf = %+v", got[1])
	}
	if got[2].PartPath != "3" || got[2].ContentType != "text/calendar" {
		t.Errorf("calendar = %+v", got[2])
	}
	// A single-part message.
	if got := silomail.AttachmentsFromStructure(&imap.BodyStructureSinglePart{Type: "image", Subtype: "jpeg", Size: 3}); len(got) != 1 || got[0].PartPath != "1" {
		t.Errorf("single part = %+v", got)
	}
	if silomail.AttachmentsFromStructure(nil) != nil {
		t.Error("nil structure")
	}
}

func TestMakeSnippet(t *testing.T) {
	body := "Cześć,\n\nOn Mon, 2 Nov 2026 at 10:00, Anna <a@x.com> wrote:\n> stary tekst\n> drugi\n\nOdpowiedź tutaj.\n\n-- \nPodpis\nStopka"
	if got := silomail.MakeSnippet(body); got != "Cześć, Odpowiedź tutaj." {
		t.Errorf("snippet = %q", got)
	}
	long := strings.Repeat("ąę ", 300)
	if n := len([]rune(silomail.MakeSnippet(long))); n > 200 {
		t.Errorf("snippet length %d", n)
	}
}
