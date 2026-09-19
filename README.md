<h1 align="center">amnesia</h1>

<p align="center">
  <b>Stop googling CLI flags. Ask in English, get the command, confirm, run.</b><br>
  One static binary · zero dependencies · works offline
</p>

<p align="center">
  <a href="https://github.com/AkashKamal/amnesia/actions/workflows/ci.yml"><img src="https://github.com/AkashKamal/amnesia/actions/workflows/ci.yml/badge.svg" alt="ci"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="license"></a>
  <img src="https://img.shields.io/badge/dependencies-0-brightgreen" alt="zero dependencies">
  <img src="https://img.shields.io/badge/commands-30%2C011-blue" alt="30,011 commands">
</p>

<!-- TODO before launch: asciinema GIF here. It matters more than this README. -->

```console
$ amnesia check disk usage

  df -h
  Show free disk space, human readable
  corpus · 100% · 2.2ms

  Execute? [Y/n]:
```

No network call. No API key. No config file. It answered from a command corpus
compiled into the binary.

## Why

`docker system df`. `ffmpeg -ss 10 -to 30 -i in.mp4 -c copy out.mp4`.
`kubectl rollout undo deployment/api`. You have looked all three up more than once.

Amnesia resolves your question against **30,011 built-in commands** first —
microseconds, offline, no key. Only when it isn't confident does it ask a model,
and then it **caches the answer**, so you pay for any given question at most once.

```
your query ──▶ corpus exact ──▶ learned cache ──▶ corpus fuzzy ──▶ model
                  ~2ms             ~2ms            ~13ms          ~1s
                                                                   └─ cached for next time
```

**6.4 ms of wall clock** for a full `amnesia check disk usage`, process start
included. Every stage except the last is offline.

## Install

```sh
go install github.com/AkashKamal/amnesia/cmd/amnesia@latest
```

