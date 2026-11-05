-- 000004_operations: what undoing a user operation takes once its jobs ran
-- (plan 3.6, 5.2). One row per operation that queued imap_op jobs (id =
-- jobs.group_id). inverse lists, per message, the folder it came from and the
-- flag and user-label changes that really happened; NULL when the operation
-- cannot be undone (permanent delete). Rows are kept 24 h (blob_gc job).
CREATE TABLE operations (
  id         uuid        PRIMARY KEY,
  user_id    bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  undo_until timestamptz NULL,
  inverse    jsonb       NULL
);
CREATE INDEX operations_created ON operations (created_at);
