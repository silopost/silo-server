package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// SearchSuggestion is one suggestion of the search box.
type SearchSuggestion struct {
	Kind  string `json:"kind" enum:"history,contact,label,operator" doc:"history: a recent query; contact: from:/to:/cc:/bcc: an address; label: label:/in: a user or folder label; operator: an operator."`
	Text  string `json:"text" example:"from:jan@firma.pl " doc:"The whole query after accepting the suggestion (the last token replaced); put it in the search box as is."`
	Label string `json:"label" example:"Jan Kowalski <jan@firma.pl>" doc:"What to show in the list."`
}

// SearchSuggestions is the answer of GET /search/suggest.
type SearchSuggestions struct {
	Suggestions []SearchSuggestion `json:"suggestions"`
}

type suggestInput struct {
	AccountID string `query:"accountId" doc:"Account; the active account when omitted."`
	Q         string `query:"q" maxLength:"500" doc:"The query as typed so far (may be empty)."`
}

type suggestOutput struct{ Body SearchSuggestions }

type deleteHistoryInput struct {
	AccountID string `query:"accountId" doc:"Account; the active account when omitted."`
	Query     string `query:"query" maxLength:"500" doc:"The query to forget (exact text, as in a history suggestion); omitted: the whole history of the account."`
}

func (d *deps) registerSearch(a huma.API) {
	huma.Register(a, d.secured(huma.Operation{
		OperationID: "suggestSearch", Method: http.MethodGet, Path: "/search/suggest", Tags: []string{"search"},
		Summary: "Suggest completions for the search box",
		Description: "Everything is computed on the server (plan 2.6): up to 5 recent queries of the account starting " +
			"with q (the 5 most recent for an empty q), then for the last token of q up to 5 contacts (from:<email>, " +
			"or the key typed: to:, cc:, bcc:), up to 5 user or folder labels (label:<name>, or in:<name>) and up to 5 " +
			"operators the token is a prefix of (is:u -> is:unread). A query is saved in the history when it is " +
			"searched (GET /threads?q=, first page); the 50 most recent are kept per account.",
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound},
	}), func(ctx context.Context, in *suggestInput) (*suggestOutput, error) {
		accID, err := d.accountFrom(ctx, in.AccountID)
		if err != nil {
			return nil, err
		}
		list, err := d.view.Suggest(ctx, sessionFrom(ctx).UserID, accID, in.Q)
		if err != nil {
			return nil, viewError(err)
		}
		out := &suggestOutput{Body: SearchSuggestions{Suggestions: make([]SearchSuggestion, 0, len(list))}}
		for _, s := range list {
			out.Body.Suggestions = append(out.Body.Suggestions, SearchSuggestion(s))
		}
		return out, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "deleteSearchHistory", Method: http.MethodDelete, Path: "/search/history", Tags: []string{"search"},
		Summary:       "Forget searches",
		Description:   "Removes one query (query=) or the whole search history of the account. Idempotent: 204 also when nothing matched.",
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound},
	}), func(ctx context.Context, in *deleteHistoryInput) (*struct{}, error) {
		accID, err := d.accountFrom(ctx, in.AccountID)
		if err != nil {
			return nil, err
		}
		if _, err := d.view.DeleteSearchHistory(ctx, sessionFrom(ctx).UserID, accID, in.Query); err != nil {
			return nil, viewError(err)
		}
		return nil, nil
	})
}
