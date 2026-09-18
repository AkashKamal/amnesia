// Command amnesia resolves natural language to a shell command, offline first.
//
// v0.1 is the CLI only. `daemon`, `mcp` and `recall` are reserved here so the
// command surface does not change under users when the memory layer lands; see
// docs/ARCHITECTURE.md.
//
// The import graph is the latency budget: this package and internal/resolve
// must not reach anything that opens a file or a socket at startup. CI enforces
// it.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/AkashKamal/amnesia/internal/config"
	"github.com/AkashKamal/amnesia/internal/corpus"
	"github.com/AkashKamal/amnesia/internal/model"
	"github.com/AkashKamal/amnesia/internal/resolve"
	"github.com/AkashKamal/amnesia/internal/risk"
	"github.com/AkashKamal/amnesia/internal/store"
	"github.com/AkashKamal/amnesia/internal/toolpath"
)

// Set via -ldflags at release time.
var version = "dev"

// placeholder matches <container>, <pod-name> and friends, but deliberately not
// a shell redirect - `foo > out.txt` is a real command, not a template.
var placeholder = regexp.MustCompile(`<[a-zA-Z_][a-zA-Z0-9_.-]*>`)

const usage = `amnesia - natural language to shell commands, offline first

usage:
  amnesia <what you want to do>     resolve a command and confirm before running
  amnesia setup                     pick a provider and paste an API key
  amnesia model [spec] [api-key]    show or set the model non-interactively
  amnesia doctor                    show what amnesia detected, and what to fix
  amnesia forget                    delete everything amnesia has cached
  amnesia version                   print the version

setup:
  amnesia works with no setup at all; the built-in corpus is offline.
  For the questions it cannot answer confidently, run "amnesia setup" and
  pick a provider - Ollama, Claude, Gemini, ChatGPT, Groq, DeepSeek and more.

flags:
  --offline     never call a model; corpus and cache only
  --yes         skip the prompt for commands classified safe
  --json        print candidates as JSON and exit without running anything
  --no-cache    do not read or write the learned cache
  --min-confidence <0..1>
                how sure the corpus must be before answering instead of
                asking the model. Higher is more accurate and slower.

environment:
  all optional, and they override the config file
  AMNESIA_MODEL      provider[/model], e.g. groq/llama-3.3-70b-versatile, ollama
  AMNESIA_API_KEY    key for the chosen provider (or GROQ_API_KEY, OPENAI_API_KEY, ...)
  AMNESIA_BASE_URL   any OpenAI-compatible endpoint (LM Studio, llama.cpp, vLLM)
  AMNESIA_TIMEOUT    e.g. 5m, for a slow local model
  AMNESIA_MIN_CONFIDENCE  0..1, the corpus confidence floor
  AMNESIA_OFFLINE    set to anything to force offline
  AMNESIA_HOME       where to keep the config and cache
`

type options struct {
	offline bool
	yes     bool
	asJSON  bool
	noCache bool
	minConf float64
}

func main() {
	os.Exit(run())
}

func run() int {
	// Handled before flag parsing: these are spelled as flags often enough that
	// treating them as undefined flags and exiting 2 would just be rude.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Printf("amnesia %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
			return 0
		case "help", "--help", "-h":
			fmt.Print(usage)
			return 0
		}
	}

	var opt options
	fs := flag.NewFlagSet("amnesia", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.BoolVar(&opt.offline, "offline", false, "")
	fs.BoolVar(&opt.yes, "yes", false, "")
	fs.BoolVar(&opt.asJSON, "json", false, "")
	fs.BoolVar(&opt.noCache, "no-cache", false, "")
	fs.Float64Var(&opt.minConf, "min-confidence", 0, "")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}

	args := fs.Args()
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}

	switch args[0] {
	case "doctor":
		return doctor(opt)
	case "setup":
		return setupCmd(args[1:])
	case "model":
		return modelCmd(args[1:])
	case "forget":
		return forget()
	case "daemon", "mcp", "recall", "service":
		fmt.Fprintf(os.Stderr, "amnesia: %q is part of the memory layer, landing in v0.2.\n"+
			"Follow https://github.com/AkashKamal/amnesia for progress.\n", args[0])
		return 1
	}

	return resolveAndRun(strings.Join(args, " "), opt)
}

