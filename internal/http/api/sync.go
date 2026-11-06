package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	silosync "github.com/silopost/silo-server/internal/sync"
)

// SyncProgress is the initial sync progress (same shape as the
// account.sync_progress event).
type SyncProgress struct {
	Phase         string `json:"phase" enum:"headers_recent,headers,bodies,done" doc:"headers_recent: last 30 days of INBOX and Sent (phase A); headers: header backfill; bodies: body backfill; done."`
	FoldersDone   int    `json:"foldersDone"`
	FoldersTotal  int    `json:"foldersTotal"`
	MessagesDone  int64  `json:"messagesDone"`
	MessagesTotal int64  `json:"messagesTotal"`
	BytesDone     int64  `json:"bytesDone"`
	BytesTotal    int64  `json:"bytesTotal"`
}

// Folder is a server folder of an account with its role and sync mode.
type Folder struct {
	ID         string     `json:"id" example:"f_3"`
	Name       string     `json:"name" example:"INBOX"`
	Role       string     `json:"role" enum:"inbox,sent,drafts,trash,junk,archive,all,snoozed,user"`
	SpecialUse *string    `json:"specialUse" doc:"RFC 6154 attribute reported by the server, e.g. \\Sent."`
	Messages   int        `json:"messages"`
	Unread     int        `json:"unread"`
	SyncMode   string     `json:"syncMode" enum:"sync,ignore"`
	LastSyncAt *time.Time `json:"lastSyncAt"`
	Error      *string    `json:"error"`
	ErrorCount int        `json:"errorCount"`
}

// SyncStatus is the sync state of an account (plan 5.2).
type SyncStatus struct {
	ID                string       `json:"id" example:"acc_1"`
	Email             string       `json:"email"`
	State             string       `json:"state" enum:"connecting,discovering,initial_sync,live,backoff,auth_failed,disabled"`
	LastSyncAt        *time.Time   `json:"lastSyncAt"`
	Error             *string      `json:"error"`
	InitialSync       SyncProgress `json:"initialSync"`
	InitialSyncDoneAt *time.Time   `json:"initialSyncDoneAt"`
	PendingOps        int          `json:"pendingOps" doc:"Local operations waiting for write-back."`
	DeadOps           int          `json:"deadOps" doc:"Jobs of the account that failed for good."`
	Capabilities      []string     `json:"capabilities"`
	KeywordsSupported bool         `json:"keywordsSupported"`
	MissingRoles      []string     `json:"missingRoles" doc:"Roles without a folder on the server (archive: created on the first archive operation)."`
	Folders           []Folder     `json:"folders"`
}

type syncStatusOutput struct{ Body SyncStatus }

// Resync is the body of POST /accounts/{id}/resync.
type Resync struct {
	Mode string `json:"mode,omitempty" enum:"incremental,full,rebuild" default:"incremental" doc:"incremental: sync all folders now; full: also compare every flag; rebuild: drop the cache of the account and run the initial sync from scratch (labels, filters, settings, search history and local drafts stay; snoozes and labels that are local only are re-attached by Message-ID and size; user labels come back from the IMAP keywords). Scheduled mail stays; refused with 409 rebuild_blocked while a send is in flight or due within a minute, or operations wait for write-back."`
}

type resyncInput struct {
	ID   string `path:"id" example:"acc_1"`
	Body Resync
}

// FolderChange is one entry of PUT /accounts/{id}/folders.
type FolderChange struct {
	ID       string  `json:"id" example:"f_3"`
	Role     *string `json:"role,omitempty" enum:"sent,drafts,trash,junk,archive,all,snoozed,user" doc:"Manual role; wins over SPECIAL-USE and names. One folder per role. snoozed: the folder of snoozed mail (Silo creates Snoozed on the first snooze when no folder has the role)."`
	SyncMode *string `json:"syncMode,omitempty" enum:"sync,ignore" doc:"ignore removes the folder from the mirror."`
}

type putFoldersInput struct {
	ID   string `path:"id" example:"acc_1"`
	Body struct {
		Folders []FolderChange `json:"folders" maxItems:"1000"`
	}
}

type foldersOutput struct {
	Body struct {
		Folders []Folder `json:"folders"`
	}
}

func folderList(in []silosync.FolderInfo) []Folder {
	out := make([]Folder, 0, len(in))
	for _, f := range in {
		v := Folder{ID: formatID("f_", f.ID), Name: f.Name, Role: f.Role, Messages: f.Messages, Unread: f.Unread,
			SyncMode: f.SyncMode, LastSyncAt: f.LastSyncAt, Error: f.Error, ErrorCount: f.ErrorCount}
		if f.SpecialUse != "" {
			su := f.SpecialUse
			v.SpecialUse = &su
		}
		out = append(out, v)
	}
	return out
}

