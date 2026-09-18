// Package resolve implements the resolution cascade: the ordered set of
// strategies Amnesia tries to turn "check disk usage" into "df -h".
//
// The cascade is strictly ordered by cost, and every stage may decline.
// Nothing below the embedded corpus is touched unless the stage above it
// misses, so the common case never opens a file, a socket or a database.
//
//  1. Static exact   embedded corpus, binary search     ~2ms,  offline
//  2. Learned exact  on-disk cache of past resolutions  ~2ms,  offline
//  3. Static fuzzy   linear scan + token scoring        ~13ms, offline
//  4. Model          Groq / Ollama / OpenAI-shaped API  ~300ms-2s, network
//
// A stage also declines when the tool it would name is not installed here; see
// HasTool. Stage 1 of the cascade is cheap, not authoritative.
//
// A model answer is written back to the learned cache, so any given query is
// slow at most once. Stage 4 is the only stage that can leave the machine, and
// a nil Model removes it entirely.
package resolve

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/AkashKamal/amnesia/internal/corpus"
	"github.com/AkashKamal/amnesia/internal/risk"
)

type Source uint8

const (
	SourceStaticExact Source = iota
	SourceLearned
	SourceStaticFuzzy
	SourceModel
)

func (s Source) String() string {
	return [...]string{"corpus", "cache", "fuzzy", "model"}[s]
}

// Result is one candidate command, ready to show behind a confirm prompt.
type Result struct {
	Command    string
	Desc       string
	Tool       string
	Source     Source
	Confidence float64 // 0..1; exact matches are 1
	Risk       risk.Level
	RiskReason string
	Elapsed    time.Duration

	// Installed reports whether Tool was found on PATH. A command for a tool
	// you do not have is not an answer, so callers must not offer to run one.
	Installed bool
}

// Env is the machine we are resolving for. Captured once per invocation;
// corpus rows are platform-tagged so we never suggest ss(8) on macOS.
type Env struct {
	Platform string // "linux", "osx", "windows"
	Shell    string // "bash", "zsh", "fish", "powershell"
}

// Cache is the learned store: resolutions we have already paid for. The real
// implementation is SQLite; NopCache covers the first run and --no-cache.
// Misses must be cheap and errors must never be fatal - a broken cache
// degrades latency, not correctness.
type Cache interface {
	Get(ctx context.Context, key, platform string) ([]Result, error)
	Put(ctx context.Context, key, platform string, r Result) error
}

type NopCache struct{}

func (NopCache) Get(context.Context, string, string) ([]Result, error) { return nil, nil }
func (NopCache) Put(context.Context, string, string, Result) error     { return nil }

// Model is the paid fallback. Implementations wrap Groq, DeepSeek, Ollama or
// anything else of that shape; the resolver does not care which.
type Model interface {
	Suggest(ctx context.Context, query string, env Env) ([]Result, error)
	Name() string
}

var ErrNoMatch = errors.New("amnesia: no command found")

type Resolver struct {
	Env   Env
	Cache Cache // nil disables the learned stage
	Model Model // nil means offline

	// FuzzyFloor is the minimum score a fuzzy candidate needs before we return
	// it instead of escalating to the model. Too low and users get confidently
	// wrong commands; too high and we spend money on questions the corpus
	// already answers. Tuned against testdata/queries.txt.
	FuzzyFloor float64
	// MaxResults caps what we show. Past a handful the user is reading a menu
	// instead of confirming a command.
	MaxResults int

	// OnModelCall is invoked just before the model stage runs, with the model's
	// name. A cold local model can take a minute to load, and a CLI that prints
	// nothing for a minute looks hung rather than busy. Optional; resolve stays
	// free of any opinion about how to display it.
	OnModelCall func(name string)

	// HasTool reports whether an executable is on PATH. nil means "assume yes",
	// which is what tests want; New wires up the real check.
	//
	// This exists because the corpus platform tag is not enough. tldr's
	// "common" really means "common across Unix-likes", so on Windows it will
	// happily offer df, lsof and aconnect. Ranking by what is actually
	// installed fixes that, and it also stops a machine with docker but not
	// podman from being shown podman.
	HasTool func(tool string) bool
}

