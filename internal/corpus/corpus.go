// Package corpus is the pre-seeded, read-only command knowledge base.
//
// Rows are TSV blobs compiled into the binary and sorted by Phrase, so an exact
// lookup is a binary search over a []byte with no file I/O, no decode step and
// no database. This is what keeps the hot path fast enough to feel instant;
// everything slower lives behind it in the resolution cascade.
//
// There are two layers, searched in order:
//
//	overrides.tsv  hand-curated, small. Edited by people.
//	commands.tsv   generated from tldr-pages by tools/corpus-gen. Never edited.
//
// The split exists because the generated layer is the only maintainable way to
// get breadth - nobody can hand-write thousands of docker/ffmpeg invocations and
// keep them current - but it words things the way a man page does, not the way a
// person asks. tldr's df page has no "check disk usage" and no `df -h` at all.
// Overrides are where the few dozen high-frequency intents live, and the reason
// `make corpus` can clobber commands.tsv without destroying curation.
package corpus

import (
	"bytes"
	_ "embed"
	"sort"
	"strings"
	"sync"
)

//go:embed data/overrides.tsv
var overridesTSV []byte

//go:embed data/commands.tsv
var commandsTSV []byte

// Entry is one row: a natural-language phrase mapped to a concrete command.
type Entry struct {
	Platform string // "common", "linux", "osx", "windows"
	Tool     string // "docker", "kubectl", ...
	Phrase   string // normalized, lowercase; the sort key
	Command  string // may contain <placeholders>
	Desc     string
}

// table is one sorted TSV blob plus a lazily built index of line starts. The
// index is one memchr sweep (~100us/MB) on first use rather than at init, so
// `amnesia version` and other non-lookup paths pay nothing for it.
type table struct {
	raw     []byte
	once    sync.Once
	offsets []int32
}

var (
	overrides = &table{raw: overridesTSV}
	commands  = &table{raw: commandsTSV}
	layers    = []*table{overrides, commands}
)

func (t *table) index() []int32 {
	t.once.Do(func() {
		t.offsets = make([]int32, 0, bytes.Count(t.raw, []byte{'\n'})+1)
		for i := 0; i < len(t.raw); {
			t.offsets = append(t.offsets, int32(i))
			n := bytes.IndexByte(t.raw[i:], '\n')
			if n < 0 {
				break
			}
			i += n + 1
		}
	})
	return t.offsets
}

func (t *table) lineAt(i int32) []byte {
	end := len(t.raw)
	if n := bytes.IndexByte(t.raw[i:], '\n'); n >= 0 {
		end = int(i) + n
	}
	return bytes.TrimSuffix(t.raw[i:end], []byte{'\r'})
}

// keyAt returns field 3 (Phrase) without allocating the other four.
func (t *table) keyAt(i int32) string {
	line := t.lineAt(i)
	tabs := 0
	for j := 0; j < len(line); j++ {
		if line[j] != '\t' {
			continue
		}
		tabs++
		if tabs == 2 {
			rest := line[j+1:]
			if k := bytes.IndexByte(rest, '\t'); k >= 0 {
				return string(rest[:k])
			}
			return string(rest)
		}
	}
	return ""
}

func parse(line []byte) (Entry, bool) {
	f := bytes.SplitN(line, []byte{'\t'}, 5)
	if len(f) != 5 {
		return Entry{}, false
	}
	return Entry{
		Platform: string(f[0]),
		Tool:     string(f[1]),
		Phrase:   string(f[2]),
		Command:  string(f[3]),
		Desc:     string(f[4]),
	}, true
}

func (t *table) lookup(phrase, platform string, out []Entry) []Entry {
	idx := t.index()
	lo := sort.Search(len(idx), func(i int) bool { return t.keyAt(idx[i]) >= phrase })
	for i := lo; i < len(idx); i++ {
		if t.keyAt(idx[i]) != phrase {
			break
		}
		if e, ok := parse(t.lineAt(idx[i])); ok && platformOK(e.Platform, platform) {
			out = append(out, e)
		}
	}
	return out
}

// Lookup returns every entry whose phrase matches exactly, filtered to platform
// (plus "common"). Overrides come first, so a curated row outranks whatever
// tldr happened to word similarly. Callers pass an already-normalized phrase.
func Lookup(phrase, platform string) []Entry {
	var out []Entry
	for _, t := range layers {
		out = t.lookup(phrase, platform, out)
	}
	return out
}

