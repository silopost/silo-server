package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/mailview"
	"github.com/silopost/silo-server/internal/ops"
)

// ThreadList is the answer of GET /threads (plan 5.2).
type ThreadList struct {
	Threads            []Thread  `json:"threads"`
	NextCursor         *string   `json:"nextCursor" doc:"Opaque; pass it as cursor for the next page. null on the last page."`
	ResultSizeEstimate int       `json:"resultSizeEstimate" doc:"Threads in the view (exact for a label; for a search the number of matching threads, at most 2000)."`
	Approximate        bool      `json:"approximate,omitempty" doc:"Set (true) when a search hit its time limit (3 s): the page is empty and may be retried with a narrower query. Absent otherwise."`
	Indexing           *Indexing `json:"indexing,omitempty" doc:"Present with q only: how much of the account's mail has its body indexed; results may miss words of bodies not downloaded yet."`
}

type listThreadsInput struct {
	AccountID string `query:"accountId" doc:"Account; the account of labelId, or the active account, when omitted."`
	LabelID   string `query:"labelId" doc:"System key (INBOX, ...) or l_ id. Omitted: all mail except threads entirely in Trash or Spam."`
	Q         string `query:"q" maxLength:"500" doc:"Search query in the Gmail syntax (from:, to:, subject:, label:, in:, is:, has:, before:, after:, older_than:, newer_than:, larger:, smaller:, filename:, list:, cc:, bcc:, deliveredto:, rfc822msgid:, OR, -, ( ), { }, \"phrases\"); parsed on the server. Saved in the search history of the account (first page only)."`
	Cursor    string `query:"cursor"`
	Limit     int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
}

// Indexing is the progress of body indexing of a searched account.
type Indexing struct {
	Done float64 `json:"done" minimum:"0" maximum:"1" example:"0.62" doc:"Share of the account's messages whose body is downloaded and indexed; 1 when the backfill is over."`
}

type listThreadsOutput struct{ Body ThreadList }

// Address is a mailbox of a message.
type Address struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// MessageFolder is where a message is on the server. The schema is nullable:
// Message.folder is null for a message that is only local (a draft not yet
// in the Drafts folder).
type MessageFolder struct {
	_    struct{} `nullable:"true"`
	ID   string   `json:"id" example:"f_3"`
	Name string   `json:"name"`
	Role string   `json:"role"`
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
	ReplyTo         *Address            `json:"replyTo,omitempty" doc:"Reply-To of the message; absent without one."`
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
	DraftID         *string             `json:"draftId" example:"d_1x9" doc:"d_ id for GET/PUT /drafts/{id} when the message is a draft (DRAFT label; also one written in another program); null otherwise."`
}

// ThreadDetail is the answer of GET /threads/{id}.
type ThreadDetail struct {
	ID           string     `json:"id" example:"t_8f3k2"`
	AccountID    string     `json:"accountId"`
	Subject      string     `json:"subject"`
	LabelIDs     []string   `json:"labelIds"`
	SnoozedUntil *time.Time `json:"snoozedUntil" doc:"As in the thread list."`
	ScheduledAt  *time.Time `json:"scheduledAt" doc:"As in the thread list: when the queued send of the thread leaves; null otherwise."`
	OutboundID   *string    `json:"outboundId" doc:"As in the thread list: ob_ id of that send, for POST /outbound/{id}/cancel."`
	Messages     []Message  `json:"messages"`
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
	if slices.Contains(m.SystemLabels, "DRAFT") {
		s := formatID("d_", m.ID)
		out.DraftID = &s
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
			"Spam are left out of the all-mail view. With q: the threads with a message matching the query (plan 3.12; " +
			"messages in Spam and Trash only with in:spam, in:trash or in:anywhere), newest first, at most 2000; labelId " +
			"narrows them like in:. Words match prefixes of words in the subject, people, attachment names and the " +
			"downloaded body (see indexing); has:attachment works before bodies are downloaded. A search that runs over " +
			"3 s answers an empty page with approximate: true. 422 invalid_query: a closing bracket without its " +
			"opening one, or over 64 terms or 16 nested brackets (unclosed brackets and quotes are accepted).",
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusUnprocessableEntity},
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
		out.Body.Approximate = page.Approximate
		if page.IndexingDone != nil {
			out.Body.Indexing = &Indexing{Done: *page.IndexingDone}
		}
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
			SnoozedUntil: th.SnoozedUntil, ScheduledAt: th.ScheduledAt, OutboundID: th.OutboundID,
			Messages: make([]Message, 0, len(t.Messages)),
		}}
		for _, m := range t.Messages {
			out.Body.Messages = append(out.Body.Messages, messageOf(m))
		}
		return out, nil
	})
}
