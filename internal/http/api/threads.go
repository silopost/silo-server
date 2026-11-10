package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/ops"
)

// Participant of a thread.
type Participant struct {
	Email  string `json:"email"`
	Name   string `json:"name"`
	Unread bool   `json:"unread"`
}

// Thread is the list shape of a thread (plan 5.2).
type Thread struct {
	ID             string        `json:"id" example:"t_8f3k2"`
	AccountID      string        `json:"accountId" example:"acc_1"`
	Subject        string        `json:"subject"`
	Snippet        string        `json:"snippet"`
	LastMessageAt  time.Time     `json:"lastMessageAt"`
	MessageCount   int           `json:"messageCount"`
	UnreadCount    int           `json:"unreadCount"`
	HasAttachments bool          `json:"hasAttachments"`
	LabelIDs       []string      `json:"labelIds" doc:"System labels (INBOX, STARRED, ..., UNREAD) and l_ ids of user/folder labels."`
	Participants   []Participant `json:"participants"`
	SnoozedUntil   *time.Time    `json:"snoozedUntil" doc:"Wake time of a snoozed thread (POST /threads/{id}/snooze); null when not snoozed."`
	PendingSync    bool          `json:"pendingSync" doc:"Local changes still waiting for the mail server."`
	DraftID        *string       `json:"draftId" example:"d_4k2" doc:"d_ id of the newest draft in the thread (a message with the DRAFT label), for opening it in compose; null without one."`
	DraftCount     int           `json:"draftCount" minimum:"0" doc:"Number of drafts in the thread (messages with the DRAFT label; a queued or sending message is SCHEDULED, not a draft); 0 without one. The list shows it as Draft / Drafts (n)."`
	ScheduledAt    *time.Time    `json:"scheduledAt" doc:"When the queued send of the thread leaves (a scheduled send, or the undo-send window): outbound send_at while the send is queued or sending and still ahead; null otherwise. The Scheduled list is GET /threads?labelId=SCHEDULED."`
	OutboundID     *string       `json:"outboundId" example:"ob_3k" doc:"ob_ id of that send, for POST /outbound/{id}/cancel; null when scheduledAt is null."`
}

// threadOf maps a thread of the ops/mail layer to the API shape; GET /threads
// reuses it.
func threadOf(t ops.Thread) Thread {
	out := Thread{
		ID: formatID("t_", t.ID), AccountID: formatID("acc_", t.AccountID), Subject: t.Subject, Snippet: t.Snippet,
		LastMessageAt: t.LastMessageAt, MessageCount: t.MessageCount, UnreadCount: t.UnreadCount,
		HasAttachments: t.HasAttachments, SnoozedUntil: t.SnoozedUntil, PendingSync: t.PendingSync, DraftCount: t.DraftCount,
		LabelIDs: append([]string{}, t.SystemLabels...), Participants: make([]Participant, 0, len(t.Participants)),
	}
	for _, id := range t.LabelIDs {
		out.LabelIDs = append(out.LabelIDs, formatID("l_", id))
	}
	for _, p := range t.Participants {
		out.Participants = append(out.Participants, Participant(p))
	}
	if t.DraftID != 0 {
		s := formatID("d_", t.DraftID)
		out.DraftID = &s
	}
	if t.OutboundID != 0 && t.ScheduledAt != nil {
		s := formatID("ob_", t.OutboundID)
		out.ScheduledAt, out.OutboundID = t.ScheduledAt, &s
	}
	return out
}

// ModifyBody is the body of POST /threads/modify and /messages/modify.
type ModifyBody struct {
	IDs            []string `json:"ids" minItems:"1" maxItems:"1000" doc:"t_ ids (threads/modify) or m_ ids (messages/modify)."`
	AddLabelIDs    []string `json:"addLabelIds,omitempty" maxItems:"50"`
	RemoveLabelIDs []string `json:"removeLabelIds,omitempty" maxItems:"50"`
}

// IDsBody is the body of POST /threads/trash, /untrash and /delete.
type IDsBody struct {
	IDs []string `json:"ids" minItems:"1" maxItems:"1000"`
}

// OperationResult answers every modify-style request.
type OperationResult struct {
	Threads     []Thread   `json:"threads"`
	OperationID *string    `json:"operationId" doc:"Id of the operation (undo); null when nothing had to be sent to the server."`
	UndoUntil   *time.Time `json:"undoUntil" doc:"End of the undo window of folder operations; null for flag-only changes."`
}

type modifyInput struct {
	Client string `header:"X-Silo-Client" doc:"Client id, echoed as origin in the events it causes."`
	Body   ModifyBody
}

type idsInput struct {
	Client string `header:"X-Silo-Client"`
	Body   IDsBody
}

type operationOutput struct{ Body OperationResult }

func parseIDs(prefix string, in []string) ([]int64, error) {
	out := make([]int64, 0, len(in))
	for _, s := range in {
		id, ok := parseID(prefix, s)
		if !ok {
			return nil, problem(http.StatusBadRequest, CodeValidation, "invalid id "+s)
		}
		out = append(out, id)
	}
	return out, nil
}

func parseLabels(in []string) ([]ops.LabelRef, error) {
	out := make([]ops.LabelRef, 0, len(in))
	for _, s := range in {
		if strings.HasPrefix(s, "l_") {
			id, ok := parseID("l_", s)
			if !ok {
				return nil, problem(http.StatusBadRequest, CodeValidation, "invalid label id "+s)
			}
			out = append(out, ops.LabelRef{ID: id})
			continue
		}
		out = append(out, ops.LabelRef{System: s})
	}
	return out, nil
}

