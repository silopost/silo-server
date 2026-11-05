package sync

import (
	"context"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
)

// discover lists the folders of the account (plan 3.5.3 step 1): LIST with
// SPECIAL-USE (and STATUS when LIST-STATUS is available, STATUS per folder
// otherwise), assigns roles, upserts mailbox_folders, creates folder labels
// and handles folders that disappeared (rename = same UIDVALIDITY and message
// count under a new name; otherwise the disappearance procedure and the row is
// deleted). It returns the STATUS of every folder by id, for the STATUS sweep.
func (s *session) discover(ctx context.Context) (map[int64]*imap.StatusData, error) {
	caps := s.sc.caps
	opts := &imap.ListOptions{ReturnSpecialUse: caps.HasSpecialUse()}
	statusOpts := &imap.StatusOptions{NumMessages: true, UIDNext: true, UIDValidity: true, HighestModSeq: caps.HasCondstore()}
	if caps.HasListStatus() {
		opts.ReturnStatus = statusOpts
	}
	var list []*imap.ListData
	if err := s.sc.await(ctx, 0, func() error {
		var err error
		list, err = s.sc.imap().List("", "*", opts).Collect()
		return err
	}); err != nil {
		return nil, err
	}
	var entries []listEntry
	for _, l := range list {
		if isVirtual(l.Attrs) || l.Mailbox == "" {
			continue
		}
		e := listEntry{name: l.Mailbox, specialUse: specialUseOf(l.Attrs), status: l.Status}
		if l.Delim != 0 {
			e.delim = string(l.Delim)
		}
		if isInbox(e.name) {
			e.name = "INBOX"
		}
		entries = append(entries, e)
	}
	if !caps.HasListStatus() {
		for i := range entries {
			var st *imap.StatusData
			err := s.sc.await(ctx, 0, func() error {
				var err error
				st, err = s.sc.imap().Status(entries[i].name, statusOpts).Wait()
				return err
			})
			if err != nil && !isCommandError(err) {
				return nil, err
			}
			entries[i].status = st
		}
	}

	acc, err := loadAccount(ctx, s.d.Pool, s.acc.id) // the mapping may have changed (PUT /folders)
	if err != nil {
		return nil, err
	}
	s.acc.mapping = acc.mapping
	roles := assignRoles(entries, acc.mapping)

	err = inTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		existing, err := loadFolders(ctx, tx, s.acc.id)
		if err != nil {
			return err
		}
		byName := map[string]*folder{}
		for _, f := range existing {
			byName[f.name] = f
		}
		seen := map[string]bool{}
		var fresh []*listEntry
		for i := range entries {
			e := &entries[i]
			seen[e.name] = true
			if byName[e.name] == nil {
				fresh = append(fresh, e)
			}
		}
		// Folders gone from LIST: renamed or deleted.
		for _, f := range existing {
			if seen[f.name] {
				continue
			}
			if e := renameTarget(f, fresh); e != nil {
				s.log.Info("folder renamed on the server", "from", f.name, "to", e.name)
				if err := renameFolder(ctx, tx, f, e.name, e.delim); err != nil {
					return err
				}
				byName[e.name] = f
				f.name = e.name
				continue
			}
			s.log.Info("folder deleted on the server", "folder", f.name)
			if _, err := vanishAll(ctx, tx, f.id, false); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM mailbox_folders WHERE id = $1`, f.id); err != nil {
				return err
			}
		}
		gmail := caps.HasGmailExt()
		for _, e := range entries {
			role := roles[e.name]
			f := byName[e.name]
			var id int64
			if f == nil {
				mode := "sync"
				if gmail && role == RoleUser { // generic mode on Gmail: INBOX + special-use only (plan 3.4)
					mode = "ignore"
				}
				msgs := 0
				if e.status != nil && e.status.NumMessages != nil {
					msgs = int(*e.status.NumMessages)
				}
				err := tx.QueryRow(ctx, `
INSERT INTO mailbox_folders (account_id, user_id, name, name_raw, delimiter, special_use, role, uidvalidity, messages_count, sync_mode)
VALUES ($1, $2, $3, $3, $4, $5, $6, 0, $7, $8)
ON CONFLICT (account_id, name_raw) DO UPDATE SET selectable = true
RETURNING id`, s.acc.id, s.acc.userID, e.name, nullIfEmpty(e.delim), nullIfEmpty(e.specialUse), role, msgs, mode).Scan(&id)
				if err != nil {
					return err
				}
			} else {
				id = f.id
				if f.delim != e.delim || f.specialUse != e.specialUse || f.role != role {
					if _, err := tx.Exec(ctx, `UPDATE mailbox_folders SET delimiter = $2, special_use = $3, role = $4 WHERE id = $1`,
						id, nullIfEmpty(e.delim), nullIfEmpty(e.specialUse), role); err != nil {
						return err
					}
				}
				if f.role != role {
					s.log.Info("folder role changed", "folder", e.name, "from", f.role, "to", role)
					if role == RoleUser {
						if _, err := mail.EnsureFolderLabel(ctx, tx, id); err != nil {
							return err
						}
					}
					ids, err := folderMessageIDs(ctx, tx, id)
					if err != nil {
						return err
					}
					if err := rederive(ctx, tx, ids); err != nil {
						return err
					}
				}
			}
			if role == RoleUser {
				if _, err := mail.EnsureFolderLabel(ctx, tx, id); err != nil {
					return err
				}
			}
		}
		return mail.EnsureSystemLabels(ctx, tx, s.acc.id)
	})
	if err != nil {
		return nil, err
	}
	if err := s.reloadFolders(ctx); err != nil {
		return nil, err
	}
	out := map[int64]*imap.StatusData{}
	for _, e := range entries {
		for _, f := range s.folders {
			if f.name == e.name && e.status != nil {
				out[f.id] = e.status
			}
		}
	}
	return out, nil
}

// renameTarget finds the new name of a folder that left LIST: a new folder
// with the same UIDVALIDITY and message count (servers keep UIDVALIDITY on
// RENAME). Ambiguous candidates are not treated as a rename.
func renameTarget(f *folder, fresh []*listEntry) *listEntry {
	if f.uidValidity == 0 {
		return nil
	}
	var hit *listEntry
	for _, e := range fresh {
		st := e.status
		if st == nil || st.UIDValidity != f.uidValidity || st.NumMessages == nil || int(*st.NumMessages) != f.messages {
			continue
		}
		if hit != nil {
			return nil
		}
		hit = e
	}
	return hit
}

func renameFolder(ctx context.Context, tx pgx.Tx, f *folder, name, delim string) error {
	if _, err := tx.Exec(ctx, `UPDATE mailbox_folders SET name = $2, name_raw = $2, delimiter = $3 WHERE id = $1`,
		f.id, name, nullIfEmpty(delim)); err != nil {
		return err
	}
	if f.labelID == nil {
		return nil
	}
	_, err := tx.Exec(ctx, `
UPDATE labels l SET name = $2 WHERE l.id = $1
  AND NOT EXISTS (SELECT 1 FROM labels o WHERE o.account_id = l.account_id AND lower(o.name) = lower($2) AND o.id <> l.id)`,
		*f.labelID, name)
	return err
}

func nullIfEmpty(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}
