# Amnesia — Architecture

Status: **v0.1 (CLI) is implemented** — `internal/corpus`, `internal/resolve`,
`internal/risk`, `internal/model`, `internal/store`, `cmd/amnesia`,
`tools/corpus-gen`. Sections 4 (memory pipeline) and 5 (MCP) are the plan for
v0.2/v0.3 and are not built yet.

One decision changed during implementation: **v0.1 ships no database.** The
learned cache is an append-only JSONL file, so the binary has zero external
module dependencies. SQLite arrives in v0.2 with the memory store, where FTS5
earns its weight; see §2.

---

## 0. The product-shape decision

The two features look unrelated. They are not — but they are not equals either.

|                | `amnesia <query>` (NL→CLI)      | Memory daemon + MCP            |
| -------------- | ------------------------------- | ------------------------------ |
| Crowded?        | Very (`tldr`, `thefuck`, `aichat`, Warp AI, `gh copilot`) | Young (mem0, OpenMemory) |
| Time to value  | 5 seconds                        | Days (needs sessions to accrue) |
| Trust required | Low                              | High (reads every AI transcript) |
| Retention      | Low (it is a lookup)             | High (it is a store)            |

So: **one binary, but they are not co-equal features.** The CLI is the acquisition
funnel — it works offline, in five seconds, with no daemon and no config, and it is
what earns the GitHub star. The memory layer is the product and the moat, and it is
**opt-in**, enabled by an explicit `amnesia daemon enable`.

Why one binary rather than two projects:

- Distribution is the hard part of OSS CLI adoption. Two install flows halves adoption.
- They genuinely share: config, the model-client abstraction, the on-disk store, the
  redaction pipeline, and the release pipeline.
- `amnesia recall` and `amnesia <query>` are the same muscle memory.

Why they stay in **separate packages with a one-way import edge**: `internal/resolve`
must never import `internal/daemon` or `internal/mcpsrv`. That edge is what keeps
startup cost near zero, and it is enforced in CI (see §9). If the memory layer ever
outgrows the CLI, that edge is also the seam to split on.

### The latency claim, honestly

**Sub-millisecond end-to-end is not achievable and should not be promised.** A Go
process launch on Linux is ~2–4 ms before `main` runs; on Windows, more. What *is*
achievable and what we should claim:

| Stage             | Budget | Measured (v0.1, linux/amd64, 30,007 rows) |
| ----------------- | ------ | ----------------------------------------- |
| Process start     | 2–4 ms (floor, not ours to spend) | ~4 ms |
| Corpus exact hit  | < 100 µs | **~2.2 ms cold, ~10 µs warm** |
| Corpus fuzzy miss | ~2–5 ms | **~13 ms** |
| Learned cache hit | ~1 ms (read one JSONL file) | ~2 ms |
| **Total, one invocation** | **~5–10 ms** | **6.4 ms** — indistinguishable from instant |

The cold exact-hit figure is one-time index construction: a memchr sweep over the
4.2 MB embedded blob to find line starts. The benchmark amortizes it away (9.7 µs
per op); a real process pays it once. It could be removed by binary-searching the
raw bytes with line snapping instead of an offsets array, but at 6.4 ms total
against a ~100 ms perception threshold, that is optimizing something nobody can
feel. The claim in the README was corrected to the measured number instead.

Claim "instant, offline, no network" in the README and put the µs number on the
*lookup*, which is the honest and still impressive framing. Do not build a
client/daemon IPC path to chase the process-start cost: a Unix-socket round trip
costs more than the lookup it saves.

---

## 1. Process model — one binary, three entry points

```
amnesia <query>        short-lived, ~10ms, exits         cmd/amnesia → internal/resolve
amnesia daemon         long-lived, supervised by the OS  → internal/daemon
amnesia mcp            stdio, spawned by the IDE         → internal/mcpsrv
amnesia recall <q>     short-lived, reads the store      → internal/recall
```

