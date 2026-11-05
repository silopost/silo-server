package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
)

// Errors of the label operations.
var (
	// ErrExists: another label (or folder) of the account has the name.
	ErrExists = errors.New("ops: a label with this name already exists")
	// ErrNotEmpty: only an empty folder can be deleted.
	ErrNotEmpty = errors.New("ops: the folder is not empty")
)

// Label is a row of GET /labels with database ids.
type Label struct {
	ID            int64
	AccountID     int64
	Name          string
	Type          string // system, folder, user
	SystemKey     *string
	KeywordState  string // mirrored, local_only, partial (mirrored, but local in LocalOnlyFolders)
	ColorBG       *string
	ColorText     *string
	ShowInList    string
	ParentID      *int64
	Position      int
	FolderID      *int64
	ThreadsTotal  int
	ThreadsUnread int
	// LocalOnlyFolders: folders where the keyword of the label is not kept
	// (plan 3.7: the 26 keywords of a maildir folder); its assignments there
	// live only in Silo.
	LocalOnlyFolders []int64
}

const maxLabelName = 200

func cleanName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > maxLabelName || strings.ContainsAny(s, "\r\n\x00") {
		return "", fmt.Errorf("%w: label name must be 1-%d characters", ErrInvalid, maxLabelName)
	}
	return s, nil
}

// CreateLabel is the input of Service.CreateLabel.
type CreateLabel struct {
	UserID, AccountID  int64
	Name               string
	Type               string // user or folder
	ColorBG, ColorText *string
	ParentID           *int64
	Origin             string
}

func (s *Service) accountOf(ctx context.Context, tx pgx.Tx, userID, accountID int64) (keywords bool, err error) {
	err = tx.QueryRow(ctx, `SELECT keywords_supported FROM accounts WHERE id = $1 AND user_id = $2`, accountID, userID).Scan(&keywords)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	return keywords, err
}

func labelChanged(ctx context.Context, tx pgx.Tx, userID, accountID, labelID int64, origin string) error {
	p := map[string]any{"accountId": accountID, "labelId": labelID}
	if origin != "" {
		p["origin"] = origin
	}
	return mail.WriteEvent(ctx, tx, userID, mail.EventLabelChanged, p)
}

// CreateLabel creates a user label (local; the IMAP keyword is assigned now
// when the server keeps keywords) or a folder (a mailbox_folders row and its
// label at once, and an imap_op job create_folder that runs CREATE on the
// server; the next discovery confirms the folder).
func (s *Service) CreateLabel(ctx context.Context, in CreateLabel) (*Label, error) {
	name, err := cleanName(in.Name)
	if err != nil {
		return nil, err
	}
	if in.Type != "user" && in.Type != "folder" {
		return nil, fmt.Errorf("%w: type must be user or folder", ErrInvalid)
	}
	var id int64
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := mail.LockAccount(ctx, tx, in.AccountID); err != nil {
			return err
		}
		keywords, err := s.accountOf(ctx, tx, in.UserID, in.AccountID)
		if err != nil {
			return err
		}
		if in.ParentID != nil {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM labels WHERE id = $1 AND account_id = $2 AND type <> 'system')`, *in.ParentID, in.AccountID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: unknown parent label", ErrInvalid)
			}
		}
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM labels WHERE account_id = $1 AND lower(name) = lower($2))`, in.AccountID, name).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return ErrExists
		}
		if in.Type == "user" {
			var kw *string
			state := "local_only"
			if keywords {
				k, err := mail.AssignKeyword(ctx, tx, in.AccountID, name)
				if err != nil {
					return err
				}
				kw, state = &k, "mirrored"
			}
			return tx.QueryRow(ctx, `
INSERT INTO labels (user_id, account_id, name, type, imap_keyword, keyword_state, color_bg, color_text, parent_id)
VALUES ($1, $2, $3, 'user', $4, $5, $6, $7, $8) RETURNING id`,
				in.UserID, in.AccountID, name, kw, state, in.ColorBG, in.ColorText, in.ParentID).Scan(&id)
		}
		// Folder: the name on the server uses the account's hierarchy delimiter.
		raw := name
		var delim *string
		if err := tx.QueryRow(ctx, `SELECT delimiter FROM mailbox_folders WHERE account_id = $1 AND delimiter IS NOT NULL ORDER BY (role = 'inbox') DESC, id LIMIT 1`, in.AccountID).Scan(&delim); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if delim != nil && *delim != "/" {
			raw = strings.ReplaceAll(raw, "/", *delim)
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mailbox_folders WHERE account_id = $1 AND lower(name_raw) = lower($2))`, in.AccountID, raw).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrExists
		}
		var folderID int64
		if err := tx.QueryRow(ctx, `
