-- 000052: pending OAuth2 authorizations of XOAUTH2 accounts (plan 3.15,
-- phase 5). POST /oauth/{provider}/start stores one row per sign-in attempt;
-- GET /oauth/{provider}/callback looks it up by the state the provider
-- returns, deletes it and uses its PKCE code_verifier for the token exchange.
-- The callback has no session (the browser comes back from the provider), so
-- the row is what ties the authorization to the Silo user. account_id is set
-- when an existing account is re-authorized. Expired rows are deleted by the
-- periodic blob_gc job.
--
-- Numbered 000052 to stay clear of the migrations of phase 4 (000006..).

CREATE TABLE oauth_states (
  state         text   PRIMARY KEY,               -- 32 random bytes, base64url
  user_id       bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider      text   NOT NULL CHECK (provider IN ('google','microsoft')),
  code_verifier text   NOT NULL,
  account_id    bigint NULL REFERENCES accounts(id) ON DELETE CASCADE,
  return_to     text   NOT NULL DEFAULT '/',
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL
);
CREATE INDEX oauth_states_expires ON oauth_states (expires_at);
