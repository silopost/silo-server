package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/silopost/silo-server/internal/mailview"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

// Error codes of PATCH /settings.
const (
	CodeInvalidSetting  = "invalid_setting"
	CodeInvalidTimezone = "invalid_timezone"
)

// MaxSignatureBytes is the limit of settings.signature.
const MaxSignatureBytes = 32 << 10

// Allowed values of the settings.
var (
	densities          = []string{"default", "comfortable", "compact"}
	undoSendSecondsSet = []int{5, 10, 20, 30}
)

// NullableID is a JSON value that may be absent, null or a string: PATCH
// tells "clear" (null) from "keep" (absent).
type NullableID struct {
	Set   bool
	Value *string
}

// UnmarshalJSON records that the field was present.
func (n *NullableID) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(b) == "null" {
		n.Value = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	n.Value = &s
	return nil
}

// Schema is a nullable string.
func (NullableID) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{Type: huma.TypeString, Nullable: true, Examples: []any{"acc_1"},
		Description: "acc_ id of an account of the user; null clears it (the first account is then the default)."}
}

// SettingsPatch is the body of PATCH /settings: only the fields present
// change (JSON merge).
type SettingsPatch struct {
	Density                  *string    `json:"density,omitempty" doc:"default, comfortable or compact; else 422 invalid_setting."`
	UndoSendSeconds          *int       `json:"undoSendSeconds,omitempty" doc:"5, 10, 20 or 30; else 422 invalid_setting. Sets undoUntil of POST /drafts/{id}/send."`
	Signature                *string    `json:"signature,omitempty" doc:"HTML, at most 32 KiB (else 422 invalid_setting); stored sanitized like message HTML."`
	Timezone                 *string    `json:"timezone,omitempty" example:"Europe/Warsaw" doc:"IANA time zone name; else 422 invalid_timezone. Used by before:/after: of search and filters and by GET /snooze/presets."`
	KeyboardShortcutsEnabled *bool      `json:"keyboardShortcutsEnabled,omitempty"`
	ConversationView         *bool      `json:"conversationView,omitempty"`
	ActiveAccountID          NullableID `json:"activeAccountId,omitempty"`
	DesktopNotifications     *bool      `json:"desktopNotifications,omitempty"`
}

type settingsOutput struct{ Body Settings }

type patchSettingsInput struct {
	Client string `header:"X-Silo-Client" doc:"Client id, echoed as origin in settings.updated."`
	Body   SettingsPatch
}

func invalidSetting(detail string) error {
	return problem(http.StatusUnprocessableEntity, CodeInvalidSetting, detail)
}

func (d *deps) currentSettings(ctx context.Context, userID int64) (Settings, error) {
	user, err := d.auth.User(ctx, userID)
	if err != nil {
		return Settings{}, errInternal(err)
	}
	accs, err := d.accts.List(ctx, userID)
	if err != nil {
		return Settings{}, errInternal(err)
	}
	return settingsFrom(user.Settings, accs), nil
}

func (d *deps) registerSettings(a huma.API) {
	huma.Register(a, d.secured(huma.Operation{
		OperationID: "getSettings", Method: http.MethodGet, Path: "/settings", Tags: []string{"session"},
		Summary:     "Get the user's settings",
		Description: "The same object as settings in GET /session.",
		Errors:      []int{http.StatusUnauthorized},
	}), func(ctx context.Context, _ *struct{}) (*settingsOutput, error) {
		s, err := d.currentSettings(ctx, sessionFrom(ctx).UserID)
		if err != nil {
			return nil, err
		}
		return &settingsOutput{Body: s}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "updateSettings", Method: http.MethodPatch, Path: "/settings", Tags: []string{"session"},
		Summary: "Change settings",
		Description: "JSON merge: only the fields sent change; activeAccountId null clears it. The answer is the whole " +
			"settings object with updatedAt; the other sessions of the user get settings.updated {updatedAt} and " +
			"reload GET /settings. 422 invalid_setting (density, undoSendSeconds, signature over 32 KiB, an " +
			"activeAccountId that is not the user's) or invalid_timezone.",
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity},
	}), func(ctx context.Context, in *patchSettingsInput) (*settingsOutput, error) {
		userID := sessionFrom(ctx).UserID
		b := in.Body
		set := map[string]any{}
		var unset []string
		if b.Density != nil {
			if !slices.Contains(densities, *b.Density) {
				return nil, invalidSetting("density must be one of default, comfortable, compact.")
			}
			set["density"] = *b.Density
		}
		if b.UndoSendSeconds != nil {
			if !slices.Contains(undoSendSecondsSet, *b.UndoSendSeconds) {
				return nil, invalidSetting("undoSendSeconds must be 5, 10, 20 or 30.")
			}
			set["undo_send_seconds"] = *b.UndoSendSeconds
		}
		if b.Signature != nil {
			if len(*b.Signature) > MaxSignatureBytes {
				return nil, invalidSetting("signature is larger than 32 KiB.")
			}
			sig := ""
			if strings.TrimSpace(*b.Signature) != "" {
				sig = mailview.SanitizeHTML(*b.Signature, func(string) string { return "" })
			}
			set["signature"] = sig
		}
		if b.Timezone != nil {
			tz := strings.TrimSpace(*b.Timezone)
			if _, err := time.LoadLocation(tz); err != nil || tz == "" || tz == "Local" {
				return nil, problem(http.StatusUnprocessableEntity, CodeInvalidTimezone, "timezone must be an IANA time zone name, e.g. Europe/Warsaw.")
			}
			set["timezone"] = tz
		}
		if b.KeyboardShortcutsEnabled != nil {
			set["keyboard_shortcuts_enabled"] = *b.KeyboardShortcutsEnabled
		}
		if b.ConversationView != nil {
			set["conversation_view"] = *b.ConversationView
		}
		if b.DesktopNotifications != nil {
			set["desktop_notifications"] = *b.DesktopNotifications
		}
		if b.ActiveAccountID.Set {
			if b.ActiveAccountID.Value == nil {
				unset = append(unset, "active_account_id")
			} else {
				id, ok := parseID("acc_", *b.ActiveAccountID.Value)
				accs, err := d.accts.List(ctx, userID)
				if err != nil {
					return nil, errInternal(err)
				}
				if !ok || !slices.ContainsFunc(accs, func(a2 sqlcgen.Account) bool { return a2.ID == id }) {
					return nil, invalidSetting("activeAccountId is not an account of the user.")
				}
				set["active_account_id"] = id
			}
		}
		user, err := d.auth.UpdateSettings(ctx, userID, set, unset, in.Client)
		if err != nil {
			return nil, errInternal(err)
		}
		accs, err := d.accts.List(ctx, userID)
		if err != nil {
			return nil, errInternal(err)
		}
		return &settingsOutput{Body: settingsFrom(user.Settings, accs)}, nil
	})
}
