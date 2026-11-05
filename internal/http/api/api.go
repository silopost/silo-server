// Package api defines the public REST API (huma operations mounted under /api/v1).
// The OpenAPI document is generated from these definitions; see cmd/silo-openapi.
package api

import (
	"log/slog"
	"net/url"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/silopost/silo-server/internal/accounts"
	"github.com/silopost/silo-server/internal/auth"
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

	// Auth and Accounts back the session and account endpoints. They may be nil
	// when only the OpenAPI document is needed (cmd/silo-openapi).
	Auth     *auth.Service
	Accounts *accounts.Service
	// PublicURL (SILO_PUBLIC_URL) is the origin browsers use; it feeds the Origin
	// check and decides the Secure flag of the session cookie (https only).
	PublicURL string
	Logger    *slog.Logger
}

// deps is what the handlers share.
type deps struct {
	auth         *auth.Service
	accts        *accounts.Service
	log          *slog.Logger
	publicOrigin string
	cookieSecure bool
}

func newDeps(o Options) *deps {
	d := &deps{auth: o.Auth, accts: o.Accounts, log: o.Logger}
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
		}

		a = humachi.New(r, cfg)
		d := newDeps(o)
		registerMeta(a, o)
		d.registerSession(a)
		d.registerAccounts(a)
	})
	return a
}
