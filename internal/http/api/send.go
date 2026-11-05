package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/drafts"
)

// Error codes of sending (plan 5.3).
const (
	CodeNoRecipients    = "no_recipients"
	CodeInvalidAddress  = "invalid_address"
	CodeMessageTooLarge = "message_too_large"
	CodeSendInProgress  = "send_in_progress"
)

func sendError(err error, limit int64) error {
	switch {
	case errors.Is(err, drafts.ErrNoRecipients):
		return problem(http.StatusUnprocessableEntity, CodeNoRecipients, "Add at least one recipient.")
	case errors.Is(err, drafts.ErrInvalidAddress):
		p := problem(http.StatusUnprocessableEntity, CodeInvalidAddress, "Invalid recipient address.")
		withAddress(p, err)
		return p
	case errors.Is(err, drafts.ErrMessageTooLarge):
		p := problem(http.StatusRequestEntityTooLarge, CodeMessageTooLarge,
			"The message with its attachments is larger than the limit of "+formatBytes(limit)+".")
		p.Limit = &limit
		var se *drafts.SizeError
		if errors.As(err, &se) {
			p.Size = &se.Size
		}
		return p
	case errors.Is(err, drafts.ErrSendInProgress):
		return problem(http.StatusConflict, CodeSendInProgress, "The message is already being sent.")
	}
	return draftsError(err)
}

// SendDraftBody is the optional body of POST /drafts/{id}/send.
type SendDraftBody struct {
	SendAt *time.Time `json:"sendAt,omitempty" doc:"Schedule the send (at most a year ahead). Omitted or earlier than the end of the undo window: the message leaves when the window ends."`
}

type sendDraftInput struct {
	ID     string         `path:"id" example:"d_4k2"`
	Client string         `header:"X-Silo-Client"`
	Body   *SendDraftBody `required:"false"`
}

// SendResult is a queued send (plan 5.1).
type SendResult struct {
	MessageID  string    `json:"messageId" example:"m_4k2" doc:"The message, labelled SCHEDULED until it is sent, then SENT."`
	OutboundID string    `json:"outboundId" example:"ob_9" doc:"For POST /outbound/{id}/cancel and the outbound.status events."`
	UndoUntil  time.Time `json:"undoUntil" doc:"The message leaves at this time; until then it can be cancelled."`
}

type sendDraftOutput struct {
	Body SendResult
}

type outboundIDInput struct {
	ID     string `path:"id" example:"ob_9"`
	Client string `header:"X-Silo-Client"`
}

// ContactSuggestion is one autocomplete suggestion.
type ContactSuggestion struct {
	Email string `json:"email" example:"jan@firma.pl"`
	Name  string `json:"name" example:"Jan Kowalski"`
}

// ContactSuggestions is the answer of GET /contacts/autocomplete.
type ContactSuggestions struct {
	Contacts []ContactSuggestion `json:"contacts" maxItems:"8"`
}

type autocompleteInput struct {
	Q string `query:"q" required:"true" minLength:"1" maxLength:"200" doc:"Start of an address, a name or a word of the name."`
}

type autocompleteOutput struct {
	Body ContactSuggestions
}

func (d *deps) registerSend(a huma.API) {
	huma.Register(a, d.secured(huma.Operation{
		OperationID: "sendDraft", Method: http.MethodPost, Path: "/drafts/{id}/send", Tags: []string{"drafts"},
		Summary: "Send a draft",
		Description: "Queues the draft as last saved (save it first). The message is built at once (Bcc only in the " +
			"envelope) and labelled SCHEDULED; it leaves undoSendSeconds (5, 10, 20 or 30; 5 by default) later, or at " +
			"sendAt. Until undoUntil, POST /outbound/{id}/cancel brings the draft back. After sending: SENT, a copy in " +
			"the Sent folder (when the account's sentAppend is on; otherwise the server's own copy is matched by " +
			"Message-ID), \\Answered or $Forwarded on the original, the copy in Drafts removed. Delivery problems are " +
			"retried (30 s, 2 min, 10 min, 30 min); a refusal fails the send at once and the message is a draft again " +
			"(outbound.status event with error). 422 no_recipients / invalid_address, 413 message_too_large (over " +
			"SILO_MAX_ATTACHMENT_BYTES; the SMTP server's SIZE is checked when sending), 409 send_in_progress.",
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
			http.StatusConflict, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity},
	}), func(ctx context.Context, in *sendDraftInput) (*sendDraftOutput, error) {
		id, ok := parseID("d_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		p := drafts.SendParams{UserID: sessionFrom(ctx).UserID, ID: id, Origin: in.Client}
		if in.Body != nil {
			p.SendAt = in.Body.SendAt
		}
		r, err := d.drafts.Send(ctx, p)
		if err != nil {
			return nil, sendError(err, d.drafts.MaxUploadBytes())
		}
		return &sendDraftOutput{Body: SendResult{MessageID: formatID("m_", r.MessageID), OutboundID: formatID("ob_", r.OutboundID), UndoUntil: r.UndoUntil}}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "cancelOutbound", Method: http.MethodPost, Path: "/outbound/{id}/cancel", Tags: []string{"drafts"},
		Summary: "Undo send",
		Description: "Cancels a queued send (undo send, or a scheduled send) before it leaves: nothing reaches the SMTP " +
			"server and the message is the draft it was (same content and version), returned for the compose window. " +
			"A send already being delivered or delivered is 409 send_in_progress; a cancelled or failed one returns " +
			"its draft.",
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict},
	}), func(ctx context.Context, in *outboundIDInput) (*draftOutput, error) {
		id, ok := parseID("ob_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		x, err := d.drafts.Cancel(ctx, sessionFrom(ctx).UserID, id, in.Client)
		if err != nil {
			return nil, sendError(err, d.drafts.MaxUploadBytes())
		}
		return &draftOutput{Status: http.StatusOK, Body: draftOf(x)}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "autocompleteContacts", Method: http.MethodGet, Path: "/contacts/autocomplete", Tags: []string{"drafts"},
		Summary: "Suggest recipients",
		Description: "Up to 8 addresses of the user whose address, name or a word of the name starts with q " +
			"(case-insensitive): the most written to first, then the most recent. Learnt from received and sent mail.",
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized},
	}), func(ctx context.Context, in *autocompleteInput) (*autocompleteOutput, error) {
		list, err := d.drafts.Autocomplete(ctx, sessionFrom(ctx).UserID, in.Q)
		if err != nil {
			return nil, errInternal(err)
		}
		out := &autocompleteOutput{Body: ContactSuggestions{Contacts: make([]ContactSuggestion, 0, len(list))}}
		for _, c := range list {
			out.Body.Contacts = append(out.Body.Contacts, ContactSuggestion{Email: c.Email, Name: c.Name})
		}
		return out, nil
	})
}
