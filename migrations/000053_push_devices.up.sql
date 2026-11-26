-- 000053: push devices of native clients (plan 4.2 push_devices, phase 6,
-- ADR 0007). A device belongs to one bearer session: signing out or the
-- expiry of the session ends its push (ON DELETE CASCADE). One device per
-- session (UNIQUE session_id); the same push token registered from a new
-- session moves the row to that session (UNIQUE provider, push_token), so a
-- second sign-in on the same phone leaves no duplicate. The only provider is
-- expo (Expo Push Service); the payload carries no message content.
-- account_ids NULL means every account of the user.

CREATE TABLE push_devices (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id      bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  session_id   bigint NOT NULL UNIQUE REFERENCES sessions(id) ON DELETE CASCADE,
  provider     text   NOT NULL CHECK (provider IN ('expo')),
  push_token   text   NOT NULL,
  account_ids  bigint[] NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz NOT NULL DEFAULT now(),
  last_error   text   NULL,
  UNIQUE (provider, push_token)
);
CREATE INDEX push_devices_user ON push_devices (user_id);
