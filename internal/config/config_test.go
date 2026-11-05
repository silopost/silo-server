package config

import (
	"log/slog"
	"testing"
)

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
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("SILO_HTTP_ADDR", ":18080")
	t.Setenv("SILO_LOG_FORMAT", "text")
	t.Setenv("SILO_LOG_LEVEL", "debug")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":18080" || c.LogFormat != "text" || c.LogLevel != "debug" {
		t.Fatalf("unexpected config: %+v", c)
	}
}

func TestLoadValidation(t *testing.T) {
	tests := map[string][2]string{
		"bad format": {"SILO_LOG_FORMAT", "xml"},
		"bad level":  {"SILO_LOG_LEVEL", "loud"},
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