**Do not write a supervisor.** Use `github.com/kardianos/service` to emit a
launchd plist / systemd `--user` unit / Windows service and let the OS restart it.
`amnesia service install` writes it; that is the whole feature.

**The MCP server does not embed the daemon.** IDEs spawn `amnesia mcp` as a stdio
subprocess, possibly several at once. It is a read-mostly client of the same SQLite
file. Concurrency is handled by SQLite in WAL mode, not by a lock we write.

```
  Cursor ─┐
  Claude ─┼─ spawn ─→ amnesia mcp (stdio, N instances, read-mostly)
  Zed    ─┘                        │
                                   ▼
                          ~/.local/share/amnesia/amnesia.db  (WAL)
                                   ▲
   ~/.cursor/**, ~/.claude/** ──────┤
        (fsnotify)      amnesia daemon (1 instance, sole writer)
```

Single-writer, many-reader. The daemon holds the only write connection for ingest;
the CLI's cache write-back is a short transaction and is best-effort by design.

---

## 2. Storage — one file, zero CGO

**The constitutional constraint: no CGO, ever.** It is what makes "single binary"
true, makes `GOOS=windows GOARCH=arm64 go build` work, and makes goreleaser produce
8 platforms from one CI job. Every storage decision falls out of it.

| Need | Choice | Why not the alternative |
| ---- | ------ | ----------------------- |
| Pre-seeded commands | `go:embed` of a sorted TSV, binary-searched | It is read-only and known at build time. A database for read-only build-time data is pure cost: open latency, a file to ship, a migration story. |
| Learned cache (v0.1) | **append-only JSONL**, zero deps | v0.1 has no memory layer, so nothing yet needs FTS, transactions or concurrent writers. A file keeps the binary at *zero external dependencies*, which is a stronger README line than any database. |
| Memory + FTS (v0.2) | **`modernc.org/sqlite`** (pure Go) | `bbolt` opens faster but has no full-text search, and you would then run two engines, two schemas, two backup stories. When FTS5 is actually needed, the cache moves in alongside it. |
| Vectors (v0.2) | int8-quantized flat matrix, brute-force cosine | `sqlite-vec` is excellent but needs CGO — disqualified. LanceDB's Go bindings are immature. `chromem-go` is a fine drop-in if you would rather not own the math. |

Verify FTS5 is compiled into the `modernc.org/sqlite` version you pin; assert it in
a startup test, not at runtime.

### The retrieval decision that de-risks the project

**When the memory layer lands, ship it with FTS5 keyword search and no embeddings at all.**

Your own example query is `amnesia fetch the Jira 123 database fix from last week`.
"Jira 123" is an exact token and a date filter. Vector search is *worse* than BM25 at
that. Developer memory is full of identifiers — error codes, file paths, ticket IDs,
function names — where lexical retrieval wins outright.

This removes the single hardest dependency (a pure-Go embedding model does not
meaningfully exist; ONNX Runtime means CGO) from the critical path to a shipped
product. Add vectors in v0.2 as **hybrid** retrieval — Reciprocal Rank Fusion over
BM25 + cosine — behind a pluggable `Embedder`:

- `ollama` — default when `localhost:11434` answers (`nomic-embed-text`)
- `api` — Voyage / Gemini / OpenAI, batched, opt-in
- `none` — FTS5 only, and genuinely good

Store embeddings in a sidecar `vectors.bin` (fixed-stride, mmap-able) keyed by rowid,
not as SQLite BLOBs — so a re-embed after a model change is `rm vectors.bin`, not a
migration.

### Schema sketch

