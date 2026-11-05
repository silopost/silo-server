package search

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ErrInvalidQuery is returned by Parse for a query it refuses (HTTP 422
// invalid_query): a closing ) or } without its opening bracket, a ) closing
// a { (or } closing a (), or a query over the complexity limits. Unclosed
// brackets and quotes are closed at the end of the query, as Gmail does
// while the user is still typing.
var ErrInvalidQuery = errors.New("search: invalid query")

// Limits of a query; beyond them Parse returns ErrInvalidQuery.
const (
	MaxTerms = 64
	MaxDepth = 16
)

// Parse parses a query of the grammar of plan 3.12. OR (upper case) and |
// separate alternatives, juxtaposition and AND join conditions, - negates,
// ( ) group, {a b} is a OR b, "..." is a phrase, key:value an operator whose
// value may be a word, a phrase or a ( ) / { } group (subject:(a b) =
// subject:a subject:b). An unknown key, or a value that is not valid for its
// key, makes a plain word. The empty query parses to an empty AST.
func Parse(q string) (AST, error) {
	p := &parser{s: []rune(q)}
	root := p.orExpr()
	if p.err == nil && p.i < len(p.s) {
		p.err = fmt.Errorf("%w: unexpected %q at position %d", ErrInvalidQuery, p.s[p.i], p.i+1)
	}
	if p.err == nil && p.terms > MaxTerms {
		p.err = fmt.Errorf("%w: more than %d terms", ErrInvalidQuery, MaxTerms)
	}
	if p.err != nil {
		return AST{}, p.err
	}
	return AST{Root: root}, nil
}

// MustParse is Parse for queries known to be valid (tests, constants).
func MustParse(q string) AST {
	a, err := Parse(q)
	if err != nil {
		panic(err)
	}
	return a
}

type parser struct {
	s     []rune
	i     int
	depth int
	terms int
	err   error
}

func isSpace(r rune) bool { return unicode.IsSpace(r) }

func isDelim(r rune) bool { return strings.ContainsRune("(){}|", r) }

func (p *parser) skip() {
	for p.i < len(p.s) && isSpace(p.s[p.i]) {
		p.i++
	}
}

func (p *parser) peek() rune {
	if p.i < len(p.s) {
		return p.s[p.i]
	}
	return 0
}

func (p *parser) atEnd() bool { return p.i >= len(p.s) }

// word scans a word at p.i without consuming it.
func (p *parser) word() string {
	j := p.i
	for j < len(p.s) {
		r := p.s[j]
		if isSpace(r) || isDelim(r) || (r == '"' && j > p.i && p.s[j-1] == ':') {
			break
		}
		j++
	}
	return string(p.s[p.i:j])
}

// isOr reports whether an OR separator (OR or |) is at p.i.
func (p *parser) isOr() bool {
	if p.peek() == '|' {
		return true
	}
	return p.word() == "OR"
}

func (p *parser) closer() bool {
	r := p.peek()
	return r == ')' || r == '}'
}

func (p *parser) fail(format string, args ...any) {
	if p.err == nil {
		p.err = fmt.Errorf("%w: "+format, append([]any{ErrInvalidQuery}, args...)...)
	}
}

func (p *parser) orExpr() Node {
	nodes := []Node{p.andExpr()}
	for p.err == nil {
		p.skip()
		if p.atEnd() || !p.isOr() {
			break
		}
		if p.peek() == '|' {
			p.i++
		} else {
			p.i += 2
		}
		p.skip()
		if p.atEnd() || p.closer() || p.isOr() {
			continue // dangling OR: ignored
		}
		nodes = append(nodes, p.andExpr())
	}
	return newOr(nodes)
}

func (p *parser) andExpr() Node {
	var nodes []Node
	for p.err == nil {
		p.skip()
		if p.atEnd() || p.closer() {
			break
		}
		if p.isOr() {
			if len(nodes) > 0 {
				break
			}
			// A leading OR has nothing to join: "OR" is a word, | is dropped.
			if p.peek() == '|' {
				p.i++
			} else {
				p.i += 2
				nodes = append(nodes, p.term(Term{Value: "OR"}))
			}
			continue
		}
		if p.word() == "AND" {
			p.i += 3
			continue
		}
		nodes = append(nodes, p.unary())
	}
	return newAnd(nodes)
}

func (p *parser) unary() Node {
	if p.peek() == '-' && p.i+1 < len(p.s) {
		if nx := p.s[p.i+1]; !isSpace(nx) && (!isDelim(nx) || nx == '(' || nx == '{') {
			p.i++
			n := p.unary()
			if n == nil {
				return nil
			}
			return Not{Node: n}
		}
	}
	return p.primary()
}

func (p *parser) term(t Term) Node {
	p.terms++
	return t
}

// group parses ( ... ) or { ... } at p.i (the opening bracket).
func (p *parser) group() Node {
	open := p.s[p.i]
	p.i++
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > MaxDepth {
		p.fail("brackets nested deeper than %d", MaxDepth)
		return nil
	}
	want, other := ')', '}'
	if open == '{' {
		want, other = '}', ')'
	}
	var n Node
	if open == '(' {
		n = p.orExpr()
	} else {
		var nodes []Node
		for p.err == nil {
			p.skip()
			if p.atEnd() || p.closer() {
				break
			}
			if p.isOr() {
				if p.peek() == '|' {
					p.i++
				} else {
					p.i += 2
				}
				continue
			}
			if p.word() == "AND" {
				p.i += 3
				continue
			}
			nodes = append(nodes, p.unary())
		}
		n = newOr(nodes)
	}
	p.skip()
	switch {
	case p.err != nil, p.atEnd(): // unclosed: closed at the end
	case p.peek() == want:
		p.i++
	case p.peek() == other:
		p.fail("%q closes %q at position %d", other, open, p.i+1)
	}
	return n
}

// phrase parses "..." at p.i; an unclosed phrase runs to the end.
func (p *parser) phrase() string {
	p.i++
	j := p.i
	for j < len(p.s) && p.s[j] != '"' {
		j++
	}
	v := string(p.s[p.i:j])
	p.i = j
	if j < len(p.s) {
		p.i++
	}
	return strings.TrimSpace(v)
}

func (p *parser) primary() Node {
	switch p.peek() {
	case '(', '{':
		return p.group()
	case '"':
		v := p.phrase()
		if v == "" {
			return nil
		}
		return p.term(Term{Value: v, Phrase: true})
	}
	w := p.word()
	p.i += len([]rune(w))
	k, v, ok := strings.Cut(w, ":")
	if !ok || !keySet[strings.ToLower(k)] {
		return p.term(Term{Value: w})
	}
	if v != "" {
		return p.term(checkTerm(k, v, false))
	}
	switch p.peek() {
	case '(', '{':
		return p.withKey(strings.ToLower(k), p.group())
	case '"':
		ph := p.phrase()
		if ph == "" {
			return nil
		}
		return p.term(checkTerm(k, ph, true))
	}
	return p.term(Term{Value: w}) // "from:" alone
}

// withKey applies key to the words and phrases of a group (subject:(a b)).
func (p *parser) withKey(key string, n Node) Node {
	switch x := n.(type) {
	case Term:
		if x.Key == "" {
			return checkTerm(key, x.Value, x.Phrase)
		}
		return x
	case Not:
		return Not{Node: p.withKey(key, x.Node)}
	case And:
		out := make([]Node, len(x.Nodes))
		for i, c := range x.Nodes {
			out[i] = p.withKey(key, c)
		}
		return newAnd(out)
	case Or:
		out := make([]Node, len(x.Nodes))
		for i, c := range x.Nodes {
			out[i] = p.withKey(key, c)
		}
		return newOr(out)
	}
	return n
}
