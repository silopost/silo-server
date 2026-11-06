// Package oauthtest is a fake OAuth2 / OpenID Connect provider for tests of
// XOAUTH2 accounts: authorization endpoint with PKCE (S256 only), token
// endpoint (authorization_code, refresh_token), userinfo and an RFC 7662
// introspection endpoint that Dovecot's oauth2 passdb asks (imaptest
// profile oauth2). Nothing talks to Google or Microsoft.
package oauthtest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Client credentials the server accepts.
const (
	ClientID     = "silo-test-client"
	ClientSecret = "silo-test-secret"
)

// Server is the fake provider. Configure the exported fields before use;
// they are read under the lock.
type Server struct {
	URL string // http://127.0.0.1:port

	mu sync.Mutex
	// Email and Name are the identity of the signed-in user.
	Email, Name string
	// IDTokenEmail: whether the id_token carries the email claim (else only
	// userinfo has it).
	IDTokenEmail bool
	// ExpiresIn is the lifetime of access tokens in seconds.
	ExpiresIn int
	// Deny makes /authorize answer error=access_denied.
	Deny bool
	// FailRefresh makes refresh_token grants fail with invalid_grant.
	FailRefresh bool
	// RotateRefresh makes refresh_token grants return a new refresh token.
	RotateRefresh bool

	codes     map[string]grant  // authorization codes
	refresh   map[string]string // refresh token -> email
	access    map[string]string // access token -> email
	refreshes int
	exchanges int
	lastAuth  url.Values
}

type grant struct{ challenge, redirect, email string }

// Start runs a Server until the test ends. It listens on 127.0.0.1, reachable
// from containers as host.docker.internal on Docker Desktop; on Linux it
// listens on all interfaces so that the host gateway reaches it.
func Start(t *testing.T) *Server {
	t.Helper()
	s := &Server{Email: "oauth@silo.test", Name: "OAuth User", IDTokenEmail: true, ExpiresIn: 3600,
		codes: map[string]grant{}, refresh: map[string]string{}, access: map[string]string{}}
	addr := "127.0.0.1:0"
	if runtime.GOOS == "linux" {
		addr = ":0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", s.authorize)
	mux.HandleFunc("POST /token", s.token)
	mux.HandleFunc("GET /userinfo", s.userinfo)
	mux.HandleFunc("POST /introspect", s.introspect)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	s.URL = fmt.Sprintf("http://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port)
	return s
}

// DockerURL is the URL of the server as seen from a container.
func (s *Server) DockerURL() string {
	return strings.Replace(s.URL, "127.0.0.1", "host.docker.internal", 1)
}

// Set changes the configuration under the lock.
func (s *Server) Set(fn func(s *Server)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

// Refreshes is the number of successful refresh_token grants.
func (s *Server) Refreshes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshes
}

// Exchanges is the number of successful authorization_code grants.
func (s *Server) Exchanges() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exchanges
}

// LastAuthorize returns the query of the last /authorize request.
func (s *Server) LastAuthorize() url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAuth
}

// ValidAccessToken reports whether tok is an access token the server issued.
func (s *Server) ValidAccessToken(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.access[tok]
	return ok
}

// Authorize plays the browser at the provider: it opens authURL (the user
// consents at once) and returns the redirect it answers with (the callback
// URL with code and state, or error).
func (s *Server) Authorize(t *testing.T, authURL string) *url.URL {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: HTTP %d", resp.StatusCode)
	}
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func random() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAuth = q
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || redirect.Scheme == "" || q.Get("client_id") != ClientID || q.Get("response_type") != "code" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	back := redirect.Query()
	back.Set("state", q.Get("state"))
	switch {
	case q.Get("state") == "":
		http.Error(w, "state required", http.StatusBadRequest)
		return
	case q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43:
		// PKCE is mandatory here (as for public clients at Google and Microsoft).
		back.Set("error", "invalid_request")
		back.Set("error_description", "PKCE S256 code_challenge required")
	case s.Deny:
		back.Set("error", "access_denied")
	default:
		code := random()
		s.codes[code] = grant{challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), email: s.Email}
		back.Set("code", code)
	}
	redirect.RawQuery = back.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func tokenError(w http.ResponseWriter, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, "invalid_request", err.Error())
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != ClientID || secret != ClientSecret {
		tokenError(w, "invalid_client", "bad client credentials")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var email, refresh string
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		g, ok := s.codes[r.PostForm.Get("code")]
		delete(s.codes, r.PostForm.Get("code"))
		if !ok || g.redirect != r.PostForm.Get("redirect_uri") {
			tokenError(w, "invalid_grant", "unknown code or redirect_uri")
			return
		}
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			tokenError(w, "invalid_grant", "code_verifier does not match the code_challenge")
			return
		}
		email, refresh = g.email, random()
		s.refresh[refresh] = email
		s.exchanges++
	case "refresh_token":
		e, ok := s.refresh[r.PostForm.Get("refresh_token")]
		if !ok || s.FailRefresh {
			tokenError(w, "invalid_grant", "Token has been expired or revoked.")
			return
		}
		email = e
		if s.RotateRefresh {
			delete(s.refresh, r.PostForm.Get("refresh_token"))
			refresh = random()
			s.refresh[refresh] = email
		}
		s.refreshes++
	default:
		tokenError(w, "unsupported_grant_type", r.PostForm.Get("grant_type"))
		return
	}
	access := "ya29." + random()
	s.access[access] = email
	out := map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": s.ExpiresIn}
	if refresh != "" {
		out["refresh_token"] = refresh
	}
	if r.PostForm.Get("grant_type") == "authorization_code" {
		claims := map[string]any{"iss": s.URL, "aud": ClientID, "sub": "1234", "name": s.Name}
		if s.IDTokenEmail {
			claims["email"], claims["email_verified"] = email, true
		}
		out["id_token"] = IDToken(claims)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// IDToken builds an unsigned JWT ("alg":"none") with the given claims; Silo
// reads only the payload of an id_token from the token endpoint.
func IDToken(claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none", "typ": "JWT"}) + "." + enc(claims) + "."
}

func (s *Server) userinfo(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	email, ok := s.access[tok]
	name := s.Name
	s.mu.Unlock()
	if !ok {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"sub": "1234", "email": email, "email_verified": true, "name": name})
}

// introspect answers Dovecot's oauth2 passdb (introspection_mode = post).
func (s *Server) introspect(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	s.mu.Lock()
	email, ok := s.access[r.PostForm.Get("token")]
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		_ = json.NewEncoder(w).Encode(map[string]any{"active": false})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "email": email})
}

// IssueAccessToken returns a new access token of email, as if the token
// endpoint had issued it (for tests that need a token but not the flow).
func (s *Server) IssueAccessToken(email string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok := "ya29." + random()
	s.access[tok] = email
	return tok
}

// RevokeAccessTokens makes every access token issued so far invalid (the
// introspection endpoint reports them inactive).
func (s *Server) RevokeAccessTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.access = map[string]string{}
}