```sql
CREATE TABLE memory (
  id INTEGER PRIMARY KEY,
  source TEXT NOT NULL,        -- 'cursor' | 'claude-code' | ...
  session_id TEXT NOT NULL,
  project TEXT,                -- repo root, for scoping recall to cwd
  role TEXT NOT NULL,          -- 'user' | 'assistant'
  content TEXT NOT NULL,       -- post-redaction
  ts INTEGER NOT NULL,
  content_hash TEXT NOT NULL   -- dedup; ingest is at-least-once
);
CREATE UNIQUE INDEX memory_dedup ON memory(source, session_id, content_hash);
CREATE VIRTUAL TABLE memory_fts USING fts5(content, content=memory, content_rowid=id);

CREATE TABLE cache (              -- learned CLI resolutions
  key TEXT, platform TEXT, command TEXT, description TEXT, tool TEXT,
  model TEXT, hits INTEGER DEFAULT 0, created_at INTEGER,
  PRIMARY KEY (key, platform, command)
);

CREATE TABLE watermark (source TEXT PRIMARY KEY, path TEXT, offset INTEGER, mtime INTEGER);
```

`watermark` is what makes ingest resumable and idempotent across daemon restarts.

---

## 3. The resolution cascade

Implemented in [`internal/resolve/resolve.go`](../internal/resolve/resolve.go).
Strictly ordered by cost; every stage may decline; nothing below a hit is touched.

```
query ──▶ Normalize ──▶ ① corpus exact  ──hit──▶ risk.Classify ──▶ confirm ──▶ exec
                              │miss
                              ▼
                        ② learned cache ──hit──▶ ┘
                              │miss
                              ▼
                        ③ corpus fuzzy  ──score ≥ floor──▶ ┘
                              │below floor
                              ▼
                        ④ model (Groq/Ollama) ──▶ write-back to ② ──▶ ┘
                              │nil model (offline)
                              ▼
                          ErrNoMatch  (fail, never guess)
```

Design points worth defending in review:

- **`Normalize` is shared by the corpus generator and the resolver.** If they drift,
  exact lookup silently degrades to fuzzy and nobody notices. `TestInvariants`
  asserts every stored phrase is already normalized — that is the whole reason it
  exists.
