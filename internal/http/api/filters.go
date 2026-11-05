package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/filters"
	"github.com/silopost/silo-server/internal/search"
)

// Error codes of filters.
const (
	CodeInvalidFilter     = "invalid_filter"
	CodeInvalidLabel      = "invalid_label"
	CodeUnsupportedAction = "unsupported_action"
)

// FilterActions are what a filter does with a matching message (plan 3.11).
type FilterActions struct {
	Archive     bool     `json:"archive" doc:"Skip the inbox: -INBOX, a move to Archive on the server."`
	MarkRead    bool     `json:"markRead" doc:"Mark as read (\\Seen)."`
	Star        bool     `json:"star" doc:"Star (\\Flagged)."`
	Important   bool     `json:"important" doc:"Mark as important (IMPORTANT, $Important)."`
	AddLabelIDs []string `json:"addLabelIds" doc:"l_ ids of user labels of the filter's account (IMAP keywords on the server)."`
	Trash       bool     `json:"trash" doc:"Move to Trash; cannot be combined with archive."`
	ForwardTo   *string  `json:"forwardTo" format:"email" doc:"Send a copy to this address at once (no undo-send window); new mail only, never the account's own mail."`
}

// FilterActionsInput is FilterActions in a request; absent fields are off.
type FilterActionsInput struct {
	Archive     bool     `json:"archive,omitempty"`
	MarkRead    bool     `json:"markRead,omitempty"`
	Star        bool     `json:"star,omitempty"`
	Important   bool     `json:"important,omitempty"`
	AddLabelIDs []string `json:"addLabelIds,omitempty" maxItems:"20"`
	Trash       bool     `json:"trash,omitempty"`
	ForwardTo   *string  `json:"forwardTo,omitempty" maxLength:"254" doc:"null or absent: no forward."`
}

// Filter is a stored filter (plan 3.11).
type Filter struct {
	ID                 string        `json:"id" example:"f_7"`
	AccountID          string        `json:"accountId" example:"acc_1"`
	Name               string        `json:"name" doc:"Optional name given by the user (may be empty)."`
	Criteria           string        `json:"criteria" example:"from:newsletter@x.com has:attachment" doc:"Query in the search syntax, as written."`
	CriteriaNormalized string        `json:"criteriaNormalized" doc:"The criteria as the server parsed them (AST.String())."`
	Actions            FilterActions `json:"actions"`
	Enabled            bool          `json:"enabled"`
	Position           int           `json:"position" doc:"0-based order of evaluation within the account."`
	CreatedAt          time.Time     `json:"createdAt"`
	UpdatedAt          time.Time     `json:"updatedAt"`
}

// FilterBody is the body of POST /filters and PUT /filters/{id} (a full
// replacement).
type FilterBody struct {
	AccountID string             `json:"accountId,omitempty" doc:"POST: the account (the active one when omitted). PUT: ignored, a filter keeps its account."`
	Name      string             `json:"name,omitempty" maxLength:"100"`
	Criteria  string             `json:"criteria" minLength:"1" maxLength:"1000" doc:"Query in the search syntax; 422 invalid_query as in search, 422 invalid_filter when it matches everything (only in:anywhere)."`
	Actions   FilterActionsInput `json:"actions" doc:"At least one action."`
	Enabled   *bool              `json:"enabled,omitempty" doc:"Default true."`
	Position  *int               `json:"position,omitempty" minimum:"0" doc:"0-based place in the account's list (the others shift); POST: the end when omitted, PUT: unchanged when omitted. This is how filters are reordered."`
}

// FilterApplyResult is the answer of POST /filters/{id}/apply.
type FilterApplyResult struct {
	JobID           string `json:"jobId" example:"j_k2" doc:"The background job; filter.applied is sent when it is done."`
	MatchedEstimate *int   `json:"matchedEstimate" doc:"Conversations the criteria match now; null when counting took too long."`
}

type filtersInput struct {
	AccountID string `query:"accountId" doc:"Account; the active account when omitted."`
}

type filtersOutput struct {
	Body struct {
		Filters []Filter `json:"filters"`
	}
}

