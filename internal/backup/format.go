// Package backup exports and imports the metadata of a Silo instance (plan
// 3.3: what Postgres is the source of truth for) and rebuilds the mail cache
// of an account from the IMAP server.
//
// The export is one JSON document: a header and named sections, each an
// array. Rows are keyed by stable identities, never by local ids: users by
// email, accounts by (user, imap host, username), labels by (account, name),
// messages by Message-ID within the account. The format is extensible: an
// importer skips sections it does not know (with a warning) and treats
// missing sections as empty, so a newer server can add sections or fields
// without breaking older backups.
//
// Assignments that point at messages (user labels, snoozes) cannot be
// applied at import time: the messages come from the IMAP server with the
// first sync. They are staged in metadata_restores and applied by the
// metadata_restore job once the sync has ingested the messages.
package backup

import (
	"encoding/json"
	"time"
)

// Format identifies a Silo metadata export.
const Format = "silo-metadata"

// Version is the version of the document layout. Additive changes (new
// sections, new optional fields) keep it; a change an older importer would
// misread bumps it.
const Version = 1

// settingsKeyActiveAccount is the users.settings key of the active account (a
// number: accounts.id; written by PATCH /settings), translated to the new
// account id on import.
const settingsKeyActiveAccount = "active_account_id"

// Section names, in the order they are written.
const (
	SectionUsers            = "users"
	SectionAccounts         = "accounts"
	SectionLabels           = "labels"
	SectionLabelAssignments = "labelAssignments"
	SectionSnoozes          = "snoozes"
	SectionFilters          = "filters"
	SectionSearchHistory    = "searchHistory"
	SectionContacts         = "contacts"
	SectionBlobs            = "blobs"
	SectionDrafts           = "drafts"
	SectionOutbound         = "outbound"
)

// knownSections lists the sections this version reads.
var knownSections = map[string]bool{
	SectionUsers: true, SectionAccounts: true, SectionLabels: true, SectionLabelAssignments: true,
	SectionSnoozes: true, SectionFilters: true, SectionSearchHistory: true, SectionContacts: true,
	SectionBlobs: true, SectionDrafts: true, SectionOutbound: true,
}

// Header is the part of the document before "sections".
type Header struct {
	Format        string    `json:"format"`
	Version       int       `json:"version"`
	ExportedAt    time.Time `json:"exportedAt"`
	ServerVersion string    `json:"serverVersion"`
	// SchemaVersion is the database migration the export was made at.
	SchemaVersion int64 `json:"schemaVersion"`
	// LocalBlobs is false when the export was made with --no-local-blobs:
	// draft attachments carry their sha256 only.
	LocalBlobs bool `json:"localBlobs"`
}

// Sealed secrets (account passwords, TOTP secrets) stay encrypted with
// SILO_SECRET_KEY. The ciphertext in the database is bound to the row id
// (AES-GCM additional data), which an import cannot keep, so the export
// re-seals each secret with the additional data below, made of the stable
// keys. Importing therefore needs the same SILO_SECRET_KEY (or it as
// SILO_SECRET_KEY_PREVIOUS); the importer re-seals with the new row ids.

// AccountAAD is the additional data of Account.SecretEnc.
func AccountAAD(r AccountRef) []byte {
	return []byte("silo-export/account/" + r.User + "/" + r.IMAPHost + "/" + r.Username)
}

// TOTPAAD is the additional data of User.TOTPSecretEnc.
func TOTPAAD(email string) []byte { return []byte("silo-export/totp/" + email) }

// User is one row of users.
type User struct {
	Email        string          `json:"email"`
	DisplayName  string          `json:"displayName"`
	PasswordHash *string         `json:"passwordHash,omitempty"`
	IsAdmin      bool            `json:"isAdmin"`
	Settings     json.RawMessage `json:"settings"` // without active_account_id
	// ActiveAccount is settings.active_account_id as an account key.
	ActiveAccount     *AccountRef `json:"activeAccount,omitempty"`
	TOTPSecretEnc     []byte      `json:"totpSecretEnc,omitempty"` // base64; sealed with TOTPAAD
	TOTPEnabledAt     *time.Time  `json:"totpEnabledAt,omitempty"`
	TOTPLastStep      int64       `json:"totpLastStep"`
	TOTPBackupCodes   []string    `json:"totpBackupCodes"`
	DisabledAt        *time.Time  `json:"disabledAt,omitempty"`
	PasswordChangedAt *time.Time  `json:"passwordChangedAt,omitempty"`
	CreatedAt         time.Time   `json:"createdAt"`
}

// AccountRef identifies an account: UNIQUE (user_id, imap_host, username).
type AccountRef struct {
	User     string `json:"user"`
	IMAPHost string `json:"imapHost"`
	Username string `json:"username"`
}

// Account is the configuration of one account, without sync state.
type Account struct {
	AccountRef
	Email             string          `json:"email"`
	DisplayName       string          `json:"displayName"`
	Provider          string          `json:"provider"`
	IMAPPort          int32           `json:"imapPort"`
	IMAPTLS           string          `json:"imapTls"`
	SMTPHost          string          `json:"smtpHost"`
	SMTPPort          int32           `json:"smtpPort"`
	SMTPTLS           string          `json:"smtpTls"`
	AuthType          string          `json:"authType"`
	SecretEnc         []byte          `json:"secretEnc"` // base64; sealed with AccountAAD
	FolderMapping     json.RawMessage `json:"folderMapping"`
	KeywordsSupported bool            `json:"keywordsSupported"`
	KeywordPrefix     string          `json:"keywordPrefix"`
	SentAppend        bool            `json:"sentAppend"`
	Position          int32           `json:"position"`
	FiltersSince      *time.Time      `json:"filtersSince,omitempty"`
	CreatedAt         time.Time       `json:"createdAt"`
}