func New(env Env) *Resolver {
	return &Resolver{
		Env:        env,
		Cache:      NopCache{},
		FuzzyFloor: 0.55,
		MaxResults: 3,
		HasTool:    lookPath(),
	}
}

// lookPath returns a memoized PATH check. Memoized because the same tool shows
// up across many candidate rows and each miss is a full PATH walk.
func lookPath() func(string) bool {
	seen := map[string]bool{}
	return func(tool string) bool {
		if tool == "" {
			return true // unknown, so do not penalise it
		}
		if v, ok := seen[tool]; ok {
			return v
		}
		_, err := exec.LookPath(tool)
		seen[tool] = err == nil
		return err == nil
	}
}

func (r *Resolver) installed(tool string) bool {
	if r.HasTool == nil {
		return true
	}
	return r.HasTool(tool)
}

// preferInstalled stable-sorts runnable commands ahead of ones whose tool is
// missing, and records which is which. Order within each group is preserved, so
// the score ranking still decides among things you can actually run.
func (r *Resolver) preferInstalled(in []Result) []Result {
	out := make([]Result, 0, len(in))
	var missing []Result
	for _, res := range in {
		res.Installed = r.installed(res.Tool)
		if res.Installed {
			out = append(out, res)
		} else {
			missing = append(missing, res)
		}
	}
	return append(out, missing...)
}

// Resolve runs the cascade, returning as soon as a stage is confident.
func (r *Resolver) Resolve(ctx context.Context, query string) ([]Result, error) {
	start := time.Now()
	key := corpus.Normalize(query)
	if key == "" {
		return nil, ErrNoMatch
	}

	// best holds the strongest answer found so far whose tool is not installed.
	// A missing tool does not end the cascade - a later stage may know a command
	// for something this machine actually has - but it beats returning nothing.
	var best []Result

	if out := toResults(corpus.Lookup(key, r.Env.Platform), SourceStaticExact, 1); len(out) > 0 {
		out = r.preferInstalled(out)
		if out[0].Installed {
			return r.finish(out, start), nil
		}
		best = out
	}

	if r.Cache != nil {
		// A cache error is reported by the caller and otherwise ignored on
		// purpose: it must not block a resolution the model can still answer.
		if out, err := r.Cache.Get(ctx, key, r.Env.Platform); err == nil && len(out) > 0 {
			return r.finish(r.preferInstalled(out), start), nil
		}
	}

	if out := r.fuzzy(key); len(out) > 0 && out[0].Confidence >= r.FuzzyFloor {
		// An exact-but-uninstalled match still describes the intent better than
		// a fuzzy-and-uninstalled one, so only displace it with something
		// runnable.
		if out[0].Installed || best == nil {
			return r.finish(out, start), nil
		}
	}

	if r.Model == nil {
		if best != nil {
			return r.finish(best, start), nil
		}
		return nil, ErrNoMatch
	}
	if r.OnModelCall != nil {
		r.OnModelCall(r.Model.Name())
	}
	out, err := r.Model.Suggest(ctx, query, r.Env)
	if err != nil || len(out) == 0 {
		// A provider outage should not throw away a real answer we already
		// have, even if its tool is missing here.
		if best != nil {
			return r.finish(best, start), nil
		}
		if err != nil {
			return nil, err
		}
		return nil, ErrNoMatch
	}
	for i := range out {
		out[i].Source = SourceModel
	}
	out = r.finish(out, start)

	// Write-back is best effort: a failed write costs latency next time and
	// nothing else. Synchronous on purpose - the process is about to exit.
	if r.Cache != nil {
		for _, res := range out {
			_ = r.Cache.Put(ctx, key, r.Env.Platform, res)
		}
	}
	return out, nil
}

func toResults(entries []corpus.Entry, src Source, conf float64) []Result {
	out := make([]Result, 0, len(entries))
	for _, e := range entries {
		out = append(out, Result{
			Command: e.Command, Desc: e.Desc, Tool: e.Tool,
			Source: src, Confidence: conf,
		})
	}
	return out
}

