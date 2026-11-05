package sync

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/emersion/go-imap/v2"
	"github.com/jackc/pgx/v5"

	"github.com/silopost/silo-server/internal/mail"
)

// draftFlags are the flags of a draft saved in the Drafts folder (plan 3.8).
var draftFlags = []string{`\Draft`, `\Seen`}

// sentFlags are the flags of a sent message saved in the Sent folder (plan 3.9).
var sentFlags = []string{`\Seen`}

// draftsFolder returns the Drafts folder, creating it when the account has
// none and create is set (nil without one).
func (s *session) draftsFolder(ctx context.Context, c *conn, folders map[int64]*opFolder, create bool) (*opFolder, error) {
	return s.roleFolder(ctx, c, folders, RoleDrafts, "Drafts", create)
}

// roleFolder returns the folder of a role (Drafts, Sent), creating it as base
// when the account has none and create is set (nil without one).
func (s *session) roleFolder(ctx context.Context, c *conn, folders map[int64]*opFolder, role, base string, create bool) (*opFolder, error) {
	for _, id := range sortedKeys(folders) {
		if folders[id].role == role {
			return folders[id], nil
		}
	}
	if !create {
		return nil, nil
	}
	return s.createRoleFolder(ctx, c, folders, role, base)
}

// appendRaw APPENDs a message to folder f and returns its UID when the
// server reports it (APPENDUID, UIDPLUS), else 0.
func appendRaw(ctx context.Context, c *conn, f *opFolder, raw []byte, flags []string) (uint32, error) {
	var data *imap.AppendData
	err := c.await(ctx, 0, func() error {
		cmd := c.imap().Append(f.name, int64(len(raw)), &imap.AppendOptions{Flags: toFlags(flags)})
		if _, err := cmd.Write(raw); err != nil {
			_ = cmd.Close()
			return err
		}
		if err := cmd.Close(); err != nil {
			return err
		}
		var err error
		data, err = cmd.Wait()
		return err
	})
	if err != nil || data == nil {
		return 0, err
	}
	return uint32(data.UID), nil
}

// appendSent saves a message sent by Silo in the Sent folder (plan 3.9 step
// 5: job append with toRole sent, enqueued only when accounts.sent_append):
// APPEND of the sent bytes (messages.raw_blob_sha256) with (\Seen); the copy
// is recorded as a location of the message right away when the server reports
// its UID, otherwise the sync attaches it by Message-ID. A message without a
// stored body (send cancelled meanwhile) is skipped.
func (s *session) appendSent(ctx context.Context, c *conn, folders map[int64]*opFolder, msgIDs []int64) (func(tx pgx.Tx) error, int64, error) {
	if len(msgIDs) != 1 {
		return nil, 0, fmt.Errorf("%w: append to Sent takes one message", errOpPermanent)
	}
	msgID := msgIDs[0]
	var sha []byte
	err := s.d.Pool.QueryRow(ctx, `
SELECT m.raw_blob_sha256 FROM messages m JOIN outbound o ON o.message_id = m.id
WHERE m.id = $1 AND o.status = 'sent' AND m.raw_blob_sha256 IS NOT NULL`, msgID).Scan(&sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	rc, err := s.d.Blobs.Open(ctx, hex.EncodeToString(sha))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: open sent message: %v", errOpPermanent, err)
	}
	raw, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return nil, 0, err
	}
	f, err := s.roleFolder(ctx, c, folders, RoleSent, "Sent", true)
	if err != nil {
		return nil, 0, err
	}
	uid, err := appendRaw(ctx, c, f, raw, sentFlags)
	if err != nil {
		return nil, 0, err
	}
	return func(tx pgx.Tx) error {
		if uid == 0 || f.id <= 0 {
			return nil
		}
		_, err := tx.Exec(context.Background(), `
INSERT INTO message_locations (folder_id, uid, message_id, account_id, user_id, flags)
SELECT $1, $2, m.id, m.account_id, m.user_id, $4 FROM messages m WHERE m.id = $3
ON CONFLICT (folder_id, uid) DO NOTHING`, f.id, int64(uid), msgID, sentFlags)
		return err
	}, f.id, nil
}

