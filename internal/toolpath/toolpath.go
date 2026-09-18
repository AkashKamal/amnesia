// Package toolpath answers "is this executable on PATH?" cheaply and often.
//
// The obvious implementation, exec.LookPath per tool, is a trap. A lookup that
// *fails* walks every directory in PATH, and most lookups here fail - amnesia
// asks about two dozen candidate tools per query and a machine has a handful of
// them. On WSL, where PATH inherits 37 Windows directories over a 9p mount,
// that measured 5.9 seconds for a single query against 57ms with a native PATH.
// WSL is not an edge case for this audience.
//
// So: read each PATH directory once into a set, and cache that set on disk keyed
// by the PATH string. A cold build is ~600ms on WSL and a few ms natively; every
// run after that is a small file read. PATH changes invalidate it, and it ages
// out so a newly installed tool is noticed.
//
// This lives outside internal/resolve on purpose. resolve takes a HasTool
// function and must not reach disk itself; CI enforces that.
package toolpath

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// maxAge bounds how stale the answer can be. A day means "I installed docker
// this morning" is noticed by tomorrow without paying for a rebuild per run.
// Shorter would trade real latency for a rare correctness win.
const maxAge = 24 * time.Hour

// buildBudget caps a cold build. A pathological PATH - a dead network share, a
// directory with a million entries - must slow amnesia down once, not forever.
const buildBudget = 2 * time.Second

type index struct {
	PathHash string   `json:"path_hash"`
	Built    int64    `json:"built"`
	Complete bool     `json:"complete"`
	Names    []string `json:"names"`

	set map[string]bool
}

// Has returns a lookup function backed by a cached PATH index. dir is where the
// cache file lives; an unwritable dir costs speed, never correctness.
func Has(dir string) func(string) bool {
	idx := load(dir)
	if idx == nil {
		idx = build()
		if idx.Complete {
			save(dir, idx)
		}
	}

	return func(tool string) bool {
		if tool == "" {
			return true // unknown, so do not penalise it
		}
		if idx.set[normalize(tool)] {
			return true
		}
		// An incomplete index cannot prove absence. Reporting "not installed"
		// on a partial scan would demote correct answers, which is worse than
		// the problem this package exists to solve.
		return !idx.Complete
	}
}

// normalize strips the extension on Windows so "docker" matches "docker.exe",
// and lowercases because Windows PATH lookups are case-insensitive.
func normalize(name string) string {
	if runtime.GOOS != "windows" {
		return name
	}
	name = strings.ToLower(name)
	if ext := filepath.Ext(name); ext != "" {
		name = strings.TrimSuffix(name, ext)
	}
	return name
}

func pathHash() string {
	sum := sha256.Sum256([]byte(os.Getenv("PATH")))
	return hex.EncodeToString(sum[:8])
}

func cacheFile(dir string) string { return filepath.Join(dir, "tools.json") }

func load(dir string) *index {
	b, err := os.ReadFile(cacheFile(dir))
	if err != nil {
		return nil
	}
	var idx index
	if json.Unmarshal(b, &idx) != nil {
		return nil
	}
	if idx.PathHash != pathHash() {
		return nil // PATH changed; the answer may have too
	}
	if time.Since(time.Unix(idx.Built, 0)) > maxAge {
		return nil
	}
	idx.set = make(map[string]bool, len(idx.Names))
	for _, n := range idx.Names {
		idx.set[n] = true
	}
	return &idx
}

func save(dir string, idx *index) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	b, err := json.Marshal(idx)
	if err != nil {
		return
	}
	tmp := cacheFile(dir) + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	if os.Rename(tmp, cacheFile(dir)) != nil {
		os.Remove(tmp)
	}
}

func build() *index {
	deadline := time.Now().Add(buildBudget)
	idx := &index{PathHash: pathHash(), Built: time.Now().Unix(), Complete: true, set: map[string]bool{}}

	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if time.Now().After(deadline) {
			idx.Complete = false
			break
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // a PATH entry that does not exist is normal
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			idx.set[normalize(e.Name())] = true
		}
	}

	idx.Names = make([]string, 0, len(idx.set))
	for n := range idx.set {
		idx.Names = append(idx.Names, n)
	}
	return idx
}
