package corpus

import "testing"

// TestInvariants guards the properties Lookup depends on. commands.tsv is
// machine-generated and tens of thousands of rows, so this is the gate that
// catches a bad generator run before it ships inside a binary.
func TestInvariants(t *testing.T) {
	platforms := map[string]bool{"common": true, "linux": true, "osx": true, "windows": true}

	for name, tb := range map[string]*table{"overrides": overrides, "commands": commands} {
		t.Run(name, func(t *testing.T) {
			var n int
			var prev string

			for _, off := range tb.index() {
				line := tb.lineAt(off)
				if len(line) == 0 {
					continue
				}
				n++
				e, ok := parse(line)
				if !ok {
					t.Fatalf("row %d: want 5 tab-separated fields, got %q", n, line)
				}
				if !platforms[e.Platform] {
					t.Errorf("row %d: unknown platform %q", n, e.Platform)
				}
				if e.Command == "" || e.Tool == "" {
					t.Errorf("row %d: empty tool or command", n)
				}
				if got := Normalize(e.Phrase); got != e.Phrase {
					t.Fatalf("row %d: phrase %q is not normalized (want %q); the generator "+
						"and the resolver must agree or exact lookup silently misses", n, e.Phrase, got)
				}
				if e.Phrase < prev {
					t.Fatalf("row %d: not sorted by phrase (%q after %q); binary search is invalid", n, e.Phrase, prev)
				}
				prev = e.Phrase
			}
			if n == 0 {
				t.Fatal("layer is empty")
			}
			t.Logf("%s: %d rows", name, n)
		})
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"How do I check disk usage?": "check disk usage",
		"  LIST   Running  Pods  ":   "list running pods",
		"remove the images":          "remove images",

		// tldr writes mnemonic flags in brackets. Splitting there would give
		// "a"+"ll" and no user query would match.
		"List [a]ll containers":              "list all containers",
		"Use [k]ibibyte (1024 byte) units":   "use kibibyte 1024 byte units",
		"Display an overview of disk usage.": "display overview of disk usage",
		"Limit depth with --max-depth=1":     "limit depth with --max-depth=1",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	// The whole corpus contract rests on this: the generator stores
	// Normalize(desc), and the resolver looks up Normalize(query).
	for _, in := range []string{
		"List [a]ll containers, including stopped ones.",
		"Use [k]ibibyte (1024 byte) units when showing size figures:",
		"How do I `docker` things?",
	} {
		once := Normalize(in)
		if twice := Normalize(once); twice != once {
			t.Errorf("Normalize not idempotent: %q -> %q -> %q", in, once, twice)
		}
	}
}

func TestLookupIsPlatformScoped(t *testing.T) {
	linux := Lookup("show listening ports", "linux")
	if len(linux) == 0 || linux[0].Tool != "ss" {
		t.Fatalf("linux lookup = %+v", linux)
	}
	osx := Lookup("show listening ports", "osx")
	if len(osx) == 0 || osx[0].Tool != "lsof" {
		t.Fatalf("osx lookup = %+v", osx)
	}
	if got := Lookup("no such phrase exists anywhere at all", "linux"); len(got) != 0 {
		t.Fatalf("miss returned %d rows", len(got))
	}
}

// TestOverridesWin is the point of having two layers: a curated row must come
// first even when the generated corpus has something for the same phrase.
func TestOverridesWin(t *testing.T) {
	got := Lookup("check disk usage", "linux")
	if len(got) == 0 {
		t.Fatal("no rows for the README's flagship example")
	}
	if got[0].Command != "df -h" {
		t.Fatalf("first result = %q, want the curated %q", got[0].Command, "df -h")
	}
}

func BenchmarkLookup(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if len(Lookup("check disk usage", "linux")) == 0 {
			b.Fatal("miss")
		}
	}
}