// draftCopies returns the UIDs in folder f (selected) of the copies of a draft:
// the messages with its X-Silo-Draft-Id plus the given known UIDs, without
// copies already flagged \Deleted.
func draftCopies(ctx context.Context, c *conn, draftID string, known []uint32) ([]uint32, error) {
	out := slices.Clone(known)
	if draftID != "" {
		var data *imap.SearchData
		err := c.await(ctx, 0, func() error {
			var err error
			data, err = c.imap().UIDSearch(&imap.SearchCriteria{
				Header:  []imap.SearchCriteriaHeaderField{{Key: mail.HeaderSiloDraftID, Value: draftID}},
				NotFlag: []imap.Flag{imap.FlagDeleted},
			}, nil).Wait()
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, u := range data.AllUIDs() {
			out = append(out, uint32(u))
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// expungeUIDs deletes UIDs of the selected Drafts folder: \Deleted, then UID
// EXPUNGE with UIDPLUS or EXPUNGE (the folder holds drafts only, plan 3.8).
func expungeUIDs(ctx context.Context, c *conn, uids []uint32) error {
	if len(uids) == 0 {
		return nil
	}
	if err := c.store(ctx, uids, imap.StoreFlagsAdd, []string{`\Deleted`}); err != nil {
		return err
	}
	if c.caps.HasUIDPlus() {
		return c.await(ctx, 0, func() error { return c.imap().UIDExpunge(uidSet(uids)).Close() })
	}
	return c.await(ctx, 0, func() error { return c.imap().Expunge().Close() })
}

// appendDraft saves the current version of a local draft in the Drafts folder
// (plan 3.8 step 2): APPEND with (\Draft \Seen), then the older copies (same
// X-Silo-Draft-Id, or known locations of the message in Drafts) are deleted.
// The new copy is recorded as a location of the draft's message right away
// (APPENDUID, or the highest UID of the copies without UIDPLUS) and
// drafts.server_synced_version set. A draft discarded meanwhile is skipped.
func (s *session) appendDraft(ctx context.Context, c *conn, folders map[int64]*opFolder, p mail.OpPayload, msgIDs []int64) (func(tx pgx.Tx) error, int64, error) {
	if p.ToRole != RoleDrafts || len(msgIDs) != 1 {
		return nil, 0, fmt.Errorf("%w: append supports one draft to the Drafts folder", errOpPermanent)
	}
	msgID := msgIDs[0]
	d, err := mail.BuildDraftMIME(ctx, s.d.Pool, s.d.Blobs, msgID)
	if errors.Is(err, mail.ErrNoDraft) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	f, err := s.draftsFolder(ctx, c, folders, true)
	if err != nil {
		return nil, 0, err
	}
	newUID, err := appendRaw(ctx, c, f, d.Raw, draftFlags)
	if err != nil {
		return nil, 0, err
	}
	if err := c.ensure(ctx, f.name); err != nil {
		return nil, 0, err
	}
	known, err := s.knownUIDs(ctx, f.id, msgID)
	if err != nil {
		return nil, 0, err
	}
	copies, err := draftCopies(ctx, c, d.SiloDraftID, known)
	if err != nil {
		return nil, 0, err
	}
	if newUID == 0 && len(copies) > 0 {
		newUID = copies[len(copies)-1] // UIDs grow: the copy just appended is the last
	}
	old := slices.DeleteFunc(copies, func(u uint32) bool { return u == newUID })
	if err := expungeUIDs(ctx, c, old); err != nil {
		return nil, 0, err
	}
	return func(tx pgx.Tx) error {
		ctx := context.Background()
		if newUID != 0 && f.id > 0 {
			if _, err := tx.Exec(ctx, `
INSERT INTO message_locations (folder_id, uid, message_id, account_id, user_id, flags)
SELECT $1, $2, m.id, m.account_id, m.user_id, $4 FROM messages m WHERE m.id = $3
ON CONFLICT (folder_id, uid) DO NOTHING`, f.id, int64(newUID), msgID, draftFlags); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE drafts SET server_synced_version = GREATEST(server_synced_version, $2) WHERE message_id = $1`, msgID, d.Version); err != nil {
			return err
		}
		if len(old) > 0 && f.id > 0 {
			if _, err := mail.LocationVanished(ctx, tx, f.id, old); err != nil {
				return err
			}
		}
		return nil
	}, f.id, nil
}

// knownUIDs returns the UIDs of a message in a folder from the mirror.
func (s *session) knownUIDs(ctx context.Context, folderID, msgID int64) ([]uint32, error) {
	if folderID <= 0 {
		return nil, nil
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT uid FROM message_locations WHERE folder_id = $1 AND message_id = $2`, folderID, msgID)
	if err != nil {
		return nil, err
	}
	uids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	out := make([]uint32, len(uids))
	for i, u := range uids {
		out[i] = uint32(u)
	}
	return out, nil
}

// deleteDraftCopies removes the copies of a discarded draft from the Drafts
// folder (plan 3.8 step 4): those carrying its X-Silo-Draft-Id and the UIDs
// recorded when it was discarded.
func (s *session) deleteDraftCopies(ctx context.Context, c *conn, folders map[int64]*opFolder, p mail.OpPayload) (func(tx pgx.Tx) error, int64, error) {
	f, err := s.draftsFolder(ctx, c, folders, false)
	if err != nil || f == nil {
		return nil, 0, err
	}
	if err := c.ensure(ctx, f.name); err != nil {
		return nil, 0, err
	}
	var known []uint32
	if p.FolderID == f.id {
		known = p.UIDs
	}
	uids, err := draftCopies(ctx, c, p.DraftID, known)
	if err != nil {
		return nil, 0, err
	}
	if err := expungeUIDs(ctx, c, uids); err != nil {
		return nil, 0, err
	}
	return func(tx pgx.Tx) error {
		_, err := mail.LocationVanished(context.Background(), tx, f.id, uids)
		return err
	}, f.id, nil
}
