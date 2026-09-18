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

	"github.com/AkashKamal/amnesia/internal/corpus"
	"github.com/AkashKamal/amnesia/internal/model"
	"github.com/AkashKamal/amnesia/internal/resolve"
	"github.com/AkashKamal/amnesia/internal/risk"
	"github.com/AkashKamal/amnesia/internal/store"
)

// Set via -ldflags at release time.
var version = "dev"

// placeholder matches <container>, <pod-name> and friends, but deliberately not
// a shell redirect - `foo > out.txt` is a real command, not a template.
var placeholder = regexp.MustCompile(`<[a-zA-Z_][a-zA-Z0-9_.-]*>`)

const usage = `amnesia - natural language to shell commands, offline first

usage:
  amnesia <what you want to do>     resolve a command and confirm before running
  amnesia doctor                    show what amnesia detected about this machine
  amnesia forget                    delete everything amnesia has cached
  amnesia version                   print the version

flags:
  --offline     never call a model; corpus and cache only
  --yes         skip the prompt for commands classified safe
  --json        print candidates as JSON and exit without running anything
  --no-cache    do not read or write the learned cache

environment:
  AMNESIA_MODEL      provider[/model], e.g. groq/llama-3.3-70b-versatile, ollama
  AMNESIA_API_KEY    key for the chosen provider (or GROQ_API_KEY, OPENAI_API_KEY, ...)
  AMNESIA_BASE_URL   override the provider endpoint
  AMNESIA_OFFLINE    set to anything to force offline
  AMNESIA_HOME       where to keep the cache
`

type options struct {
	offline bool
	yes     bool
	asJSON  bool
	noCache bool
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
		if m := model.New(); m != nil {
			r.Model = m
		}
	}

	results, err := r.Resolve(context.Background(), query)
	if err != nil {
		if errors.Is(err, resolve.ErrNoMatch) {
			fmt.Fprintf(os.Stderr, "amnesia: no command found for %q\n", query)
			if r.Model == nil {
				fmt.Fprintln(os.Stderr, "  running offline - set AMNESIA_MODEL or start Ollama to ask a model")
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

	if !isTerminal() {
		fmt.Fprintln(os.Stderr, "  "+dim("not a terminal; not running anything"))
		return false, nil
	}

	switch r.Risk {
	case risk.Destructive:
		// --yes deliberately does not apply here.
		fmt.Fprintf(os.Stderr, "  %s %s\n", red("DESTRUCTIVE:"), r.RiskReason)
		fmt.Fprintf(os.Stderr, "  type %q to run, anything else to cancel: ", r.Tool)
		var typed string
		fmt.Scanln(&typed)
		return strings.TrimSpace(typed) == r.Tool && r.Tool != "", nil
	case risk.Caution:
		if opt.yes {
			return true, nil
		}
		fmt.Fprintf(os.Stderr, "  %s. Execute? [y/N]: ", r.RiskReason)
		var answer string
		fmt.Scanln(&answer)
		return yes(answer, false), nil
	default:
		if opt.yes {
			return true, nil
		}
		fmt.Fprint(os.Stderr, "  Execute? [Y/n]: ")
		var answer string
		fmt.Scanln(&answer)
		return yes(answer, true), nil
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
	fmt.Printf("amnesia %s\n", version)
	fmt.Printf("  platform    %s (GOOS=%s)\n", env.Platform, runtime.GOOS)
	fmt.Printf("  shell       %s\n", env.Shell)
	fmt.Printf("  corpus      %d commands, embedded\n", corpus.Len())

	if opt.offline {
		fmt.Printf("  model       disabled (--offline)\n")
	} else if m := model.New(); m != nil {
		fmt.Printf("  model       %s\n", m.Name())
	} else {
		fmt.Printf("  model       none - offline. Set AMNESIA_MODEL or run Ollama.\n")
	}

	if c, err := store.Open(); err == nil {
		fmt.Printf("  cache       %s\n", c.Path())
	}
	return 0
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

// isTerminal reports whether we can prompt. Without it, `amnesia ... | tee`
// would hang on a confirm nobody can answer, or worse, take EOF for consent.
func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
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
