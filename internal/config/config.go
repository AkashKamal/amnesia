// Package config is the small amount of state a user should not have to retype.
//
// Amnesia works with no configuration at all - the corpus answers offline and a
// running Ollama is found automatically. Config exists only for the case that
// cannot be detected: a hosted provider and its key.
//
// Precedence is environment over file, the usual way round, so CI and one-off
// shells can override a saved setting without editing anything.
//
// The format is deliberately `key = value` and not TOML or YAML. Three settings
// do not justify a parser dependency in a binary whose selling point is having
// none, and a format anyone can fix in a text editor is a feature for a tool
// that people install once and forget.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Origin records where a value came from, so `amnesia doctor` can explain
// itself instead of leaving the user guessing which of three places wins.
type Origin string

const (
	FromEnv      Origin = "env"
	FromFile     Origin = "config"
	FromDetected Origin = "detected"
	FromNothing  Origin = ""
)

type Config struct {
	// Model is "provider" or "provider/model-id", e.g. "ollama" or
	// "groq/llama-3.3-70b-versatile". Empty means: detect, or stay offline.
	Model   string
	APIKey  string
	BaseURL string

	// MinConfidence is how sure the corpus must be before amnesia answers from
	// it instead of asking the model. Zero means "unset, use the built-in
	// default". Raising it buys accuracy with latency and API calls; lowering
	// it does the reverse. Measured against stress/queries.txt.
	MinConfidence float64

	ModelFrom  Origin
	APIKeyFrom Origin

	// Path is the file consulted, whether or not it existed.
	Path string
}

// Dir is where amnesia keeps state. AMNESIA_HOME overrides it so dotfile
// setups, tests and portable installs do not have to guess at OS conventions.
func Dir() (string, error) {
	if v := os.Getenv("AMNESIA_HOME"); v != "" {
		return v, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "amnesia"), nil
}

func path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config"), nil
}

// Load merges the file with the environment. A missing or unreadable file is
// not an error: it is the normal state for someone who never configured
// anything, which is most people.
func Load() Config {
	c := Config{}
	c.Path, _ = path()

	if c.Path != "" {
		if kv, err := readFile(c.Path); err == nil {
			c.Model, c.APIKey, c.BaseURL = kv["model"], kv["api_key"], kv["base_url"]
			if v, err := strconv.ParseFloat(kv["min_confidence"], 64); err == nil {
				c.MinConfidence = v
			}
			if c.Model != "" {
				c.ModelFrom = FromFile
			}
			if c.APIKey != "" {
				c.APIKeyFrom = FromFile
			}
		}
	}

	if v := os.Getenv("AMNESIA_MODEL"); v != "" {
		c.Model, c.ModelFrom = v, FromEnv
	}
	if v := os.Getenv("AMNESIA_API_KEY"); v != "" {
		c.APIKey, c.APIKeyFrom = v, FromEnv
	}
	if v := os.Getenv("AMNESIA_BASE_URL"); v != "" {
		c.BaseURL = v
	}
	if v, err := strconv.ParseFloat(os.Getenv("AMNESIA_MIN_CONFIDENCE"), 64); err == nil {
		c.MinConfidence = v
	}
	return c
}

func readFile(p string) (map[string]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue // a malformed line is skipped, never fatal
		}
		kv[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return kv, sc.Err()
}

// Save writes the configuration.
//
// It rewrites the whole file rather than patching it: a handful of settings,
// and a partial write is worse than a clean one. The file is 0600 and the
// directory 0700 because it can hold an API key.
//
// A negative minConfidence means "keep whatever is already there", so changing
// provider does not silently reset a threshold the user tuned.
func Save(model, apiKey, baseURL string, minConfidence float64) (string, error) {
	if minConfidence < 0 {
		minConfidence = Load().MinConfidence
	}
	p, err := path()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("# amnesia configuration\n")
	b.WriteString("# Environment variables of the same name override anything here:\n")
	b.WriteString("#   AMNESIA_MODEL, AMNESIA_API_KEY, AMNESIA_BASE_URL\n\n")
	fmt.Fprintf(&b, "model = %s\n", model)
	if apiKey != "" {
		fmt.Fprintf(&b, "api_key = %s\n", apiKey)
	}
	if baseURL != "" {
		fmt.Fprintf(&b, "base_url = %s\n", baseURL)
	}
	if minConfidence > 0 {
		fmt.Fprintf(&b, "min_confidence = %g\n", minConfidence)
	}

	// Write-then-rename so an interrupted save cannot leave a half-written
	// config that makes amnesia look broken on the next run.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return p, nil
}

// Clear removes the config file. Absent is already the desired state.
func Clear() error {
	p, err := path()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// MaskKey renders a key safe to print. Config values end up in bug reports and
// screen shares, so nothing should ever print one in full.
func MaskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return "********"
	}
	return k[:4] + strings.Repeat("*", 8) + k[len(k)-2:]
}
