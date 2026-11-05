// Package search is the query language of the mailbox (plan 3.12): the
// parser of Gmail-style queries, the SQL builder over messages, the
// in-memory matcher used by filters (plan 3.11) and the tokenizer that
// builds messages.search_tsv. The SQL builder and the matcher evaluate the
// same AST with the same semantics; a property test keeps them in step.
package search

import "strings"

// Node is a node of the query AST: And, Or, Not or Term.
type Node interface {
	// String is the normalized query text of the node; Parse(String())
	// yields an equal AST.
	String() string
	isNode()
}

// And matches when every node matches (juxtaposition or AND).
type And struct{ Nodes []Node }

// Or matches when any node matches (OR, | or {a b}).
type Or struct{ Nodes []Node }

// Not negates a node (-x).
type Not struct{ Node Node }

// Term is a word, a phrase or a key:value operator. Key is "" for words
// and phrases, else one of Keys (lower case; older/newer are stored as
// before/after). Phrase is set for a quoted value. A Term whose value is not
// valid for its key (before:2024/13/40) is never produced by Parse: such
// input becomes a plain word, as in Gmail.
type Term struct {
	Key    string
	Value  string
	Phrase bool
}

func (And) isNode()  {}
func (Or) isNode()   {}
func (Not) isNode()  {}
func (Term) isNode() {}

// AST is a parsed query. A nil Root is the empty query (no filter).
type AST struct{ Root Node }

// Empty reports whether the query has no condition.
func (a AST) Empty() bool { return a.Root == nil }

// String is the normalized query text ("" for the empty query).
func (a AST) String() string {
	if a.Root == nil {
		return ""
	}
	return a.Root.String()
}

func (n And) String() string {
	parts := make([]string, len(n.Nodes))
	for i, c := range n.Nodes {
		if _, ok := c.(Or); ok {
			parts[i] = "(" + c.String() + ")"
		} else {
			parts[i] = c.String()
		}
	}
	return strings.Join(parts, " ")
}

func (n Or) String() string {
	parts := make([]string, len(n.Nodes))
	for i, c := range n.Nodes {
		if _, ok := c.(And); ok {
			parts[i] = "(" + c.String() + ")"
		} else {
			parts[i] = c.String()
		}
	}
	return strings.Join(parts, " OR ")
}

func (n Not) String() string {
	switch n.Node.(type) {
	case And, Or:
		return "-(" + n.Node.String() + ")"
	}
	return "-" + n.Node.String()
}

func (t Term) String() string {
	v := t.Value
	if t.Phrase {
		v = `"` + v + `"`
	}
	if t.Key != "" {
		return t.Key + ":" + v
	}
	return v
}

// newAnd flattens nested Ands and drops nil nodes; one node is returned as
// is, none as nil.
func newAnd(nodes []Node) Node {
	var out []Node
	for _, n := range nodes {
		switch x := n.(type) {
		case nil:
		case And:
			out = append(out, x.Nodes...)
		default:
			out = append(out, n)
		}
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out[0]
	}
	return And{Nodes: out}
}

// newOr is newAnd for Or.
func newOr(nodes []Node) Node {
	var out []Node
	for _, n := range nodes {
		switch x := n.(type) {
		case nil:
		case Or:
			out = append(out, x.Nodes...)
		default:
			out = append(out, n)
		}
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out[0]
	}
	return Or{Nodes: out}
}

// Walk calls fn for every node of the tree, parents first.
func Walk(n Node, fn func(Node)) {
	if n == nil {
		return
	}
	fn(n)
	switch x := n.(type) {
	case And:
		for _, c := range x.Nodes {
			Walk(c, fn)
		}
	case Or:
		for _, c := range x.Nodes {
			Walk(c, fn)
		}
	case Not:
		Walk(x.Node, fn)
	}
}
