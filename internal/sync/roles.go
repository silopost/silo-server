package sync

import (
	"strings"

	"github.com/emersion/go-imap/v2"
)

// Folder roles (mailbox_folders.role). "user" folders get a folder label.
const (
	RoleInbox   = "inbox"
	RoleSent    = "sent"
	RoleDrafts  = "drafts"
	RoleTrash   = "trash"
	RoleJunk    = "junk"
	RoleArchive = "archive"
	RoleAll     = "all"
	RoleUser    = "user"
)

// MappableRoles are the roles a user may assign manually (folder_mapping keys).
var MappableRoles = []string{RoleSent, RoleDrafts, RoleTrash, RoleJunk, RoleArchive, RoleAll}

// specialUseRole maps RFC 6154 attributes to roles. \Flagged and \Important
// are virtual views of flags and are skipped by discovery.
var specialUseRole = map[imap.MailboxAttr]string{
	imap.MailboxAttrSent:    RoleSent,
	imap.MailboxAttrDrafts:  RoleDrafts,
	imap.MailboxAttrTrash:   RoleTrash,
	imap.MailboxAttrJunk:    RoleJunk,
	imap.MailboxAttrArchive: RoleArchive,
	imap.MailboxAttrAll:     RoleAll,
}

// roleNames is the name fallback of plan 3.4 (compared case-insensitively,
// also below an "INBOX." / "INBOX/" parent), extended with the names used by
// common servers and clients in English, Polish, German, French, Spanish,
// Italian, Dutch and Russian, and Gmail's [Gmail]/ and [Google Mail]/ folders.
var roleNames = []struct {
	role  string
	names []string
}{
	{RoleSent, []string{"Sent", "Sent Items", "Sent Messages", "Sent Mail", "Wysłane", "Elementy wysłane",
		"Gesendet", "Gesendete Elemente", "Gesendete Objekte", "Envoyés", "Éléments envoyés", "Messages envoyés",
		"Enviados", "Elementos enviados", "Inviati", "Posta inviata", "Verzonden", "Verzonden items",
		"Отправленные", "[Gmail]/Sent Mail", "[Google Mail]/Sent Mail"}},
	{RoleDrafts, []string{"Drafts", "Draft", "Wersje robocze", "Kopie robocze", "Entwürfe", "Brouillons",
		"Borradores", "Bozze", "Concepten", "Черновики", "[Gmail]/Drafts", "[Google Mail]/Drafts"}},
	{RoleTrash, []string{"Trash", "Deleted Items", "Deleted Messages", "Deleted", "Bin", "Kosz", "Elementy usunięte",
		"Papierkorb", "Gelöschte Elemente", "Gelöschte Objekte", "Corbeille", "Éléments supprimés",
		"Papelera", "Elementos eliminados", "Cestino", "Posta eliminata", "Prullenbak", "Verwijderde items",
		"Корзина", "Удаленные", "[Gmail]/Trash", "[Gmail]/Bin", "[Google Mail]/Trash", "[Google Mail]/Bin"}},
	{RoleJunk, []string{"Junk", "Spam", "Junk E-mail", "Junk Email", "Junk Mail", "Bulk Mail", "Wiadomości-śmieci",
		"Junk-E-Mail", "Courrier indésirable", "Indésirables", "Correo no deseado", "Posta indesiderata",
		"Ongewenste e-mail", "Спам", "[Gmail]/Spam", "[Google Mail]/Spam"}},
	{RoleArchive, []string{"Archive", "Archives", "Archiwum", "Archiv", "Archivio", "Archivo", "Archief", "Архив"}},
	{RoleAll, []string{"[Gmail]/All Mail", "[Gmail]/Wszystkie", "[Google Mail]/All Mail", "All Mail"}},
}

// listEntry is one selectable folder from LIST.
type listEntry struct {
	name       string // decoded (go-imap decodes modified UTF-7)
	delim      string
	specialUse string // the RFC 6154 attribute, e.g. \Sent; "" if none
	status     *imap.StatusData
}

// isVirtual reports a folder discovery skips: not selectable, nonexistent, or
// a virtual flag view.
func isVirtual(attrs []imap.MailboxAttr) bool {
	for _, a := range attrs {
		for _, skip := range []imap.MailboxAttr{imap.MailboxAttrNoSelect, imap.MailboxAttrNonExistent, imap.MailboxAttrFlagged, imap.MailboxAttrImportant} {
			if strings.EqualFold(string(a), string(skip)) {
				return true
			}
		}
	}
	return false
}

func specialUseOf(attrs []imap.MailboxAttr) string {
	for _, a := range attrs {
		for known := range specialUseRole {
			if strings.EqualFold(string(a), string(known)) {
				return string(known)
			}
		}
	}
	return ""
}

func isInbox(name string) bool { return strings.EqualFold(name, "INBOX") }

// baseName strips an INBOX parent ("INBOX.Sent", "INBOX/Sent" -> "Sent").
func baseName(name, delim string) string {
	if delim != "" && len(name) > 6 && strings.EqualFold(name[:5], "INBOX") && name[5:6] == delim {
		return name[6:]
	}
	return name
}

// assignRoles picks the role of every folder (plan 3.4): INBOX, then the
// manual mapping (a key with an empty value means "no folder for this role"),
// then SPECIAL-USE, then the name fallback; every other folder is "user". Each
// role except user is given to at most one folder.
func assignRoles(entries []listEntry, mapping map[string]string) map[string]string {
	roles := make(map[string]string, len(entries)) // folder name -> role
	taken := map[string]bool{}
	byName := map[string]string{} // lower name -> name
	for _, e := range entries {
		byName[strings.ToLower(e.name)] = e.name
		if isInbox(e.name) {
			roles[e.name] = RoleInbox
		}
	}
	taken[RoleInbox] = true
	for _, role := range MappableRoles {
		target, ok := mapping[role]
		if !ok {
			continue
		}
		taken[role] = true // manual: auto-detection never assigns this role
		if name, ok := byName[strings.ToLower(target)]; ok && target != "" && roles[name] == "" {
			roles[name] = role
		}
	}
	for _, e := range entries {
		role, ok := specialUseRole[imap.MailboxAttr(e.specialUse)]
		if ok && !taken[role] && roles[e.name] == "" {
			roles[e.name], taken[role] = role, true
		}
	}
	for _, rn := range roleNames {
		if taken[rn.role] {
			continue
		}
	names:
		for _, want := range rn.names {
			for _, e := range entries {
				if roles[e.name] == "" && strings.EqualFold(baseName(e.name, e.delim), want) {
					roles[e.name], taken[rn.role] = rn.role, true
					break names
				}
			}
		}
	}
	for _, e := range entries {
		if roles[e.name] == "" {
			roles[e.name] = RoleUser
		}
	}
	return roles
}

// backfillOrder is the folder order of phase B (plan 3.5.3).
func backfillOrder(role string) int {
	switch role {
	case RoleInbox:
		return 0
	case RoleSent:
		return 1
	case RoleArchive:
		return 2
	case RoleUser, RoleAll:
		return 3
	case RoleDrafts:
		return 4
	case RoleJunk:
		return 5
	case RoleTrash:
		return 6
	}
	return 3
}