func resolveAndRun(query string, opt options) int {
	r := resolve.New(detectEnv())

	// A cached PATH index, rather than resolve's per-tool exec.LookPath. On WSL
	// the naive version cost 5.9s per query; see internal/toolpath.
	if dir, err := config.Dir(); err == nil {
		r.HasTool = toolpath.Has(dir)
	}

	// How sure the corpus must be before answering instead of asking the model.
	// Flag beats config beats the measured default.
	cfg := config.Load()
	if cfg.MinConfidence > 0 {
		r.EscalateBelow = cfg.MinConfidence
	}
	if opt.minConf > 0 {
		r.EscalateBelow = opt.minConf
	}

	var cache *store.Cache
	if !opt.noCache {
		c, err := store.Open()
		if err != nil {
			warn("cache unavailable: %v", err)
		} else {
			cache, r.Cache = c, c
		}
	}
	if !opt.offline {
		if m := model.New(cfg); m != nil {
			r.Model = m
			r.OnModelCall = func(name string) {
				if opt.asJSON {
					return // never contaminate machine-readable output
				}
				fmt.Fprintf(os.Stderr, "  %s\n", dim("asking "+name+" - a local model can take a moment to load"))
			}
		}
	}

	results, err := r.Resolve(context.Background(), query)
	if err != nil {
		if errors.Is(err, resolve.ErrNoMatch) {
			fmt.Fprintf(os.Stderr, "amnesia: no command found for %q\n", query)
			if r.Model == nil && !opt.offline {
				// The most common reason amnesia cannot answer is that nobody
				// ever told it about a model. Say so here, where it matters,
				// rather than making the user go looking.
				fmt.Fprintln(os.Stderr, "  no model configured, so this is corpus-only.")
				fmt.Fprintln(os.Stderr, "  run `amnesia model` to set one up (a local one is free).")
			}
			return 1
		}
		fmt.Fprintln(os.Stderr, "amnesia:", err)
		return 1
	}

	if opt.asJSON {
		return printJSON(results)
	}

	top := results[0]
	fmt.Fprintf(os.Stderr, "\n  %s\n", bold(top.Command))
	if top.Desc != "" {
		fmt.Fprintf(os.Stderr, "  %s\n", dim(top.Desc))
	}
	fmt.Fprintf(os.Stderr, "  %s\n", dim(fmt.Sprintf("%s · %.0f%% · %s", top.Source, top.Confidence*100, top.Elapsed.Round(time.Microsecond))))
	for _, alt := range results[1:] {
		fmt.Fprintf(os.Stderr, "  %s %s\n", dim("or"), dim(alt.Command))
	}
	fmt.Fprintln(os.Stderr)

	ok, err := confirm(top, opt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amnesia:", err)
		return 1
	}
	if !ok {
		return 0
	}

	// The cache is only written after a model call produced something, and only
	// once the user has seen it. Caching a command nobody looked at would teach
	// amnesia its own mistakes.
	if cache != nil && top.Source == resolve.SourceModel {
		if err := cache.Put(context.Background(), corpus.Normalize(query), r.Env.Platform, top); err != nil {
			warn("could not cache: %v", err)
		}
	}

	return execute(top.Command)
}

// confirm gates execution. Safe and Caution take y/N; Destructive requires
// retyping the tool name, because a reflexive "y" is how someone loses a
// cluster. Commands with placeholders are never offered at all - amnesia does
// not guess a container name on your behalf.
func confirm(r resolve.Result, opt options) (bool, error) {
	if !r.Installed {
		// Offering to run a tool that is not here just produces a confusing
		// shell error two keystrokes later.
		fmt.Fprintf(os.Stderr, "  %s\n", dim(r.Tool+" is not installed on this machine - nothing to run"))
		return false, nil
	}
	if placeholder.MatchString(r.Command) {
		fmt.Fprintln(os.Stderr, "  "+dim("fill in the <placeholders> and run it yourself"))
		return false, nil
	}

	// Order matters. --yes is explicit consent and is the only way to use
	// amnesia from a script, so it must not require a terminal. A destructive
	// command needs a typed answer and therefore a human, so --yes never
	// applies to one and a script simply cannot run it.
	switch r.Risk {
	case risk.Destructive:
		if !isTerminal() {
			fmt.Fprintln(os.Stderr, "  "+dim("destructive, and no terminal to confirm at; not running it"))
			return false, nil
		}
		fmt.Fprintf(os.Stderr, "  %s %s\n", red("DESTRUCTIVE:"), r.RiskReason)
		fmt.Fprintf(os.Stderr, "  type %q to run, anything else to cancel: ", r.Tool)
		typed, ok := askLine()
		return ok && typed == r.Tool && r.Tool != "", nil

	case risk.Caution:
		if opt.yes {
			return true, nil
		}
		if !isTerminal() {
			fmt.Fprintln(os.Stderr, "  "+dim("not a terminal; not running anything (pass --yes to consent)"))
			return false, nil
		}
		fmt.Fprintf(os.Stderr, "  %s. Execute? [y/N]: ", r.RiskReason)
		answer, ok := askLine()
		return ok && yes(answer, false), nil

	default:
		if opt.yes {
			return true, nil
		}
		if !isTerminal() {
			fmt.Fprintln(os.Stderr, "  "+dim("not a terminal; not running anything (pass --yes to consent)"))
			return false, nil
		}
		fmt.Fprint(os.Stderr, "  Execute? [Y/n]: ")
		answer, ok := askLine()
		// No input at all is not consent, even though the default is yes.
		return ok && yes(answer, true), nil
	}
}

