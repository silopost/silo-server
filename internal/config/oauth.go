package config

import "fmt"

// OAuth provider names: the {provider} of /oauth/{provider}/... and the
// entries of GET /meta oauthProviders.
const (
	OAuthGoogle    = "google"
	OAuthMicrosoft = "microsoft"
)

// OAuthConfig holds the OAuth2 applications registered by the admin for
// XOAUTH2 accounts (plan 3.15, phase 5). A provider is enabled when both its
// client id and secret are set. The *_URL fields only override the official
// endpoints for tests against a fake authorization server; leave them empty
// in production.
type OAuthConfig struct {
	GoogleClientID     string `env:"SILO_OAUTH_GOOGLE_CLIENT_ID"`
	GoogleClientSecret string `env:"SILO_OAUTH_GOOGLE_CLIENT_SECRET"`
	GoogleAuthURL      string `env:"SILO_OAUTH_GOOGLE_AUTH_URL"`
	GoogleTokenURL     string `env:"SILO_OAUTH_GOOGLE_TOKEN_URL"`
	GoogleUserinfoURL  string `env:"SILO_OAUTH_GOOGLE_USERINFO_URL"`

	MicrosoftClientID     string `env:"SILO_OAUTH_MICROSOFT_CLIENT_ID"`
	MicrosoftClientSecret string `env:"SILO_OAUTH_MICROSOFT_CLIENT_SECRET"`
	// MicrosoftTenant is the Entra ID tenant of the endpoints: common (any
	// work, school or personal account), organizations, consumers or a
	// tenant id/domain.
	MicrosoftTenant      string `env:"SILO_OAUTH_MICROSOFT_TENANT" envDefault:"common"`
	MicrosoftAuthURL     string `env:"SILO_OAUTH_MICROSOFT_AUTH_URL"`
	MicrosoftTokenURL    string `env:"SILO_OAUTH_MICROSOFT_TOKEN_URL"`
	MicrosoftUserinfoURL string `env:"SILO_OAUTH_MICROSOFT_USERINFO_URL"`
}

// GoogleEnabled reports whether Google sign-in is configured.
func (o OAuthConfig) GoogleEnabled() bool {
	return o.GoogleClientID != "" && o.GoogleClientSecret != ""
}

// MicrosoftEnabled reports whether Microsoft sign-in is configured.
func (o OAuthConfig) MicrosoftEnabled() bool {
	return o.MicrosoftClientID != "" && o.MicrosoftClientSecret != ""
}

// Providers lists the configured providers (google first); never nil.
func (o OAuthConfig) Providers() []string {
	out := []string{}
	if o.GoogleEnabled() {
		out = append(out, OAuthGoogle)
	}
	if o.MicrosoftEnabled() {
		out = append(out, OAuthMicrosoft)
	}
	return out
}

// validate requires the client id and secret of a provider together.
func (o OAuthConfig) validate(errs *[]error) {
	for _, p := range []struct{ name, id, secret string }{
		{"GOOGLE", o.GoogleClientID, o.GoogleClientSecret},
		{"MICROSOFT", o.MicrosoftClientID, o.MicrosoftClientSecret},
	} {
		if (p.id == "") != (p.secret == "") {
			*errs = append(*errs, fmt.Errorf("SILO_OAUTH_%[1]s_CLIENT_ID and SILO_OAUTH_%[1]s_CLIENT_SECRET must be set together", p.name))
		}
	}
}