type filterOutput struct{ Body Filter }

type filterIDInput struct {
	ID     string `path:"id" doc:"f_ id of the filter."`
	Client string `header:"X-Silo-Client" doc:"Client id, echoed as origin in the events it causes."`
}

type createFilterInput struct {
	Body FilterBody
}

type updateFilterInput struct {
	ID   string `path:"id" doc:"f_ id of the filter."`
	Body FilterBody
}

type applyFilterOutput struct{ Body FilterApplyResult }

func filterOf(f *filters.Filter) Filter {
	out := Filter{
		ID: formatID("f_", f.ID), AccountID: formatID("acc_", f.AccountID), Name: f.Name, Criteria: f.Criteria,
		CriteriaNormalized: f.Normalized(), Enabled: f.Enabled, Position: f.Position, CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt,
		Actions: FilterActions{Archive: f.Actions.Archive, MarkRead: f.Actions.MarkRead, Star: f.Actions.Star,
			Important: f.Actions.Important, Trash: f.Actions.Trash, AddLabelIDs: make([]string, 0, len(f.Actions.AddLabelIDs))},
	}
	for _, id := range f.Actions.AddLabelIDs {
		out.Actions.AddLabelIDs = append(out.Actions.AddLabelIDs, formatID("l_", id))
	}
	if f.Actions.ForwardTo != "" {
		to := f.Actions.ForwardTo
		out.Actions.ForwardTo = &to
	}
	return out
}

func filterInputOf(b FilterBody) (filters.Input, error) {
	in := filters.Input{Name: b.Name, Criteria: b.Criteria, Enabled: true, Position: b.Position,
		Actions: filters.Actions{Archive: b.Actions.Archive, MarkRead: b.Actions.MarkRead, Star: b.Actions.Star,
			Important: b.Actions.Important, Trash: b.Actions.Trash}}
	if b.Enabled != nil {
		in.Enabled = *b.Enabled
	}
	if b.Actions.ForwardTo != nil {
		in.Actions.ForwardTo = strings.TrimSpace(*b.Actions.ForwardTo)
	}
	for _, s := range b.Actions.AddLabelIDs {
		id, ok := parseID("l_", s)
		if !ok {
			return in, problem(http.StatusUnprocessableEntity, CodeInvalidLabel, "addLabelIds must name user labels (l_ ids) of the account.")
		}
		in.Actions.AddLabelIDs = append(in.Actions.AddLabelIDs, id)
	}
	return in, nil
}

func filterError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, filters.ErrNotFound):
		return errNotFound()
	case errors.Is(err, search.ErrInvalidQuery):
		return problem(http.StatusUnprocessableEntity, CodeInvalidQuery, strings.TrimPrefix(err.Error(), "search: "))
	case errors.Is(err, filters.ErrInvalidLabel):
		return problem(http.StatusUnprocessableEntity, CodeInvalidLabel, "addLabelIds must name user labels of the filter's account.")
	case errors.Is(err, filters.ErrUnsupported):
		return problem(http.StatusUnprocessableEntity, CodeUnsupportedAction, "forwardTo is not supported by this server.")
	case errors.Is(err, filters.ErrInvalid):
		return problem(http.StatusUnprocessableEntity, CodeInvalidFilter, strings.TrimPrefix(err.Error(), "filters: invalid filter: "))
	}
	return errInternal(err)
}