INSERT INTO mailbox_folders (account_id, user_id, name, name_raw, delimiter, role, uidvalidity)
VALUES ($1, $2, $3, $3, $4, 'user', 0) RETURNING id`, in.AccountID, in.UserID, raw, delim).Scan(&folderID); err != nil {
			return err
		}
		if id, err = mail.EnsureFolderLabel(ctx, tx, folderID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE labels SET color_bg = $2, color_text = $3, parent_id = $4 WHERE id = $1`, id, in.ColorBG, in.ColorText, in.ParentID); err != nil {
			return err
		}
		return s.folderJob(ctx, tx, in.AccountID, mail.OpPayload{Op: mail.OpCreateFolder, Name: raw})
	})
	if err != nil {
		return nil, err
	}
	if err := s.inTx(ctx, func(tx pgx.Tx) error { return labelChanged(ctx, tx, in.UserID, in.AccountID, id, in.Origin) }); err != nil {
		return nil, err
	}
	return s.GetLabel(ctx, in.UserID, id)
}

// folderJob inserts an imap_op job that has no messages.
func (s *Service) folderJob(ctx context.Context, tx pgx.Tx, accountID int64, p mail.OpPayload) error {
	_, err := tx.Exec(ctx, `
INSERT INTO jobs (kind, account_id, group_id, payload, max_attempts)
VALUES ('imap_op', $1, gen_random_uuid(), $2::jsonb, $3)`, accountID, payloadJSON(p), MaxAttempts)
	return err
}

const labelSelect = `
SELECT l.id, l.account_id, l.name, l.type, l.system_key, l.keyword_state, l.color_bg, l.color_text, l.show_in_list,
       l.parent_id, l.position, l.folder_id,
       count(tl.thread_id) FILTER (WHERE NOT tl.hidden)::int,
       count(tl.thread_id) FILTER (WHERE NOT tl.hidden AND tl.unread_count > 0)::int,
       ARRAY(SELECT s.folder_id FROM label_folder_state s WHERE s.label_id = l.id ORDER BY s.folder_id)
FROM labels l LEFT JOIN thread_labels tl ON tl.label_id = l.id
WHERE l.user_id = $1 AND `

func scanLabel(row pgx.Row) (*Label, error) {
	var l Label
	err := row.Scan(&l.ID, &l.AccountID, &l.Name, &l.Type, &l.SystemKey, &l.KeywordState, &l.ColorBG, &l.ColorText, &l.ShowInList,
		&l.ParentID, &l.Position, &l.FolderID, &l.ThreadsTotal, &l.ThreadsUnread, &l.LocalOnlyFolders)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if l.KeywordState == "mirrored" && len(l.LocalOnlyFolders) > 0 {
		l.KeywordState = "partial"
	}
	return &l, err
}

// GetLabel returns one label of the user with its counters.
func (s *Service) GetLabel(ctx context.Context, userID, labelID int64) (*Label, error) {
	return scanLabel(s.pool.QueryRow(ctx, labelSelect+`l.id = $2 GROUP BY l.id`, userID, labelID))
}

// SystemLabel resolves a system key (INBOX, ...) of an account.
func (s *Service) SystemLabel(ctx context.Context, userID, accountID int64, key string) (*Label, error) {
	return scanLabel(s.pool.QueryRow(ctx, labelSelect+`l.account_id = $2 AND l.system_key = $3 GROUP BY l.id`, userID, accountID, key))
}

// UpdateLabel is the input of Service.UpdateLabel; nil fields stay unchanged.
type UpdateLabel struct {
	UserID, LabelID int64
	Name            *string
	// ColorBG / ColorText: a pointer to "" clears the colour.
	ColorBG, ColorText *string
	ShowInList         *string // show, hide, show_if_unread
	Origin             string
}

