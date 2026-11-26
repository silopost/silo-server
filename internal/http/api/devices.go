package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/push"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

// Error codes of /devices (phase 6).
const (
	CodePushDisabled          = "push_disabled"
	CodeBearerSessionRequired = "bearer_session_required"
	CodeInvalidAccount        = "invalid_account"
)

// Device is the push device of a bearer session.
type Device struct {
	ID         string    `json:"id" example:"dev_1"`
	Provider   string    `json:"provider" enum:"expo"`
	PushToken  string    `json:"pushToken" doc:"The token registered by this session."`
	AccountIDs []string  `json:"accountIds" nullable:"true" doc:"Accounts whose new mail is pushed; null: every account of the user."`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt" doc:"Last registration or accepted push."`
	LastError  *string   `json:"lastError" doc:"Last delivery error reported by the push service (e.g. MessageRateExceeded), null after a successful push. A token the service reports as not registered removes the device instead."`
}

func deviceOf(d sqlcgen.PushDevice) Device {
	out := Device{
		ID: formatID("dev_", d.ID), Provider: d.Provider, PushToken: d.PushToken,
		CreatedAt: d.CreatedAt.Time, LastUsedAt: d.LastUsedAt.Time,
	}
	if d.AccountIds != nil {
		out.AccountIDs = make([]string, 0, len(d.AccountIds))
		for _, id := range d.AccountIds {
			out.AccountIDs = append(out.AccountIDs, formatID("acc_", id))
		}
	}
	if d.LastError.Valid {
		out.LastError = &d.LastError.String
	}
	return out
}

// RegisterDevice is the body of POST /devices.
type RegisterDevice struct {
	Provider   string    `json:"provider" enum:"expo" doc:"Push provider; one of GET /meta push.providers."`
	PushToken  string    `json:"pushToken" maxLength:"255" pattern:"^Expo(nent)?PushToken\\[[^\\[\\]\\s]{1,200}\\]$" doc:"Expo push token (ExponentPushToken[...] or ExpoPushToken[...])."`
	AccountIDs *[]string `json:"accountIds,omitempty" maxItems:"100" nullable:"true" doc:"Accounts of the user (acc_ ids) to push; omitted or null: every account (also accounts added later)."`
}

type deviceOutput struct{ Body Device }

type deviceIDInput struct {
	ID string `path:"id" example:"dev_1"`
}

func (d *deps) registerDevices(a huma.API) {
	huma.Register(a, d.secured(huma.Operation{
		OperationID: "registerDevice", Method: http.MethodPost, Path: "/devices", Tags: []string{"devices"},
		Summary: "Register the push device of this session",
		Description: "Bearer sessions only (403 bearer_session_required for the cookie session). A session has one device: " +
			"registering again updates it (another token replaces it). The same token registered by another session moves " +
			"to this one. Signing out (DELETE /session) or the expiry of the session removes the device. " +
			"403 push_disabled when the instance has push switched off (GET /meta push.providers is empty); " +
			"422 invalid_account when accountIds names an account the user does not have. " +
			"Notifications carry no message content: a generic title and data " +
			"`{v: 1, type: \"mail\", accountId, threadId, count}` (Android channel `mail`).",
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity},
	}), func(ctx context.Context, in *struct{ Body RegisterDevice }) (*deviceOutput, error) {
		reg := push.Registration{Provider: in.Body.Provider, Token: in.Body.PushToken}
		if in.Body.AccountIDs != nil {
			reg.AccountIDs = make([]int64, 0, len(*in.Body.AccountIDs))
			for _, s := range *in.Body.AccountIDs {
				id, ok := parseID("acc_", s)
				if !ok {
					return nil, errInvalidAccount()
				}
				reg.AccountIDs = append(reg.AccountIDs, id)
			}
		}
		dev, err := d.push.Register(ctx, sessionFrom(ctx), reg)
		if err != nil {
			return nil, d.pushError(err)
		}
		return &deviceOutput{Body: deviceOf(dev)}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "getCurrentDevice", Method: http.MethodGet, Path: "/devices/current", Tags: []string{"devices"},
		Summary:     "Get the push device of this session",
		Description: "Bearer sessions only (403 bearer_session_required). 404 when the session has no device (never registered, or removed after the push service reported the token as not registered).",
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound},
	}), func(ctx context.Context, _ *struct{}) (*deviceOutput, error) {
		dev, err := d.push.Current(ctx, sessionFrom(ctx))
		if err != nil {
			return nil, d.pushError(err)
		}
		return &deviceOutput{Body: deviceOf(dev)}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "deleteDevice", Method: http.MethodDelete, Path: "/devices/{id}", Tags: []string{"devices"},
		Summary:       "Unregister a push device",
		Description:   "Removes a device of the user (any of their sessions); push to it stops. Works with push switched off too.",
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound},
	}), func(ctx context.Context, in *deviceIDInput) (*struct{}, error) {
		id, ok := parseID("dev_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		if err := d.push.Delete(ctx, sessionFrom(ctx).UserID, id); err != nil {
			return nil, d.pushError(err)
		}
		return nil, nil
	})
}

func errInvalidAccount() error {
	return problem(http.StatusUnprocessableEntity, CodeInvalidAccount, "accountIds must name accounts of the user (acc_ ids).")
}

// pushError maps a push service error to a problem.
func (d *deps) pushError(err error) error {
	switch {
	case errors.Is(err, push.ErrDisabled):
		return problem(http.StatusForbidden, CodePushDisabled, "Push notifications are switched off on this instance.")
	case errors.Is(err, push.ErrNotBearer):
		return problem(http.StatusForbidden, CodeBearerSessionRequired, "Push devices belong to bearer sessions of native clients.")
	case errors.Is(err, push.ErrInvalidAccount):
		return errInvalidAccount()
	case errors.Is(err, push.ErrInvalid):
		return problem(http.StatusBadRequest, CodeValidation, "Unsupported provider or invalid push token.")
	case errors.Is(err, push.ErrNotFound):
		return errNotFound()
	}
	d.log.Error("push devices", "error", err)
	return errInternal(err)
}
