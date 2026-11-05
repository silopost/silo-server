package seclog

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestMaskLogin(t *testing.T) {
	for in, want := range map[string]string{
		"jan@example.com": "j***@example.com",
		"żaba@pl.pl":      "ż***@pl.pl",
		"jan":             "j***",
		"":                "-",
		"@x.pl":           "***@x.pl",
		"a@b c":           "a***@?",
	} {
		if got := MaskLogin(in); got != want {
			t.Errorf("MaskLogin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLogFormats(t *testing.T) {
	var text, js bytes.Buffer
	Log(context.Background(), slog.New(slog.NewTextHandler(&text, nil)), AuthFailed, "1.2.3.4", "/api/v1/session", "jan@example.com")
	if !strings.Contains(text.String(), `level=WARN msg="security event" event=auth_failed ip=1.2.3.4 path=/api/v1/session login=j***@example.com`) {
		t.Errorf("text line: %s", text.String())
	}
	Log(context.Background(), slog.New(slog.NewJSONHandler(&js, nil)), RateLimited, "2001:db8::1", "/api/v1/threads", "")
	var m map[string]any
	if err := json.Unmarshal(js.Bytes(), &m); err != nil || m["event"] != "rate_limited" || m["ip"] != "2001:db8::1" || m["login"] != "-" {
		t.Errorf("json line: %s (%v)", js.String(), err)
	}
	if !strings.Contains(js.String(), `"event":"rate_limited","ip":"2001:db8::1"`) {
		t.Errorf("json field order: %s", js.String())
	}
}