// finish annotates results with risk, caps the list and stamps latency. Every
// success path goes through it, so a command cannot reach the confirm prompt
// unclassified.
func (r *Resolver) finish(out []Result, start time.Time) []Result {
	limit := r.MaxResults
	if limit <= 0 {
		limit = 3
	}
	if len(out) > limit {
		out = out[:limit]
	}
	elapsed := time.Since(start)
	for i := range out {
		out[i].Risk, out[i].RiskReason = risk.Classify(out[i].Command)
		out[i].Installed = r.installed(out[i].Tool)
		out[i].Elapsed = elapsed
	}
	return out
}

// maxQueryTokens bounds the fixed-size "already matched" array in score. A
// query longer than this is not a CLI request; the extra tokens are dropped.
const maxQueryTokens = 16

// fuzzy asks the corpus for its best rows under our scoring. The scan is linear
// over the embedded bytes, but allocation-free: scoring reads raw phrase bytes
// and only the winners are turned into entries.
func (r *Resolver) fuzzy(key string) []Result {
	qt := strings.Fields(key)
	if len(qt) == 0 {
		return nil
	}
	if len(qt) > maxQueryTokens {
		qt = qt[:maxQueryTokens]
	}

	limit := r.MaxResults
	if limit <= 0 {
		limit = 3
	}

	// Over-fetch, then rank installed tools first. Taking only `limit` from the
	// scan would hand back three commands for tools this machine does not have
	// and never look at the runnable fourth. The extra rows cost a PATH lookup
	// each, memoized, on a path that already scans 30k rows.
	over := limit * 8
	if over < 24 {
		over = 24
	}

	cands := corpus.Best(r.Env.Platform, over, func(phrase, tool []byte) float64 {
		return score(qt, phrase, tool)
	})

	out := make([]Result, 0, len(cands))
	for _, c := range cands {
		out = append(out, Result{
			Command: c.Entry.Command, Desc: c.Entry.Desc, Tool: c.Entry.Tool,
			Source: SourceStaticFuzzy, Confidence: c.Score,
		})
	}

	out = r.preferInstalled(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// score is token coverage of the query by the row, with a bonus when the query
// names the tool. Coverage beats edit distance here because users describe
// intent in their own words ("shell into pod") rather than misspelling a known
// phrase, and edit distance scores a rewording near zero.
//
// It takes raw bytes and allocates nothing: it runs once per corpus row, tens
// of thousands of times per miss.
func score(qt []string, phrase, tool []byte) float64 {
	var used [maxQueryTokens]bool
	hits, tokens := 0, 0

	for len(phrase) > 0 {
		tok := phrase
		if i := bytes.IndexByte(phrase, ' '); i >= 0 {
			tok, phrase = phrase[:i], phrase[i+1:]
		} else {
			phrase = nil
		}
		if len(tok) == 0 {
			continue
		}
		tokens++
		for i, q := range qt {
			// One row token satisfies at most one query token, so "list list"
			// cannot fake coverage of a two-word query.
			if !used[i] && tokenMatch(tok, q) {
				used[i], hits = true, hits+1
				break
			}
		}
	}
	if hits == 0 || tokens == 0 {
		return 0
	}

	// coverage: did the row explain the query. precision: is the row mostly
	// about it, or did it just happen to contain a common word.
	coverage := float64(hits) / float64(len(qt))
	precision := float64(hits) / float64(tokens)
	s := 0.7*coverage + 0.3*precision

	for _, q := range qt {
		if string(tool) == q {
			s += 0.15 // "docker prune" should outrank a generic prune row
			break
		}
	}
	if s > 1 {
		s = 1
	}
	return s
}

// tokenMatch is equality, or a shared prefix once both tokens are long enough
// to make that meaningful. It absorbs plural and tense drift ("container" vs
// "containers") without a stemmer, and without allocating.
func tokenMatch(tok []byte, q string) bool {
	if string(tok) == q {
		return true
	}
	if len(tok) < 4 || len(q) < 4 {
		return false
	}
	n := len(tok)
	if len(q) < n {
		n = len(q)
	}
	for i := 0; i < n; i++ {
		if tok[i] != q[i] {
			return false
		}
	}
	return true
}
