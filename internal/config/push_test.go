package config

import (
	"slices"
	"testing"
)

func TestPushConfig(t *testing.T) {
	setServeEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Push.Enabled || !slices.Equal(c.Push.Providers(), []string{PushExpo}) ||
		c.Push.SendURL() != ExpoSendURL || c.Push.ReceiptsURL() != ExpoReceiptsURL {
		t.Fatalf("defaults: %+v", c.Push)
	}

	t.Setenv("SILO_PUSH_EXPO_SEND_URL", "http://127.0.0.1:1/send")
	t.Setenv("SILO_PUSH_EXPO_RECEIPTS_URL", "http://127.0.0.1:1/receipts")
	t.Setenv("SILO_PUSH_ENABLED", "false")
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateServe(); err != nil {
		t.Fatal(err)
	}
	if got := c.Push.Providers(); got == nil || len(got) != 0 {
		t.Fatalf("providers with push off = %v, want []", got)
	}
	if c.Push.SendURL() != "http://127.0.0.1:1/send" || c.Push.ReceiptsURL() != "http://127.0.0.1:1/receipts" {
		t.Fatalf("overrides: %+v", c.Push)
	}
}
