package mail

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/textproto"
)

// Address is a parsed mailbox. Email is lower-cased.
type Address struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// ParsedHeader is the result of ParseHeader.
type ParsedHeader struct {
	MessageID       string // without <>; synthetic ("...@silo.invalid") when absent
	SyntheticID     bool
	InReplyTo       string   // first id of In-Reply-To, if any
	Refs            []string // threading parents: References then In-Reply-To, filtered, last 50
	From            Address
	To, Cc, ReplyTo []Address
	Date            *time.Time
	Subject         string
	ListID          string
	ListUnsubscribe string
	ContentType     string
	SiloDraftID     string // X-Silo-Draft-Id: a draft saved by Silo (plan 3.8)
	ParseError      string // non-empty when something was unreadable; the rest is best effort
}

var angleID = regexp.MustCompile(`<([^<>\s]+)>`)

var wordDecoder = &mime.WordDecoder{CharsetReader: charset.Reader}

// ParseHeader parses a raw RFC 5322 header block (RFC822.HEADER). Problems do
// not fail the call: they are collected in ParseError and the remaining fields
// are filled as far as possible. size is RFC822.SIZE, used for the synthetic
// Message-ID of messages that have none.
func ParseHeader(raw []byte, size int64) *ParsedHeader {
	p := &ParsedHeader{}
	var errs []string
	if !bytes.HasSuffix(raw, []byte("\r\n\r\n")) && !bytes.HasSuffix(raw, []byte("\n\n")) {
		raw = append(append([]byte{}, raw...), "\r\n\r\n"...)
	}
	h, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		errs = append(errs, "header: "+err.Error())
	}
	get := func(k string) string {
		v := h.Get(k)
		v = strings.NewReplacer("\r", "", "\n", "").Replace(v)
		return strings.TrimSpace(v)
	}
	addrs := func(k string) []Address {
		v := get(k)
		if v == "" {
			return nil
		}
		l, e := parseAddressList(v)
		if e != nil {
			errs = append(errs, k+": "+e.Error())
		}
		return l
	}

	if l := addrs("From"); len(l) > 0 {
		p.From = l[0]
	}
	p.To, p.Cc, p.ReplyTo = addrs("To"), addrs("Cc"), addrs("Reply-To")
	if v := get("Date"); v != "" {
		if t, e := mail.ParseDate(v); e == nil {
			p.Date = &t
		} else {
			errs = append(errs, "Date: "+e.Error())
		}
	}
	p.Subject = decodeWords(get("Subject"))
	p.ListID = get("List-Id")
	p.ListUnsubscribe = get("List-Unsubscribe")
	p.ContentType = strings.ToLower(get("Content-Type"))
	p.SiloDraftID = get(HeaderSiloDraftID)

	if ids := msgIDs(get("Message-ID")); len(ids) > 0 {
		p.MessageID = ids[0]
	} else {
		p.SyntheticID = true
		date := get("Date")
		sum := sha256.Sum256([]byte(get("From") + "\x00" + date + "\x00" + get("Subject") + "\x00" + fmt.Sprint(size)))
		p.MessageID = hex.EncodeToString(sum[:]) + "@silo.invalid"
	}
	irt := msgIDs(get("In-Reply-To"))
	if len(irt) > 0 {
		p.InReplyTo = irt[0]
	}
	// Parents: References in order, then In-Reply-To; duplicates and the
	// message's own id dropped; the last maxReferences kept.
	seen := map[string]bool{p.MessageID: true}
	var refs []string
	for _, id := range append(msgIDs(get("References")), irt...) {
		if !seen[id] {
			seen[id] = true
			refs = append(refs, id)
		}
	}
	if len(refs) > maxReferences {
		refs = refs[len(refs)-maxReferences:]
	}
	p.Refs = refs
	p.ParseError = strings.Join(errs, "; ")
	return p
}

// msgIDs extracts message ids from a header value, dropping garbage (no "@",
// longer than 250 characters, whitespace). Ids are returned without <>.
func msgIDs(v string) []string {
	var out []string
	add := func(id string) {
		if id != "" && len(id) <= maxRefLen && strings.Contains(id, "@") && !strings.ContainsAny(id, " \t<>") {
			out = append(out, id)
		}
	}
	if m := angleID.FindAllStringSubmatch(v, -1); len(m) > 0 {
		for _, g := range m {
			add(g[1])
		}
		return out
	}
	for _, f := range strings.Fields(v) {
		add(strings.Trim(f, "<>,"))
	}
	return out
}

func decodeWords(s string) string {
	if !strings.Contains(s, "=?") {
		return s
	}
	d, err := wordDecoder.DecodeHeader(s)
	if err != nil {
		return s
	}
	return d
}

// parseAddressList is lenient: when the whole list does not parse, it falls
// back to the comma-separated pieces and keeps those that do.
func parseAddressList(v string) ([]Address, error) {
	parser := mail.AddressParser{WordDecoder: wordDecoder}
	conv := func(l []*mail.Address) []Address {
		out := make([]Address, 0, len(l))
		for _, a := range l {
			if a.Address != "" {
				out = append(out, Address{Name: strings.TrimSpace(a.Name), Email: strings.ToLower(a.Address)})
			}
		}
		return out
	}
	if l, err := parser.ParseList(v); err == nil {
		return conv(l), nil
	}
	var out []Address
	var firstErr error
	for _, piece := range splitAddrList(v) {
		l, err := parser.ParseList(piece)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, conv(l)...)
	}
	return out, firstErr
}

// splitAddrList splits an address list at commas outside quotes and angle
// brackets.
func splitAddrList(v string) []string {
	var out []string
	var cur strings.Builder
	inQuote, inAngle, escaped := false, false, false
	for _, r := range v {
		switch {
		case escaped:
			escaped = false
		case r == 0x5c && inQuote:
			escaped = true
		case r == '"':
			inQuote = !inQuote
		case r == '<' && !inQuote:
			inAngle = true
		case r == '>' && !inQuote:
			inAngle = false
		case r == ',' && !inQuote && !inAngle:
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	return append(out, cur.String())
}
