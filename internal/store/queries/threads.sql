-- name: ListThreadsByLabel :many
-- Inbox-style list (plan 4.7): keyset pagination over thread_labels_list.
-- First page: sort_at 'infinity', thread_id 9223372036854775807. Ask for
-- page_size + 1 rows to detect a next page.
SELECT t.id, t.account_id, t.subject, t.snippet, t.last_message_at, t.message_count, t.unread_count,
       t.has_attachments, t.participants, t.snoozed_until, t.pending_ops,
       tl.sort_at,
       ARRAY(SELECT x.label_id FROM thread_labels x WHERE x.thread_id = t.id AND NOT x.hidden ORDER BY x.label_id)::bigint[] AS label_ids
FROM thread_labels tl
JOIN threads t ON t.id = tl.thread_id
WHERE tl.user_id  = sqlc.arg(user_id)
  AND tl.label_id = sqlc.arg(label_id)
  AND NOT tl.hidden
  AND (tl.sort_at, tl.thread_id) < (sqlc.arg(cursor_sort_at)::timestamptz, sqlc.arg(cursor_thread_id)::bigint)
ORDER BY tl.sort_at DESC, tl.thread_id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListThreadsAll :many
-- "All mail" view of one account: threads with at least one visible label
-- other than TRASH and SPAM (threads entirely in them are hidden elsewhere).
SELECT t.id, t.account_id, t.subject, t.snippet, t.last_message_at, t.message_count, t.unread_count,
       t.has_attachments, t.participants, t.snoozed_until, t.pending_ops,
       t.sort_at,
       ARRAY(SELECT x.label_id FROM thread_labels x WHERE x.thread_id = t.id AND NOT x.hidden ORDER BY x.label_id)::bigint[] AS label_ids
FROM threads t
WHERE t.account_id = sqlc.arg(account_id)
  AND t.user_id = sqlc.arg(user_id)
  AND (t.sort_at, t.id) < (sqlc.arg(cursor_sort_at)::timestamptz, sqlc.arg(cursor_thread_id)::bigint)
  AND EXISTS (SELECT 1 FROM thread_labels x JOIN labels l ON l.id = x.label_id
              WHERE x.thread_id = t.id AND NOT x.hidden
                AND l.system_key IS DISTINCT FROM 'TRASH' AND l.system_key IS DISTINCT FROM 'SPAM')
ORDER BY t.sort_at DESC, t.id DESC
LIMIT sqlc.arg(page_limit);