// Label is a user label (type 'user'); system and folder labels are cache.
type Label struct {
	Account      AccountRef `json:"account"`
	Name         string     `json:"name"`
	IMAPKeyword  *string    `json:"imapKeyword,omitempty"`
	KeywordState string     `json:"keywordState"`
	ColorBG      *string    `json:"colorBg,omitempty"`
	ColorText    *string    `json:"colorText,omitempty"`
	ShowInList   string     `json:"showInList"`
	Parent       *string    `json:"parent,omitempty"` // name of the parent label
	Position     int32      `json:"position"`
}

// LabelAssignment is one message_labels row of a user label.
type LabelAssignment struct {
	Account      AccountRef `json:"account"`
	Label        string     `json:"label"`
	MessageIDHdr string     `json:"messageIdHdr"`
}

// Snooze is a snoozed thread: the messages it holds and when it wakes.
type Snooze struct {
	Account   AccountRef `json:"account"`
	WakeAt    time.Time  `json:"wakeAt"`
	CreatedAt time.Time  `json:"createdAt"`
	Items     []string   `json:"items"` // message_id_hdr
	// SourceFolders maps an item to the name of the folder it was snoozed
	// from (snooze_items.source_folder_id), where known.
	SourceFolders map[string]string `json:"sourceFolders,omitempty"`
}

// Filter is one row of filters, columns as in the schema. Label ids inside
// actions are local ids and are not translated (the filters slice of phase
// 4 extends this section).
type Filter struct {
	Account   AccountRef      `json:"account"`
	Criteria  string          `json:"criteria"`
	Actions   json.RawMessage `json:"actions"`
	Position  int32           `json:"position"`
	CreatedAt time.Time       `json:"createdAt"`
}

// SearchHistory is one row of search_history.
type SearchHistory struct {
	Account AccountRef `json:"account"`
	Query   string     `json:"query"`
	UsedAt  time.Time  `json:"usedAt"`
}

// Contact is a contact added by hand (is_manual).
type Contact struct {
	User              string     `json:"user"`
	Email             string     `json:"email"`
	Name              string     `json:"name"`
	SentCount         int32      `json:"sentCount"`
	ReceivedCount     int32      `json:"receivedCount"`
	LastInteractionAt *time.Time `json:"lastInteractionAt,omitempty"`
}

// Blob is the content of a blob referenced by a draft or a queued message.
// Data is base64 in JSON; the exporter streams it.
type Blob struct {
	SHA256 string `json:"sha256"` // lower-case hex
	Size   int64  `json:"size"`
	Data   []byte `json:"data"`
}

// Attachment is an attachment of a draft (attachments row).
type Attachment struct {
	SHA256      *string `json:"sha256,omitempty"` // hex; nil while the content was never fetched
	Filename    string  `json:"filename"`
	ContentType string  `json:"contentType"`
	Size        int64   `json:"size"`
	ContentID   *string `json:"contentId,omitempty"`
	IsInline    bool    `json:"isInline"`
	PartPath    string  `json:"partPath"`
	// Listed: the attachment is in drafts.attachment_ids.
	Listed bool `json:"listed"`
}

// Draft is a drafts row with its local message (plan 3.8). Messages queued
// for sending are drafts too (Outbound refers to them).
type Draft struct {
	Account      AccountRef `json:"account"`
	SiloDraftID  string     `json:"siloDraftId"`
	MessageIDHdr string     `json:"messageIdHdr"`
	InReplyTo    *string    `json:"inReplyTo,omitempty"` // In-Reply-To of the message
	References   []string   `json:"references"`
	// ReplyToMessageIDHdr is the Message-ID of drafts.in_reply_to_message_id.
	ReplyToMessageIDHdr *string         `json:"replyToMessageIdHdr,omitempty"`
	FromEmail           string          `json:"fromEmail"`
	FromName            string          `json:"fromName"`
	To                  json.RawMessage `json:"to"`
	Cc                  json.RawMessage `json:"cc"`
	Bcc                 json.RawMessage `json:"bcc"`
	Subject             string          `json:"subject"`
	BodyHTML            string          `json:"bodyHtml"`
	BodyJSON            json.RawMessage `json:"bodyJson,omitempty"`
	Version             int32           `json:"version"`
	ServerSyncedVersion int32           `json:"serverSyncedVersion"`
	UpdatedAt           time.Time       `json:"updatedAt"`
	InternalDate        time.Time       `json:"internalDate"`
	// RawSHA256 is the stored MIME of a message queued for sending.
	RawSHA256   *string      `json:"rawSha256,omitempty"`
	RawSize     int64        `json:"rawSize,omitempty"`
	Attachments []Attachment `json:"attachments"`
}

// Outbound is a message waiting to be sent (status queued or sending).
type Outbound struct {
	Account     AccountRef `json:"account"`
	SiloDraftID string     `json:"siloDraftId"`
	SendAt      time.Time  `json:"sendAt"`
	Attempts    int32      `json:"attempts"`
}
