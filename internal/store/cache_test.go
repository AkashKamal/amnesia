package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AkashKamal/amnesia/internal/resolve"
)

func newCache(t *testing.T) *Cache {
	t.Helper()
	t.Setenv("AMNESIA_HOME", t.TempDir())
	c, err := Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return c
}

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	c := newCache(t)

	// A cold cache is a miss, not an error: first run is the normal case.
	got, err := c.Get(ctx, "list azure vms", "linux")
	if err != nil || len(got) != 0 {
		t.Fatalf("cold get = %v, %v; want empty, nil", got, err)
	}

	want := resolve.Result{Command: "az vm list", Desc: "list vms", Tool: "az"}
	if err := c.Put(ctx, "list azure vms", "linux", want); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Re-open to prove it survives the process, which is the entire point.
	c2, err := Open()
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err = c2.Get(ctx, "list azure vms", "linux")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 1 || got[0].Command != want.Command {
		t.Fatalf("got %+v, want one %q", got, want.Command)
	}
	if got[0].Source != resolve.SourceLearned {
		t.Errorf("source = %v, want cache", got[0].Source)
	}

	// Platform scoping: a macOS answer must not be served to a Linux user.
	if got, _ := c2.Get(ctx, "list azure vms", "osx"); len(got) != 0 {
		t.Errorf("osx lookup returned a linux entry: %+v", got)
	}
}

func TestPutIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := newCache(t)
	r := resolve.Result{Command: "az vm list", Tool: "az"}

	for i := 0; i < 3; i++ {
		if err := c.Put(ctx, "k", "linux", r); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	got, _ := c.Get(ctx, "k", "linux")
	if len(got) != 1 {
		t.Fatalf("got %d entries after 3 identical puts, want 1", len(got))
	}
}

func TestCorruptLineDoesNotLoseTheCache(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("AMNESIA_HOME", dir)

	body := "{ this is not json\n" + `{"k":"k","p":"linux","c":"az vm list","ts":1}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "learned.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(ctx, "k", "linux")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1: one bad line must not discard the rest", len(got))
	}
}

func TestForget(t *testing.T) {
	ctx := context.Background()
	c := newCache(t)
	if err := c.Put(ctx, "k", "linux", resolve.Result{Command: "x", Tool: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Forget(); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if got, _ := c.Get(ctx, "k", "linux"); len(got) != 0 {
		t.Fatalf("forget left %d entries", len(got))
	}
	// Forgetting an already-empty cache is not an error.
	if err := c.Forget(); err != nil {
		t.Fatalf("second forget: %v", err)
	}
}
