package drafts

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	netmail "net/mail"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
)

// Errors of sending.
var (
	ErrNoRecipients    = errors.New("drafts: the draft has no recipients")
	ErrInvalidAddress  = errors.New("drafts: invalid recipient address")
	ErrMessageTooLarge = errors.New("drafts: message too large")
	ErrSendInProgress  = errors.New("drafts: the message is being sent")
)

// AddressError is an invalid recipient address; it matches ErrInvalidAddress
// (send) or ErrInvalid (save) with errors.Is.
type AddressError struct {
	Field   string // to, cc or bcc
	Address string // as given
	base    error
}

func (e *AddressError) Error() string { return fmt.Sprintf("%v: %s %q", e.base, e.Field, e.Address) }

// Unwrap returns ErrInvalidAddress or ErrInvalid.
func (e *AddressError) Unwrap() error { return e.base }

// SizeError is a message larger than the limit; it matches ErrMessageTooLarge.
type SizeError struct {
	Size, Limit int64
}

func (e *SizeError) Error() string {
	return fmt.Sprintf("%v: %d bytes over the limit of %d", ErrMessageTooLarge, e.Size, e.Limit)
}

// Unwrap returns ErrMessageTooLarge.
func (e *SizeError) Unwrap() error { return ErrMessageTooLarge }

// JobSend is the general job that delivers one outbound message (plan 3.9
// step 4); its payload is {"outboundId": id}.
const JobSend = "send"

// EventOutboundStatus is written when a send changes state (plan 5.4):
// {outboundId, accountId, messageId, status, error?, failedRecipients?}.
const EventOutboundStatus = "outbound.status"

// DefaultUndoSendSeconds and the allowed values of users.settings
// undo_send_seconds (plan 3.9 step 1, 4.2).
const DefaultUndoSendSeconds = 5

var undoSendChoices = []int{5, 10, 20, 30}

// DefaultSendRetry are the delays after transient SMTP failures (plan 3.9
// step 6: 30 s, 2 min, 10 min, 30 min, then failed).
var DefaultSendRetry = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}

// maxSchedule bounds sendAt of a scheduled send.
const maxSchedule = 366 * 24 * time.Hour

// notSending: the draft is not queued, being sent or sent (drafts alias d).
const notSending = `NOT EXISTS (SELECT 1 FROM outbound o WHERE o.message_id = d.message_id AND o.status IN ('queued', 'sending', 'sent'))`

// SendParams is POST /drafts/{id}/send.
type SendParams struct {
	UserID, ID int64
	// SendAt schedules the send; a time before the end of the undo window
	// (or nil) sends when the window ends.
	SendAt *time.Time
	Origin string
}

// SendResult is the queued send.
type SendResult struct {
	MessageID, OutboundID int64
	// UndoUntil is outbound.send_at: until then POST /outbound/{id}/cancel
	// brings the draft back and nothing reaches the SMTP server.
	UndoUntil time.Time
}

func validRecipient(e string) bool {
	if e == "" || strings.ContainsAny(e, " <>,;\"\r\n") || !strings.Contains(e, "@") {
		return false
	}
	a, err := netmail.ParseAddress(e)
	return err == nil && strings.EqualFold(a.Address, e)
}

