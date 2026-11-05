// Package version exposes the build version, injected at link time:
//
//	go build -ldflags "-X github.com/silopost/silo-server/internal/version.Version=v0.1.0"
package version

// Version is the server version; "dev" for local builds.
var Version = "dev"
