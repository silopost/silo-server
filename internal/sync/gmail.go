package sync

import "strings"

// archiveIsUnInbox reports whether archiving on this account removes the
// INBOX copy instead of moving the message to an Archive folder: Gmail in
// generic mode (plan 3.4), recognised by its All Mail (\All under "[Gmail]/"
// or "[Google Mail]/") and no Archive folder. There every message also stays
// in All Mail, so STORE \Deleted + UID EXPUNGE in INBOX is Gmail's "remove
// the Inbox label"; a MOVE to a new Archive folder would instead add a
// Gmail label "Archive". Other servers with \All (a virtual folder, as with
// Dovecot's virtual plugin) keep the MOVE: expunging from INBOX would delete
// the message there.
func archiveIsUnInbox(folders map[int64]*opFolder) bool {
	all := false
	for _, f := range folders {
		switch {
		case f.role == RoleArchive:
			return false
		case f.role == RoleAll && (strings.HasPrefix(f.name, "[Gmail]/") || strings.HasPrefix(f.name, "[Google Mail]/")):
			all = true
		}
	}
	return all
}
