package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/mailview"
	"github.com/silopost/silo-server/internal/ops"
)

// ThreadList is the answer of GET /threads (plan 5.2).
type ThreadList struct {
	Threads            []Thread `json:"threads"`
	NextCursor         *string  `json:"nextCursor" doc:"Opaque; pass it as cursor for the next page. null on the last page."`
	ResultSizeEstimate int      `json:"resultSizeEstimate" doc:"Threads in the view (exact for a label, an estimate otherwise)."`
}

type listThreadsInput struct {
	AccountID string `query:"accountId" doc:"Account; the account of labelId, or the active account, when omitted."`
	LabelID   string `query:"labelId" doc:"System key (INBOX, ...) or l_ id. Omitted: all mail except threads entirely in Trash or Spam."`
	Q         string `query:"q" maxLength:"500" doc:"Full-text query (plain words; no operators yet)."`
	Cursor    string `query:"cursor"`
	Limit     int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
}

type listThreadsOutput struct{ Body ThreadList }

// Address is a mailbox of a message.
type Address struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// MessageFolder is where a message is on the server.
type MessageFolder struct {
	ID   string `json:"id" example:"f_3"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// MessageAttachment of a message.
type MessageAttachment struct {
	ID          string `json:"id" example:"a_44"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	Inline      bool   `json:"inline"`
	Downloaded  bool   `json:"downloaded" doc:"false: the bytes are fetched from the mail server on first download."`
}

// Message is a message of a thread (plan 5.2).
type Message struct {
	ID              string              `json:"id" example:"m_1x9"`
	From            Address             `json:"from"`
	To              []Address           `json:"to"`
	Cc              []Address           `json:"cc"`
	Bcc             []Address           `json:"bcc"`
	ReplyTo         *Address            `json:"replyTo"`
	Date            time.Time           `json:"date"`
	Subject         string              `json:"subject"`
	Snippet         string              `json:"snippet"`
	LabelIDs        []string            `json:"labelIds"`
	Folder          *MessageFolder      `json:"folder"`
	BodyState       string              `json:"bodyState" enum:"headers,full,deferred,local" doc:"headers: only the headers are in the mirror; GET /messages/{id}/html downloads the body."`
	HasHTML         bool                `json:"hasHtml" doc:"A text/html part exists; known once the body is downloaded."`
	BodyText        string              `json:"bodyText"`
	Attachments     []MessageAttachment `json:"attachments"`
	ListUnsubscribe *string             `json:"listUnsubscribe"`
	PendingSync     bool                `json:"pendingSync"`
}

// ThreadDetail is the answer of GET /threads/{id}.
type ThreadDetail struct {
	ID        string    `json:"id" example:"t_8f3k2"`
	AccountID string    `json:"accountId"`
	Subject   string    `json:"subject"`
	LabelIDs  []string  `json:"labelIds"`
	Messages  []Message `json:"messages"`
}

type threadIDInput struct {
	ID string `path:"id"`
}

type threadDetailOutput struct{ Body ThreadDetail }

func addressList(in []mailview.Address) []Address {
	out := make([]Address, len(in))
	for i, a := range in {
		out[i] = Address(a)
	}
	return out
}

func messageOf(m mailview.Message) Message {
	out := Message{
		ID: formatID("m_", m.ID), From: Address(m.From), To: addressList(m.To), Cc: addressList(m.Cc), Bcc: addressList(m.Bcc),
		Date: m.Date, Subject: m.Subject, Snippet: m.Snippet, BodyState: m.BodyState, HasHTML: m.HasHTML, BodyText: m.BodyText,
		ListUnsubscribe: m.ListUnsubscribe, PendingSync: m.PendingSync,
		LabelIDs: append([]string{}, m.SystemLabels...), Attachments: make([]MessageAttachment, 0, len(m.Attachments)),
	}
	if m.ReplyTo != nil {
		r := Address(*m.ReplyTo)
		out.ReplyTo = &r
	}
	for _, id := range m.LabelIDs {
		out.LabelIDs = append(out.LabelIDs, formatID("l_", id))
	}
	if m.Folder != nil {
		out.Folder = &MessageFolder{ID: formatID("f_", m.Folder.ID), Name: m.Folder.Name, Role: m.Folder.Role}
	}
	for _, a := range m.Attachments {
		out.Attachments = append(out.Attachments, MessageAttachment{ID: formatID("a_", a.ID), Filename: a.Filename,
			ContentType: a.ContentType, Size: a.Size, Inline: a.Inline, Downloaded: a.Downloaded})
	}
	return out
}

