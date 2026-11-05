package search

import (
	"slices"
	"strings"
	"time"
)

// Address is a mailbox of MessageView.
type Address struct {
	Name, Email string
}

// MessageView is what the matcher sees of a message: the columns ToSQL
// reads, in their stored form.
type MessageView struct {
	Subject string
	// FromText and RecipientsText are messages.from_text and
	// recipients_text ("Name <email>", comma separated).
	FromText, RecipientsText string
	Cc, Bcc                  []Address
	ListID                   string
	MessageIDHdr             string
	// Date is the internal date.
	Date           time.Time
	Size           int64
	HasAttachments bool
	Unread         bool
	// SystemLabels are system keys (INBOX, STARRED, ...); FolderLabels and
	// UserLabels are label names.
	SystemLabels, FolderLabels, UserLabels []string
	// Attachments are the file names of the attachments (inline ones too).
	Attachments []string
	// Body is the downloaded body text ("" before the download).
	Body string
}

// Index is the document of the message, as stored in messages.search_tsv.
func (v MessageView) Index() *Doc {
	return BuildDoc(Fields{Subject: v.Subject, People: append([]string{v.FromText, v.RecipientsText}, v.Attachments...), Body: v.Body})
}

// Matcher evaluates queries on messages in memory with the semantics of
// ToSQL (filters, plan 3.11).
type Matcher struct {
	// Location, Now and IncludeSpamTrash as in Scope.
	Location         *time.Location
	Now              time.Time
	IncludeSpamTrash bool
}

// Match reports whether the message matches the query.
func (m Matcher) Match(a AST, v MessageView) bool {
	e := &evaluator{m: m, v: &v, now: now(m.Now)}
	if a.Root != nil && !e.node(a.Root) {
		return false
	}
	if !m.IncludeSpamTrash && !MentionsSpamTrash(a) {
		if slices.Contains(v.SystemLabels, "SPAM") || slices.Contains(v.SystemLabels, "TRASH") {
			return false
		}
	}
	return true
}

type evaluator struct {
	m   Matcher
	v   *MessageView
	doc *Doc
	now time.Time
}

func (e *evaluator) node(n Node) bool {
	switch x := n.(type) {
	case And:
		for _, c := range x.Nodes {
			if !e.node(c) {
				return false
			}
		}
		return true
	case Or:
		for _, c := range x.Nodes {
			if e.node(c) {
				return true
			}
		}
		return false
	case Not:
		return !e.node(x.Node)
	case Term:
		return e.term(x.normalized())
	}
	return true
}

// contains is ILIKE '%needle%'.
func contains(hay, needle string) bool {
	return strings.Contains(strings.ToLower(hay), strings.ToLower(needle))
}

func (e *evaluator) fts(value string, phrase, subjectOnly bool) bool {
	toks := Tokens(value)
	if len(toks) == 0 {
		return true
	}
	if e.doc == nil {
		e.doc = e.v.Index()
	}
	return e.doc.match(toks, !phrase, subjectOnly)
}

func addrsContain(l []Address, v string) bool {
	for _, a := range l {
		if contains(a.Name+" "+a.Email, v) {
			return true
		}
	}
	return false
}

func (e *evaluator) hasLabelName(name string) bool {
	for _, l := range [][]string{e.v.SystemLabels, e.v.FolderLabels, e.v.UserLabels} {
		for _, x := range l {
			if NormalizeLabel(x) == name {
				return true
			}
		}
	}
	return false
}

func (e *evaluator) term(t Term) bool {
	v := t.Value
	switch t.Key {
	case "":
		return e.fts(v, t.Phrase, false)
	case "subject":
		return e.fts(v, t.Phrase, true)
	case "from":
		return contains(e.v.FromText, v)
	case "to", "deliveredto":
		return contains(e.v.RecipientsText, v)
	case "cc":
		return addrsContain(e.v.Cc, v)
	case "bcc":
		return addrsContain(e.v.Bcc, v)
	case "list":
		return contains(e.v.ListID, v)
	case "filename":
		for _, f := range e.v.Attachments {
			if contains(f, v) {
				return true
			}
		}
		return false
	case "rfc822msgid":
		return e.v.MessageIDHdr == v
	case "has":
		switch v {
		case "attachment":
			return e.v.HasAttachments
		case "userlabels":
			return len(e.v.UserLabels) > 0
		case "nouserlabels":
			return len(e.v.UserLabels) == 0
		}
	case "is":
		switch v {
		case "unread":
			return e.v.Unread
		case "read":
			return !e.v.Unread
		default:
			return slices.Contains(e.v.SystemLabels, strings.ToUpper(v))
		}
	case "in", "label":
		sys, unread, anywhere, name := labelTarget(t.Key, v)
		switch {
		case anywhere:
			return true
		case unread:
			return e.v.Unread
		case sys != "":
			return slices.Contains(e.v.SystemLabels, sys)
		}
		return e.hasLabelName(name)
	case "before", "after":
		loc := e.m.Location
		if loc == nil {
			loc = time.UTC
		}
		d, _ := parseDate(v, loc)
		if t.Key == "before" {
			return e.v.Date.Before(d)
		}
		return !e.v.Date.Before(d)
	case "older_than", "newer_than":
		c, _ := parseAge(v, e.now)
		if t.Key == "older_than" {
			return e.v.Date.Before(c)
		}
		return e.v.Date.After(c)
	case "larger", "smaller":
		n, _ := parseSize(v)
		if t.Key == "larger" {
			return e.v.Size > n
		}
		return e.v.Size < n
	}
	return false
}
