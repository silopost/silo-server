package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/ops"
)

// Label of an account (plan 5.5). System labels have their key as id.
type Label struct {
	ID            string  `json:"id" example:"l_7" doc:"The key (INBOX, STARRED, ..., UNREAD) for system labels, l_ id otherwise."`
	AccountID     string  `json:"accountId" example:"acc_1"`
	Name          string  `json:"name"`
	Type          string  `json:"type" enum:"system,folder,user"`
	KeywordState  *string `json:"keywordState" enum:"mirrored,local_only" doc:"User labels only: mirrored when the label is an IMAP keyword on the server, local_only when the server keeps no keywords."`
	ColorBG       *string `json:"colorBg"`
	ColorText     *string `json:"colorText"`
	ShowInList    string  `json:"showInList" enum:"show,hide,show_if_unread"`
	ParentID      *string `json:"parentId"`
	FolderID      *string `json:"folderId" doc:"Folder labels: the f_ id of the server folder."`
	Position      int     `json:"position"`
	ThreadsTotal  int     `json:"threadsTotal"`
	ThreadsUnread int     `json:"threadsUnread"`
}

func labelOf(l ops.Label) Label {
	out := Label{
		ID: formatID("l_", l.ID), AccountID: formatID("acc_", l.AccountID), Name: l.Name, Type: l.Type,
		ColorBG: l.ColorBG, ColorText: l.ColorText, ShowInList: l.ShowInList, Position: l.Position,
		ThreadsTotal: l.ThreadsTotal, ThreadsUnread: l.ThreadsUnread,
	}
	if l.SystemKey != nil {
		out.ID = *l.SystemKey
	}
	if l.Type == "user" {
		out.KeywordState = &l.KeywordState
	}
	if l.ParentID != nil {
		p := formatID("l_", *l.ParentID)
		out.ParentID = &p
	}
	if l.FolderID != nil {
		f := formatID("f_", *l.FolderID)
		out.FolderID = &f
	}
	return out
}

type labelsInput struct {
	AccountID string `query:"accountId" doc:"Account; the active account when omitted."`
}

type labelsOutput struct {
	Body struct {
		Labels []Label `json:"labels"`
	}
}

// CreateLabelBody is the body of POST /labels.
type CreateLabelBody struct {
	AccountID string  `json:"accountId,omitempty" doc:"Account; the active account when omitted."`
	Name      string  `json:"name" minLength:"1" maxLength:"200"`
	Type      string  `json:"type" enum:"user,folder" doc:"user: a label (IMAP keyword); folder: a server folder (CREATE)."`
	ColorBG   *string `json:"colorBg,omitempty" maxLength:"32"`
	ColorText *string `json:"colorText,omitempty" maxLength:"32"`
	ParentID  *string `json:"parentId,omitempty"`
}

type createLabelInput struct {
	Client string `header:"X-Silo-Client"`
	Body   CreateLabelBody
}

type labelOutput struct{ Body Label }

// UpdateLabelBody is the body of PATCH /labels/{id}; absent fields stay.
type UpdateLabelBody struct {
	Name       *string `json:"name,omitempty" minLength:"1" maxLength:"200" doc:"User labels only."`
	ColorBG    *string `json:"colorBg,omitempty" maxLength:"32" doc:"Empty string clears the colour."`
	ColorText  *string `json:"colorText,omitempty" maxLength:"32"`
	ShowInList *string `json:"showInList,omitempty" enum:"show,hide,show_if_unread"`
}

type labelIDInput struct {
	Client    string `header:"X-Silo-Client"`
	ID        string `path:"id"`
	AccountID string `query:"accountId" doc:"Needed for system label keys; the active account when omitted."`
}

type updateLabelInput struct {
	Client    string `header:"X-Silo-Client"`
	ID        string `path:"id"`
	AccountID string `query:"accountId" doc:"Needed for system label keys; the active account when omitted."`
	Body      UpdateLabelBody
}

// accountFrom resolves an optional acc_ id to a database id of the user.
func (d *deps) accountFrom(ctx context.Context, s string) (int64, error) {
	var id int64
	if s != "" {
		var ok bool
		if id, ok = parseID("acc_", s); !ok {
			return 0, errNotFound()
		}
	}
	id, err := d.view.ResolveAccount(ctx, sessionFrom(ctx).UserID, id)
	return id, viewError(err)
}

// labelByID resolves the id of a label in a path: l_ ids directly, system keys
// in the (given or active) account.
func (d *deps) labelByID(ctx context.Context, idStr, accountStr string) (int64, error) {
	if strings.HasPrefix(idStr, "l_") {
		id, ok := parseID("l_", idStr)
		if !ok {
			return 0, errNotFound()
		}
		return id, nil
	}
	acc, err := d.accountFrom(ctx, accountStr)
	if err != nil {
		return 0, err
	}
	l, err := d.ops.SystemLabel(ctx, sessionFrom(ctx).UserID, acc, idStr)
	if err != nil {
		return 0, opsError(err)
	}
	return l.ID, nil
}

