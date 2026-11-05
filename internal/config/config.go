// Package config loads the instance configuration from SILO_* environment variables.
package config

import (
	"fmt"
	"log/slog"

	"github.com/caarlos0/env/v11"
)

// Config holds the instance configuration. Only the fields needed so far are
// defined; the rest of the SILO_* table is added as features land.
type Config struct {
	HTTPAddr  string `env:"SILO_HTTP_ADDR" envDefault:":8080"`
	LogFormat string `env:"SILO_LOG_FORMAT" envDefault:"json"`
	LogLevel  string `env:"SILO_LOG_LEVEL" envDefault:"info"`
}

// Load parses and validates the configuration from the process environment.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, fmt.Errorf("parse env: %w", err)
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) validate() error {
	if c.HTTPAddr == "" {
		return fmt.Errorf("SILO_HTTP_ADDR must not be empty")
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		return fmt.Errorf("SILO_LOG_FORMAT must be json or text, got %q", c.LogFormat)
	}
	if _, err := c.SlogLevel(); err != nil {
		return err
	}
	return nil
}

// SlogLevel returns the configured log level.
func (c Config) SlogLevel() (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(c.LogLevel)); err != nil {
		return 0, fmt.Errorf("SILO_LOG_LEVEL invalid %q: %w", c.LogLevel, err)
	}
	return l, nil
}