func yes(answer string, dflt bool) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		return dflt
	}
}

func execute(command string) int {
	sh, flagName := shell()
	cmd := exec.Command(sh, flagName, command)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "amnesia:", err)
		return 1
	}
	return 0
}

func printJSON(results []resolve.Result) int {
	type out struct {
		Command    string  `json:"command"`
		Desc       string  `json:"description,omitempty"`
		Tool       string  `json:"tool,omitempty"`
		Source     string  `json:"source"`
		Confidence float64 `json:"confidence"`
		Risk       string  `json:"risk"`
		RiskReason string  `json:"risk_reason,omitempty"`
		Installed  bool    `json:"installed"`
	}
	list := make([]out, 0, len(results))
	for _, r := range results {
		list = append(list, out{r.Command, r.Desc, r.Tool, r.Source.String(), r.Confidence, r.Risk.String(), r.RiskReason, r.Installed})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(list); err != nil {
		fmt.Fprintln(os.Stderr, "amnesia:", err)
		return 1
	}
	return 0
}

func doctor(opt options) int {
	env := detectEnv()
	cfg := config.Load()

	fmt.Printf("amnesia %s\n\n", version)
	fmt.Printf("  platform    %s (GOOS=%s)\n", env.Platform, runtime.GOOS)
	fmt.Printf("  shell       %s\n", env.Shell)
	fmt.Printf("  corpus      %d commands, built in\n", corpus.Len())
	fmt.Printf("  config      %s\n", cfg.Path)
	if c, err := store.Open(); err == nil {
		fmt.Printf("  cache       %s\n", c.Path())
	}

	fmt.Println()
	if opt.offline {
		fmt.Println("  model       disabled by --offline")
		fmt.Println()
		fmt.Println("amnesia works offline. The model is only consulted when the built-in")
		fmt.Println("corpus has no answer.")
		return 0
	}

	st := model.Describe(cfg)
	if st.Ready {
		fmt.Printf("  model       %s/%s  (%s)\n", st.Provider, st.Model, origin(st.Origin))
		if cfg.APIKey != "" {
			fmt.Printf("  api key     %s  (%s)\n", config.MaskKey(cfg.APIKey), cfg.APIKeyFrom)
		}
		fmt.Println()
		fmt.Println("Everything is set up. amnesia answers offline first, and only asks the")
		fmt.Println("model when the corpus does not know.")
		return 0
	}

	fmt.Println("  model       none  (offline only)")
	fmt.Println()
	fmt.Println("amnesia still works: the corpus answers most questions with no model at all.")
	fmt.Println("To also handle the ones it does not know:")
	fmt.Println()
	if st.Hint != "" {
		fmt.Printf("  %s\n\n", st.Hint)
	}
	fmt.Print(setupHelp)
	return 0
}

func corpusSize() int { return corpus.Len() }

func origin(o config.Origin) string {
	if o == "" {
		return "detected"
	}
	return string(o)
}

const setupHelp = `  Free and private, on your own machine:
      1. install Ollama          https://ollama.com/download
      2. ollama pull llama3.2:1b (~1.3 GB, and any chat model works)
      3. nothing else - amnesia finds a running Ollama by itself

  Or a hosted provider, nothing to download:
      amnesia model groq/llama-3.3-70b-versatile <api-key>

  Providers: ollama, groq, deepseek, openai, openrouter, together, custom
  "custom" plus AMNESIA_BASE_URL covers LM Studio, llama.cpp, vLLM and friends.

  Run "amnesia model" any time to see or change this.
`

// modelCmd is the whole setup story: show what is configured, or set it.
//
// It writes a config file so a provider is configured once, rather than
// exporting environment variables into every shell the user ever opens - which
// on Windows is a genuinely unpleasant thing to ask of someone, and is the main
// reason a CLI like this gets abandoned during setup.
func modelCmd(args []string) int {
	cfg := config.Load()

	if len(args) == 0 {
		st := model.Describe(cfg)
		if st.Ready {
			fmt.Printf("using %s/%s (%s)\n", st.Provider, st.Model, origin(st.Origin))
		} else {
			fmt.Println("no model configured - amnesia is answering offline only")
			if st.Hint != "" {
				fmt.Printf("\n  %s\n", st.Hint)
			}
		}
		fmt.Println()
		fmt.Print(setupHelp)
		fmt.Printf("\n  config file: %s\n", cfg.Path)
		return 0
	}

	spec := args[0]
	if spec == "none" || spec == "off" {
		if err := config.Clear(); err != nil {
			fmt.Fprintln(os.Stderr, "amnesia:", err)
			return 1
		}
		fmt.Println("cleared. amnesia will answer offline only.")
		return 0
	}

	name, _, _ := strings.Cut(spec, "/")
	if !knownProvider(name) {
		fmt.Fprintf(os.Stderr, "amnesia: unknown provider %q\n  known: %s\n",
			name, strings.Join(model.Names(), ", "))
		return 2
	}

	apiKey := cfg.APIKey // keep an existing key when only the model changes
	if len(args) > 1 {
		apiKey = args[1]
	}

	path, err := config.Save(spec, apiKey, cfg.BaseURL, -1)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amnesia:", err)
		return 1
	}
	fmt.Printf("saved to %s\n", path)

	// Report what that actually resolves to. A typo or a missing key should
	// surface now, not the next time the corpus happens to miss.
	st := model.Describe(config.Load())
	if st.Ready {
		fmt.Printf("using %s/%s\n", st.Provider, st.Model)
		return 0
	}
	fmt.Fprintf(os.Stderr, "\nnot usable yet:\n  %s\n", st.Hint)
	return 1
}