// fieldAt returns field n of a row without allocating, so a scan can read a
// phrase without materializing the four fields it does not need.
func fieldAt(line []byte, n int) []byte {
	for i := 0; i < n; i++ {
		j := bytes.IndexByte(line, '\t')
		if j < 0 {
			return nil
		}
		line = line[j+1:]
	}
	if j := bytes.IndexByte(line, '\t'); j >= 0 {
		return line[:j]
	}
	return line
}

// Candidate is a scored row.
type Candidate struct {
	Entry Entry
	Score float64
}

// ScoreFunc scores one row from its raw phrase and tool bytes. It must not
// retain the slices: they point into the embedded corpus and are only valid for
// the duration of the call.
type ScoreFunc func(phrase, tool []byte) float64

// Best scans every platform-relevant row and returns the top n by score,
// overrides first on ties.
//
// The scan deliberately does not build an Entry per row. At ~30k rows that was
// five string allocations and a map per row - 15MB and 70ms for one query, on
// the miss path of a CLI that promises to feel instant. Only the winners are
// parsed.
//
// ponytail: still a linear scan, now an allocation-free one. A trigram index is
// the next step, and it is not needed until the corpus is several times larger.
func Best(platform string, n int, score ScoreFunc) []Candidate {
	if n <= 0 {
		return nil
	}
	type hit struct {
		t   *table
		off int32
		s   float64
	}
	best := make([]hit, 0, n+1)

	for _, t := range layers {
		for _, off := range t.index() {
			line := t.lineAt(off)
			if len(line) == 0 {
				continue
			}
			if p := fieldAt(line, 0); string(p) != "common" && string(p) != platform {
				continue
			}
			s := score(fieldAt(line, 2), fieldAt(line, 1))
			if s <= 0 || (len(best) == n && s <= best[len(best)-1].s) {
				continue
			}
			// Insert, keeping the slice sorted high to low. n is 3 in practice,
			// so a heap would be more code and slower.
			i := len(best)
			for i > 0 && best[i-1].s < s {
				i--
			}
			best = append(best, hit{})
			copy(best[i+1:], best[i:])
			best[i] = hit{t, off, s}
			if len(best) > n {
				best = best[:n]
			}
		}
	}

	out := make([]Candidate, 0, len(best))
	for _, h := range best {
		if e, ok := parse(h.t.lineAt(h.off)); ok {
			out = append(out, Candidate{Entry: e, Score: h.s})
		}
	}
	return out
}

// Len reports how many rows are compiled into this binary. Used by
// `amnesia doctor`, and as a cheap check that go:embed actually ran.
func Len() int {
	n := 0
	for _, t := range layers {
		n += len(t.index())
	}
	return n
}

func platformOK(entry, want string) bool {
	return entry == "common" || entry == want
}

// Normalize collapses a user query to the corpus key form. Both the corpus
// generator and the resolver call it, so a query and a stored phrase cannot
// drift apart; corpus_test asserts every stored phrase is already a fixed point.
func Normalize(s string) string {
	// Deleted, not split on. tldr writes mnemonic flags as "List [a]ll
	// containers"; splitting on the brackets would yield "a" and "ll" and no
	// user query would ever match.
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune("[]\"'`*_", r) {
			return -1
		}
		return r
	}, strings.ToLower(s))

	fields := strings.FieldsFunc(s, isSep)
	out := fields[:0]
	for _, f := range fields {
		if !filler[f] {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return strings.TrimSpace(strings.ToLower(s))
	}
	return strings.Join(out, " ")
}

// isSep splits on whitespace and sentence punctuation, but never on "-" or "/":
// "max-depth" and "path/to/file" are single tokens to anyone reading them.
func isSep(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', ',', '?', '!', '(', ')', '{', '}', ':', ';', '.', '<', '>':
		return true
	}
	return false
}

var filler = map[string]bool{
	"a": true, "an": true, "the": true, "how": true, "do": true, "i": true,
	"to": true, "please": true, "can": true, "you": true, "my": true, "me": true,
}

// ponytail: index() is a memchr sweep over the whole blob, ~2ms for 4.2MB, paid
// once per process by the first lookup. Removing it means binary-searching the
// raw bytes and snapping to line boundaries - correct but fiddly. At 6.4ms total
// per invocation against a ~100ms perception threshold, it has not earned that
// code yet. Revisit if the corpus grows several times larger.
