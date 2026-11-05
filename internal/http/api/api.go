// Package api defines the public REST API (huma operations mounted under /api/v1).
// The OpenAPI document is generated from these definitions; see cmd/silo-openapi.
package api

import (
	"log/slog"
	"net/url"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/silopost/silo-server/internal/accounts"
	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/drafts"
	"github.com/silopost/silo-server/internal/events"
	"github.com/silopost/silo-server/internal/mailview"
	"github.com/silopost/silo-server/internal/ops"
	silosync "github.com/silopost/silo-server/internal/sync"
)

// Prefix is the URL prefix of the API on the root router.
const Prefix = "/api/v1"

// Version is the major version of the API, reported by GET /meta.
const Version = "1"

// MinWebVersion is the oldest silo-web release compatible with this server.
const MinWebVersion = "0.0.0"

// Options carries the instance settings the API exposes.
type Options struct {
	ServerVersion      string
	AuthMode           string
	AllowCustomServers bool
	// MaxAttachmentBytes is SILO_MAX_ATTACHMENT_BYTES (GET /meta); when 0, the
	// limit of Drafts.
	MaxAttachmentBytes int64

	// Auth and Accounts back the session and account endpoints. They may be nil
	// when only the OpenAPI document is needed (cmd/silo-openapi).
	Auth     *auth.Service
	Accounts *accounts.Service
	// Sync backs sync-status, resync and folders.
	Sync *silosync.Service
	// Ops backs the thread and message modify endpoints.
	Ops *ops.Service
	// View backs the thread list, thread detail and message content endpoints.
	View *mailview.View
	// Drafts backs /drafts and /uploads.
	Drafts *drafts.Service
	// Events backs GET /events and /events/poll.
	Events *events.Hub
	// SSEHeartbeat is the interval of the ": ping" comment on /events (25 s when 0).
	SSEHeartbeat time.Duration
	// PublicURL (SILO_PUBLIC_URL) is the origin browsers use; it feeds the Origin
	// check and decides the Secure flag of the session cookie (https only).
	PublicURL string
	Logger    *slog.Logger
}

// deps is what the handlers share.
type deps struct {
	auth         *auth.Service
	accts        *accounts.Service
	sync         *silosync.Service
	ops          *ops.Service
	view         *mailview.View
	drafts       *drafts.Service
	events       *events.Hub
	heartbeat    time.Duration
	log          *slog.Logger
	publicOrigin string
	cookieSecure bool
}

func newDeps(o Options) *deps {
	d := &deps{auth: o.Auth, accts: o.Accounts, sync: o.Sync, ops: o.Ops, view: o.View, drafts: o.Drafts, events: o.Events, heartbeat: o.SSEHeartbeat, log: o.Logger}
	if d.heartbeat <= 0 {
		d.heartbeat = 25 * time.Second
	}
	if d.log == nil {
		d.log = slog.Default()
	}
	if u, err := url.Parse(o.PublicURL); err == nil && u.Host != "" {
		d.publicOrigin = u.Scheme + "://" + u.Host
		d.cookieSecure = u.Scheme == "https"
	}
	return d
}

// Mount registers the API under Prefix on root and returns the huma API,
// whose OpenAPI() holds the generated document. It needs no database.
func Mount(root chi.Router, o Options) huma.API {
	var a huma.API
	root.Route(Prefix, func(r chi.Router) {
		cfg := huma.DefaultConfig("SiloPost API", Version)
		cfg.Info.Description = "REST API of silo-server, the SiloPost backend."
		cfg.OpenAPI.Servers = []*huma.Server{{URL: Prefix}}
		// Keep responses exactly as the contract defines them: no "$schema"
		// property, no generated schema or docs routes (the docs page loads
		// assets from a CDN).
		cfg.CreateHooks = nil
		cfg.DocsPath = ""
		cfg.SchemasPath = ""

		cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
			"cookieAuth": {Type: "apiKey", In: "cookie", Name: SessionCookie, Description: "Session cookie set by POST /session."},
			"csrfHeader": {Type: "apiKey", In: "header", Name: CSRFHeader, Description: "csrfToken of the session; required on mutating requests."},
			"bearerAuth": {Type: "http", Scheme: "bearer", Description: "Session token returned by POST /session with client.kind=bearer " +
				"(native clients). No cookie and no CSRF header are used with it."},
		}

		a = humachi.New(r, cfg)
		d := newDeps(o)
		registerMeta(a, o)
		d.registerSession(a)
		d.registerAccounts(a)
		d.registerSync(a)
		d.registerThreads(a)
		d.registerSnooze(a)
		d.registerOperations(a)
		d.registerDrafts(a)
		d.registerSend(a)
		d.registerLabels(a)
		d.registerThreadViews(a)
		d.registerContent(a)
		d.registerSearch(a)
		d.registerSettings(a)
		d.registerEvents(a)
	})
	return a
}
