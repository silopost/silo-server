// Package config loads the instance configuration from SILO_* environment variables.
//
// The variable table lives in section 2.5 of ARCHITECTURE_AND_PLAN.md (silo repo).
// Load parses the environment and fills derived defaults; ValidateServe checks
// everything the HTTP server needs. Subcommands such as migrate call only Load.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/caarlos0/env/v11"
)

// TLS modes accepted by the *_TLS variables.
const (
	TLSImplicit = "implicit"
	TLSStartTLS = "starttls"
	TLSNone     = "none"
)

// Auth modes accepted by SILO_AUTH_MODE.
const (
	AuthModeIMAP  = "imap"
	AuthModeLocal = "local"
)

// Login formats accepted by SILO_DEFAULT_LOGIN_FORMAT.
const (
	LoginFormatEmail     = "email"
	LoginFormatLocalpart = "localpart"
)

// SecretKeyLen is the required decoded length of SILO_SECRET_KEY in bytes.
const SecretKeyLen = 32

// Config holds the instance configuration.
type Config struct {
	HTTPAddr  string `env:"SILO_HTTP_ADDR" envDefault:":8080"`
	LogFormat string `env:"SILO_LOG_FORMAT" envDefault:"json"`
	LogLevel  string `env:"SILO_LOG_LEVEL" envDefault:"info"`

	DatabaseURL string `env:"SILO_DATABASE_URL"`
	PublicURL   string `env:"SILO_PUBLIC_URL"`

	// SecretKey and SecretKeyPrevious are base64 of 32 bytes; use SecretKeys to decode.
	SecretKey         string `env:"SILO_SECRET_KEY"`
	SecretKeyPrevious string `env:"SILO_SECRET_KEY_PREVIOUS"`

	AuthMode string `env:"SILO_AUTH_MODE" envDefault:"imap"`

	DefaultIMAPHost string `env:"SILO_DEFAULT_IMAP_HOST"`
	DefaultIMAPPort int    `env:"SILO_DEFAULT_IMAP_PORT" envDefault:"993"`
	DefaultIMAPTLS  string `env:"SILO_DEFAULT_IMAP_TLS" envDefault:"implicit"`
	// DefaultSMTPHost falls back to DefaultIMAPHost (resolved in Load).
	DefaultSMTPHost string `env:"SILO_DEFAULT_SMTP_HOST"`
	DefaultSMTPPort int    `env:"SILO_DEFAULT_SMTP_PORT" envDefault:"587"`
	DefaultSMTPTLS  string `env:"SILO_DEFAULT_SMTP_TLS" envDefault:"starttls"`

	DefaultLoginFormat string `env:"SILO_DEFAULT_LOGIN_FORMAT" envDefault:"email"`
	DefaultDomain      string `env:"SILO_DEFAULT_DOMAIN"`

	AllowCustomServers     bool     `env:"SILO_ALLOW_CUSTOM_SERVERS"`
	CustomServerAllowlist  []string `env:"SILO_CUSTOM_SERVER_ALLOWLIST" envSeparator:","`
	AllowInsecureTransport bool     `env:"SILO_ALLOW_INSECURE_TRANSPORT"`
	MaxAccountsPerUser     int      `env:"SILO_MAX_ACCOUNTS_PER_USER" envDefault:"5"`

	MirrorMaxMessageBytes ByteSize `env:"SILO_MIRROR_MAX_MESSAGE_BYTES" envDefault:"50MB"`
	MirrorQuotaPerAccount ByteSize `env:"SILO_MIRROR_QUOTA_PER_ACCOUNT" envDefault:"0"`
	// MaxAttachmentBytes limits one file of POST /uploads (plan 5.6).
	MaxAttachmentBytes ByteSize `env:"SILO_MAX_ATTACHMENT_BYTES" envDefault:"25MB"`

	SyncPollInterval          time.Duration `env:"SILO_SYNC_POLL_INTERVAL" envDefault:"5m"`
	SyncMaxConnectionsPerAcct int           `env:"SILO_SYNC_MAX_CONNECTIONS_PER_ACCOUNT" envDefault:"3"`
	SyncInitialConcurrency    int           `env:"SILO_SYNC_INITIAL_CONCURRENCY" envDefault:"2"`
	SyncBackfillRate          ByteRate      `env:"SILO_SYNC_BACKFILL_RATE" envDefault:"5MB/s"`
	// SyncKeywordFolderLimit: distinct keywords a folder keeps (26 = Dovecot
	// maildir); a label keyword beyond it stays local in that folder. 0 = no limit.
	SyncKeywordFolderLimit int           `env:"SILO_SYNC_KEYWORD_FOLDER_LIMIT" envDefault:"26"`
	UndoWindow             time.Duration `env:"SILO_UNDO_WINDOW" envDefault:"10s"`

	WorkerConcurrency int `env:"SILO_WORKER_CONCURRENCY" envDefault:"4"`

	BlobBackend string `env:"SILO_BLOB_BACKEND" envDefault:"fs"`
	BlobFSDir   string `env:"SILO_BLOB_FS_DIR" envDefault:"/data/blobs"`

	// SILO_BLOB_BACKEND=s3 (S3Store): an S3-compatible bucket (MinIO, AWS,
	// ...). The bucket must exist.
	BlobS3Endpoint  string `env:"SILO_BLOB_S3_ENDPOINT"` // host:port or URL
	BlobS3Bucket    string `env:"SILO_BLOB_S3_BUCKET"`
	BlobS3AccessKey string `env:"SILO_BLOB_S3_ACCESS_KEY"`
	BlobS3SecretKey string `env:"SILO_BLOB_S3_SECRET_KEY"`
	BlobS3Region    string `env:"SILO_BLOB_S3_REGION" envDefault:"us-east-1"`
	BlobS3Prefix    string `env:"SILO_BLOB_S3_PREFIX"`
	BlobS3UseSSL    bool   `env:"SILO_BLOB_S3_USE_SSL" envDefault:"true"`
	BlobS3PathStyle bool   `env:"SILO_BLOB_S3_PATH_STYLE" envDefault:"true"`
}

