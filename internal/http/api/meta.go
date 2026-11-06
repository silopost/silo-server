package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// Meta describes the server to clients; the frontend uses it to detect
// incompatible versions and to shape the login screen.
type Meta struct {
	ServerVersion      string `json:"serverVersion" doc:"Version of silo-server." example:"0.3.0"`
	APIVersion         string `json:"apiVersion" doc:"Major version of the REST API." example:"1"`
	MinWebVersion      string `json:"minWebVersion" doc:"Oldest silo-web release this server supports." example:"0.3.0"`
	AuthMode           string `json:"authMode" enum:"imap,local" doc:"How users sign in: with their mailbox credentials (imap) or Silo accounts (local)."`
	AllowCustomServers bool   `json:"allowCustomServers" doc:"Whether users may add accounts on arbitrary IMAP/SMTP hosts."`
	MaxAttachmentBytes int64  `json:"maxAttachmentBytes" doc:"Limit in bytes of one upload and of a sent message with its attachments (SILO_MAX_ATTACHMENT_BYTES); larger ones are 413 upload_too_large / message_too_large." example:"26214400"`

	OAuthProviders []string `json:"oauthProviders" doc:"OAuth2 providers configured on the instance (google, microsoft): silo-web offers \"Sign in with ...\" for them (POST /oauth/{provider}/start). Empty when none."`
}

// MetaOutput is the response of GET /meta.
type MetaOutput struct {
	Body Meta
}

func registerMeta(a huma.API, o Options) {
	if o.MaxAttachmentBytes == 0 && o.Drafts != nil {
		o.MaxAttachmentBytes = o.Drafts.MaxUploadBytes()
	}
	meta := Meta{
		ServerVersion:      o.ServerVersion,
		APIVersion:         Version,
		MinWebVersion:      MinWebVersion,
		AuthMode:           o.AuthMode,
		AllowCustomServers: o.AllowCustomServers,
		MaxAttachmentBytes: o.MaxAttachmentBytes,
		OAuthProviders:     o.OAuth.Configured(),
	}
	huma.Register(a, huma.Operation{
		OperationID: "getMeta",
		Method:      http.MethodGet,
		Path:        "/meta",
		Summary:     "Get server metadata",
		Description: "Public endpoint: versions and instance settings needed before signing in.",
		Tags:        []string{"meta"},
	}, func(context.Context, *struct{}) (*MetaOutput, error) {
		return &MetaOutput{Body: meta}, nil
	})
}