Or grab a prebuilt binary for Linux, macOS or Windows (amd64 and arm64) from
[releases](https://github.com/AkashKamal/amnesia/releases) and verify it against
`checksums.txt`.

## Usage

```sh
amnesia <what you want to do>     # resolve, show, confirm, run
amnesia setup                     # pick a provider, paste a key
amnesia doctor                    # what amnesia detected, and what to fix
amnesia forget                    # delete everything amnesia has cached
```

| Flag | |
|---|---|
| `--offline` | never call a model; corpus and cache only |
| `--yes` | skip the prompt for commands classified safe |
| `--json` | print candidates as JSON, run nothing |
| `--no-cache` | don't read or write the learned cache |
| `--min-confidence <0..1>` | how sure the corpus must be before answering instead of asking a model |

### It will not run something dangerous on a stray keystroke

```console
$ amnesia delete all stopped containers and unused volumes

  docker system prune -a --volumes
  Reclaim space; deletes unused images, networks and volumes
  corpus · 100% · 1.8ms

  DESTRUCTIVE: deletes unused images and volumes
  type "docker" to run, anything else to cancel:
```

Five rules, no exceptions:

- **Nothing runs without being shown first.** Not even with `--yes`.
- **Destructive commands need the tool name retyped.** `--yes` never applies, and
  they cannot run headless at all. A reflexive `y` is how people lose clusters.
- **Commands with `<placeholders>` are never offered.** Amnesia will not guess
  your pod name.
- **Commands for tools you don't have are never offered.** It checks PATH and
  ranks what you can actually run first.
- **No terminal, no execution.** `--yes` is the only headless consent.

Every answer — including one from a model — passes through the same classifier,
so nothing reaches the prompt unlabelled. All 30,011 shippable commands are
audited in CI: zero dangerous commands labelled safe.

Shell injection is structurally impossible: your query is never interpolated
into a shell, only the resolved command is, and only after you confirm it.

## Setup: a model for what the corpus can't answer confidently

```sh
amnesia setup
```

Pick a provider, paste a key, done. The key is checked immediately, and amnesia
asks the provider which models it can actually use rather than guessing an id
that will be stale in a year.

```
   1. Ollama                 free, runs on your machine, nothing leaves it
   2. Claude (Anthropic)     strong on shell and code
   3. Gemini (Google)        generous free tier
   4. ChatGPT (OpenAI)
   5. Groq                   fastest hosted, free tier
   6. DeepSeek               cheapest hosted
   7. OpenRouter             one key, many models
   8. Together
   9. Other (OpenAI-compatible)   LM Studio, llama.cpp, vLLM, LiteLLM
  10. None                   corpus only, fully offline

  Choose [1-10]: 3
  Paste your API key: ****
  Checking the key... works
  Picking a model...  gemini-3.5-flash-lite
```

Claude and Gemini are spoken **natively** — the Messages API and
`generateContent`, not an OpenAI-compatibility shim — because a shim works right
up until it quietly doesn't, and a wrong shell command is the one thing this tool
must not produce.

Saved to a `0600` config file, so you set it once instead of exporting
environment variables into every shell. `amnesia model none` turns it off.

### When does it call the API?

Only when the corpus isn't confident enough. Every answer carries a match score:

| | |
|---|---|
| Exact corpus hit (100%) | answered locally, **never** costs an API call |
| Confident fuzzy match | answered locally |
| Below the threshold | **automatically escalated**, then cached |
| Provider fails or rate-limits | falls back to the best local answer, and says so |

The default threshold is **0.80 when a provider is configured** and 0.55 when one
isn't — there's no point holding out for certainty when there's nothing to
escalate to.

### Measured

68 real engineering questions (`stress/queries.txt`), Windows 11, Gemini
`flash-lite`:

| Setting | Correct first try | In top 3 | Unanswerable |
|---|---|---|---|
| Corpus only, offline | 55% | 67% | 7 |
| **+ model @ 0.80** (default) | **66%** | **75%** | **0** |
| + model @ 1.00 (max accuracy) | **73%** | **82%** | **0** |

Cache behaviour over the same set: **100% hit rate** on the second pass, 0 repeat
API calls, **12 ms cached vs 1.06 s live** — an 88× speedup once an answer is known.

Tune it:

```sh
amnesia --min-confidence 0.95 <query>   # one-off
AMNESIA_MIN_CONFIDENCE=0.95             # this shell
# or min_confidence = 0.95 in the config file
```

**Set it to `1.0` for maximum accuracy:** only exact corpus hits are answered
locally, everything else goes to the model.

<details>
<summary>Non-interactive setup, and environment variables</summary>

```sh
amnesia model claude/claude-haiku-4-5 sk-ant-...
amnesia model gemini <api-key>
amnesia model ollama                      # auto-detects your installed models
```

Both paths validate the key against the provider before saving anything.

| | |
|---|---|
| `AMNESIA_MODEL` | `provider[/model]` |
| `AMNESIA_API_KEY` | key for the chosen provider (or `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `OPENAI_API_KEY`, …) |
| `AMNESIA_BASE_URL` | any OpenAI-compatible endpoint |
| `AMNESIA_MIN_CONFIDENCE` | `0`–`1`, the escalation threshold |
| `AMNESIA_TIMEOUT` | e.g. `5m`, for a slow local model |
| `AMNESIA_OFFLINE` | set to anything to force offline |
| `AMNESIA_HOME` | where the config and cache live |

Anything on `localhost` automatically gets a long timeout, because a cold local
model can take a minute to load.

</details>

## Your data stays on your machine

- **No telemetry. No account. No network call** unless *you* configure a model.
  `--offline` hard-disables egress; `amnesia doctor` shows exactly what is wired up.
- The cache is a plain JSONL file you can read, grep and delete. `amnesia doctor`
  prints the path; `amnesia forget` empties it.
- **Only your question is ever sent** — no history, no file contents, no
  environment. One question, one answer, stateless.
- Your query only leaves the machine on a corpus **miss**, and only to the
  provider you configured. Amnesia prints which model it is asking before it does.
- The config file is `0600` and `amnesia doctor` masks your API key, because
  config output ends up in bug reports and screen shares.

## How it works

Four stages, cheapest first, stopping at the first confident answer.

The corpus ships in two layers: **29,966 rows generated from
[tldr-pages](https://github.com/tldr-pages/tldr)**, so breadth stays current
without anyone hand-maintaining it, plus **45 curated rows** for the
high-frequency intents tldr words like a man page — its `df` page has no
"check disk usage" and no `df -h` at all. Curated rows win on ties, so
regenerating the generated layer can never destroy curation.

Matching is token coverage, not edit distance: people reword ("shell into a
running container"), they don't misspell, and edit distance scores a rewording
near zero.

[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) has the full design.

## Contributing

**A command is wrong or missing?** The corpus is *generated* — fix it upstream in
[tldr-pages](https://github.com/tldr-pages/tldr) and it lands here on the next
sync. Don't hand-edit `commands.tsv`; CI regenerates and diffs it. Genuine gaps
go in `internal/corpus/data/overrides.tsv`.

```sh
make corpus     # re-pull tldr-pages and rebuild the corpus
make test
go run ./stress -bin ./amnesia -mode all    # accuracy, robustness, safety, concurrency, audit
```

**Ranking feels wrong?** Add your query to `stress/queries.txt` and show the
score move. That is what makes a scoring change reviewable instead of arguable.

**The most useful thing right now:** confidence scores are poorly calibrated.
42% of escalations are for questions the corpus already answered correctly but
scored low. Better calibration would cut API calls substantially with no accuracy
loss, and the golden set makes it measurable.

## License

Apache-2.0. The command corpus is derived from
[tldr-pages](https://github.com/tldr-pages/tldr) and redistributed under
CC-BY-4.0 — see [NOTICE](NOTICE).
