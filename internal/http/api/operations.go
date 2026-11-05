package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/ops"
)

// Error codes of undo (plan 5.3).
const (
	// CodeUndoExpired: the operation is older than the undo retention (24 h).
	CodeUndoExpired = "undo_expired"
	// CodeNotUndoable: the operation cannot be undone (permanent delete).
	CodeNotUndoable = "operation_not_undoable"
)

type undoInput struct {
	ID     string `path:"id" doc:"operationId returned by a modify, trash, untrash, delete or undo request."`
	Client string `header:"X-Silo-Client" doc:"Client id, echoed as origin in the events it causes."`
}

func (d *deps) registerOperations(a huma.API) {
	huma.Register(a, d.secured(huma.Operation{
		OperationID: "undoOperation", Method: http.MethodPost, Path: "/operations/{id}/undo", Tags: []string{"threads"},
		Summary: "Undo an operation",
		Description: "Until undoUntil (while its jobs still wait) the operation is cancelled: the jobs are deleted and " +
			"the threads return to the state of the mail server in the same transaction, so the server never sees " +
			"the operation; operationId and undoUntil of the answer are null. Later (the operation is being sent or " +
			"was executed, e.g. flag changes, which are sent at once) the server applies the inverse itself: messages " +
			"go back to the folders they came from, flags and user labels that the operation changed are changed back. " +
			"That is a new operation, with its own operationId and undoUntil, which can be undone in turn. Either way " +
			"the answer is the threads after the undo and the usual thread.updated events (with origin) are written. " +
			"An operation can be undone once, for 24 hours; later 409 undo_expired. A permanent delete " +
			"(/threads/delete) can only be undone inside its window, later 409 operation_not_undoable. An unknown " +
			"operation, or one of another user, is 404.",
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict},
	}), func(ctx context.Context, in *undoInput) (*operationOutput, error) {
		if len(in.ID) != 36 || strings.Count(in.ID, "-") != 4 {
			return nil, errNotFound()
		}
		r, err := d.ops.Undo(ctx, sessionFrom(ctx).UserID, in.ID, in.Client)
		switch {
		case errors.Is(err, ops.ErrTooLate):
			return nil, problem(http.StatusConflict, CodeUndoExpired, "The operation is too old to be undone.")
		case errors.Is(err, ops.ErrNotUndoable):
			return nil, problem(http.StatusConflict, CodeNotUndoable, "Messages deleted for good on the mail server cannot be restored.")
		case err != nil:
			return nil, opsError(err)
		}
		return operationOf(r), nil
	})
}