func (d *deps) registerFilters(a huma.API) {
	errs := []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity}

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "listFilters", Method: http.MethodGet, Path: "/filters", Tags: []string{"filters"},
		Summary:     "List the filters of an account",
		Description: "In the order they are evaluated (position).",
		Errors:      []int{http.StatusUnauthorized, http.StatusNotFound},
	}), func(ctx context.Context, in *filtersInput) (*filtersOutput, error) {
		acc, err := d.accountFrom(ctx, in.AccountID)
		if err != nil {
			return nil, err
		}
		list, err := d.filters.List(ctx, sessionFrom(ctx).UserID, acc)
		if err != nil {
			return nil, errInternal(err)
		}
		out := &filtersOutput{}
		out.Body.Filters = make([]Filter, 0, len(list))
		for i := range list {
			out.Body.Filters = append(out.Body.Filters, filterOf(&list[i]))
		}
		return out, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "getFilter", Method: http.MethodGet, Path: "/filters/{id}", Tags: []string{"filters"},
		Summary: "Get a filter", Errors: []int{http.StatusUnauthorized, http.StatusNotFound},
	}), func(ctx context.Context, in *filterIDInput) (*filterOutput, error) {
		id, ok := parseID("f_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		f, err := d.filters.Get(ctx, sessionFrom(ctx).UserID, id)
		if err != nil {
			return nil, filterError(err)
		}
		return &filterOutput{Body: filterOf(f)}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "createFilter", Method: http.MethodPost, Path: "/filters", Tags: []string{"filters"},
		Summary: "Create a filter",
		Description: "Criteria are a query of the search syntax, evaluated on the server only (plan 2.6, 3.11) on new " +
			"mail arriving in INBOX after the account went live; POST /filters/{id}/apply runs it on existing mail. " +
			"Actions run as write-back operations without the undo window. 422: invalid_query (criteria), " +
			"invalid_filter (no action, archive with trash, criteria matching everything, bad forwardTo), " +
			"invalid_label (addLabelIds not user labels of the account).",
		DefaultStatus: http.StatusCreated,
		Errors:        errs,
	}), func(ctx context.Context, in *createFilterInput) (*filterOutput, error) {
		acc, err := d.accountFrom(ctx, in.Body.AccountID)
		if err != nil {
			return nil, err
		}
		fi, err := filterInputOf(in.Body)
		if err != nil {
			return nil, err
		}
		fi.AccountID = acc
		f, err := d.filters.Create(ctx, sessionFrom(ctx).UserID, fi)
		if err != nil {
			return nil, filterError(err)
		}
		return &filterOutput{Body: filterOf(f)}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "updateFilter", Method: http.MethodPut, Path: "/filters/{id}", Tags: []string{"filters"},
		Summary:     "Replace a filter",
		Description: "A full replacement (absent actions are off); position moves the filter within its account. Errors as in POST.",
		Errors:      errs,
	}), func(ctx context.Context, in *updateFilterInput) (*filterOutput, error) {
		id, ok := parseID("f_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		fi, err := filterInputOf(in.Body)
		if err != nil {
			return nil, err
		}
		f, err := d.filters.Update(ctx, sessionFrom(ctx).UserID, id, fi)
		if err != nil {
			return nil, filterError(err)
		}
		return &filterOutput{Body: filterOf(f)}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "deleteFilter", Method: http.MethodDelete, Path: "/filters/{id}", Tags: []string{"filters"},
		Summary: "Delete a filter", DefaultStatus: http.StatusNoContent,
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound},
	}), func(ctx context.Context, in *filterIDInput) (*struct{}, error) {
		id, ok := parseID("f_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		return nil, filterError(d.filters.Delete(ctx, sessionFrom(ctx).UserID, id))
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "applyFilter", Method: http.MethodPost, Path: "/filters/{id}/apply", Tags: []string{"filters"},
		Summary: "Apply a filter to existing mail",
		Description: "Queues the job filter_apply: the messages of the account matching the criteria now (the same AST as " +
			"search, as SQL; Spam and Trash left out unless the criteria name in:spam, in:trash or in:anywhere; drafts " +
			"left out) get the actions in batches of 500, as operations without undo (thread.updated and label.counts " +
			"as for modify). forwardTo is not applied to existing mail. When done, the event filter.applied " +
			"{filterId, accountId, messages, threads} is sent. A disabled filter can be applied too.",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity},
	}), func(ctx context.Context, in *filterIDInput) (*applyFilterOutput, error) {
		id, ok := parseID("f_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		r, err := d.filters.Apply(ctx, sessionFrom(ctx).UserID, id, in.Client)
		if err != nil {
			return nil, filterError(err)
		}
		return &applyFilterOutput{Body: FilterApplyResult{JobID: formatID("j_", r.JobID), MatchedEstimate: r.MatchedEstimate}}, nil
	})
}
