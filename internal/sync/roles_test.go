package sync

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestAssignRoles(t *testing.T) {
	entries := func(names ...string) []listEntry {
		var out []listEntry
		for _, n := range names {
			out = append(out, listEntry{name: n, delim: "."})
		}
		return out
	}
	cases := []struct {
		name    string
		in      []listEntry
		mapping map[string]string
		want    map[string]string
	}{
		{"special-use wins over names", []listEntry{
			{name: "INBOX"}, {name: "Sent", delim: "/"}, {name: "Wysłane", delim: "/", specialUse: string(imap.MailboxAttrSent)},
			{name: "Kosz", delim: "/"}, {name: "Archive", delim: "/", specialUse: string(imap.MailboxAttrArchive)},
		}, nil, map[string]string{"INBOX": RoleInbox, "Sent": RoleUser, "Wysłane": RoleSent, "Kosz": RoleTrash, "Archive": RoleArchive}},
		{"polish names below INBOX.", entries("INBOX", "INBOX.Elementy wysłane", "INBOX.Wersje robocze", "INBOX.Elementy usunięte",
			"INBOX.Wiadomości-śmieci", "INBOX.Archiwum", "INBOX.Faktury"), nil,
			map[string]string{"INBOX": RoleInbox, "INBOX.Elementy wysłane": RoleSent, "INBOX.Wersje robocze": RoleDrafts,
				"INBOX.Elementy usunięte": RoleTrash, "INBOX.Wiadomości-śmieci": RoleJunk, "INBOX.Archiwum": RoleArchive, "INBOX.Faktury": RoleUser}},
		{"gmail", entries("INBOX", "[Gmail]/All Mail", "[Gmail]/Sent Mail", "[Gmail]/Spam", "[Gmail]/Trash", "[Gmail]/Drafts", "Praca"), nil,
			map[string]string{"[Gmail]/All Mail": RoleAll, "[Gmail]/Sent Mail": RoleSent, "[Gmail]/Spam": RoleJunk,
				"[Gmail]/Trash": RoleTrash, "[Gmail]/Drafts": RoleDrafts, "Praca": RoleUser}},
		{"one folder per role, first name in list order wins", entries("INBOX", "Sent Items", "Sent"), nil,
			map[string]string{"Sent": RoleSent, "Sent Items": RoleUser}},
		{"manual mapping wins; empty means none", []listEntry{
			{name: "INBOX"}, {name: "Sent", specialUse: string(imap.MailboxAttrSent)}, {name: "Wychodzące"}, {name: "Archive"},
		}, map[string]string{"sent": "Wychodzące", "archive": ""},
			map[string]string{"Sent": RoleUser, "Wychodzące": RoleSent, "Archive": RoleUser}},
		{"german and case", entries("inbox", "Gesendete Elemente", "papierkorb", "Entwürfe"), nil,
			map[string]string{"inbox": RoleInbox, "Gesendete Elemente": RoleSent, "papierkorb": RoleTrash, "Entwürfe": RoleDrafts}},
	}
	for _, c := range cases {
		got := assignRoles(c.in, c.mapping)
		for name, role := range c.want {
			if got[name] != role {
				t.Errorf("%s: %q got role %q, want %q (all: %v)", c.name, name, got[name], role, got)
			}
		}
	}
}

func TestIsVirtual(t *testing.T) {
	if !isVirtual([]imap.MailboxAttr{`\NoSelect`}) || !isVirtual([]imap.MailboxAttr{imap.MailboxAttrFlagged}) || isVirtual([]imap.MailboxAttr{imap.MailboxAttrSent}) {
		t.Error("isVirtual")
	}
	if specialUseOf([]imap.MailboxAttr{`\HasNoChildren`, `\sent`}) != `\Sent` {
		t.Error("specialUseOf")
	}
}
