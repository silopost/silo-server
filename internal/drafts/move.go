package drafts

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
)

// ErrAccountLocked: the account ("From") of a reply or forward cannot change;
// the draft belongs to the thread of the account that received the original.
var ErrAccountLocked = errors.New("drafts: the account of a reply or forward cannot change")

// moveDraft saves a new-message draft under another account of the user
// (the "From" choice of compose; plan 7 phase 5). Messages, threads and
// labels are per account, so the draft becomes a new local message in a new
// thread of the target account (new d_/m_/t_ ids) with the saved fields,
// attachments and version+1; the old message is deleted (thread.deleted) and
// its copy in the old account's Drafts folder removed by a delete_draft job.
// The copy in the new account's Drafts folder follows like any save.
// Replies and forwards (in_reply_to set, also adopted drafts of another
// program with In-Reply-To) are ErrAccountLocked.
func (s *Service) moveDraft(ctx context.Context, p UpdateParams, from int64) (*Draft, error) {
	to := p.AccountID
	var self, name string
	err := s.o.Pool.QueryRow(ctx, `SELECT lower(email::text), display_name FROM accounts WHERE id = $1 AND user_id = $2`, to, p.UserID).Scan(&self, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	domain := "silo.invalid"
	if i := strings.LastIndexByte(self, '@'); i >= 0 && i < len(self)-1 {
		domain = self[i+1:]
	}
	var newID int64
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		// Both accounts change: lock them in id order (no deadlock with a
		// move the other way).
		for _, id := range []int64{min(from, to), max(from, to)} {
			if err := mail.LockAccount(ctx, tx, id); err != nil {
				return err
			}
		}
		var version int32
		var accountID int64
		var locked bool
		err := tx.QueryRow(ctx, `
SELECT d.version, d.account_id, d.in_reply_to_message_id IS NOT NULL OR m.in_reply_to_hdr IS NOT NULL
FROM drafts d JOIN messages m ON m.id = d.message_id
WHERE d.message_id = $1 AND d.user_id = $2 AND `+notSending+` FOR UPDATE OF d`, p.ID, p.UserID).Scan(&version, &accountID, &locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if version != p.Version || accountID != from {
			return ErrConflict
		}
		if locked {
			return ErrAccountLocked
		}
		var n int
		var hasAtt bool
		if err := tx.QueryRow(ctx, `
SELECT count(*), COALESCE(bool_or(NOT is_inline), false) FROM attachments
WHERE id = ANY($1) AND user_id = $2 AND blob_sha256 IS NOT NULL AND (message_id IS NULL OR message_id = $3)`,
			p.AttachmentIDs, p.UserID, p.ID).Scan(&n, &hasAtt); err != nil {
			return err
		}
		if n != len(p.AttachmentIDs) {
			return invalid("unknown attachment id")
		}
		nd := mail.NewDraft{AccountID: to, UserID: p.UserID, SiloDraftID: "d_" + randomToken(),
			MessageIDHdr: randomToken() + "@" + domain, DraftFields: mail.DraftFields{
				From: mail.Address{Name: name, Email: self}, To: p.To, Cc: p.Cc, Bcc: p.Bcc,
				Subject: p.Subject, BodyHTML: p.BodyHTML, HasAttachments: hasAtt,
			}}
		if newID, _, err = mail.CreateDraftMessage(ctx, tx, nd); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO drafts (message_id, user_id, account_id, to_addrs, cc_addrs, bcc_addrs, subject, body_html, body_json,
  attachment_ids, version, silo_draft_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`, newID, p.UserID, to, addrJSON(p.To), addrJSON(p.Cc),
			addrJSON(p.Bcc), p.Subject, p.BodyHTML, []byte(p.BodyJSON), p.AttachmentIDs, version+1, nd.SiloDraftID); err != nil {
			return fmt.Errorf("drafts: insert moved draft: %w", err)
		}
		// Uploads go with the draft before the old message (and, by cascade,
		// its attachment rows) is deleted; the ones left out are unpinned.
		if _, err := tx.Exec(ctx, `UPDATE attachments SET message_id = $1 WHERE id = ANY($2) AND (message_id IS NULL OR message_id = $3)`,
			newID, p.AttachmentIDs, p.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE attachments SET message_id = NULL, created_at = now() WHERE message_id = $1 AND part_path = ''`, p.ID); err != nil {
			return err
		}
		if _, err := removeServerCopy(ctx, tx, from, p.ID); err != nil {
			return err
		}
		if err := mail.DeleteMessages(ctx, tx, from, []int64{p.ID}, p.Origin); err != nil {
			return err
		}
		if _, err := mail.RefreshMessages(ctx, tx, to, []int64{newID}, p.Origin); err != nil {
			return err
		}
		ev := map[string]any{"draftId": newID, "accountId": to, "version": version + 1, "source": "silo", "previousDraftId": p.ID}
		if p.Origin != "" {
			ev["origin"] = p.Origin
		}
		if err := mail.WriteEvent(ctx, tx, p.UserID, EventDraftUpdated, ev); err != nil {
			return err
		}
		delay := s.o.SyncDelay
		if p.SyncNow {
			delay = 0
		}
		return s.scheduleAppend(ctx, tx, to, p.UserID, newID, delay)
	})
	if err != nil {
		return nil, err
	}
	return s.load(ctx, s.o.Pool, p.UserID, newID)
}
