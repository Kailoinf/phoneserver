package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testAppWithShare(t *testing.T, root string) *App {
	t.Helper()
	cfg := defaultConfig()
	cfg.Files.Enabled = true
	cfg.Files.Shares = map[string]string{"data": root}
	return &App{cfg: cfg}
}

// Every path the file service accepts has to end up inside the share root once
// symlinks are resolved. This is the property that matters, so the test asserts
// it directly rather than asserting specific error strings.
func TestResolveTargetNeverEscapesTheShare(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	a := testAppWithShare(t, root)

	good := []string{"data", "data/", "data/sub/file.txt", "data/./sub//file.txt"}
	for _, p := range good {
		tgt, err := a.resolveTarget(p)
		if err != nil {
			t.Fatalf("resolveTarget(%q) = %v, want success", p, err)
		}
		if tgt.Abs != root && !strings.HasPrefix(tgt.Abs, root+string(os.PathSeparator)) {
			t.Fatalf("resolveTarget(%q) resolved outside the share: %s", p, tgt.Abs)
		}
	}

	bad := []string{
		"../etc/passwd",
		"data/../../etc/passwd",
		"data/sub/../../../etc/passwd",
		"data/..",
		"data/\\evil",
		"data/link/secret.txt",
		"nosuchshare/file.txt",
		"",
	}
	for _, p := range bad {
		tgt, err := a.resolveTarget(p)
		if err == nil {
			t.Fatalf("resolveTarget(%q) was accepted (abs=%s), want a refusal", p, tgt.Abs)
		}
	}
}

func TestShareNameSanitisation(t *testing.T) {
	cfg := defaultConfig()
	cfg.Files.Shares = map[string]string{
		" sdcard/":  "/storage/emulated/0",
		"a b":       "/tmp/a b",
		"\\weird\\": "/tmp/weird",
		"keep":      "/tmp/keep",
	}
	cfg.norm()
	if _, ok := cfg.Files.Shares["sdcard"]; !ok {
		t.Errorf("slashes/space were not trimmed from share names: %v", cfg.Files.Shares)
	}
	if _, ok := cfg.Files.Shares["keep"]; !ok {
		t.Errorf("norm dropped a valid share: %v", cfg.Files.Shares)
	}
	for name := range cfg.Files.Shares {
		if strings.ContainsAny(name, "/\\ ") {
			t.Errorf("share name %q still contains a separator or space", name)
		}
	}
}
