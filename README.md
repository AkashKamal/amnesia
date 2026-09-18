<h1 align="center">amnesia</h1>

<p align="center">
  <b>Stop googling CLI flags. Ask in English, get the command, confirm, run.</b><br>
  One static binary · zero dependencies · works offline
</p>

<p align="center">
  <a href="https://github.com/AkashKamal/amnesia/actions/workflows/ci.yml"><img src="https://github.com/AkashKamal/amnesia/actions/workflows/ci.yml/badge.svg" alt="ci"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="license"></a>
  <img src="https://img.shields.io/badge/dependencies-0-brightgreen" alt="zero dependencies">
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
`kubectl rollout undo deployment/api`. You have looked all three up more than
once.

Amnesia resolves your question against an embedded corpus first — microseconds,
offline, no key. Only when it genuinely doesn't know does it ask a model, and
then it **caches the answer**, so you pay for any given question at most once.

```
your query ──▶ corpus exact ──▶ learned cache ──▶ corpus fuzzy ──▶ model
                  ~2ms             ~2ms            ~13ms          ~500ms
                                                                   └─ cached for next time
```

Measured on linux/amd64 over the shipped 30,007-row corpus: **6.4 ms of wall clock
for a full `amnesia check disk usage`**, process start included. Every stage
except the last is offline.

## Install

```sh
go install github.com/AkashKamal/amnesia/cmd/amnesia@latest
```

Or grab a prebuilt binary for Linux, macOS or Windows (amd64 and arm64) from
[releases](https://github.com/AkashKamal/amnesia/releases) and verify it against
`checksums.txt`.

<sub>Homebrew and Scoop packages come once there is a tagged release worth
packaging.</sub>

## Usage

```sh
amnesia <what you want to do>     # resolve, show, confirm, run
amnesia model [spec] [api-key]    # show or set the model used when the corpus misses
amnesia doctor                    # what amnesia detected, and what to fix
amnesia forget                    # delete everything amnesia has cached
```

| Flag | |
|---|---|
| `--offline` | never call a model; corpus and cache only |
| `--yes` | skip the prompt for commands classified safe |
| `--json` | print candidates as JSON, run nothing |
| `--no-cache` | don't read or write the learned cache |

### It will not run something dangerous on a stray keystroke

```console
$ amnesia delete all stopped containers and unused volumes

  docker system prune -a --volumes
  Reclaim space; deletes unused images, networks and volumes
  corpus · 100% · 1.8ms

  DESTRUCTIVE: deletes unused images and volumes
  type "docker" to run, anything else to cancel:
```

Three rules, no exceptions:

- **Nothing runs without being shown first.** Not even with `--yes`.
- **Destructive commands need the tool name retyped.** `--yes` does not apply.
  A reflexive `y` is how people lose clusters.
- **Commands with `<placeholders>` are never offered.** Amnesia will not guess
  your pod name.
- **Commands for tools you don't have are never offered.** Amnesia checks PATH
  and ranks what you can actually run first, then tells you plainly when the
  best answer needs something you haven't installed.

## Optional: a model for the questions the corpus can't answer

Amnesia is useful with no model at all — the corpus is offline and answers most
things. For the rest, point it at a model **once**:

```sh
amnesia model                 # shows what you have, and every way to set it up
```

**Free and private, on your own machine.** Install
[Ollama](https://ollama.com/download), pull any chat model, and amnesia finds it
by itself — no configuration:

```sh
ollama pull llama3.2:1b       # ~1.3 GB. Small is good here: this task is easy,
                              # and a 1B model answers in a second.
```

> Amnesia asks Ollama what you have installed and uses the smallest one. It never
> assumes a model name, so it works whatever you happen to have pulled.

**Or a hosted provider**, nothing to download:

```sh
amnesia model groq/llama-3.3-70b-versatile gsk_your_key_here
```

Saved to a config file, so you set it once instead of exporting environment
variables into every shell. `amnesia model none` turns it back off.

Works with **ollama, groq, deepseek, openai, openrouter, together**, and
`custom` + `AMNESIA_BASE_URL` for anything else that speaks the OpenAI API —
LM Studio, llama.cpp, vLLM, LiteLLM. Anything on `localhost` automatically gets
a long timeout, because a cold local model can take a minute to load.

<details>
<summary>Environment variables (all optional, and they override the config file)</summary>

| | |
|---|---|
| `AMNESIA_MODEL` | `provider[/model]`, e.g. `ollama`, `groq/llama-3.3-70b-versatile` |
| `AMNESIA_API_KEY` | key for the chosen provider (or `GROQ_API_KEY`, …) |
| `AMNESIA_BASE_URL` | any OpenAI-compatible endpoint |
| `AMNESIA_TIMEOUT` | e.g. `5m`, for a slow local model |
| `AMNESIA_OFFLINE` | set to anything to force offline |
| `AMNESIA_HOME` | where the config and cache live |

</details>

## Your data stays on your machine

- **No telemetry. No account. No network call** unless *you* configure a model.
  `--offline` hard-disables egress; `amnesia doctor` shows exactly what is wired up.
- The cache is a plain JSONL file you can read, grep and delete. `amnesia doctor`
  prints the path; `amnesia forget` empties it.
- Your query only ever leaves the machine on a corpus **miss**, and only to the
  provider you configured. Amnesia tells you before it does: it prints which
  model it is asking.
- The config file is `0600` and `amnesia doctor` masks your API key, because
  config output ends up in bug reports and screen shares.

## Roadmap

v0.1 is the CLI. The larger idea is a **unified memory layer for local AI tools** —
Cursor not knowing what you fixed in Claude Code yesterday is a real problem, and
it is the same binary's job.

- **v0.2** — memory daemon (opt-in): watch local AI tool state, keep sessions in
  one local store, `amnesia recall <question>`
- **v0.3** — MCP server, so any IDE can query that memory natively
- **v0.4** — hybrid vector search, community collectors

Design is written up in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Contributing

**A command is wrong or missing?** The corpus is *generated* from
[tldr-pages](https://github.com/tldr-pages/tldr) — fix it upstream and it lands
here on the next sync. Don't hand-edit `commands.tsv`; CI regenerates it.

```sh
make corpus     # re-pull tldr-pages and rebuild the corpus
make test
```

**Ranking feels wrong?** Add your query to the tests in
`internal/resolve/resolve_test.go` and show the score move. That's what makes a
scoring change reviewable instead of arguable.

**Want to help with v0.2?** The collector interface in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#4-the-memory-pipeline) is designed so
each AI tool is ~100 lines and one fixture. Open an issue.

## License

Apache-2.0. The command corpus is derived from
[tldr-pages](https://github.com/tldr-pages/tldr) and redistributed under
CC-BY-4.0 — see [NOTICE](NOTICE).
