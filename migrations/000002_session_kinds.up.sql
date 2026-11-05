-- 000002_session_kinds: bearer sessions for native clients (ADR 0007, plan 3.15).
-- A session is bound to the channel it was issued for: 'cookie' (browser,
-- silo_session + CSRF) or 'bearer' (Authorization: Bearer). client_name is the
-- device name a native client gives at sign-in, shown in the session list.

ALTER TABLE sessions
  ADD COLUMN kind        text NOT NULL DEFAULT 'cookie' CHECK (kind IN ('cookie','bearer')),
  ADD COLUMN client_name text NOT NULL DEFAULT '';