// Load parses the process environment, applies derived defaults and checks the
// settings every subcommand relies on (logging). It does not require the
// serve-only variables; call ValidateServe for those.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, fmt.Errorf("parse env: %w", err)
	}
	if c.DefaultSMTPHost == "" {
		c.DefaultSMTPHost = c.DefaultIMAPHost
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		return Config{}, fmt.Errorf("SILO_LOG_FORMAT must be json or text, got %q", c.LogFormat)
	}
	if _, err := c.SlogLevel(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// ValidateServe checks the full configuration required by `serve`. All
// problems are reported together.
func (c Config) ValidateServe() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.HTTPAddr == "" {
		add("SILO_HTTP_ADDR must not be empty")
	}
	if c.DatabaseURL == "" {
		add("SILO_DATABASE_URL is required")
	}
	if c.PublicURL == "" {
		add("SILO_PUBLIC_URL is required")
	}
	if c.SecretKey == "" {
		add("SILO_SECRET_KEY is required")
	} else if _, err := decodeKey(c.SecretKey); err != nil {
		add("SILO_SECRET_KEY: %v", err)
	}
	if c.SecretKeyPrevious != "" {
		if _, err := decodeKey(c.SecretKeyPrevious); err != nil {
			add("SILO_SECRET_KEY_PREVIOUS: %v", err)
		}
	}

	switch c.AuthMode {
	case AuthModeIMAP:
		if c.DefaultIMAPHost == "" {
			add("SILO_DEFAULT_IMAP_HOST is required when SILO_AUTH_MODE=imap")
		}
	case AuthModeLocal:
	default:
		add("SILO_AUTH_MODE must be imap or local, got %q", c.AuthMode)
	}

	c.checkTransport(&errs, "SILO_DEFAULT_IMAP_TLS", c.DefaultIMAPTLS)
	c.checkTransport(&errs, "SILO_DEFAULT_SMTP_TLS", c.DefaultSMTPTLS)
	checkPort(&errs, "SILO_DEFAULT_IMAP_PORT", c.DefaultIMAPPort)
	checkPort(&errs, "SILO_DEFAULT_SMTP_PORT", c.DefaultSMTPPort)

	switch c.DefaultLoginFormat {
	case LoginFormatEmail:
	case LoginFormatLocalpart:
		if c.DefaultDomain == "" {
			add("SILO_DEFAULT_DOMAIN is required when SILO_DEFAULT_LOGIN_FORMAT=localpart")
		}
	default:
		add("SILO_DEFAULT_LOGIN_FORMAT must be email or localpart, got %q", c.DefaultLoginFormat)
	}

	if c.MaxAccountsPerUser < 1 {
		add("SILO_MAX_ACCOUNTS_PER_USER must be >= 1")
	}
	if c.SyncPollInterval <= 0 {
		add("SILO_SYNC_POLL_INTERVAL must be > 0")
	}
	if c.SyncMaxConnectionsPerAcct < 2 {
		add("SILO_SYNC_MAX_CONNECTIONS_PER_ACCOUNT must be >= 2")
	}
	if c.SyncInitialConcurrency < 1 {
		add("SILO_SYNC_INITIAL_CONCURRENCY must be >= 1")
	}
	if c.SyncBackfillRate <= 0 {
		add("SILO_SYNC_BACKFILL_RATE must be > 0")
	}
	if c.SyncKeywordFolderLimit < 0 {
		add("SILO_SYNC_KEYWORD_FOLDER_LIMIT must not be negative")
	}
	if c.UndoWindow < 0 {
		add("SILO_UNDO_WINDOW must not be negative")
	}
	if c.MaxAttachmentBytes <= 0 {
		add("SILO_MAX_ATTACHMENT_BYTES must be > 0")
	}

	if c.WorkerConcurrency < 1 {
		add("SILO_WORKER_CONCURRENCY must be >= 1")
	}

	switch c.BlobBackend {
	case "fs":
	case "s3":
		for _, v := range []struct{ name, val string }{
			{"SILO_BLOB_S3_ENDPOINT", c.BlobS3Endpoint}, {"SILO_BLOB_S3_BUCKET", c.BlobS3Bucket},
			{"SILO_BLOB_S3_ACCESS_KEY", c.BlobS3AccessKey}, {"SILO_BLOB_S3_SECRET_KEY", c.BlobS3SecretKey},
		} {
			if v.val == "" {
				add("%s is required when SILO_BLOB_BACKEND=s3", v.name)
			}
		}
	default:
		add("SILO_BLOB_BACKEND must be fs or s3, got %q", c.BlobBackend)
	}
	return errors.Join(errs...)
}

