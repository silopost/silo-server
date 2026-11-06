package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/silopost/silo-server/internal/version"
)

func newTestRouter(o Options) http.Handler {
	r := chi.NewRouter()
	Mount(r, o)
	return r
}

func TestMeta(t *testing.T) {
	h := newTestRouter(Options{ServerVersion: "0.3.0", AuthMode: "imap", AllowCustomServers: false, MaxAttachmentBytes: 25 << 20})
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
		"minWebVersion":      "0.3.0", // silo-web of phase 4; phase 5 is additive
		"authMode":           "imap",
		"allowCustomServers": false,
		"maxAttachmentBytes": float64(25 << 20),
	}
	if p, ok := got["oauthProviders"].([]any); !ok || len(p) != 0 {
		t.Fatalf("oauthProviders = %#v, want []", got["oauthProviders"])
	}
	if len(got) != len(want)+1 {
		t.Fatalf("got %v, want %v and oauthProviders", got, want)
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

// semVer is SemVer 2.0.0 (semver.org), with an optional "v" prefix.
var semVer = regexp.MustCompile(`^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
	`(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
	`(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

// validServerVersion accepts a release (SemVer, as git describe gives it too),
// "dev" (local builds) and "edge-<sha>" (CI images of main).
func validServerVersion(v string) bool {
	if v == "dev" || semVer.MatchString(v) {
		return true
	}
	sha, ok := strings.CutPrefix(v, "edge-")
	return ok && regexp.MustCompile(`^[0-9a-f]{7,40}$`).MatchString(sha)
}

func TestMetaVersionsAreSemVer(t *testing.T) {
	for _, v := range []string{"0.2.0", "v0.3.0", "1.0.0-rc.1", "v0.3.0-12-gabc1234", "1.2.3+build.5"} {
		if !semVer.MatchString(v) {
			t.Errorf("%q rejected", v)
		}
	}
	for _, v := range []string{"", "0.2", "v01.2.3", "1.2.3-", "latest", "dev"} {
		if semVer.MatchString(v) {
			t.Errorf("%q accepted", v)
		}
	}
	for v, ok := range map[string]bool{
		"dev": true, "edge-0123456789abcdef0123456789abcdef01234567": true, "v0.4.0": true,
		"edge-": false, "edge-xyz": false, "nightly": false,
	} {
		if validServerVersion(v) != ok {
			t.Errorf("validServerVersion(%q) = %v", v, !ok)
		}
	}

	for _, sv := range []string{version.Version, "v0.4.0", "edge-0123456789abcdef0123456789abcdef01234567"} {
		h := newTestRouter(Options{ServerVersion: sv, AuthMode: "imap"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil))
		var m Meta
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		if !semVer.MatchString(m.MinWebVersion) {
			t.Errorf("minWebVersion %q is not SemVer", m.MinWebVersion)
		}
		if !validServerVersion(m.ServerVersion) {
			t.Errorf("serverVersion %q is neither SemVer, dev nor edge-<sha>", m.ServerVersion)
		}
	}
}
