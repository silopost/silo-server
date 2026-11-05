-- Drops everything 000001_init created, including its extensions.
DROP TABLE IF EXISTS
  events, search_history, op_messages, jobs, outbound, snooze_items, snoozes, filters, drafts,
  contacts, thread_labels, message_labels, labels, attachments, message_references,
  message_locations, messages, sync_cursors, mailbox_folders, threads, blobs,
  sessions, accounts, users
  CASCADE;

DROP FUNCTION IF EXISTS jobs_notify();

DROP EXTENSION IF EXISTS unaccent;
DROP EXTENSION IF EXISTS pg_trgm;
DROP EXTENSION IF EXISTS citext;