func opsError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ops.ErrNotFound):
		return errNotFound()
	case errors.Is(err, ops.ErrExists):
		return problem(http.StatusConflict, CodeLabelExists, "A label or folder with this name already exists.")
	case errors.Is(err, ops.ErrNotEmpty):
		return problem(http.StatusConflict, CodeFolderNotEmpty, "Only an empty folder can be deleted.")
	case errors.Is(err, ops.ErrNotModifiable), errors.Is(err, ops.ErrInvalid):
		return problem(http.StatusBadRequest, CodeValidation, strings.TrimPrefix(strings.TrimPrefix(err.Error(), "ops: invalid request: "), "ops: "))
	}
	return errInternal(err)
}

func operationOf(r *ops.Result) *operationOutput {
	out := &operationOutput{}
	out.Body.Threads = make([]Thread, 0, len(r.Threads))
	for _, t := range r.Threads {
		out.Body.Threads = append(out.Body.Threads, threadOf(t))
	}
	if r.OperationID != "" {
		id := r.OperationID
		out.Body.OperationID = &id
	}
	out.Body.UndoUntil = r.UndoUntil
	return out
}

func (d *deps) registerThreads(a huma.API) {
	errs := []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound}
	modifyDoc := "Changes labels in the Gmail model: the change is visible at once (pendingSync: true) and written " +
		"back to the mail server by a queued operation. Folder changes (INBOX, TRASH, SPAM, folder labels) wait for " +
		"the undo window; flags (UNREAD, STARRED, IMPORTANT, user labels as keywords) are sent at once. " +
		"SENT, DRAFT, SNOOZED, SCHEDULED and ARCHIVE cannot be changed directly (400)."

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "modifyThreads", Method: http.MethodPost, Path: "/threads/modify", Tags: []string{"threads"},
		Summary: "Add or remove labels on threads", Description: modifyDoc, Errors: errs,
	}), func(ctx context.Context, in *modifyInput) (*operationOutput, error) {
		ids, err := parseIDs("t_", in.Body.IDs)
		if err != nil {
			return nil, err
		}
		add, err := parseLabels(in.Body.AddLabelIDs)
		if err != nil {
			return nil, err
		}
		rem, err := parseLabels(in.Body.RemoveLabelIDs)
		if err != nil {
			return nil, err
		}
		r, err := d.ops.ModifyLabels(ctx, ops.Modify{UserID: sessionFrom(ctx).UserID, ThreadIDs: ids, Add: add, Remove: rem, Origin: in.Client})
		if err != nil {
			return nil, opsError(err)
		}
		return operationOf(r), nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "modifyMessages", Method: http.MethodPost, Path: "/messages/modify", Tags: []string{"threads"},
		Summary: "Add or remove labels on single messages", Description: modifyDoc, Errors: errs,
	}), func(ctx context.Context, in *modifyInput) (*operationOutput, error) {
		ids, err := parseIDs("m_", in.Body.IDs)
		if err != nil {
			return nil, err
		}
		add, err := parseLabels(in.Body.AddLabelIDs)
		if err != nil {
			return nil, err
		}
		rem, err := parseLabels(in.Body.RemoveLabelIDs)
		if err != nil {
			return nil, err
		}
		r, err := d.ops.ModifyLabels(ctx, ops.Modify{UserID: sessionFrom(ctx).UserID, MessageIDs: ids, Add: add, Remove: rem, Origin: in.Client})
		if err != nil {
			return nil, opsError(err)
		}
		return operationOf(r), nil
	})

	batch := func(id, path, summary, doc string, fn func(ctx context.Context, userID int64, ids []int64, origin string) (*ops.Result, error)) {
		huma.Register(a, d.secured(huma.Operation{
			OperationID: id, Method: http.MethodPost, Path: path, Tags: []string{"threads"},
			Summary: summary, Description: doc, Errors: errs,
		}), func(ctx context.Context, in *idsInput) (*operationOutput, error) {
			ids, err := parseIDs("t_", in.Body.IDs)
			if err != nil {
				return nil, err
			}
			r, err := fn(ctx, sessionFrom(ctx).UserID, ids, in.Client)
			if err != nil {
				return nil, opsError(err)
			}
			return operationOf(r), nil
		})
	}
	batch("trashThreads", "/threads/trash", "Move threads to the trash", "Same as modify with addLabelIds [TRASH]; the folder each message came from is kept for untrash.",
		func(ctx context.Context, u int64, ids []int64, o string) (*ops.Result, error) {
			return d.ops.Trash(ctx, u, ids, o)
		})
	batch("untrashThreads", "/threads/untrash", "Restore threads from the trash", "Same as modify with removeLabelIds [TRASH]: messages go back to their folder (INBOX when unknown).",
		func(ctx context.Context, u int64, ids []int64, o string) (*ops.Result, error) {
			return d.ops.Untrash(ctx, u, ids, o)
		})
	batch("deleteThreads", "/threads/delete", "Delete threads for good",
		"Only the messages in Trash or Spam are deleted (\\Deleted + UID EXPUNGE on the server); threads without such messages answer 400.",
		func(ctx context.Context, u int64, ids []int64, o string) (*ops.Result, error) {
			return d.ops.Delete(ctx, u, ids, o)
		})
}
