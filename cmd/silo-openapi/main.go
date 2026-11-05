// Command silo-openapi writes the OpenAPI 3.1 document of the REST API to a
// file (default api/openapi.yaml). It needs no database or configuration, and
// its output is deterministic so CI can diff it against the committed copy.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-chi/chi/v5"

	"github.com/silopost/silo-server/internal/http/api"
)

func main() {
	out := flag.String("o", "api/openapi.yaml", "output file")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintf(os.Stderr, "silo-openapi: %v\n", err)
		os.Exit(1)
	}
}

func run(out string) error {
	// Instance-specific values never appear in the document, so fixed options are fine.
	a := api.Mount(chi.NewRouter(), api.Options{})
	doc, err := a.OpenAPI().YAML()
	if err != nil {
		return fmt.Errorf("render openapi: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(out, doc, 0o644)
}