func (d *deps) registerThreadViews(a huma.API) {
	huma.Register(a, d.secured(huma.Operation{
		OperationID: "listThreads", Method: http.MethodGet, Path: "/threads", Tags: []string{"threads"},
		Summary: "List threads of a label (or all mail), newest first",
		Description: "Keyset pagination: the cost does not depend on the page number. Threads entirely in Trash or " +
			"Spam are left out of the all-mail view. q is a plain full-text query over subject, sender, " +
			"recipients, attachment names and downloaded body text.",
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound},
	}), func(ctx context.Context, in *listThreadsInput) (*listThreadsOutput, error) {
		userID := sessionFrom(ctx).UserID
		p := mailview.ListParams{UserID: userID, Query: strings.TrimSpace(in.Q), Cursor: in.Cursor, Limit: in.Limit}
		var err error
		switch {
		case in.LabelID == "":
			if p.AccountID, err = d.accountFrom(ctx, in.AccountID); err != nil {
				return nil, err
			}
		case strings.HasPrefix(in.LabelID, "l_"):
			id, ok := parseID("l_", in.LabelID)
			if !ok {
				return nil, errNotFound()
			}
			l, err := d.ops.GetLabel(ctx, userID, id)
			if err != nil {
				return nil, opsError(err)
			}
			if in.AccountID != "" {
				if acc, ok := parseID("acc_", in.AccountID); !ok || acc != l.AccountID {
					return nil, errNotFound()
				}
			}
			p.AccountID, p.LabelID = l.AccountID, l.ID
		default:
			if p.AccountID, err = d.accountFrom(ctx, in.AccountID); err != nil {
				return nil, err
			}
			if in.LabelID == ops.LabelUnread {
				return nil, problem(http.StatusBadRequest, CodeValidation, "UNREAD is a filter, not a list; use q or filter the label's threads")
			}
			l, err := d.ops.SystemLabel(ctx, userID, p.AccountID, in.LabelID)
			if err != nil {
				return nil, opsError(err)
			}
			p.LabelID = l.ID
		}
		page, err := d.view.ListThreads(ctx, p)
		if err != nil {
			return nil, viewError(err)
		}
		out := &listThreadsOutput{}
		out.Body.Threads = make([]Thread, 0, len(page.Threads))
		for _, t := range page.Threads {
			out.Body.Threads = append(out.Body.Threads, threadOf(t))
		}
		out.Body.NextCursor, out.Body.ResultSizeEstimate = page.NextCursor, page.ResultSizeEstimate
		return out, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "getThread", Method: http.MethodGet, Path: "/threads/{id}", Tags: []string{"threads"},
		Summary: "Get a thread with its messages",
		Description: "Messages in date order with the text of the body when it is downloaded. Opening a thread does not " +
			"mark it as read; the client sends modify removeLabelIds [UNREAD].",
		Errors: []int{http.StatusUnauthorized, http.StatusNotFound},
	}), func(ctx context.Context, in *threadIDInput) (*threadDetailOutput, error) {
		id, ok := parseID("t_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		t, err := d.view.GetThread(ctx, sessionFrom(ctx).UserID, id)
		if err != nil {
			return nil, viewError(err)
		}
		th := threadOf(t.Thread)
		out := &threadDetailOutput{Body: ThreadDetail{
			ID: th.ID, AccountID: th.AccountID, Subject: th.Subject, LabelIDs: th.LabelIDs,
			Messages: make([]Message, 0, len(t.Messages)),
		}}
		for _, m := range t.Messages {
			out.Body.Messages = append(out.Body.Messages, messageOf(m))
		}
		return out, nil
	})
}
