package config

import (
	"slices"
	"strings"
	"testing"
)

func TestOAuthConfig(t *testing.T) {
	setServeEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.OAuth.Providers(); got == nil || len(got) != 0 || c.OAuth.MicrosoftTenant != "common" {
		t.Fatalf("defaults: providers %v tenant %q", got, c.OAuth.MicrosoftTenant)
	}

	t.Setenv("SILO_OAUTH_GOOGLE_CLIENT_ID", "gid")
	t.Setenv("SILO_OAUTH_GOOGLE_CLIENT_SECRET", "gsecret")
	t.Setenv("SILO_OAUTH_GOOGLE_TOKEN_URL", "http://127.0.0.1:1/token")
	t.Setenv("SILO_OAUTH_MICROSOFT_CLIENT_ID", "mid")
	t.Setenv("SILO_OAUTH_MICROSOFT_CLIENT_SECRET", "msecret")
	t.Setenv("SILO_OAUTH_MICROSOFT_TENANT", "organizations")
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateServe(); err != nil {
		t.Fatal(err)
	}
	if got := c.OAuth.Providers(); !slices.Equal(got, []string{OAuthGoogle, OAuthMicrosoft}) ||
		c.OAuth.GoogleTokenURL != "http://127.0.0.1:1/token" || c.OAuth.MicrosoftTenant != "organizations" {
		t.Fatalf("configured: %+v", c.OAuth)
	}

	t.Setenv("SILO_OAUTH_MICROSOFT_CLIENT_SECRET", "")
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateServe(); err == nil || !strings.Contains(err.Error(), "SILO_OAUTH_MICROSOFT_CLIENT_ID and SILO_OAUTH_MICROSOFT_CLIENT_SECRET") {
		t.Fatalf("half-configured provider: %v", err)
	}
	if got := c.OAuth.Providers(); !slices.Equal(got, []string{OAuthGoogle}) {
		t.Fatalf("providers = %v", got)
	}
}
