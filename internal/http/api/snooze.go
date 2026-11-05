package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/ops"
)

// SnoozeBody is the body of POST /threads/{id}/snooze.
type SnoozeBody struct {
	Until time.Time `json:"until" doc:"Wake time (RFC 3339), typically the until of a preset from GET /snooze/presets; at least 30 seconds and at most a year ahead, else 422 invalid_snooze."`
}

// SnoozeResult answers snooze and unsnooze.
type SnoozeResult struct {
	Thread      Thread     `json:"thread" doc:"The thread after the change (list shape): SNOOZED instead of INBOX and snoozedUntil set, or back in INBOX."`
	OperationID *string    `json:"operationId" doc:"Id of the operation for POST /operations/{id}/undo; null for unsnooze, which is not undoable (snooze again instead)."`
	UndoUntil   *time.Time `json:"undoUntil" doc:"End of the undo window: until then nothing reached the mail server; null for unsnooze."`
}

type snoozeInput struct {
	ID     string `path:"id" doc:"t_ id of the thread."`
	Client string `header:"X-Silo-Client" doc:"Client id, echoed as origin in the events it causes."`
	Body   SnoozeBody
}

type unsnoozeInput struct {
	ID     string `path:"id" doc:"t_ id of the thread."`
	Client string `header:"X-Silo-Client" doc:"Client id, echoed as origin in the events it causes."`
}

type snoozeOutput struct{ Body SnoozeResult }

// SnoozePreset is a suggested wake time.
type SnoozePreset struct {
	Key   string    `json:"key" enum:"later_today,tomorrow,this_weekend,next_week" doc:"Stable key; clients may translate the label by it."`
	Label string    `json:"label" example:"Tomorrow" doc:"English label."`
	Until time.Time `json:"until" doc:"The wake time to send to POST /threads/{id}/snooze."`
}

// SnoozePresets is the answer of GET /snooze/presets.
type SnoozePresets struct {
	Timezone string         `json:"timezone" example:"Europe/Warsaw" doc:"The time zone the presets were computed in (settings.timezone, UTC when unset)."`
	Presets  []SnoozePreset `json:"presets"`
}

type presetsInput struct {
	Now string `query:"now" doc:"Compute the presets at this time (RFC 3339) instead of the server's clock; for tests and previews."`
}

type presetsOutput struct{ Body SnoozePresets }

func snoozeError(err error) error {
	switch {
	case errors.Is(err, ops.ErrInvalidSnooze):
		return problem(http.StatusUnprocessableEntity, CodeInvalidSnooze, "The snooze time must be at least 30 seconds and at most a year ahead.")
	case errors.Is(err, ops.ErrNotInInbox):
		return problem(http.StatusConflict, CodeNotInInbox, "Only a thread with mail in the inbox can be snoozed.")
	case errors.Is(err, ops.ErrNotSnoozed):
		return problem(http.StatusConflict, CodeNotSnoozed, "The thread is not snoozed.")
	}
	return opsError(err)
}

func snoozeResultOf(r *ops.Result, threadID int64) (*snoozeOutput, error) {
	out := &snoozeOutput{}
	if len(r.Threads) == 0 {
		return nil, errNotFound()
	}
	out.Body.Thread = threadOf(r.Threads[0])
	for _, t := range r.Threads {
		if t.ID == threadID {
			out.Body.Thread = threadOf(t)
		}
	}
	if r.OperationID != "" {
		id := r.OperationID
		out.Body.OperationID = &id
	}
	out.Body.UndoUntil = r.UndoUntil
	return out, nil
}

func (d *deps) registerSnooze(a huma.API) {
	huma.Register(a, d.secured(huma.Operation{
		OperationID: "snoozeThread", Method: http.MethodPost, Path: "/threads/{id}/snooze", Tags: []string{"threads"},
		Summary: "Snooze a thread until a time",
		Description: "The messages of the thread that are in INBOX turn SNOOZED at once (pendingSync) and move to the " +
			"Snoozed folder of the mail server after the undo window (folder role snoozed; Silo creates the folder, named " +
			"by the snoozed folder mapping or Snoozed, on the first snooze), with the keyword $SiloSnoozed when the " +
			"server keeps keywords, so other clients do not show them in their inbox either. At until (precision 30 s, " +
			"Silo must be running) they go back to INBOX, the newest unread, and the thread moves to the top of the " +
			"list; a new message in the thread wakes it at once. Snoozing a snoozed thread again only changes its time. " +
			"The answer can be undone with POST /operations/{id}/undo. A thread without mail in INBOX is 409 " +
			"not_in_inbox (snooze is an inbox action, as in Gmail); until less than 30 seconds or more than a year " +
			"ahead is 422 invalid_snooze. SNOOZED cannot be added with modify.",
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity},
	}), func(ctx context.Context, in *snoozeInput) (*snoozeOutput, error) {
		id, ok := parseID("t_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		r, err := d.ops.Snooze(ctx, sessionFrom(ctx).UserID, id, in.Body.Until, in.Client)
		if err != nil {
			return nil, snoozeError(err)
		}
		return snoozeResultOf(r, id)
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "unsnoozeThread", Method: http.MethodDelete, Path: "/threads/{id}/snooze", Tags: []string{"threads"},
		Summary: "Wake a snoozed thread now",
		Description: "The same as the wake-up at the snooze time: the messages go back to INBOX at once (no undo window), " +
			"without $SiloSnoozed, the newest unread, and the thread moves to the top of the list. Not undoable " +
			"(operationId null). A thread that is not snoozed is 409 not_snoozed.",
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict},
	}), func(ctx context.Context, in *unsnoozeInput) (*snoozeOutput, error) {
		id, ok := parseID("t_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		r, err := d.ops.Unsnooze(ctx, sessionFrom(ctx).UserID, id, in.Client)
		if err != nil {
			return nil, snoozeError(err)
		}
		return snoozeResultOf(r, id)
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "snoozePresets", Method: http.MethodGet, Path: "/snooze/presets", Tags: []string{"threads"},
		Summary: "Suggested snooze times",
		Description: "Gmail's suggestions computed by the server in the user's time zone (settings.timezone, UTC when " +
			"unset), so that clients do no date arithmetic: later_today (18:00, only when more than an hour away), " +
			"tomorrow (08:00), this_weekend (Saturday 08:00, not on Saturday or Sunday) and next_week (Monday 08:00), " +
			"in this order; a preset at the same time as an earlier one is left out (this_weekend on Friday, " +
			"next_week on Sunday).",
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized},
	}), func(ctx context.Context, in *presetsInput) (*presetsOutput, error) {
		now := time.Now()
		if in.Now != "" {
			t, err := time.Parse(time.RFC3339, in.Now)
			if err != nil {
				return nil, problem(http.StatusBadRequest, CodeValidation, "now must be an RFC 3339 time")
			}
			now = t
		}
		loc, err := d.ops.UserLocation(ctx, sessionFrom(ctx).UserID)
		if err != nil {
			return nil, opsError(err)
		}
		out := &presetsOutput{Body: SnoozePresets{Timezone: loc.String(), Presets: []SnoozePreset{}}}
		for _, p := range ops.SnoozePresets(now, loc) {
			out.Body.Presets = append(out.Body.Presets, SnoozePreset{Key: p.Key, Label: p.Label, Until: p.Until})
		}
		return out, nil
	})
}
