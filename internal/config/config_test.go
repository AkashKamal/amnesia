package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoConfigIsNotAnError(t *testing.T) {
	t.Setenv("AMNESIA_HOME", t.TempDir())
	// A user who never configured anything is the common case, not a failure.
	c := Load()
	if c.Model != "" || c.APIKey != "" {
		t.Fatalf("empty home produced %+v", c)
	}
	if c.Path == "" {
		t.Fatal("Path should still report where amnesia looked")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AMNESIA_HOME", dir)

	if _, err := Save("groq/llama-3.3-70b-versatile", "gsk_secret_value", ""); err != nil {
		t.Fatalf("save: %v", err)
	}
	c := Load()
	if c.Model != "groq/llama-3.3-70b-versatile" || c.APIKey != "gsk_secret_value" {
		t.Fatalf("round trip lost data: %+v", c)
	}
	if c.ModelFrom != FromFile {
		t.Errorf("ModelFrom = %q, want config", c.ModelFrom)
	}
}

func TestEnvironmentOverridesFile(t *testing.T) {
	t.Setenv("AMNESIA_HOME", t.TempDir())
	if _, err := Save("groq/from-file", "key-from-file", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMNESIA_MODEL", "ollama/from-env")

	c := Load()
	if c.Model != "ollama/from-env" {
		t.Fatalf("model = %q, want the environment to win", c.Model)
	}
	if c.ModelFrom != FromEnv {
		t.Errorf("ModelFrom = %q, want env", c.ModelFrom)
	}
	// Only the overridden key changes; the rest of the file still applies.
	if c.APIKey != "key-from-file" {
		t.Errorf("api key = %q, want the file value to survive", c.APIKey)
	}
}

func TestChangingModelKeepsTheSavedKey(t *testing.T) {
	t.Setenv("AMNESIA_HOME", t.TempDir())
	if _, err := Save("groq/one", "gsk_keep_me", ""); err != nil {
		t.Fatal(err)
	}
	c := Load()
	if _, err := Save("groq/two", c.APIKey, c.BaseURL); err != nil {
		t.Fatal(err)
	}
	if got := Load(); got.APIKey != "gsk_keep_me" {
		t.Fatalf("api key = %q; switching models must not silently log you out", got.APIKey)
	}
}

func TestConfigFileIsNotWorldReadable(t *testing.T) {
	if os.Getenv("GOOS") == "windows" {
		t.Skip("POSIX permission bits do not apply")
	}
	dir := t.TempDir()
	t.Setenv("AMNESIA_HOME", dir)
	p, err := Save("groq/x", "gsk_secret", "")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	// The file can hold an API key.
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("mode %o allows group or other access to a file holding a key", mode)
	}
}

func TestMalformedLinesAreSkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AMNESIA_HOME", dir)
	body := "# a comment\nthis line has no equals sign\n\nmodel = ollama\n"
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Load(); got.Model != "ollama" {
		t.Fatalf("model = %q; one bad line must not discard the file", got.Model)
	}
}

func TestClearIsIdempotent(t *testing.T) {
	t.Setenv("AMNESIA_HOME", t.TempDir())
	if _, err := Save("ollama", "", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Clear(); err != nil {
			t.Fatalf("clear %d: %v", i, err)
		}
	}
	if got := Load(); got.Model != "" {
		t.Fatalf("model = %q after clear", got.Model)
	}
}

// Config values end up in bug reports and screen shares.
func TestMaskKey(t *testing.T) {
	if got := MaskKey("gsk_abcdefghijklmnop"); strings.Contains(got, "defghijklm") {
		t.Fatalf("MaskKey leaked the key: %q", got)
	}
	if got := MaskKey("short"); got != "********" {
		t.Fatalf("MaskKey(short) = %q", got)
	}
	if got := MaskKey(""); got != "" {
		t.Fatalf("MaskKey(empty) = %q", got)
	}
}
