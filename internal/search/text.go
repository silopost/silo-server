package search

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Index limits. The tsvector is built here, not by Postgres' text parser,
// so that the matcher sees exactly the lexemes and positions the index has.
const (
	// MaxBodyBytes is the part of the body text that is indexed (plan 3.12).
	MaxBodyBytes = 512 << 10
	// MaxTokenBytes: longer tokens (base64 runs, hashes) are not indexed
	// and not searchable.
	MaxTokenBytes = 255
	// Postgres limits: positions 1..16383 (larger ones clamp), at most 256
	// positions per lexeme (later ones are dropped).
	maxPos        = 16383
	maxLexemePos  = 256
	weightSubject = 'A'
	weightPeople  = 'B'
	weightBody    = 'C'
)

// fold is the "unaccent" of the tokenizer: letters lose diacritics (NFD
// without nonspacing marks, plus the letters that do not decompose: ł, đ,
// ø, ß, æ, œ, þ, ð, ı, ħ, ŧ, ŀ) and are lower case.
var foldSpecial = map[rune]string{
	'ł': "l", 'đ': "d", 'ø': "o", 'ß': "ss", 'æ': "ae", 'œ': "oe", 'þ': "th", 'ð': "d", 'ı': "i",
	'ħ': "h", 'ŧ': "t", 'ŀ': "l",
}

// Tokens splits text into the lexemes of the index: maximal runs of letters
// and digits after folding (lower case, no diacritics). Everything else
// separates tokens, so "jan.kowalski@firma.pl" is jan, kowalski, firma, pl.
// This is a deliberate departure from Postgres' default parser (which keeps
// e-mail addresses, hosts and hyphenated words as single tokens): the index
// and the matcher share this one definition.
func Tokens(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 && cur.Len() <= MaxTokenBytes {
			out = append(out, cur.String())
		}
		cur.Reset()
	}
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if !ascii {
		s = norm.NFD.String(s)
	}
	for _, r := range s {
		if r < utf8.RuneSelf {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				cur.WriteRune(r)
			case r >= 'A' && r <= 'Z':
				cur.WriteRune(r + 'a' - 'A')
			default:
				flush()
			}
			continue
		}
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		r = unicode.ToLower(r)
		if f, ok := foldSpecial[r]; ok {
			cur.WriteString(f)
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mc, r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// Fields are the indexed parts of a message.
type Fields struct {
	Subject string
	// People: the From text, the To/Cc text and attachment file names.
	People []string
	Body   string
}

type posting struct {
	pos    int
	weight byte
}

// Doc is the indexed form of a message: lexeme -> positions with weights,
// as stored in messages.search_tsv.
type Doc struct {
	lex map[string][]posting
}

// truncateUTF8 cuts s to at most n bytes at a rune boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// BuildDoc indexes a message: subject tokens (weight A), then people and
// attachment names (B), then the first MaxBodyBytes of the body (C), at
// consecutive positions.
func BuildDoc(f Fields) *Doc {
	d := &Doc{lex: map[string][]posting{}}
	pos := 0
	add := func(text string, w byte) {
		for _, t := range Tokens(text) {
			if pos < maxPos {
				pos++
			}
			ps := d.lex[t]
			if len(ps) >= maxLexemePos || (len(ps) > 0 && ps[len(ps)-1].pos == pos) {
				continue
			}
			d.lex[t] = append(ps, posting{pos: pos, weight: w})
		}
	}
	add(f.Subject, weightSubject)
	for _, p := range f.People {
		add(p, weightPeople)
	}
	add(truncateUTF8(f.Body, MaxBodyBytes), weightBody)
	return d
}

// TSVector is the tsvector literal of the document ('lexeme':1A,2B ...),
// stored with a ::tsvector cast.
func (d *Doc) TSVector() string {
	keys := make([]string, 0, len(d.lex))
	for k := range d.lex {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(quoteLexeme(k))
		b.WriteByte(':')
		for j, p := range d.lex[k] {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Itoa(p.pos))
			b.WriteByte(p.weight)
		}
	}
	return b.String()
}

// quoteLexeme quotes a lexeme for a tsvector/tsquery literal. Tokens are
// letters and digits only, but quoting keeps the literal safe regardless.
func quoteLexeme(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// positions of the lexeme (or of every lexeme with the prefix), restricted
// to subject positions when subjectOnly, sorted.
func (d *Doc) positions(tok string, prefix, subjectOnly bool) []int {
	var out []int
	collect := func(ps []posting) {
		for _, p := range ps {
			if !subjectOnly || p.weight == weightSubject {
				out = append(out, p.pos)
			}
		}
	}
	if !prefix {
		collect(d.lex[tok])
		return out
	}
	for k, ps := range d.lex {
		if strings.HasPrefix(k, tok) {
			collect(ps)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// match evaluates the tsquery of tsQuery(tokens, prefixLast, subjectOnly)
// on the document: one token is a lexeme (prefix) match, more are a phrase
// of consecutive positions.
func (d *Doc) match(tokens []string, prefixLast, subjectOnly bool) bool {
	if len(tokens) == 0 {
		return true
	}
	sets := make([][]int, len(tokens))
	for i, t := range tokens {
		sets[i] = d.positions(t, prefixLast && i == len(tokens)-1, subjectOnly)
		if len(sets[i]) == 0 {
			return false
		}
	}
	for _, p := range sets[0] {
		ok := true
		for k := 1; k < len(sets) && ok; k++ {
			_, ok = slices.BinarySearch(sets[k], p+k)
		}
		if ok {
			return true
		}
	}
	return false
}

// tsQuery is the tsquery literal of a word or phrase: 'a':* for one token
// with prefix, 'a' <-> 'b' <-> 'c':* for several; subjectOnly restricts
// every lexeme to weight A (the subject).
func tsQuery(tokens []string, prefixLast, subjectOnly bool) string {
	parts := make([]string, len(tokens))
	for i, t := range tokens {
		s := quoteLexeme(t)
		suffix := ""
		if prefixLast && i == len(tokens)-1 {
			suffix = "*"
		}
		if subjectOnly {
			suffix += "A"
		}
		if suffix != "" {
			s += ":" + suffix
		}
		parts[i] = s
	}
	return strings.Join(parts, " <-> ")
}
