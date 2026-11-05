package drafts

import (
	"context"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/mailview"
)

// origAttachment is an attachment of the replied or forwarded message.
type origAttachment struct {
	id                    int64
	filename, contentType string
	size                  int64
	inline                bool
	cid                   string
	sha                   []byte
}

// prefillContent is what a reply or forward takes from the original: the
// quoted HTML, the attachments to copy to the draft and the warnings.
type prefillContent struct {
	quote    string
	copies   []origAttachment
	warnings []string
}

func (s *Service) originalAttachments(ctx context.Context, id int64) ([]origAttachment, error) {
	rows, err := s.o.Pool.Query(ctx, `
SELECT id, filename, content_type, size_bytes, is_inline, COALESCE(content_id, ''), blob_sha256
FROM attachments WHERE message_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (origAttachment, error) {
		var a origAttachment
		err := r.Scan(&a.id, &a.filename, &a.contentType, &a.size, &a.inline, &a.cid, &a.sha)
		return a, err
	})
}

// prepare builds the quote of a reply or forward (plan 3.8; the same for
// every client) and picks the attachments it carries:
//
//   - a body not in the mirror yet (body_state headers or deferred) is
//     downloaded first, waiting up to PrefillWait; when that fails the quote
//     is the snippet and the draft carries WarnQuoteUnavailable;
//   - a message with HTML is quoted as its sanitised HTML (like Gmail), else
//     its text is escaped into paragraphs;
//   - inline pictures the quoted HTML shows (cid:) are copied to the draft
//     with their Content-ID, so the quote keeps them when it is sent; a
//     picture that cannot be copied is dropped from the quote;
//   - a forward also copies the other attachments (WarnAttachmentsUnavailable
//     for parts that could not be downloaded, WarnAttachmentsTooLarge for
//     those over the attachment limit, counted together).
func (s *Service) prepare(ctx context.Context, userID, id int64, mode string, o *original) (*prefillContent, error) {
	pc := &prefillContent{}
	atts, err := s.originalAttachments(ctx, id)
	if err != nil {
		return nil, err
	}
	forward := mode == ModeForward
	missing := false
	for _, a := range atts {
		if a.sha == nil && (forward || a.inline) {
			missing = true
		}
	}
	local := o.bodyState == "local" || o.draftHTML != nil
	notMirrored := o.bodyState == "headers" || o.bodyState == "deferred"
	var htmlPart string
	if !local && s.o.OpenRaw != nil && (notMirrored || o.hasHTML || missing) {
		fctx, cancel := context.WithTimeout(ctx, PrefillWait)
		rc, err := s.o.OpenRaw(fctx, userID, id)
		if err == nil {
			raw, rerr := io.ReadAll(io.LimitReader(rc, 64<<20))
			_ = rc.Close()
			if rerr == nil {
				htmlPart, _ = mailview.ExtractBodies(raw)
			}
		} else {
			s.log.Info("original not available for the quote", "message", id, "error", err)
		}
		cancel()
		if notMirrored || missing {
			// The download stored the body and the attachment bytes.
			if err := s.o.Pool.QueryRow(ctx, `SELECT body_text, body_state, snippet, has_html FROM messages WHERE id = $1`, id).
				Scan(&o.bodyText, &o.bodyState, &o.snippet, &o.hasHTML); err != nil {
				return nil, err
			}
			if atts, err = s.originalAttachments(ctx, id); err != nil {
				return nil, err
			}
		}
	}
	if local && o.draftHTML != nil {
		htmlPart = *o.draftHTML
	}

	limit := s.o.MaxUploadBytes
	var total int64
	tooLarge, unavailable := false, false
	take := func(a origAttachment) bool {
		if a.sha == nil {
			unavailable = true
			return false
		}
		if limit > 0 && total+a.size > limit {
			tooLarge = true
			return false
		}
		total += a.size
		pc.copies = append(pc.copies, a)
		return true
	}

	if strings.TrimSpace(htmlPart) != "" {
		byCID := map[string]origAttachment{}
		for _, a := range atts {
			if a.cid != "" {
				byCID[strings.ToLower(a.cid)] = a
			}
		}
		copied := map[string]bool{}
		pc.quote = mailview.SanitizeHTML(htmlPart, func(cid string) string {
			key := strings.ToLower(cid)
			a, ok := byCID[key]
			if !ok {
				return ""
			}
			a.inline = true // shown by the quote: copied with its Content-ID
			if !copied[key] {
				if !take(a) {
					return ""
				}
				copied[key] = true
			}
			return "cid:" + a.cid
		})
		if forward {
			for _, a := range atts {
				if !a.inline && !copied[strings.ToLower(a.cid)] {
					take(a)
				}
			}
		}
	} else {
		text := o.bodyText
		if text == "" && (o.bodyState == "headers" || o.bodyState == "deferred") {
			text = o.snippet
			pc.warnings = append(pc.warnings, WarnQuoteUnavailable)
		}
		pc.quote = mail.TextToHTML(text)
		if forward {
			for _, a := range atts {
				if !a.inline {
					take(a)
				}
			}
		}
	}
	if forward && unavailable {
		pc.warnings = append(pc.warnings, WarnAttachmentsUnavailable)
	}
	if tooLarge {
		pc.warnings = append(pc.warnings, WarnAttachmentsTooLarge)
	}
	return pc, nil
}

// copyAttachments pins copies of the original's attachments to a new draft:
// new attachments rows (uploads of the draft, empty part_path) sharing the blob,
// whose ref_count grows (and which becomes local: a draft is worth keeping).
// It returns their ids.
func copyAttachments(ctx context.Context, tx pgx.Tx, userID, msgID int64, l []origAttachment) ([]int64, error) {
	ids := []int64{}
	for _, a := range l {
		if _, err := tx.Exec(ctx, `UPDATE blobs SET ref_count = ref_count + 1, kind = 'local' WHERE sha256 = $1`, a.sha); err != nil {
			return nil, err
		}
		var cid *string
		if a.cid != "" && a.inline {
			cid = &a.cid
		}
		var id int64
		if err := tx.QueryRow(ctx, `
INSERT INTO attachments (message_id, user_id, blob_sha256, filename, content_type, size_bytes, part_path, is_inline, content_id)
VALUES ($1, $2, $3, $4, $5, $6, '', $7, $8) RETURNING id`, msgID, userID, a.sha, a.filename, a.contentType, a.size,
			cid != nil, cid).Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}
