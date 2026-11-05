package config

import (
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"
	"time"
)

var validKey = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))

// setServeEnv sets the minimum environment for a valid `serve` configuration.
func setServeEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SILO_DATABASE_URL", "postgres://u:p@localhost:5432/silo")
	t.Setenv("SILO_PUBLIC_URL", "https://mail.example.com")
	t.Setenv("SILO_SECRET_KEY", validKey)
	t.Setenv("SILO_DEFAULT_IMAP_HOST", "imap.example.com")
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":8080" || c.LogFormat != "json" || c.LogLevel != "info" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if l, _ := c.SlogLevel(); l != slog.LevelInfo {
		t.Fatalf("level = %v", l)
	}
	if c.AuthMode != "imap" || c.DefaultIMAPPort != 993 || c.DefaultIMAPTLS != "implicit" ||
		c.DefaultSMTPPort != 587 || c.DefaultSMTPTLS != "starttls" || c.DefaultLoginFormat != "email" {
		t.Fatalf("unexpected server defaults: %+v", c)
	}
	if c.MaxAccountsPerUser != 5 || c.MirrorMaxMessageBytes != 50<<20 || c.MirrorQuotaPerAccount != 0 ||
		c.SyncPollInterval != 5*time.Minute || c.SyncMaxConnectionsPerAcct != 3 ||
		c.SyncInitialConcurrency != 2 || c.SyncBackfillRate != 5<<20 || c.UndoWindow != 10*time.Second ||
		c.BlobBackend != "fs" || c.BlobFSDir != "/data/blobs" || c.WorkerConcurrency != 4 || c.MaxAttachmentBytes != 25<<20 {
		t.Fatalf("unexpected limits defaults: %+v", c)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("SILO_HTTP_ADDR", ":18080")
	t.Setenv("SILO_LOG_FORMAT", "text")
	t.Setenv("SILO_LOG_LEVEL", "debug")
	t.Setenv("SILO_SYNC_POLL_INTERVAL", "30s")
	t.Setenv("SILO_SYNC_BACKFILL_RATE", "512KB/s")
	t.Setenv("SILO_MIRROR_QUOTA_PER_ACCOUNT", "2GB")
	t.Setenv("SILO_CUSTOM_SERVER_ALLOWLIST", "a.example.com,b.example.com")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":18080" || c.LogFormat != "text" || c.LogLevel != "debug" ||
		c.SyncPollInterval != 30*time.Second || c.SyncBackfillRate != 512<<10 ||
		c.MirrorQuotaPerAccount != 2<<30 || len(c.CustomServerAllowlist) != 2 {
		t.Fatalf("unexpected config: %+v", c)
	}
}

