package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"

	"github.com/silopost/silo-server/internal/secrets"
)

// Errors of the Service.
var (
	// ErrNotConfigured: the provider has no client id/secret on this instance.
	ErrNotConfigured = errors.New("oauth: provider not configured")
	// ErrInvalidReturnTo: returnTo is not a relative path of Silo.
	ErrInvalidReturnTo = errors.New("oauth: returnTo must be a path starting with a single /")
	// ErrReauthRequired: the provider refused the refresh token
	// (invalid_grant: revoked, expired, password changed). Only a new
	// sign-in helps; the account goes to auth_failed.
	ErrReauthRequired = errors.New("oauth: authorization expired")
)

// ReauthMessage is accounts.sync_error of an account whose refresh token the
// provider refused.
const ReauthMessage = "OAuth authorization expired; sign in again"

// Error codes of a failed authorization, sent back to the web client as
// ?oauth=error&code=<code>.
const (
	CodeDenied         = "oauth_denied"
	CodeStateInvalid   = "oauth_state_invalid"
	CodeExchangeFailed = "oauth_exchange_failed"
	CodeNoEmail        = "oauth_no_email"
)

// FlowError is a failed callback; Code is one of the Code* constants.
type FlowError struct {
	Code string
	Err  error
}

func (e *FlowError) Error() string { return "oauth: " + e.Code + ": " + e.Err.Error() }
func (e *FlowError) Unwrap() error { return e.Err }

func flowErr(code string, format string, args ...any) *FlowError {
	return &FlowError{Code: code, Err: fmt.Errorf(format, args...)}
}

// StateTTL is how long a started authorization may take.
const StateTTL = 10 * time.Minute

// refreshMargin: an access token is renewed when it expires within this.
const refreshMargin = 60 * time.Second

// Options configures a Service.
type Options struct {
	Pool *pgxpool.Pool
	Keys *secrets.Keyring
	// Providers are the configured providers (NewProviders).
	Providers map[string]*Provider
	// PublicURL is SILO_PUBLIC_URL, the base of the redirect URI.
	PublicURL string
	// HTTPClient talks to the token and userinfo endpoints (30 s timeout
	// when nil).
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// Service runs the authorization flow and hands out access tokens.
type Service struct {
	o         Options
	providers map[string]*Provider
	client    *http.Client
	log       *slog.Logger

	mu    sync.Mutex
	cache map[int64]*cached // per account
}

// New returns a Service.
func New(o Options) *Service {
	s := &Service{o: o, providers: map[string]*Provider{}, client: o.HTTPClient, log: o.Logger, cache: map[int64]*cached{}}
	if s.client == nil {
		s.client = &http.Client{Timeout: 30 * time.Second}
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.log = s.log.With("component", "oauth")
	for name, p := range o.Providers {
		cp := *p
		cp.OAuth2.RedirectURL = s.RedirectURI(name)
		s.providers[name] = &cp
	}
	return s
}

// Configured lists the configured provider names (google first); never nil.
func (s *Service) Configured() []string {
	out := []string{}
	if s == nil {
		return out
	}
	for name := range s.providers {
		out = append(out, name)
	}
	slices.Sort(out) // google < microsoft
	return out
}

// Provider returns a configured provider.
func (s *Service) Provider(name string) (*Provider, bool) {
	if s == nil {
		return nil, false
	}
	p, ok := s.providers[name]
	return p, ok
}

// RedirectURI is the callback URL the admin registers with the provider.
func (s *Service) RedirectURI(name string) string {
	return strings.TrimRight(s.o.PublicURL, "/") + fmt.Sprintf(CallbackPath, name)
}

func (s *Service) httpCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, s.client)
}

// CleanReturnTo validates where the browser goes after the callback: a path
// of Silo ("/" when empty), never another origin ("//host", "/\host").
func CleanReturnTo(p string) (string, error) {
	if p == "" {
		return "/", nil
	}
	if len(p) > 1024 || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsRune(p, '\\') ||
		strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", ErrInvalidReturnTo
	}
	return p, nil
}

// StartRequest is a new authorization.
type StartRequest struct {
	UserID int64
	// AccountID is the account to re-authorize (0: a new account).
	AccountID int64
	// LoginHint pre-fills the address at the provider (re-authorization).
	LoginHint string
	ReturnTo  string
}

