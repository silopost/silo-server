// Package oauth signs mail accounts in with OAuth2 and authenticates their
// IMAP and SMTP connections with XOAUTH2 (plan 3.15, phase 5): Gmail and
// Microsoft 365, whose servers refuse (Microsoft) or discourage (Google)
// passwords. The flow is authorization code with PKCE (S256); the refresh
// token is stored encrypted in accounts.secret_enc like a password and a
// fresh access token is obtained before every connection.
package oauth

import (
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/endpoints"

	"github.com/silopost/silo-server/internal/config"
)

// CallbackPath is the path of the redirect URI under SILO_PUBLIC_URL; %s is
// the provider name. The admin registers SILO_PUBLIC_URL + this path with the
// provider. It must match the API route (internal/http/api, Prefix +
// "/oauth/{provider}/callback").
const CallbackPath = "/api/v1/oauth/%s/callback"

// Server is the IMAP or SMTP server of a provider.
type Server struct {
	Host string
	Port int
	TLS  string // implicit, starttls (none only in tests)
}

// Provider is an OAuth2 provider with the mail servers of its accounts.
type Provider struct {
	// Name is the API name: google or microsoft.
	Name string
	// AccountProvider is accounts.provider of its accounts: gmail or microsoft.
	AccountProvider string
	// OAuth2 is the client configuration; RedirectURL is set by the Service.
	OAuth2 oauth2.Config
	// AuthParams are extra parameters of the authorization URL.
	AuthParams []oauth2.AuthCodeOption
	// UserinfoURL (OpenID Connect) is asked for the address when the
	// id_token has no email claim.
	UserinfoURL string
	IMAP, SMTP  Server
	// RefreshScopes sends the scopes with every refresh_token grant
	// (Microsoft identity platform v2.0).
	RefreshScopes bool
	// SentAppend is accounts.sent_append of its accounts: whether Silo saves
	// sent mail to the Sent folder itself.
	SentAppend bool
}

// Default endpoints of Google's OpenID Connect userinfo and of Microsoft Graph's.
const (
	googleUserinfo    = "https://openidconnect.googleapis.com/v1/userinfo"
	microsoftUserinfo = "https://graph.microsoft.com/oidc/userinfo"
)

// NewProviders returns the providers configured on the instance, by name.
func NewProviders(c config.OAuthConfig) map[string]*Provider {
	out := map[string]*Provider{}
	if c.GoogleEnabled() {
		ep := endpoints.Google
		ep.DeviceAuthURL = ""
		out[config.OAuthGoogle] = &Provider{
			Name: config.OAuthGoogle, AccountProvider: "gmail",
			OAuth2: oauth2.Config{
				ClientID: c.GoogleClientID, ClientSecret: c.GoogleClientSecret,
				Endpoint: endpoint(ep, c.GoogleAuthURL, c.GoogleTokenURL),
				// https://mail.google.com/ is the only scope that allows
				// IMAP and SMTP; openid email name the account.
				Scopes: []string{"https://mail.google.com/", "openid", "email"},
			},
			// Google returns a refresh token only on the first consent of a
			// client unless asked offline access with a new consent screen;
			// a re-authorization must get one too.
			AuthParams:  []oauth2.AuthCodeOption{oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent")},
			UserinfoURL: or(c.GoogleUserinfoURL, googleUserinfo),
			IMAP:        Server{Host: "imap.gmail.com", Port: 993, TLS: config.TLSImplicit},
			// 465 with TLS from the first byte rather than 587 + STARTTLS:
			// nothing travels in clear text before the handshake (no
			// STARTTLS stripping to defend against) and the mode is the same
			// as IMAP's 993. Gmail offers both.
			SMTP: Server{Host: "smtp.gmail.com", Port: 465, TLS: config.TLSImplicit},
			// Gmail stores what is sent through its SMTP in [Gmail]/Sent Mail.
			SentAppend: false,
		}
	}
	if c.MicrosoftEnabled() {
		out[config.OAuthMicrosoft] = &Provider{
			Name: config.OAuthMicrosoft, AccountProvider: "microsoft",
			OAuth2: oauth2.Config{
				ClientID: c.MicrosoftClientID, ClientSecret: c.MicrosoftClientSecret,
				Endpoint: endpoint(endpoints.AzureAD(or(c.MicrosoftTenant, "common")), c.MicrosoftAuthURL, c.MicrosoftTokenURL),
				Scopes: []string{
					"https://outlook.office.com/IMAP.AccessAsUser.All", "https://outlook.office.com/SMTP.Send",
					"offline_access", "openid", "email",
				},
			},
			UserinfoURL:   or(c.MicrosoftUserinfoURL, microsoftUserinfo),
			RefreshScopes: true,
			IMAP:          Server{Host: "outlook.office365.com", Port: 993, TLS: config.TLSImplicit},
			// Exchange Online offers SMTP AUTH only on 587 with STARTTLS.
			SMTP: Server{Host: "smtp.office365.com", Port: 587, TLS: config.TLSStartTLS},
			// Plan 3.9: Exchange Online saves mail submitted with SMTP AUTH
			// in Sent Items itself (as Gmail does); a copy appended by Silo
			// would show twice in other clients.
			SentAppend: false,
		}
	}
	return out
}

func endpoint(ep oauth2.Endpoint, authURL, tokenURL string) oauth2.Endpoint {
	ep.AuthURL = or(authURL, ep.AuthURL)
	ep.TokenURL = or(tokenURL, ep.TokenURL)
	return ep
}

// ProviderName maps accounts.provider (gmail, microsoft) to the provider name.
func ProviderName(accountProvider string) string {
	switch accountProvider {
	case "gmail":
		return config.OAuthGoogle
	case "microsoft":
		return config.OAuthMicrosoft
	}
	return accountProvider
}

func or(v, def string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return def
}
