package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The loopback gate has two halves (config load warning + bind time) and a
// hand-edited config file must not be able to expose the phone.
func TestEffectiveListenGatesNonLoopback(t *testing.T) {
	cases := []struct {
		name      string
		listen    string
		allow     bool
		wantEqual string
	}{
		{"loopback default", "127.0.0.1:8080", false, "127.0.0.1:8080"},
		{"lan but switch off", "0.0.0.0:8080", false, "127.0.0.1:8080"},
		{"lan with switch on", "0.0.0.0:8080", true, "0.0.0.0:8080"},
		{"lan ip but switch off", "192.168.1.5:9000", false, "127.0.0.1:9000"},
		{"port only", ":8080", false, "127.0.0.1:8080"},
	}
	for _, c := range cases {
		cfg := defaultConfig()
		cfg.Listen = c.listen
		cfg.Security.AllowNonLoopbackListen = c.allow
		if got := cfg.effectiveListen(); got != c.wantEqual {
			t.Errorf("%s: effectiveListen(%q, allow=%v) = %q, want %q", c.name, c.listen, c.allow, got, c.wantEqual)
		}
	}
}

// A config written by an older version (or hand-edited) must keep working: the
// loader starts from the defaults and only overrides what the file names.
func TestLoadConfigFillsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	partial := map[string]any{"listen": "127.0.0.1:9999", "api_token": "tok-partial"}
	raw, _ := json.Marshal(partial)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	var warnings []string
	cfg, created, err := loadConfig(path, func(f string, a ...any) { warnings = append(warnings, f) })
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if created {
		t.Fatal("loadConfig reported creating a file that already existed")
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("listen = %q, want the value from the file", cfg.Listen)
	}
	if cfg.History.IntervalSeconds <= 0 || cfg.History.RollupEnabled == false {
		t.Errorf("history defaults not applied: %+v", cfg.History)
	}
	if cfg.Security.AllowNonLoopbackListen {
		t.Error("a partial config must not enable non-loopback listening")
	}
	if cfg.Files.Enabled == false {
		t.Error("files defaults not applied")
	}

	// A missing file is created, and the path is reported back to the caller.
	fresh := filepath.Join(dir, "sub", "new.json")
	_, created, err = loadConfig(fresh, nil)
	if err != nil {
		t.Fatalf("loadConfig(create): %v", err)
	}
	if !created {
		t.Error("loadConfig did not report creating a missing config")
	}
	if fi, err := os.Stat(fresh); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("created config mode = %v (err %v), want 0600", fi.Mode().Perm(), err)
	}
}

// redact() is the value-level mask used when a token is echoed back to a
// client: it must never return the value itself.
func TestRedactHidesTokens(t *testing.T) {
	const tok = "0123456789abcdef0123456789abcdef"
	out := redact(tok)
	if out == tok || strings.Contains(out, tok) {
		t.Fatalf("redact returned the token unchanged: %s", out)
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("redact did not mask anything: %s", out)
	}
	if got := redact(""); got != "" {
		t.Fatalf("redact of an empty string = %q, want empty", got)
	}
	if short := redact("abc"); short == "abc" || short == "" {
		t.Fatalf("short value not masked safely: %q", short)
	}
}
