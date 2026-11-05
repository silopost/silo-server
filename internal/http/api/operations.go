package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/ops"
)

// CodeUndoExpired: the operation was already sent to the mail server (plan 5.3).
const CodeUndoExpired = "undo_expired"

type undoInput struct {
	ID     string `path:"id" doc:"operationId returned by a modify, trash or untrash request."`
	Client string `header:"X-Silo-Client" doc:"Client id, echoed as origin in the events it causes."`
}

func (d *deps) registerOperations(a huma.API) {
	huma.Register(a, d.secured(huma.Operation{
		OperationID: "undoOperation", Method: http.MethodPost, Path: "/operations/{id}/undo", Tags: []string{"threads"},
		Summary: "Undo an operation inside its undo window",
		Description: "Cancels the operation while its jobs are still waiting (until undoUntil): they are deleted and the " +
			"threads return to the state of the mail server in the same transaction, so the server never sees the " +
			"operation; the answer is the threads after the undo. Once the operation is being sent to (or was " +
			"executed on) the server the answer is 409 undo_expired: the client then issues the inverse operation " +
			"itself (e.g. modify with addLabelIds [INBOX] after an archive). Flag changes (UNREAD, STARRED, labels) are " +
			"sent at once, so they are undone the same way. An unknown operation, or one of another user, is 404.",
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict},
	}), func(ctx context.Context, in *undoInput) (*operationOutput, error) {
		if len(in.ID) != 36 || strings.Count(in.ID, "-") != 4 {
			return nil, errNotFound()
		}
		r, err := d.ops.Undo(ctx, sessionFrom(ctx).UserID, in.ID, in.Client)
		if errors.Is(err, ops.ErrTooLate) {
			return nil, problem(http.StatusConflict, CodeUndoExpired, "The operation was already sent to the mail server; apply the inverse operation instead.")
		}
		if err != nil {
			return nil, opsError(err)
		}
		return operationOf(r), nil
	})
}
