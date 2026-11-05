package mail

import (
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// replyPrefix matches one reply/forward prefix: "Re:", "Odp:", "PD:", "Fwd:",
// "RE[2]:", "Re (3) :" ... The prefix list follows plan 4.6.
var replyPrefix = regexp.MustCompile(`(?i)^(?:re|fw|fwd|odp|pd|aw|wg|sv|vs|r|tr)(?:\s*[\[(]\d+[\])])?\s*:\s*`)

// leadingTag matches a mailing-list tag such as "[silo-dev]" at the start.
var leadingTag = regexp.MustCompile(`^\[[^\[\]]{1,100}\]\s*`)

var spaces = regexp.MustCompile(`\s+`)

// genericSubjects are too generic to be used for the subject fallback of the
// threading algorithm (plan 4.6, step 5).
var genericSubjects = map[string]bool{
	"": true, "hi": true, "hello": true, "hey": true, "test": true, "info": true,
	"help": true, "question": true, "pytanie": true, "cześć": true, "czesc": true,
	"hej": true, "witam": true, "(no subject)": true, "no subject": true,
	"(brak tematu)": true, "brak tematu": true, "(bez tematu)": true, "bez tematu": true,
}

// NormalizeSubject returns the threading key of a subject: repeated
// reply/forward prefixes (re, fw, fwd, odp, pd, aw, wg, sv, vs, r, tr, with an
// optional [n] or (n) counter) are removed, list tags like "[dev]" are removed
// when the message has a List-Id, whitespace is collapsed, the result is
// lower-cased and NFC-normalised. hadPrefix reports whether at least one
// reply/forward prefix was removed.
func NormalizeSubject(subject string, hasListID bool) (norm_ string, hadPrefix bool) {
	s := strings.TrimSpace(norm.NFC.String(subject))
	for {
		before := s
		if loc := replyPrefix.FindStringIndex(s); loc != nil {
			s = s[loc[1]:]
			hadPrefix = true
		}
		if hasListID {
			if loc := leadingTag.FindStringIndex(s); loc != nil {
				s = s[loc[1]:]
			}
		}
		s = strings.TrimSpace(s)
		if s == before {
			break
		}
	}
	s = spaces.ReplaceAllString(s, " ")
	return strings.ToLower(s), hadPrefix
}

// IsGenericSubject reports whether a normalised subject is too general to
// join threads on its own ("test", "hi", empty, "(no subject)").
func IsGenericSubject(subjectNorm string) bool { return genericSubjects[subjectNorm] }