// Start records a pending authorization and returns the provider's URL to
// open in the browser and its state.
func (s *Service) Start(ctx context.Context, name string, r StartRequest) (authURL, state string, err error) {
	p, ok := s.Provider(name)
	if !ok {
		return "", "", ErrNotConfigured
	}
	returnTo, err := CleanReturnTo(r.ReturnTo)
	if err != nil {
		return "", "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	state = base64.RawURLEncoding.EncodeToString(b)
	verifier := oauth2.GenerateVerifier()
	var account *int64
	if r.AccountID != 0 {
		account = &r.AccountID
	}
	if _, err := s.o.Pool.Exec(ctx, `
INSERT INTO oauth_states (state, user_id, provider, code_verifier, account_id, return_to, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, now() + make_interval(secs => $7))`,
		state, r.UserID, name, verifier, account, returnTo, StateTTL.Seconds()); err != nil {
		return "", "", fmt.Errorf("oauth: store state: %w", err)
	}
	opts := append(slices.Clone(p.AuthParams), oauth2.S256ChallengeOption(verifier))
	if r.LoginHint != "" {
		opts = append(opts, oauth2.SetAuthURLParam("login_hint", r.LoginHint))
	}
	return p.OAuth2.AuthCodeURL(state, opts...), state, nil
}

// Grant is a completed authorization.
type Grant struct {
	UserID int64
	// AccountID is the account being re-authorized (0: new account).
	AccountID int64
	// ReturnTo is the path of Start ("/" when the state was not valid).
	ReturnTo string
	Provider *Provider
	// Email is the address of the mailbox, also its IMAP/SMTP user name.
	Email string
	Name  string
	Token *oauth2.Token
}

// Complete finishes an authorization at the callback: it consumes the state
// (single use), exchanges the code with the PKCE verifier and reads the
// address of the account. providerErr is the error parameter the provider
// sent instead of a code. A *FlowError is returned with a Grant whose
// ReturnTo is known once the state matched.
func (s *Service) Complete(ctx context.Context, name, state, code, providerErr string) (*Grant, error) {
	p, ok := s.Provider(name)
	if !ok {
		return nil, ErrNotConfigured
	}
	g := &Grant{ReturnTo: "/", Provider: p}
	if state == "" {
		return g, flowErr(CodeStateInvalid, "no state")
	}
	var verifier string
	var account *int64
	var valid bool
	err := s.o.Pool.QueryRow(ctx, `
DELETE FROM oauth_states WHERE state = $1 AND provider = $2
RETURNING user_id, code_verifier, account_id, return_to, expires_at > now()`, state, name).
		Scan(&g.UserID, &verifier, &account, &g.ReturnTo, &valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, flowErr(CodeStateInvalid, "unknown or used state")
	}
	if err != nil {
		return g, err
	}
	if account != nil {
		g.AccountID = *account
	}
	if !valid {
		return g, flowErr(CodeStateInvalid, "the authorization took longer than %s", StateTTL)
	}
	if providerErr != "" {
		return g, flowErr(CodeDenied, "provider error %q", providerErr)
	}
	if code == "" {
		return g, flowErr(CodeExchangeFailed, "no code")
	}
	tok, err := p.OAuth2.Exchange(s.httpCtx(ctx), code, oauth2.VerifierOption(verifier))
	if err != nil {
		return g, &FlowError{Code: CodeExchangeFailed, Err: err}
	}
	if tok.RefreshToken == "" {
		return g, flowErr(CodeExchangeFailed, "the provider returned no refresh token")
	}
	g.Token = tok
	id := idTokenClaims(tok)
	if id.Email == "" && p.UserinfoURL != "" {
		u, err := s.userinfo(ctx, p, tok)
		if err != nil {
			s.log.Warn("userinfo", "provider", name, "error", err)
		}
		id.merge(u)
	}
	if id.Email == "" {
		return g, flowErr(CodeNoEmail, "neither the id_token nor userinfo has a verified email")
	}
	g.Email, g.Name = id.Email, id.Name
	return g, nil
}

// identity is what the OpenID Connect claims say about the account.
type identity struct{ Email, Name string }

func (i *identity) merge(o identity) {
	if i.Email == "" {
		i.Email = o.Email
	}
	if i.Name == "" {
		i.Name = o.Name
	}
}

type claims struct {
	Email             string `json:"email"`
	EmailVerified     any    `json:"email_verified"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
}

func (c claims) identity() identity {
	id := identity{Name: strings.TrimSpace(c.Name)}
	// An explicit email_verified=false (Google: an address that does not
	// belong to the Google account) is not the mailbox.
	verified := true
	switch v := c.EmailVerified.(type) {
	case bool:
		verified = v
	case string:
		verified = v != "false"
	}
	email := c.Email
	// Microsoft omits email for accounts without one in Entra ID; the
	// preferred_username of a mailbox user is its UPN, the IMAP user name.
	if email == "" && strings.Contains(c.PreferredUsername, "@") {
		email = c.PreferredUsername
	}
	if verified {
		id.Email = strings.ToLower(strings.TrimSpace(email))
	}
	return id
}

// idTokenClaims reads the claims of the OpenID Connect id_token returned with
// the access token. The signature is not verified: the token comes straight
// from the provider's token endpoint over TLS, in the response to our own
// authenticated request (client secret + PKCE verifier), so nothing between
// the provider and us could have forged it (OpenID Connect Core 3.1.3.7
// allows TLS server validation in place of the signature check for this
// case). An id_token is never accepted from the browser.
func idTokenClaims(tok *oauth2.Token) identity {
	raw, _ := tok.Extra("id_token").(string)
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return identity{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return identity{}
	}
	var c claims
	if json.Unmarshal(payload, &c) != nil {
		return identity{}
	}
	return c.identity()
}

// userinfo asks the OpenID Connect userinfo endpoint.
func (s *Service) userinfo(ctx context.Context, p *Provider, tok *oauth2.Token) (identity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.UserinfoURL, nil)
	if err != nil {
		return identity{}, err
	}
	tok.SetAuthHeader(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return identity{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return identity{}, fmt.Errorf("userinfo: HTTP %d", resp.StatusCode)
	}
	var c claims
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&c); err != nil {
		return identity{}, fmt.Errorf("userinfo: %w", err)
	}
	return c.identity(), nil
}

// PurgeStates deletes authorizations that were never completed (the blob_gc
// job runs it).
func (s *Service) PurgeStates(ctx context.Context, _ time.Time) error {
	_, err := s.o.Pool.Exec(ctx, `DELETE FROM oauth_states WHERE expires_at < now()`)
	return err
}

// cached is the token source of one account.
type cached struct {
	mu     sync.Mutex
	sealed []byte // accounts.secret_enc it was built from
	src    *refresher
	ts     oauth2.TokenSource
	stored string // refresh token in sealed
}

// refresher always asks the token endpoint (the ReuseTokenSource around it
// caches); it keeps the refresh token the provider may rotate.
type refresher struct {
	ctx    context.Context
	cfg    *oauth2.Config
	rt     string
	scoped bool // send scope (Provider.RefreshScopes)
	client *http.Client
}

func (r *refresher) Token() (*oauth2.Token, error) {
	var tok *oauth2.Token
	var err error
	if r.scoped {
		tok, err = r.refreshScoped()
	} else {
		tok, err = r.cfg.TokenSource(r.ctx, &oauth2.Token{RefreshToken: r.rt}).Token()
	}
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken != "" {
		r.rt = tok.RefreshToken
	}
	return tok, nil
}

// refreshScoped is the refresh_token grant with the scopes of the
// authorization, which the Microsoft identity platform (v2.0) requires and
// golang.org/x/oauth2 does not send. Errors are *oauth2.RetrieveError as
// from x/oauth2.
func (r *refresher) refreshScoped() (*oauth2.Token, error) {
	form := url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {r.rt}, "scope": {strings.Join(r.cfg.Scopes, " ")},
		"client_id": {r.cfg.ClientID}, "client_secret": {r.cfg.ClientSecret},
	}
	req, err := http.NewRequestWithContext(r.ctx, http.MethodPost, r.cfg.Endpoint.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth2: cannot fetch token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("oauth2: cannot fetch token: %w", err)
	}
	var tj struct {
		AccessToken      string `json:"access_token"`
		TokenType        string `json:"token_type"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &tj)
	if resp.StatusCode != http.StatusOK || tj.AccessToken == "" {
		return nil, &oauth2.RetrieveError{Response: resp, Body: body, ErrorCode: tj.Error, ErrorDescription: tj.ErrorDescription}
	}
	tok := &oauth2.Token{AccessToken: tj.AccessToken, TokenType: tj.TokenType, RefreshToken: tj.RefreshToken}
	if tj.ExpiresIn > 0 {
		tok.Expiry = time.Now().Add(time.Duration(tj.ExpiresIn) * time.Second)
	}
	return tok, nil
}