// Send queues a draft for sending (plan 3.9 step 1), in one transaction: the
// message is built (mail.BuildSendMIME: Bcc only in the envelope) and stored
// as the local message's body (blob kind local), the pending save to the
// Drafts folder is cancelled, the message turns SCHEDULED (its drafts row
// stays, so that cancel restores the same content and version), and an
// outbound row (queued) with a send job at send_at = now + undo_send_seconds
// (or sendAt) is written with thread.updated and outbound.status.
func (s *Service) Send(ctx context.Context, p SendParams) (*SendResult, error) {
	var accountID int64
	err := s.o.Pool.QueryRow(ctx, `SELECT account_id FROM drafts WHERE message_id = $1 AND user_id = $2`, p.ID, p.UserID).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	res := &SendResult{MessageID: p.ID}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := mail.LockAccount(ctx, tx, accountID); err != nil {
			return err
		}
		var status string
		var undo int
		err := tx.QueryRow(ctx, `
SELECT COALESCE((SELECT o.status FROM outbound o WHERE o.message_id = d.message_id), ''),
       CASE WHEN u.settings->>'undo_send_seconds' ~ '^[0-9]{1,4}$' THEN (u.settings->>'undo_send_seconds')::int ELSE 0 END
FROM drafts d JOIN users u ON u.id = d.user_id
WHERE d.message_id = $1 AND d.user_id = $2 FOR UPDATE OF d`, p.ID, p.UserID).Scan(&status, &undo)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch status {
		case mail.OutboundQueued, mail.OutboundSending:
			return ErrSendInProgress
		case mail.OutboundSent:
			return ErrNotFound
		}
		if !slices.Contains(undoSendChoices, undo) {
			undo = DefaultUndoSendSeconds
		}
		now := time.Now()
		sendAt := now.Add(time.Duration(undo) * time.Second)
		if p.SendAt != nil {
			if p.SendAt.After(now.Add(maxSchedule)) {
				return invalid("sendAt is more than a year ahead")
			}
			if p.SendAt.After(sendAt) {
				sendAt = *p.SendAt
			}
		}
		m, err := mail.BuildSendMIME(ctx, tx, s.o.Blobs, p.ID, sendAt)
		if err != nil {
			return err
		}
		if len(m.Recipients()) == 0 {
			return ErrNoRecipients
		}
		for _, f := range []struct {
			name string
			l    []mail.Address
		}{{"to", m.To}, {"cc", m.Cc}, {"bcc", m.Bcc}} {
			for _, a := range f.l {
				if a.Email != "" && !validRecipient(strings.ToLower(a.Email)) {
					return &AddressError{Field: f.name, Address: a.Email, base: ErrInvalidAddress}
				}
			}
		}
		if s.o.MaxUploadBytes > 0 && int64(len(m.Raw)) > s.o.MaxUploadBytes {
			return &SizeError{Size: int64(len(m.Raw)), Limit: s.o.MaxUploadBytes}
		}
		sum, size, err := s.o.Blobs.Put(ctx, bytes.NewReader(m.Raw))
		if err != nil {
			return fmt.Errorf("drafts: store sent message: %w", err)
		}
		sha, _ := hex.DecodeString(sum)
		if _, err := tx.Exec(ctx, `
INSERT INTO blobs (sha256, size_bytes, kind, ref_count) VALUES ($1, $2, 'local', 1)
ON CONFLICT (sha256) DO UPDATE SET ref_count = blobs.ref_count + 1`, sha, size); err != nil {
			return err
		}
		if err := releaseRaw(ctx, tx, p.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE messages SET raw_blob_sha256 = $2, size_bytes = $3 WHERE id = $1`, p.ID, sha, size); err != nil {
			return err
		}
		// The copy in Drafts is not updated any more; it is removed once sent.
		if _, err := tx.Exec(ctx, `DELETE FROM jobs WHERE id IN (`+unclaimedAppend+`)`, p.ID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
INSERT INTO outbound (user_id, account_id, message_id, status, send_at)
VALUES ($1, $2, $3, 'queued', $4)
ON CONFLICT (message_id) DO UPDATE SET status = 'queued', send_at = $4, attempts = 0, last_error = NULL,
  smtp_response = NULL, failed_recipients = '{}', bounced_recipients = '{}', updated_at = now()
RETURNING id`, p.UserID, accountID, p.ID, sendAt).Scan(&res.OutboundID); err != nil {
			return fmt.Errorf("drafts: insert outbound: %w", err)
		}
		payload, _ := json.Marshal(sendPayload{OutboundID: res.OutboundID})
		if _, err := tx.Exec(ctx, `INSERT INTO jobs (kind, payload, run_at, max_attempts) VALUES ($1, $2, $3, 5)`,
			JobSend, payload, sendAt); err != nil {
			return fmt.Errorf("drafts: enqueue send: %w", err)
		}
		res.UndoUntil = sendAt
		if _, err := mail.RefreshMessages(ctx, tx, accountID, []int64{p.ID}, p.Origin); err != nil {
			return err
		}
		return writeOutboundEvent(ctx, tx, p.UserID, accountID, p.ID, res.OutboundID, mail.OutboundQueued, "", nil, p.Origin)
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

type sendPayload struct {
	OutboundID int64 `json:"outboundId"`
}

func writeOutboundEvent(ctx context.Context, tx pgx.Tx, userID, accountID, msgID, outboundID int64, status, errText string, failed []string, origin string) error {
	// draftId: the same row as messageId, as the d_ id of GET /drafts/{id} (opening the draft
	// again after a failure needs no id mapping in the client).
	ev := map[string]any{"outboundId": outboundID, "accountId": accountID, "messageId": msgID, "draftId": msgID, "status": status}
	if errText != "" {
		ev["error"] = errText
	}
	if len(failed) > 0 {
		ev["failedRecipients"] = failed
	}
	if origin != "" {
		ev["origin"] = origin
	}
	return mail.WriteEvent(ctx, tx, userID, EventOutboundStatus, ev)
}

// releaseRaw drops the stored body of a local message (its blob reference).
func releaseRaw(ctx context.Context, tx pgx.Tx, msgID int64) error {
	_, err := tx.Exec(ctx, `
UPDATE blobs b SET ref_count = GREATEST(b.ref_count - 1, 0)
FROM messages m WHERE m.id = $1 AND b.sha256 = m.raw_blob_sha256`, msgID)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE messages SET raw_blob_sha256 = NULL WHERE id = $1 AND raw_blob_sha256 IS NOT NULL`, msgID)
	}
	return err
}

// Cancel is undo send (plan 3.9 step 2): while the send job is not claimed,
// in one transaction the job is deleted, the outbound row cancelled and the
// message is the draft it was (same content and version), returned for the
// compose window. A send being delivered or delivered is ErrSendInProgress;
// a cancelled or failed one returns its draft again.
func (s *Service) Cancel(ctx context.Context, userID, outboundID int64, origin string) (*Draft, error) {
	var accountID, msgID int64
	err := s.o.Pool.QueryRow(ctx, `SELECT account_id, message_id FROM outbound WHERE id = $1 AND user_id = $2`, outboundID, userID).Scan(&accountID, &msgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := mail.LockAccount(ctx, tx, accountID); err != nil {
			return err
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM outbound WHERE id = $1 FOR UPDATE`, outboundID).Scan(&status); err != nil {
			return err
		}
		switch status {
		case mail.OutboundCancelled, mail.OutboundFailed:
			return nil
		case mail.OutboundSending, mail.OutboundSent:
			return ErrSendInProgress
		}
		var locked bool
		err := tx.QueryRow(ctx, `
SELECT COALESCE(bool_or(locked_until > now()), false) FROM jobs
WHERE kind = $1 AND status = 'pending' AND (payload->>'outboundId')::bigint = $2`, JobSend, outboundID).Scan(&locked)
		if err != nil {
			return err
		}
		if locked {
			return ErrSendInProgress
		}
		if _, err := tx.Exec(ctx, `DELETE FROM jobs WHERE kind = $1 AND status = 'pending' AND (payload->>'outboundId')::bigint = $2`, JobSend, outboundID); err != nil {
			return err
		}
		return s.backToDraft(ctx, tx, accountID, userID, msgID, outboundID, mail.OutboundCancelled, "", origin)
	})
	if err != nil {
		return nil, err
	}
	return s.load(ctx, s.o.Pool, userID, msgID)
}

