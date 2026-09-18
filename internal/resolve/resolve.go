// Package resolve implements the resolution cascade: the ordered set of
// strategies Amnesia tries to turn "check disk usage" into "df -h".
//
// The cascade is strictly ordered by cost, and every stage may decline.
// Nothing below the embedded corpus is touched unless the stage above it
// misses, so the common case never opens a file, a socket or a database.
//
//  1. Static exact   embedded corpus, binary search     ~50us, offline
//  2. Learned exact  on-disk cache of past resolutions  ~3ms,  offline
//  3. Static fuzzy   linear scan + token scoring        ~5ms,  offline
//  4. Model          Groq / Ollama / OpenAI-shaped API  ~300ms-2s, network
//
// A model answer is written back to the learned cache, so any given query is
// slow at most once. Stage 4 is the only stage that can leave the machine, and
// a nil Model removes it entirely.
package resolve

import (
	"bytes"
	"context"
	"errors"
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
}

func New(env Env) *Resolver {
	return &Resolver{Env: env, Cache: NopCache{}, FuzzyFloor: 0.55, MaxResults: 3}
}

// Resolve runs the cascade, returning as soon as a stage is confident.
func (r *Resolver) Resolve(ctx context.Context, query string) ([]Result, error) {
	start := time.Now()
	key := corpus.Normalize(query)
	if key == "" {
		return nil, ErrNoMatch
	}

	if out := toResults(corpus.Lookup(key, r.Env.Platform), SourceStaticExact, 1); len(out) > 0 {
		return r.finish(out, start), nil
	}

	if r.Cache != nil {
		// A cache error is reported by the caller and otherwise ignored on
		// purpose: it must not block a resolution the model can still answer.
		if out, err := r.Cache.Get(ctx, key, r.Env.Platform); err == nil && len(out) > 0 {
			return r.finish(out, start), nil
		}
	}

	if out := r.fuzzy(key); len(out) > 0 && out[0].Confidence >= r.FuzzyFloor {
		return r.finish(out, start), nil
	}

	if r.Model == nil {
		return nil, ErrNoMatch
	}
	out, err := r.Model.Suggest(ctx, query, r.Env)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
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

	cands := corpus.Best(r.Env.Platform, limit, func(phrase, tool []byte) float64 {
		return score(qt, phrase, tool)
	})

	out := make([]Result, 0, len(cands))
	for _, c := range cands {
		out = append(out, Result{
			Command: c.Entry.Command, Desc: c.Entry.Desc, Tool: c.Entry.Tool,
			Source: SourceStaticFuzzy, Confidence: c.Score,
		})
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