func TestSMTPHostDefaultsToIMAPHost(t *testing.T) {
	t.Setenv("SILO_DEFAULT_IMAP_HOST", "imap.example.com")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.DefaultSMTPHost != "imap.example.com" {
		t.Fatalf("smtp host = %q", c.DefaultSMTPHost)
	}
	t.Setenv("SILO_DEFAULT_SMTP_HOST", "smtp.example.com")
	if c, _ = Load(); c.DefaultSMTPHost != "smtp.example.com" {
		t.Fatalf("smtp host = %q", c.DefaultSMTPHost)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := map[string][2]string{
		"bad format":    {"SILO_LOG_FORMAT", "xml"},
		"bad level":     {"SILO_LOG_LEVEL", "loud"},
		"bad port":      {"SILO_DEFAULT_IMAP_PORT", "abc"},
		"bad size":      {"SILO_MIRROR_MAX_MESSAGE_BYTES", "lots"},
		"bad rate":      {"SILO_SYNC_BACKFILL_RATE", "fast/s"},
		"bad duration":  {"SILO_UNDO_WINDOW", "soon"},
		"bad bool":      {"SILO_ALLOW_CUSTOM_SERVERS", "maybe"},
		"negative size": {"SILO_MIRROR_QUOTA_PER_ACCOUNT", "-5MB"},
	}
	for name, kv := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv(kv[0], kv[1])
			if _, err := Load(); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestValidateServe(t *testing.T) {
	otherKey := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("o", 32)))
	tests := []struct {
		name    string
		env     map[string]string // overrides applied on top of the valid baseline
		wantErr string            // substring; empty = valid
	}{
		{name: "valid"},
		{name: "valid local mode without imap host", env: map[string]string{"SILO_AUTH_MODE": "local", "SILO_DEFAULT_IMAP_HOST": ""}},
		{name: "valid with previous key", env: map[string]string{"SILO_SECRET_KEY_PREVIOUS": otherKey}},
		{name: "missing database url", env: map[string]string{"SILO_DATABASE_URL": ""}, wantErr: "SILO_DATABASE_URL"},
		{name: "missing public url", env: map[string]string{"SILO_PUBLIC_URL": ""}, wantErr: "SILO_PUBLIC_URL"},
		{name: "missing secret key", env: map[string]string{"SILO_SECRET_KEY": ""}, wantErr: "SILO_SECRET_KEY is required"},
		{name: "secret key not base64", env: map[string]string{"SILO_SECRET_KEY": "%%%"}, wantErr: "base64"},
		{name: "secret key wrong length", env: map[string]string{"SILO_SECRET_KEY": base64.StdEncoding.EncodeToString([]byte("short"))}, wantErr: "32 bytes"},
		{name: "previous key wrong length", env: map[string]string{"SILO_SECRET_KEY_PREVIOUS": "AAAA"}, wantErr: "SILO_SECRET_KEY_PREVIOUS"},
		{name: "bad auth mode", env: map[string]string{"SILO_AUTH_MODE": "ldap"}, wantErr: "SILO_AUTH_MODE"},
		{name: "imap mode needs host", env: map[string]string{"SILO_DEFAULT_IMAP_HOST": ""}, wantErr: "SILO_DEFAULT_IMAP_HOST"},
		{name: "bad imap tls", env: map[string]string{"SILO_DEFAULT_IMAP_TLS": "ssl"}, wantErr: "SILO_DEFAULT_IMAP_TLS"},
		{name: "tls none rejected", env: map[string]string{"SILO_DEFAULT_IMAP_TLS": "none"}, wantErr: "SILO_ALLOW_INSECURE_TRANSPORT"},
		{name: "smtp tls none rejected", env: map[string]string{"SILO_DEFAULT_SMTP_TLS": "none"}, wantErr: "SILO_ALLOW_INSECURE_TRANSPORT"},
		{name: "tls none allowed when insecure", env: map[string]string{"SILO_DEFAULT_IMAP_TLS": "none", "SILO_ALLOW_INSECURE_TRANSPORT": "true"}},
		{name: "port out of range", env: map[string]string{"SILO_DEFAULT_SMTP_PORT": "70000"}, wantErr: "SILO_DEFAULT_SMTP_PORT"},
		{name: "bad login format", env: map[string]string{"SILO_DEFAULT_LOGIN_FORMAT": "uid"}, wantErr: "SILO_DEFAULT_LOGIN_FORMAT"},
		{name: "localpart needs domain", env: map[string]string{"SILO_DEFAULT_LOGIN_FORMAT": "localpart"}, wantErr: "SILO_DEFAULT_DOMAIN"},
		{name: "localpart with domain", env: map[string]string{"SILO_DEFAULT_LOGIN_FORMAT": "localpart", "SILO_DEFAULT_DOMAIN": "example.com"}},
		{name: "max accounts zero", env: map[string]string{"SILO_MAX_ACCOUNTS_PER_USER": "0"}, wantErr: "SILO_MAX_ACCOUNTS_PER_USER"},
		{name: "connections below 2", env: map[string]string{"SILO_SYNC_MAX_CONNECTIONS_PER_ACCOUNT": "1"}, wantErr: "SILO_SYNC_MAX_CONNECTIONS_PER_ACCOUNT"},
		{name: "poll interval zero", env: map[string]string{"SILO_SYNC_POLL_INTERVAL": "0s"}, wantErr: "SILO_SYNC_POLL_INTERVAL"},
		{name: "initial concurrency zero", env: map[string]string{"SILO_SYNC_INITIAL_CONCURRENCY": "0"}, wantErr: "SILO_SYNC_INITIAL_CONCURRENCY"},
		{name: "backfill rate zero", env: map[string]string{"SILO_SYNC_BACKFILL_RATE": "0"}, wantErr: "SILO_SYNC_BACKFILL_RATE"},
		{name: "worker concurrency zero", env: map[string]string{"SILO_WORKER_CONCURRENCY": "0"}, wantErr: "SILO_WORKER_CONCURRENCY"},
		{name: "s3 needs endpoint and bucket", env: map[string]string{"SILO_BLOB_BACKEND": "s3"}, wantErr: "SILO_BLOB_S3_BUCKET is required"},
		{name: "s3 needs credentials", env: map[string]string{"SILO_BLOB_BACKEND": "s3", "SILO_BLOB_S3_ENDPOINT": "minio:9000", "SILO_BLOB_S3_BUCKET": "silo"}, wantErr: "SILO_BLOB_S3_SECRET_KEY"},
		{name: "s3 valid", env: map[string]string{"SILO_BLOB_BACKEND": "s3", "SILO_BLOB_S3_ENDPOINT": "minio:9000", "SILO_BLOB_S3_BUCKET": "silo", "SILO_BLOB_S3_ACCESS_KEY": "a", "SILO_BLOB_S3_SECRET_KEY": "b"}},
		{name: "bad blob backend", env: map[string]string{"SILO_BLOB_BACKEND": "gcs"}, wantErr: "SILO_BLOB_BACKEND"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setServeEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			err = c.ValidateServe()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestSecretKeys(t *testing.T) {
	setServeEnv(t)
	t.Setenv("SILO_SECRET_KEY_PREVIOUS", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("p", 32))))
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cur, prev, err := c.SecretKeys()
	if err != nil || len(cur) != 32 || string(prev) != strings.Repeat("p", 32) {
		t.Fatalf("cur=%d prev=%q err=%v", len(cur), prev, err)
	}
}

func TestParseSize(t *testing.T) {
	ok := map[string]int64{"0": 0, "123": 123, "1KB": 1024, "50MB": 50 << 20, "2 gb": 2 << 30, "7B": 7}
	for in, want := range ok {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MB", "-1", "1.5MB", "abc", "99999999999TB"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) expected error", in)
		}
	}
}

func TestHardeningSettings(t *testing.T) {
	c := Config{TrustedProxies: []string{"10.0.0.1", " 172.16.0.0/12 ", "::ffff:192.168.1.1", ""}}
	got, err := c.TrustedProxyPrefixes()
	if err != nil || len(got) != 3 || got[0].String() != "10.0.0.1/32" || got[1].String() != "172.16.0.0/12" || got[2].String() != "192.168.1.1/32" {
		t.Fatalf("TrustedProxyPrefixes = %v, %v", got, err)
	}
	api, err1 := c.APIRateLimit()
	auth, err2 := c.AuthRateLimit()
	if err1 != nil || err2 != nil || api.Burst != 600 || auth.Burst != 20 || auth.Every != 3*time.Second {
		t.Fatalf("defaults: api=%+v auth=%+v %v %v", api, auth, err1, err2)
	}
	var errs []error
	Config{TrustedProxies: []string{"proxy.local"}, RateLimitAPI: "lots", RateLimitAuth: "5/x"}.validateHardening(&errs)
	if len(errs) != 3 {
		t.Fatalf("errors = %v", errs)
	}
}
