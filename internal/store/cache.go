// Package store persists resolutions the model had to be paid for, so any
// given query is slow at most once.
//
// This is an append-only JSONL file, not a database. Nothing here needs
// full-text search, transactions or concurrent writers, and a plain file keeps
// the binary at zero external dependencies - which is the whole distribution
// story. Reach for a database when a feature actually needs one.
package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AkashKamal/amnesia/internal/resolve"
)

type record struct {
	Key      string `json:"k"`
	Platform string `json:"p"`
	Command  string `json:"c"`
	Desc     string `json:"d,omitempty"`
	Tool     string `json:"t,omitempty"`
	Model    string `json:"m,omitempty"`
	TS       int64  `json:"ts"`
}

type Cache struct {
	path string

	once    sync.Once
	mu      sync.Mutex
	byKey   map[string][]resolve.Result
	loadErr error
}

// Dir is where Amnesia keeps state. Honours AMNESIA_HOME so tests and dotfile
// setups do not have to guess at OS conventions.
func Dir() (string, error) {
	if v := os.Getenv("AMNESIA_HOME"); v != "" {
		return v, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "amnesia"), nil
}

// Open returns a cache handle. It does no I/O: the file is read on the first
// miss and never on the exact-match path, which is the common case.
func Open() (*Cache, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	return &Cache{path: filepath.Join(dir, "learned.jsonl")}, nil
}

func (c *Cache) Path() string { return c.path }

func (c *Cache) load() {
	c.once.Do(func() {
		c.byKey = map[string][]resolve.Result{}

		f, err := os.Open(c.path)
		if err != nil {
			// No cache yet is the normal first-run state, not a failure.
			if !errors.Is(err, fs.ErrNotExist) {
				c.loadErr = err
			}
			return
		}
		defer f.Close()

		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var r record
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				// One corrupt line must not cost the user the whole cache.
				continue
			}
			k := r.Platform + "|" + r.Key
			c.byKey[k] = append(c.byKey[k], resolve.Result{
				Command:    r.Command,
				Desc:       r.Desc,
				Tool:       r.Tool,
				Source:     resolve.SourceLearned,
				Confidence: 1,
			})
		}
		c.loadErr = sc.Err()
	})
}

func (c *Cache) Get(_ context.Context, key, platform string) ([]resolve.Result, error) {
	c.load()
	if c.loadErr != nil {
		return nil, c.loadErr
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byKey[platform+"|"+key], nil
}

// Put appends one resolution. Appending means a second amnesia process racing
// on the same file interleaves whole lines rather than corrupting a record,
// which is the only concurrency guarantee this needs.
//
// ponytail: no compaction. Entries are ~150 bytes and only misses are cached;
// add pruning if anyone reports a cache in the tens of megabytes.
func (c *Cache) Put(_ context.Context, key, platform string, r resolve.Result) error {
	c.load()

	c.mu.Lock()
	defer c.mu.Unlock()

	k := platform + "|" + key
	for _, existing := range c.byKey[k] {
		if existing.Command == r.Command {
			return nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	line, err := json.Marshal(record{
		Key: key, Platform: platform, Command: r.Command,
		Desc: r.Desc, Tool: r.Tool, TS: time.Now().Unix(),
	})
	if err != nil {
		return err
	}

	f, err := os.OpenFile(c.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}

	r.Source = resolve.SourceLearned
	c.byKey[k] = append(c.byKey[k], r)
	return nil
}

// Forget drops the whole cache. The corpus is embedded, so this only costs the
// user the model answers they have accumulated.
func (c *Cache) Forget() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byKey = map[string][]resolve.Result{}
	if err := os.Remove(c.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
