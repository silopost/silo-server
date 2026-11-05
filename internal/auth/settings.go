package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

// SettingsUpdatedKey is the key of users.settings holding the time of the
// last PATCH /settings (RFC 3339, UTC); no migration needed.
const SettingsUpdatedKey = "updated_at"

// EventSettingsUpdated is written by UpdateSettings (plan 5.4):
// {updatedAt, origin?}.
const EventSettingsUpdated = "settings.updated"

// UpdateSettings merges set into users.settings (snake_case keys, already
// validated by the caller), removes the keys in unset, stamps updated_at and
// writes settings.updated, in one transaction. It returns the user after
// the change.
func (s *Service) UpdateSettings(ctx context.Context, userID int64, set map[string]any, unset []string, origin string) (sqlcgen.User, error) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	if set == nil {
		set = map[string]any{}
	}
	set[SettingsUpdatedKey] = now.Format(time.RFC3339Nano)
	raw, err := json.Marshal(set)
	if err != nil {
		return sqlcgen.User{}, err
	}
	if unset == nil {
		unset = []string{}
	}
	err = pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET settings = (settings - $3::text[]) || $2::jsonb WHERE id = $1`, userID, raw, unset)
		if err != nil {
			return fmt.Errorf("auth: update settings: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		ev := map[string]any{"updatedAt": now}
		if origin != "" {
			ev["origin"] = origin
		}
		return mail.WriteEvent(ctx, tx, userID, EventSettingsUpdated, ev)
	})
	if err != nil {
		return sqlcgen.User{}, err
	}
	return s.User(ctx, userID)
}
