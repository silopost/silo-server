// Package api defines the public REST API (huma operations mounted under /api/v1).
// The OpenAPI document is generated from these definitions; see cmd/silo-openapi.
package api

import (
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
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

		a = humachi.New(r, cfg)
		registerMeta(a, o)
	})
	return a
}