func (d *deps) registerLabels(a huma.API) {
	huma.Register(a, d.secured(huma.Operation{
		OperationID: "listLabels", Method: http.MethodGet, Path: "/labels", Tags: []string{"labels"},
		Summary: "List the labels of an account with thread counters",
		Description: "System labels (plus the pseudo label UNREAD), folder labels and user labels, with " +
			"threadsTotal and threadsUnread (threads with unread mail).",
		Errors: []int{http.StatusUnauthorized, http.StatusNotFound},
	}), func(ctx context.Context, in *labelsInput) (*labelsOutput, error) {
		acc, err := d.accountFrom(ctx, in.AccountID)
		if err != nil {
			return nil, err
		}
		userID := sessionFrom(ctx).UserID
		ls, err := d.ops.ListLabels(ctx, userID, acc)
		if err != nil {
			return nil, errInternal(err)
		}
		unread, err := d.view.UnreadThreads(ctx, userID, acc)
		if err != nil {
			return nil, errInternal(err)
		}
		out := &labelsOutput{}
		out.Body.Labels = make([]Label, 0, len(ls)+1)
		for _, l := range ls {
			out.Body.Labels = append(out.Body.Labels, labelOf(l))
			if l.SystemKey != nil && *l.SystemKey == "STARRED" {
				// UNREAD is a pseudo label: it has no thread_labels rows of its own.
				out.Body.Labels = append(out.Body.Labels, Label{
					ID: ops.LabelUnread, AccountID: formatID("acc_", acc), Name: ops.LabelUnread, Type: "system",
					ShowInList: "show", ThreadsTotal: unread, ThreadsUnread: unread,
				})
			}
		}
		return out, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "createLabel", Method: http.MethodPost, Path: "/labels", Tags: []string{"labels"},
		Summary: "Create a label or a folder",
		Description: "type user creates a label locally and assigns its IMAP keyword; type folder creates the folder " +
			"on the mail server (CREATE, queued) and its folder label at once.",
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict},
	}), func(ctx context.Context, in *createLabelInput) (*labelOutput, error) {
		acc, err := d.accountFrom(ctx, in.Body.AccountID)
		if err != nil {
			return nil, err
		}
		c := ops.CreateLabel{UserID: sessionFrom(ctx).UserID, AccountID: acc, Name: in.Body.Name, Type: in.Body.Type,
			ColorBG: in.Body.ColorBG, ColorText: in.Body.ColorText, Origin: in.Client}
		if in.Body.ParentID != nil {
			p, ok := parseID("l_", *in.Body.ParentID)
			if !ok {
				return nil, problem(http.StatusBadRequest, CodeValidation, "invalid parentId")
			}
			c.ParentID = &p
		}
		l, err := d.ops.CreateLabel(ctx, c)
		if err != nil {
			return nil, opsError(err)
		}
		return &labelOutput{Body: labelOf(*l)}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "updateLabel", Method: http.MethodPatch, Path: "/labels/{id}", Tags: []string{"labels"},
		Summary: "Rename, recolour or hide a label",
		Description: "Renaming a user label gives it a new IMAP keyword and moves its messages to it on the server " +
			"(queued). System labels and folders cannot be renamed (400).",
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict},
	}), func(ctx context.Context, in *updateLabelInput) (*labelOutput, error) {
		id, err := d.labelByID(ctx, in.ID, in.AccountID)
		if err != nil {
			return nil, err
		}
		l, err := d.ops.UpdateLabel(ctx, ops.UpdateLabel{UserID: sessionFrom(ctx).UserID, LabelID: id, Name: in.Body.Name,
			ColorBG: in.Body.ColorBG, ColorText: in.Body.ColorText, ShowInList: in.Body.ShowInList, Origin: in.Client})
		if err != nil {
			return nil, opsError(err)
		}
		return &labelOutput{Body: labelOf(*l)}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "deleteLabel", Method: http.MethodDelete, Path: "/labels/{id}", Tags: []string{"labels"},
		Summary: "Delete a label or an empty folder",
		Description: "A user label is removed and its keyword taken off the messages (queued). A folder must be empty; " +
			"it is deleted on the mail server (queued) and disappears from the labels after the next discovery. " +
			"System labels answer 400.",
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict},
	}), func(ctx context.Context, in *labelIDInput) (*struct{}, error) {
		if !strings.HasPrefix(in.ID, "l_") {
			return nil, problem(http.StatusBadRequest, CodeValidation, "system labels cannot be deleted")
		}
		id, err := d.labelByID(ctx, in.ID, in.AccountID)
		if err != nil {
			return nil, err
		}
		return nil, opsError(d.ops.DeleteLabel(ctx, sessionFrom(ctx).UserID, id, in.Client))
	})
}