// UpdateLabel changes name, colours and visibility. Renaming a user label
// assigns a new IMAP keyword and moves the messages to it with one flags
// operation (UID STORE +new -old; plan 3.7 keyword_rename); system labels and
// folders cannot be renamed.
func (s *Service) UpdateLabel(ctx context.Context, in UpdateLabel) (*Label, error) {
	if in.ShowInList != nil && !(*in.ShowInList == "show" || *in.ShowInList == "hide" || *in.ShowInList == "show_if_unread") {
		return nil, fmt.Errorf("%w: showInList must be show, hide or show_if_unread", ErrInvalid)
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var accountID int64
		var typ, name, state string
		var kw *string
		err := tx.QueryRow(ctx, `SELECT account_id, type, name, imap_keyword, keyword_state FROM labels WHERE id = $1 AND user_id = $2`, in.LabelID, in.UserID).
			Scan(&accountID, &typ, &name, &kw, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := mail.LockAccount(ctx, tx, accountID); err != nil {
			return err
		}
		if in.Name != nil && *in.Name != name {
			newName, err := cleanName(*in.Name)
			if err != nil {
				return err
			}
			if typ != "user" {
				return fmt.Errorf("%w: only user labels can be renamed", ErrNotModifiable)
			}
			if newName != name {
				if err := s.renameUserLabel(ctx, tx, in.UserID, accountID, in.LabelID, newName, kw, state == "mirrored"); err != nil {
					return err
				}
			}
		}
		if in.ColorBG != nil || in.ColorText != nil {
			if _, err := tx.Exec(ctx, `
UPDATE labels SET color_bg = CASE WHEN $2::text IS NULL THEN color_bg ELSE NULLIF($2, '') END,
                  color_text = CASE WHEN $3::text IS NULL THEN color_text ELSE NULLIF($3, '') END WHERE id = $1`,
				in.LabelID, in.ColorBG, in.ColorText); err != nil {
				return err
			}
		}
		if in.ShowInList != nil {
			if _, err := tx.Exec(ctx, `UPDATE labels SET show_in_list = $2 WHERE id = $1`, in.LabelID, *in.ShowInList); err != nil {
				return err
			}
		}
		return labelChanged(ctx, tx, in.UserID, accountID, in.LabelID, in.Origin)
	})
	if err != nil {
		return nil, err
	}
	return s.GetLabel(ctx, in.UserID, in.LabelID)
}

func (s *Service) renameUserLabel(ctx context.Context, tx pgx.Tx, userID, accountID, labelID int64, newName string, oldKw *string, mirrored bool) error {
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM labels WHERE account_id = $1 AND lower(name) = lower($2) AND id <> $3)`, accountID, newName, labelID).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return ErrExists
	}
	keywords, err := s.accountOf(ctx, tx, userID, accountID)
	if err != nil {
		return err
	}
	if oldKw == nil || !mirrored || !keywords {
		_, err := tx.Exec(ctx, `UPDATE labels SET name = $2 WHERE id = $1`, labelID, newName)
		return err
	}
	// Free the old keyword for the allocation so that "Work" -> "work" keeps it.
	if _, err := tx.Exec(ctx, `UPDATE labels SET imap_keyword = NULL WHERE id = $1`, labelID); err != nil {
		return err
	}
	newKw, err := mail.AssignKeyword(ctx, tx, accountID, newName, *oldKw)
	if err != nil {
		return err
	}
	if strings.EqualFold(newKw, *oldKw) {
		newKw = *oldKw
	}
	if _, err := tx.Exec(ctx, `UPDATE labels SET name = $2, imap_keyword = $3 WHERE id = $1`, labelID, newName, newKw); err != nil {
		return err
	}
	if newKw == *oldKw {
		return nil
	}
	msgs, err := labelMessages(ctx, tx, labelID)
	if err != nil {
		return err
	}
	if err := s.flagsJob(ctx, tx, accountID, userID, mail.OpPayload{Op: mail.OpFlags, Add: []string{newKw}, Remove: []string{*oldKw}}, msgs); err != nil {
		return err
	}
	_, err = mail.RefreshMessages(ctx, tx, accountID, msgs, "")
	return err
}

// labelMessages lists the messages of a label that exist on the server.
func labelMessages(ctx context.Context, tx pgx.Tx, labelID int64) ([]int64, error) {
	rows, err := tx.Query(ctx, `
