package mail

import (
	"bytes"
	"encoding/base64"
	"html"
	"io"
	"mime/quotedprintable"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-message/charset"
)

var (
	reScriptStyle = regexp.MustCompile(`(?is)<(script|style|head)[^>]*>.*?</(script|style|head)\s*>`)
	reBlockTag    = regexp.MustCompile(`(?i)</?(br|p|div|li|tr|h[1-6]|blockquote|table|ul|ol)(\s[^>]*)?/?>`)
	reTag         = regexp.MustCompile(`(?s)<[^>]*>`)
	reHSpace      = regexp.MustCompile(`[ \t\f\v\x{00a0}]+`)
	reBlankLines  = regexp.MustCompile(`\n[ \t]*(\n[ \t]*)+`)
	reWrote       = regexp.MustCompile(`(?i)^(on .{5,120} wrote:|w dniu .{5,120} (napisał|napisała|napisał\(a\)):?|-----original message-----|-----wiadomość oryginalna-----)$`)
)

// HTMLToText is a deliberately simple HTML-to-text conversion (no new
// dependency): scripts and styles are dropped, block tags become line breaks,
// remaining tags are stripped and entities decoded.
func HTMLToText(s string) string {
	s = reScriptStyle.ReplaceAllString(s, " ")
	s = reBlockTag.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return tidyText(s)
}

func tidyText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = reHSpace.ReplaceAllString(s, " ")
	s = reBlankLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// MakeSnippet builds a snippet of about 200 characters from a plain text body,
// skipping quoted lines (">"), "On ... wrote:" headers and everything after a
// "-- " signature separator (best effort).
func MakeSnippet(body string) string {
	var kept []string
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if line == "-- " || t == "--" {
			break
		}
		if strings.HasPrefix(t, ">") || t == "" {
			continue
		}
		if reWrote.MatchString(t) {
			continue
		}
		kept = append(kept, t)
		if len(kept) > 12 {
			break
		}
	}
	s := strings.Join(strings.Fields(strings.Join(kept, " ")), " ")
	if utf8.RuneCountInString(s) > snippetRunes {
		r := []rune(s)
		s = strings.TrimSpace(string(r[:snippetRunes]))
	}
	return strings.ToValidUTF8(s, "")
}

// decodeFragment decodes a partial body fragment (BODY[TEXT]<0.n>) using the
// transfer encoding and charset of the part it belongs to.
func decodeFragment(b []byte, encoding, cs string, isHTML bool) string {
	var r io.Reader = bytes.NewReader(b)
	switch encoding {
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	case "base64":
		// A cut-off fragment ends mid-quantum: keep a multiple of 4 characters.
		clean := bytes.Map(func(c rune) rune {
			if c == '\r' || c == '\n' || c == ' ' {
				return -1
			}
			return c
		}, b)
		clean = clean[:len(clean)/4*4]
		r = base64.NewDecoder(base64.StdEncoding, bytes.NewReader(clean))
	}
	if cs != "" && !strings.EqualFold(cs, "utf-8") && !strings.EqualFold(cs, "us-ascii") {
		if cr, err := charset.Reader(cs, r); err == nil {
			r = cr
		}
	}
	out, _ := io.ReadAll(r) // a truncated stream is expected; keep what decoded
	s := strings.ToValidUTF8(string(out), "")
	if isHTML {
		s = HTMLToText(s)
	}
	return s
}