func syncError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, silosync.ErrNotFound):
		return errNotFound()
	case errors.Is(err, silosync.ErrRebuildBlocked):
		return problem(http.StatusConflict, CodeRebuildBlocked, "The cache cannot be rebuilt while a send is in flight or due or operations wait for the server ("+
			strings.TrimPrefix(err.Error(), silosync.ErrRebuildBlocked.Error()+": ")+"); try again when they are done.")
	case errors.Is(err, silosync.ErrInvalid):
		return problem(http.StatusUnprocessableEntity, CodeValidation, strings.TrimPrefix(err.Error(), "sync: invalid request: "))
	}
	return errInternal(err)
}

func (d *deps) registerSync(a huma.API) {
	huma.Register(a, d.secured(huma.Operation{
		OperationID: "getSyncStatus", Method: http.MethodGet, Path: "/accounts/{id}/sync-status", Tags: []string{"accounts"},
		Summary: "Get the sync state of an account",
		Description: "State machine, initial sync progress, pending operations, folders and their errors, " +
			"and roles still missing on the server.",
		Errors: []int{http.StatusUnauthorized, http.StatusNotFound},
	}), func(ctx context.Context, in *accountIDInput) (*syncStatusOutput, error) {
		id, ok := parseID("acc_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		st, err := d.sync.Status(ctx, sessionFrom(ctx).UserID, id)
		if err != nil {
			return nil, syncError(err)
		}
		p := st.InitialSync
		return &syncStatusOutput{Body: SyncStatus{
			ID: formatID("acc_", st.AccountID), Email: st.Email, State: st.State, LastSyncAt: st.LastSyncAt, Error: st.Error,
			InitialSync: SyncProgress{Phase: p.Phase, FoldersDone: p.FoldersDone, FoldersTotal: p.FoldersTotal,
				MessagesDone: p.MessagesDone, MessagesTotal: p.MessagesTotal, BytesDone: p.BytesDone, BytesTotal: p.BytesTotal},
			InitialSyncDoneAt: st.InitialSyncDoneAt, PendingOps: st.PendingOps, DeadOps: st.DeadOps,
			Capabilities: st.Capabilities, KeywordsSupported: st.KeywordsSupported, MissingRoles: st.MissingRoles,
			Folders: folderList(st.Folders),
		}}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "resyncAccount", Method: http.MethodPost, Path: "/accounts/{id}/resync", Tags: []string{"accounts"},
		Summary: "Force a sync of the account",
		Description: "Queues the request for the account's sync worker and returns at once. rebuild wipes the cache when " +
			"the worker gets to it (account.status, resync and label.counts events follow) and runs the initial sync from scratch.",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity},
	}), func(ctx context.Context, in *resyncInput) (*struct{}, error) {
		id, ok := parseID("acc_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		mode := in.Body.Mode
		if mode == "" {
			mode = silosync.ResyncIncremental
		}
		return nil, syncError(d.sync.RequestResync(ctx, sessionFrom(ctx).UserID, id, mode))
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "listFolders", Method: http.MethodGet, Path: "/accounts/{id}/folders", Tags: []string{"accounts"},
		Summary: "List the server folders of an account with roles and sync mode",
		Errors:  []int{http.StatusUnauthorized, http.StatusNotFound},
	}), func(ctx context.Context, in *accountIDInput) (*foldersOutput, error) {
		id, ok := parseID("acc_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		fs, err := d.sync.Folders(ctx, sessionFrom(ctx).UserID, id)
		if err != nil {
			return nil, syncError(err)
		}
		out := &foldersOutput{}
		out.Body.Folders = folderList(fs)
		return out, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "updateFolders", Method: http.MethodPut, Path: "/accounts/{id}/folders", Tags: []string{"accounts"},
		Summary: "Map folder roles and choose which folders are synced",
		Description: "Listed folders get the given role and/or sync mode; others stay unchanged. Each role except user " +
			"belongs to at most one folder and INBOX cannot be changed (422 validation_failed).",
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity},
	}), func(ctx context.Context, in *putFoldersInput) (*foldersOutput, error) {
		id, ok := parseID("acc_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		ups := make([]silosync.FolderUpdate, 0, len(in.Body.Folders))
		for _, f := range in.Body.Folders {
			fid, ok := parseID("f_", f.ID)
			if !ok {
				return nil, problem(http.StatusUnprocessableEntity, CodeValidation, "unknown folder "+f.ID)
			}
			ups = append(ups, silosync.FolderUpdate{ID: fid, Role: f.Role, SyncMode: f.SyncMode})
		}
		fs, err := d.sync.UpdateFolders(ctx, sessionFrom(ctx).UserID, id, ups)
		if err != nil {
			return nil, syncError(err)
		}
		out := &foldersOutput{}
		out.Body.Folders = folderList(fs)
		return out, nil
	})
}
