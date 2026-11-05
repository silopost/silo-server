-- 000006_search_index: search_tsv is built by the application tokenizer
-- (internal/search: letters and digits, folded, explicit positions and
-- weights) instead of to_tsvector('simple', unaccent(...)), so that the SQL
-- search and the in-memory matcher of filters see the same lexemes (plan
-- 3.12). search_ver is the tokenizer version a row was indexed with; rows
-- below the current version are re-indexed in the background at start-up
-- (mail.ReindexSearch). messages_body_pending makes indexing.done cheap
-- once every body is downloaded.
ALTER TABLE messages ADD COLUMN search_ver smallint NOT NULL DEFAULT 0;
CREATE INDEX messages_search_stale ON messages (id) WHERE search_ver < 1;
CREATE INDEX messages_body_pending ON messages (account_id)
  WHERE body_state IN ('headers', 'deferred') AND orphaned_at IS NULL;