func knownProvider(name string) bool {
	for _, n := range model.Names() {
		if n == name {
			return true
		}
	}
	return false
}

func forget() int {
	c, err := store.Open()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amnesia:", err)
		return 1
	}
	if err := c.Forget(); err != nil {
		fmt.Fprintln(os.Stderr, "amnesia:", err)
		return 1
	}
	fmt.Println("amnesia: cache cleared")
	return 0
}

func detectEnv() resolve.Env {
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "osx" // tldr-pages platform name; the corpus uses it too
	}
	return resolve.Env{Platform: platform, Shell: shellName()}
}

func shellName() string {
	if runtime.GOOS == "windows" {
		if os.Getenv("PSModulePath") != "" {
			return "powershell"
		}
		return "cmd"
	}
	if s := os.Getenv("SHELL"); s != "" {
		return s[strings.LastIndex(s, "/")+1:]
	}
	return "sh"
}

func shell() (string, string) {
	if runtime.GOOS == "windows" {
		return "powershell", "-Command"
	}
	if s := os.Getenv("SHELL"); s != "" {
		return s, "-c"
	}
	return "/bin/sh", "-c"
}

func warn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "amnesia: "+format+"\n", a...)
}

// isTerminal reports whether there is a human who can answer a prompt.
//
// The obvious check - is stdin a character device - is wrong, and wrong in the
// dangerous direction: /dev/null IS a character device. A macOS stress run
// caught amnesia executing a command under `< /dev/null`, because the prompt
// read EOF, took the empty answer as the [Y/n] default, and ran it. Every cron
// job, CI step, systemd unit and Makefile rule invokes programs exactly that
// way.
//
// stdlib has no tty check, and x/term is a dependency this binary does not
// have. Excluding the null device covers the case that actually bites; the
// EOF guard in askLine covers the rest.
func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(fi, null) {
		return false
	}
	return true
}

// askLine reads one answer. It distinguishes "the user pressed enter", which
// means take the default, from "there is no input at all", which must never
// mean yes no matter what the default is.
func askLine() (string, bool) {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", false
	}
	return strings.TrimSpace(line), true
}

func color(code, s string) string {
	fi, err := os.Stderr.Stat()
	if os.Getenv("NO_COLOR") != "" || err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func bold(s string) string { return color("1", s) }
func dim(s string) string  { return color("2", s) }
func red(s string) string  { return color("1;31", s) }