// Token returns a valid access token of an xoauth2 account, refreshed when
// it expires within a minute (called before every IMAP or SMTP connection;
// usually it is the cached one). A refresh token rotated by the provider is
// stored again. A refused refresh token yields ErrReauthRequired.
func (s *Service) Token(ctx context.Context, accountID int64) (string, error) {
	var provider, authType string
	var sealed []byte
	err := s.o.Pool.QueryRow(ctx, `SELECT provider, auth_type, secret_enc FROM accounts WHERE id = $1`, accountID).
		Scan(&provider, &authType, &sealed)
	if err != nil {
		return "", fmt.Errorf("oauth: load account %d: %w", accountID, err)
	}
	if authType != "xoauth2" {
		return "", fmt.Errorf("oauth: account %d does not use OAuth", accountID)
	}
	p, ok := s.Provider(ProviderName(provider))
	if !ok {
		return "", fmt.Errorf("%w: %s (account %d)", ErrNotConfigured, ProviderName(provider), accountID)
	}

	s.mu.Lock()
	e := s.cache[accountID]
	if e == nil || !bytes.Equal(e.sealed, sealed) {
		rt, err := s.o.Keys.Decrypt(sealed, aad(accountID))
		if err != nil {
			s.mu.Unlock()
			return "", fmt.Errorf("oauth: decrypt refresh token of account %d: %w", accountID, err)
		}
		src := &refresher{ctx: s.httpCtx(context.Background()), cfg: &p.OAuth2, rt: string(rt), scoped: p.RefreshScopes, client: s.client}
		e = &cached{sealed: sealed, src: src, stored: string(rt), ts: oauth2.ReuseTokenSourceWithExpiry(nil, src, refreshMargin)}
		s.cache[accountID] = e
	}
	s.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	tok, err := e.ts.Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
			s.mu.Lock()
			if s.cache[accountID] == e {
				delete(s.cache, accountID)
			}
			s.mu.Unlock()
			return "", fmt.Errorf("%w: %s", ErrReauthRequired, strings.TrimSpace(re.ErrorDescription))
		}
		return "", fmt.Errorf("oauth: refresh the access token of account %d: %w", accountID, err)
	}
	if e.src.rt != e.stored {
		if err := s.storeRefreshToken(ctx, accountID, e); err != nil {
			// The old token usually stays valid for a while; retry next time.
			s.log.Warn("store rotated refresh token", "account", accountID, "error", err)
		}
	}
	return tok.AccessToken, nil
}

// storeRefreshToken saves a rotated refresh token unless the account was
// re-authorized meanwhile (secret_enc changed).
func (s *Service) storeRefreshToken(ctx context.Context, accountID int64, e *cached) error {
	sealed, err := s.o.Keys.Encrypt([]byte(e.src.rt), aad(accountID))
	if err != nil {
		return err
	}
	if _, err := s.o.Pool.Exec(context.WithoutCancel(ctx), `UPDATE accounts SET secret_enc = $2 WHERE id = $1 AND secret_enc = $3`,
		accountID, sealed, e.sealed); err != nil {
		return err
	}
	s.mu.Lock()
	e.sealed, e.stored = sealed, e.src.rt
	s.mu.Unlock()
	return nil
}

// aad binds a sealed secret to its account (as the password of accounts).
func aad(accountID int64) []byte { return []byte(strconv.FormatInt(accountID, 10)) }

// Forget drops the cached access token of an account (the server refused it
// before its expiry, e.g. access revoked): the next Token refreshes.
func (s *Service) Forget(accountID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, accountID)
}