// checkTransport validates a *_TLS value; "none" needs SILO_ALLOW_INSECURE_TRANSPORT.
func (c Config) checkTransport(errs *[]error, name, v string) {
	switch v {
	case TLSImplicit, TLSStartTLS:
	case TLSNone:
		if !c.AllowInsecureTransport {
			*errs = append(*errs, fmt.Errorf("%s=none requires SILO_ALLOW_INSECURE_TRANSPORT=true", name))
		}
	default:
		*errs = append(*errs, fmt.Errorf("%s must be implicit, starttls or none, got %q", name, v))
	}
}

func checkPort(errs *[]error, name string, p int) {
	if p < 1 || p > 65535 {
		*errs = append(*errs, fmt.Errorf("%s must be in 1..65535, got %d", name, p))
	}
}

// SecretKeys returns the decoded current key and, if configured, the previous
// one (nil otherwise).
func (c Config) SecretKeys() (current, previous []byte, err error) {
	current, err = decodeKey(c.SecretKey)
	if err != nil {
		return nil, nil, fmt.Errorf("SILO_SECRET_KEY: %w", err)
	}
	if c.SecretKeyPrevious != "" {
		previous, err = decodeKey(c.SecretKeyPrevious)
		if err != nil {
			return nil, nil, fmt.Errorf("SILO_SECRET_KEY_PREVIOUS: %w", err)
		}
	}
	return current, previous, nil
}

func decodeKey(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("not valid base64")
	}
	if len(b) != SecretKeyLen {
		return nil, fmt.Errorf("must decode to %d bytes, got %d", SecretKeyLen, len(b))
	}
	return b, nil
}

// SlogLevel returns the configured log level.
func (c Config) SlogLevel() (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(c.LogLevel)); err != nil {
		return 0, fmt.Errorf("SILO_LOG_LEVEL invalid %q: %w", c.LogLevel, err)
	}
	return l, nil
}
