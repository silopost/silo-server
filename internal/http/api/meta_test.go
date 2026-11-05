package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func newTestRouter(o Options) http.Handler {
	r := chi.NewRouter()
	Mount(r, o)
	return r
}

func TestMeta(t *testing.T) {
	h := newTestRouter(Options{ServerVersion: "0.3.0", AuthMode: "imap", AllowCustomServers: false})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	// Exact key set: the contract has no extra properties (no "$schema").
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"serverVersion":      "0.3.0",
		"apiVersion":         "1",
		"minWebVersion":      MinWebVersion,
		"authMode":           "imap",
		"allowCustomServers": false,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestOpenAPIServed(t *testing.T) {
	h := newTestRouter(Options{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"/meta"`) {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}
