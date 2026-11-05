package sync

import (
	"context"

	"github.com/silopost/silo-server/internal/mail"
)

// Progress is the initial sync progress of an account (account.sync_progress,
// GET /accounts/{id}/sync-status). Phase is headers_recent (phase A), headers
// (phase B), bodies (phase C) or done. Message and byte counts are those of
// the current phase: headers fetched vs STATUS MESSAGES in the header phases,
// bodies downloaded vs messages in the bodies phase.
type Progress struct {
	Phase         string `json:"phase"`
	FoldersDone   int    `json:"foldersDone"`
	FoldersTotal  int    `json:"foldersTotal"`
	MessagesDone  int64  `json:"messagesDone"`
	MessagesTotal int64  `json:"messagesTotal"`
	BytesDone     int64  `json:"bytesDone"`
	BytesTotal    int64  `json:"bytesTotal"`
}

func loadProgress(ctx context.Context, db mail.DB, accountID int64) (Progress, error) {
	var p Progress
	var recent, headers, allDone bool
	err := db.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE c.phase IN ('bodies','done'))::int, count(*)::int,
       COALESCE(sum(c.done_count), 0)::bigint, COALESCE(sum(GREATEST(c.total_count, c.done_count)), 0)::bigint,
       COALESCE(sum(c.bytes_done), 0)::bigint,
       COALESCE(bool_or(c.phase = 'headers_recent'), false), COALESCE(bool_or(c.phase = 'headers' OR c.phase IS NULL), false),
       COALESCE(bool_and(c.phase = 'done'), false)
FROM mailbox_folders f LEFT JOIN sync_cursors c ON c.folder_id = f.id
WHERE f.account_id = $1 AND f.selectable AND f.sync_mode = 'sync'`, accountID).
		Scan(&p.FoldersDone, &p.FoldersTotal, &p.MessagesDone, &p.MessagesTotal, &p.BytesDone, &recent, &headers, &allDone)
	if err != nil {
		return p, err
	}
	switch {
	case recent:
		p.Phase = "headers_recent"
	case headers || p.FoldersTotal == 0:
		p.Phase = "headers"
	case allDone:
		p.Phase = "done"
	default:
		p.Phase = "bodies"
	}
	if p.Phase == "bodies" || p.Phase == "done" {
		err = db.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE body_state <> 'headers'), count(*),
       COALESCE(sum(size_bytes) FILTER (WHERE body_state <> 'headers'), 0)::bigint, COALESCE(sum(size_bytes), 0)::bigint
FROM messages WHERE account_id = $1 AND orphaned_at IS NULL`, accountID).
			Scan(&p.MessagesDone, &p.MessagesTotal, &p.BytesDone, &p.BytesTotal)
	}
	return p, err
}