- **Scoring is token coverage, not edit distance.** Users reword ("shell into a
  running container"), they do not misspell. Levenshtein over whole phrases scores
  rewording near zero. Coverage × precision, plus a bonus when the query names the
  tool, gets `docker exec -it` to 0.83 while `kubectl exec` sits at 0.55.
- **`FuzzyFloor` is the cost/correctness dial.** Too low, confident wrong commands;
  too high, you pay an API call for something the corpus already knew. Tune it
  against a golden query set, not by feel.
- **Offline must be a real mode, not a degraded one.** `Model == nil` returns
  `ErrNoMatch` rather than guessing. Failing is the correct behaviour for a tool
  that hands you something to execute.
- **Every success path goes through `finish()`**, which is where `risk.Classify`
  runs. There is no route to the confirm prompt that skips classification.

### Safety (this is a tool that runs shell commands)

Three layers, in `internal/risk` and `cmd/amnesia`:

1. **Never auto-execute.** Ever. Even `--yes` shows the command first.
2. **Graduated confirmation.** Safe → `[Y/n]`. Caution → `[y/N]` with a reason.
   Destructive → retype the tool name. A reflexive `y` is how someone loses a cluster.
3. **Placeholders are never auto-filled.** A command containing `<...>` is printed,
   not offered. An LLM guessing a container name is how you restart the wrong pod.

**The injection chain nobody mentions until it bites:** the memory bank stores
untrusted text (anything an AI session saw — a README, a web page, a dependency). If
`amnesia recall` feeds that to a model that then proposes a command, attacker-authored
text has reached your shell. Mitigations: memory is never fed into the *resolve*
prompt (separate code paths, separate prompts); retrieved content is delimited and
marked untrusted; and layer 2 still applies to anything that comes back.

---

## 4. The memory pipeline

```
fsnotify ──▶ debounce 2s ──▶ snapshot ──▶ Collector.Extract ──▶ redact ──▶ dedup ──▶ SQLite
```

**Snapshot before reading — this is the sharp edge.** Cursor and Claude Desktop hold
their SQLite files open in WAL mode. Reading a live WAL database from another process
is not safe to do casually, and `immutable=1` is *wrong* here (it ignores the WAL, so
you read stale data). The safe pattern:

1. Debounce 2 s after the last event (editors write in bursts).
2. Copy `db`, `db-wal` and `db-shm` to a temp dir.
3. Open the **copy** with `?mode=ro`, extract, delete.

Never write to a foreign tool's database. Not once, not for a marker.

**Two shapes of source, and the second is much easier:**

| Tool | State | Strategy |
| ---- | ----- | -------- |
| Claude Code | `~/.claude/projects/**/*.jsonl` | Append-only. Tail from a byte watermark. Cheap, exact, no snapshot needed. |
| Cursor | `state.vscdb` (SQLite, blobs in `ItemTable`) | Snapshot + read. Schema is undocumented and *will* change. |
| Claude Desktop | LevelDB / IndexedDB | Hardest; may need a version-pinned reader. |

### The Collector interface — the OSS contribution surface

```go
type Collector interface {
    Name() string
    Detect() (roots []string, ok bool)          // is this tool installed?
    Watch() []string                            // globs to hand fsnotify
    Extract(ctx context.Context, path string) ([]Turn, error)
    Watermark() (string, int64)                 // resumable ingest
}
```

One file per tool, one testdata fixture per tool, registered in an init table. This
is the single most important structural decision for the project, because it turns
"support my tool" into a ~100-line PR with an obvious template — a good-first-issue
factory. It also contains the blast radius: Cursor shipping a schema change breaks
one collector and one test, not the daemon.

**Every collector ships with a captured fixture** (`testdata/cursor/0.42/state.vscdb`,
redacted). Schema drift then shows up as a red test, not a silent gap in someone's
memory. Add a nightly CI job that runs collectors against the newest fixtures.

### Redaction is not optional

Run before anything is written, never after:
- High-confidence secret patterns (AWS keys, GitHub PATs, JWTs, private key headers,
  `postgres://user:pass@`) → replaced with `[REDACTED:type]`.
- `.env`-shaped lines, `Authorization:` headers.
- User-configurable deny-list of paths and regexes.

This is cheap, it is the difference between a tool people trust and one they
uninstall, and it cannot be retrofitted — unredacted rows are already on disk.

---

## 5. MCP server

`amnesia mcp` over stdio, using the official `github.com/modelcontextprotocol/go-sdk`.

**Three tools, not thirty.** Tool-choice accuracy collapses as the surface grows, and
every IDE pays the schema in context on every request:

- `search_memory(query, project?, since?, limit?)` → ranked snippets with source + timestamp
- `get_session(session_id)` → the full thread around a hit
- `save_memory(content, tags?)` → let the agent write deliberately, not just observe

Return **snippets with provenance, not summaries**. Summarizing at retrieval time
burns a model call, adds latency inside the IDE's own loop, and throws away the exact
tokens the calling model actually wanted. Summarize only in `amnesia recall`, where a
human is reading.

Ship the config snippet in the README for Claude Code, Cursor and Zed. Installation
friction is the whole battle here.

---

## 6. Seeding the corpus — and keeping it from rotting

**Do not hand-write the corpus.** A hand-curated set of 5,000 Docker/kubectl/ffmpeg
commands is a full-time maintenance job that goes stale within a release cycle. This
is the trap in the whole project.

**Generate from [tldr-pages](https://github.com/tldr-pages/tldr)** (CC-BY-4.0, ~10k
pages, community-maintained, *already* split by platform: `common/linux/osx/windows`).
Someone solved this problem and keeps solving it.

```
tools/corpus-gen/  ──▶  clone tldr @ pinned tag
                   ──▶  parse: description line → Phrase, code line → Command
                   ──▶  corpus.Normalize(phrase)        (same function the resolver uses)
                   ──▶  LC_ALL=C sort by phrase         (binary search invariant)
                   ──▶  internal/corpus/data/commands.tsv  (committed, reviewable)
```

Commit the generated TSV. It is diffable, so a bad generator run is visible in review,
and the build needs no network. CI opens a PR when upstream moves; a human merges.
Carry the CC-BY attribution in `NOTICE` and `--version`.

### Versions and OS — where to stop

You asked about per-version corpora. **Do not build a version matrix.** Cost scales
with tools × versions; the benefit is concentrated in about twenty famous breaking
changes. Instead:

1. **Platform** is a corpus column (already implemented) — real, cheap, high value.
2. **Version** is a small rewrite-rule table, not a dimension:
   ```
   docker-compose up   → docker compose up      (compose v1 → v2)
   kubectl run --generator=...  → removed in 1.18
   ```
   Detect the installed version lazily (`docker --version`, cached 24h in SQLite),
   apply rules, warn. ~30 rules covers the pain.
3. **Escape hatch:** when the corpus and reality disagree, the model fallback already
   handles it, and the answer gets cached. The cascade *is* the version strategy.

The trade-off, stated plainly: this is occasionally wrong at the edges for old tool
versions, in exchange for a corpus one person can maintain. A version matrix is
correct and unmaintainable; pick the one that still exists in a year.

### Consistency checks in CI

- Sortedness and field count (`TestInvariants`) — binary search is silently wrong without it.
- Every phrase already normalized — otherwise exact lookup silently degrades.
- Golden query set: ~200 real questions → expected command. Fails the build if the
  fuzzy scorer regresses. This is the test that lets you tune `FuzzyFloor` fearlessly.
- Optional, high value: run the safe read-only subset against real tool containers.

---

## 7. Model fallback

```go
type Model interface {
    Suggest(ctx context.Context, query string, env Env) ([]Result, error)
    Name() string
}
```

- **Default: Ollama if present** (`localhost:11434`). Zero config, zero cost, zero
  data leaving the machine — the best possible first-run experience.
- **Then: any OpenAI-shaped endpoint.** Groq, DeepSeek, OpenRouter, Together are one
  base URL apart. Do not vendor four SDKs; write ~150 lines of `net/http` and support
  all of them. `AMNESIA_MODEL=groq/llama-3.3-70b`.
- **Structured output**: request strict JSON (`{command, description, tool, danger}`).
  Reject anything that does not parse rather than regexing prose out of a response.
- **Bounded**: 5 s timeout, one retry, then `ErrNoMatch`. A CLI that hangs is worse
  than one that says no.
- Inject `Env` (OS, shell, detected tool versions) into the prompt — it is the
  difference between `ss -tulpn` and `lsof -iTCP`.

---

## 8. Repository layout

```
amnesia/
├── cmd/amnesia/              # argv routing, TUI, confirm prompt — thin
├── internal/
│   ├── corpus/               # embedded TSV + binary search        ✅ built
│   │   └── data/commands.tsv #   generated, committed, sorted
│   ├── resolve/              # the cascade                          ✅ built
│   ├── risk/                 # destructive-command classifier       ✅ built
│   ├── model/                # one OpenAI-shaped client, many providers  ✅ built
│   ├── store/                # learned cache (JSONL now, +sqlite in v0.2) ✅ built
│   ├── daemon/               # fsnotify loop, debounce, snapshot
│   │   └── collect/          # ← the contribution surface
│   │       ├── collector.go  #   the interface + registry
│   │       ├── claudecode.go
│   │       ├── cursor.go
│   │       └── testdata/     #   redacted fixtures per tool/version
│   ├── redact/               # secret scrubbing, pre-write
│   ├── mcpsrv/               # 3 MCP tools over stdio
│   └── recall/               # human-facing retrieval + summarization
├── tools/corpus-gen/         # tldr → commands.tsv
├── docs/
├── .goreleaser.yml
└── README.md
```

No `pkg/`. Nothing here is a library yet; `internal/` keeps the API surface at zero
and lets you refactor freely for the first year. Promote to `pkg/` the day someone
actually asks to import it.

**CI enforces the import edge** (`go list -deps ./cmd/amnesia | grep daemon` must be
empty for the resolve path) — otherwise the startup budget erodes one innocent import
at a time.

---

## 9. Open-source roadmap

### Sequencing — ship the funnel first

| Release | Contents | The point |
| ------- | -------- | --------- |
| **v0.1 "it just works"** ✅ | CLI + corpus + fuzzy + Ollama/Groq fallback + JSONL cache. No daemon, no dependencies. | Works in 5 s offline. This is the release you post. |
| **v0.2 "it remembers"** | Daemon + Claude Code collector (JSONL, easiest) + FTS5 + `amnesia recall`. Opt-in. | The novel claim, with one collector done well. |
| **v0.3 "it plugs in"** | MCP server + Cursor collector + `amnesia service install`. | Cross-tool memory becomes literally true. |
| **v0.4** | Hybrid vector search, more collectors (community), team sync (opt-in, E2EE). | Scale, mostly by other people. |

Do not launch on the memory daemon. On day one a memory bank is empty and the product
has no value; the CLI is useful in the first five seconds. Let the daemon accumulate
value quietly behind an opt-in flag.

### README structure (the top 200px decide everything)

1. **An asciinema GIF above the fold.** `amnesia check disk usage` → `df -h` → `[Y/n]`,
   with the latency visible. No paragraph beats this.
2. One sentence: *"Natural-language CLI and a unified memory layer for every AI coding
   tool on your machine. One binary. Works offline."*
3. Install, three lines, brew / scoop / `curl | sh` — and since you ship a
   `curl | sh`, publish checksums and say so.
4. **"Your data never leaves your machine"** as a top-level section, not a footnote.
   State plainly: local SQLite, no telemetry, no account, no network call unless you
   configure a model, `--offline` to hard-disable, `amnesia forget` to delete,
   `sqlite3 ~/.local/share/amnesia/amnesia.db` to read it yourself. This is the first
   objection on Hacker News and the first reason people uninstall. Answer it before
   it is asked.
5. Then features, then MCP config snippets, then contributing.

### Making contribution easy

- **`collect/README.md`: "Add your AI tool in ~100 lines."** Interface, a worked
  example, how to capture a fixture. Label them `good-first-issue` per tool and let
  the long tail (Zed, Windsurf, Continue, Aider, Cline) arrive as PRs.
- **`corpus/README.md`: "Add a command."** Explain that rows are *generated* — so the
  fix belongs upstream in tldr-pages — and keep a small `overrides.tsv` for things
  tldr genuinely lacks. Without this, you will drown in well-meant corpus PRs.
- Golden-query tests mean a contributor can prove their change helps. That is what
  makes scoring PRs mergeable instead of arguable.
- **Apache-2.0**, not MIT: the patent grant matters for a tool companies install on
  developer machines, and it is the license enterprise legal teams wave through.

### The two real risks

1. **"A daemon that reads all my AI conversations" is a hard sell.** Mitigation is
   structural, not rhetorical: opt-in, redaction before write, an inspectable plain
   SQLite file, `amnesia forget`, zero telemetry, and a reproducible build. Say it
   loudly in the README.
2. **Collector rot.** Cursor's schema is undocumented and will change without notice.
   Mitigation: fixtures + nightly CI + per-tool blast radius + a graceful
   "collector disabled, schema changed" message instead of a crash loop.

---

## 10. What is deliberately not being built

- No client/daemon IPC for the CLI — the round trip costs more than the lookup.
- No custom service supervisor — launchd/systemd/SCM already do it.
- No vector index, and no database at all in v0.1 — nothing yet needs one.
- No cobra/viper/lipgloss — stdlib `flag` and a dozen bytes of ANSI cover the whole CLI.
- No per-version corpus matrix — ~30 rewrite rules and the model fallback cover it.
- No inverted index for fuzzy — a linear scan over embedded bytes is milliseconds.
- No cloud sync — it is the feature that turns a privacy story into a privacy problem.