// backToDraft ends a send that did not happen (cancelled or failed): the
// stored body is released, the message derives DRAFT again (its drafts row
// was kept), its copy in the Drafts folder is saved again when outdated, and
// outbound.status is written.
func (s *Service) backToDraft(ctx context.Context, tx pgx.Tx, accountID, userID, msgID, outboundID int64, status, errText, origin string) error {
	if _, err := tx.Exec(ctx, `UPDATE outbound SET status = $2, last_error = NULLIF($3, ''), updated_at = now() WHERE id = $1`,
		outboundID, status, errText); err != nil {
		return err
	}
	if err := releaseRaw(ctx, tx, msgID); err != nil {
		return err
	}
	var stale bool
	if err := tx.QueryRow(ctx, `SELECT version > server_synced_version FROM drafts WHERE message_id = $1`, msgID).Scan(&stale); err != nil {
		return err
	}
	if stale {
		if err := s.scheduleAppend(ctx, tx, accountID, userID, msgID, s.o.SyncDelay); err != nil {
			return err
		}
	}
	if _, err := mail.RefreshMessages(ctx, tx, accountID, []int64{msgID}, origin); err != nil {
		return err
	}
	return writeOutboundEvent(ctx, tx, userID, accountID, msgID, outboundID, status, errText, nil, origin)
}

// Contact is an autocomplete suggestion.
type Contact = mail.Contact

// AutocompleteLimit is the number of suggestions (plan 5.1).
const AutocompleteLimit = 8

// Autocomplete suggests recipients of the user (plan 5.1): contacts whose
// address or name (or a word of it) starts with q, the most written to
// first, then the most recent. Contacts are fed by ingest (From of received,
// To/Cc of sent messages) and by sends from Silo.
func (s *Service) Autocomplete(ctx context.Context, userID int64, q string) ([]Contact, error) {
	return mail.SuggestContacts(ctx, s.o.Pool, userID, q, AutocompleteLimit)
}
