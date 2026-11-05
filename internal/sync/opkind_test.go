package sync

import "testing"

func TestOpKind(t *testing.T) {
	for payload, want := range map[string]string{
		`{"op":"move","toRole":"archive"}`:                   "move",
		`{"op":"move","toRole":"junk","add":["$Junk"]}`:      "move",
		`{"op":"flags","add":["\\Seen"]}`:                    "flags",
		`{"op":"flags","add":["$Forwarded"]}`:                "flags",
		`{"op":"flags","add":["Praca"]}`:                     "labels",
		`{"op":"flags","add":["Praca"],"remove":["Dom"]}`:    "labels",
		`{"op":"flags","add":["Praca"],"remove":["\\Seen"]}`: "flags",
		`{"op":"expunge"}`:                                   "expunge",
		`{"op":"append","toRole":"drafts"}`:                  "append",
		`{"op":"delete_draft","draftId":"d_1"}`:              "delete_draft",
		`{"op":"create_folder","name":"X"}`:                  "create_folder",
		`not json`:                                           "unknown",
	} {
		if got := OpKind([]byte(payload)); got != want {
			t.Errorf("OpKind(%s) = %q, want %q", payload, got, want)
		}
	}
}