SELECT ml.message_id FROM message_labels ml
WHERE ml.label_id = $1 AND EXISTS (SELECT 1 FROM message_locations l WHERE l.message_id = ml.message_id)
ORDER BY ml.message_id`, labelID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

func (s *Service) flagsJob(ctx context.Context, tx pgx.Tx, accountID, userID int64, p mail.OpPayload, msgs []int64) error {
	if len(msgs) == 0 {
		return nil
	}
	items := make([]item, len(msgs))
	for i, m := range msgs {
		items[i] = item{messageID: m}
	}
	var group string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&group); err != nil {
		return err
	}
	_, _, err := enqueue(ctx, tx, accountID, userID, group, p, items, 0, nil)
	return err
}

// DeleteLabel removes a user label (its keyword is taken off the messages by
// a flags operation) or an empty folder (an imap_op job runs DELETE on the
// server; the label disappears with the next discovery). System labels and
// non-empty folders are refused.
func (s *Service) DeleteLabel(ctx context.Context, userID, labelID int64, origin string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var accountID int64
		var typ, state string
		var kw *string
		var folderID *int64
		err := tx.QueryRow(ctx, `SELECT account_id, type, imap_keyword, keyword_state, folder_id FROM labels WHERE id = $1 AND user_id = $2`, labelID, userID).
			Scan(&accountID, &typ, &kw, &state, &folderID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := mail.LockAccount(ctx, tx, accountID); err != nil {
			return err
		}
		switch typ {
		case "system":
			return fmt.Errorf("%w: system labels cannot be deleted", ErrNotModifiable)
		case "folder":
			if folderID == nil {
				return ErrNotFound
			}
			var role string
			var busy bool
			if err := tx.QueryRow(ctx, `
SELECT role, EXISTS (SELECT 1 FROM message_locations WHERE folder_id = $1 AND NOT stale) OR messages_count > 0
FROM mailbox_folders WHERE id = $1`, *folderID).Scan(&role, &busy); err != nil {
				return err
			}
			if role != "user" {
				return fmt.Errorf("%w: only plain folders can be deleted", ErrNotModifiable)
			}
			if busy {
				return ErrNotEmpty
			}
			return s.folderJob(ctx, tx, accountID, mail.OpPayload{Op: mail.OpDeleteFolder, FolderID: *folderID})
		}
		keywords, err := s.accountOf(ctx, tx, userID, accountID)
		if err != nil {
			return err
		}
		msgs, err := labelMessages(ctx, tx, labelID)
		if err != nil {
			return err
		}
		if kw != nil && state == "mirrored" && keywords {
			if err := s.flagsJob(ctx, tx, accountID, userID, mail.OpPayload{Op: mail.OpFlags, Remove: []string{*kw}}, msgs); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `SELECT message_id FROM message_labels WHERE label_id = $1`, labelID)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM labels WHERE id = $1`, labelID); err != nil {
			return err
		}
		if _, err := mail.RefreshMessages(ctx, tx, accountID, ids, ""); err != nil {
			return err
		}
		return labelChanged(ctx, tx, userID, accountID, labelID, origin)
	})
}

// ListLabels returns the labels of an account with thread counters
// (threadsTotal, threadsUnread count threads with a visible thread_labels row).
func (s *Service) ListLabels(ctx context.Context, userID, accountID int64) ([]Label, error) {
	rows, err := s.pool.Query(ctx, labelSelect+`l.account_id = $2 GROUP BY l.id ORDER BY l.position, l.id`, userID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Label{}
	for rows.Next() {
		l, err := scanLabel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

// EnqueueFlags adds a flags operation (no undo window) on messages of one
// account inside tx, e.g. \Answered on the original of a sent reply (plan
// 3.9 step 5). The caller refreshes the messages (RefreshMessages).
func EnqueueFlags(ctx context.Context, tx pgx.Tx, accountID, userID int64, add []string, msgs []int64) error {
	return (&Service{}).flagsJob(ctx, tx, accountID, userID, mail.OpPayload{Op: mail.OpFlags, Add: add}, msgs)
}
