package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AkashKamal/amnesia/internal/config"
	"github.com/AkashKamal/amnesia/internal/model"
)

// setupCmd is the whole onboarding story: pick a provider, paste a key, done.
//
// It writes a config file so a provider is configured once rather than exported
// into every shell the user ever opens - which on Windows is a genuinely
// unpleasant thing to ask, and is a real reason a CLI gets abandoned during
// setup.
//
// The key is checked against the provider the moment it is pasted. A typo
// should surface here, in the place where it can be retyped, and not days later
// on the one query the corpus could not answer.
func setupCmd(args []string) int {
	cfg := config.Load()

	// Non-interactive: a CI step, a Dockerfile, or a pipe. Print the one-liner
	// rather than hanging on a prompt nobody can answer.
	if !isTerminal() {
		fmt.Fprintln(os.Stderr, "amnesia setup needs a terminal. Non-interactively, use:")
		fmt.Fprintln(os.Stderr, "    amnesia model <provider>[/<model>] <api-key>")
		fmt.Fprintf(os.Stderr, "    providers: %s\n", strings.Join(model.Names(), ", "))
		return 1
	}

	catalog := model.Catalog()
	ollamaHave := model.OllamaModels()

	fmt.Println()
	fmt.Printf("  amnesia answers %s commands offline, with no model at all.\n", humanCount(corpusSize()))
	fmt.Println("  A model handles the rest: the questions the built-in corpus does not know,")
	fmt.Println("  and anything it is not confident about. Answers get cached, so you pay once.")
	fmt.Println()

	if st := model.Describe(cfg); st.Ready {
		fmt.Printf("  Currently using %s/%s (%s)\n\n", st.Provider, st.Model, origin(st.Origin))
	}

	for i, p := range catalog {
		status := ""
		switch {
		case p.Name == "ollama" && len(ollamaHave) > 0:
			status = fmt.Sprintf("detected, %d model(s)", len(ollamaHave))
		case p.Name == "ollama":
			status = "not running"
		case p.EnvKey != "" && os.Getenv(p.EnvKey) != "":
			status = p.EnvKey + " is set"
		}
		line := fmt.Sprintf("  %2d. %-22s %s", i+1, p.Label, p.Note)
		if status != "" {
			line = fmt.Sprintf("%-62s [%s]", line, status)
		}
		fmt.Println(strings.TrimRight(line, " "))
	}
	fmt.Printf("  %2d. %-22s %s\n", len(catalog)+1, "None", "corpus only, fully offline")
	fmt.Println()

	choice, ok := prompt(fmt.Sprintf("  Choose [1-%d]: ", len(catalog)+1))
	if !ok {
		return 1
	}
	n, err := strconv.Atoi(strings.TrimSpace(choice))
	if err != nil || n < 1 || n > len(catalog)+1 {
		fmt.Fprintln(os.Stderr, "  not a valid choice")
		return 2
	}

	if n == len(catalog)+1 {
		if err := config.Clear(); err != nil {
			fmt.Fprintln(os.Stderr, "amnesia:", err)
			return 1
		}
		fmt.Println("\n  Cleared. amnesia will answer from the corpus only.")
		return 0
	}

	p := catalog[n-1]
	fmt.Println()

	switch {
	case p.Name == "ollama":
		return setupOllama(ollamaHave)
	case p.Name == "custom":
		return setupCustom(p)
	default:
		return setupHosted(p, cfg)
	}
}

func setupOllama(have []string) int {
	if len(have) == 0 {
		fmt.Println("  Ollama is not running, or has no chat models.")
		fmt.Println()
		fmt.Println("    1. install it:  https://ollama.com/download")
		fmt.Println("    2. pull one:    ollama pull llama3.2:1b   (~1.3 GB)")
		fmt.Println()
		fmt.Println("  Then run `amnesia setup` again. Nothing to configure - amnesia finds it.")
		return 1
	}

	fmt.Printf("  Found %d model(s). amnesia will use %s (the smallest, so it answers fastest).\n",
		len(have), have[0])
	fmt.Println("  Name a different one with: amnesia model ollama/<name>")

	path, err := config.Save("ollama", "", "", -1)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amnesia:", err)
		return 1
	}
	fmt.Printf("\n  Saved to %s\n", path)
	fmt.Println("  Nothing will leave your machine.")
	return 0
}

func setupCustom(p model.Provider) int {
	fmt.Println("  Any server that speaks the OpenAI API: LM Studio, llama.cpp, vLLM, LiteLLM.")
	url, ok := prompt("  Base URL (e.g. http://localhost:1234/v1): ")
	if !ok || strings.TrimSpace(url) == "" {
		fmt.Fprintln(os.Stderr, "  no URL given")
		return 2
	}
	url = strings.TrimSpace(url)

	key, _ := prompt("  API key (blank if it needs none): ")
	key = strings.TrimSpace(key)

	p.BaseURL = url
	return validateAndSave(p, key, url, "")
}

func setupHosted(p model.Provider, cfg config.Config) int {
	fmt.Printf("  %s\n", p.Label)
	if p.KeyURL != "" {
		fmt.Printf("  Get a key: %s\n", p.KeyURL)
	}
	if p.EnvKey != "" && os.Getenv(p.EnvKey) != "" {
		fmt.Printf("  (%s is already set in this shell; press enter to use it)\n", p.EnvKey)
	}
	fmt.Println()
	fmt.Println("  " + dim("The key is stored in a file only you can read, and is never printed back."))

	key, ok := prompt("  Paste your API key: ")
	if !ok {
		return 1
	}
	key = strings.TrimSpace(key)
	if key == "" {
		key = os.Getenv(p.EnvKey)
	}
	if key == "" && cfg.APIKey != "" {
		key = cfg.APIKey
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "  no key given")
		return 2
	}
	return validateAndSave(p, key, "", "")
}

// validateAndSave is the step that makes setup worth running: it proves the key
// works and asks the provider which model to use, so nothing is hardcoded and
// nothing is discovered later at the worst possible moment.
// preferID, when set, is a model the caller explicitly asked for: validate the
// key but do not second-guess their choice.
func validateAndSave(p model.Provider, key, baseURL, preferID string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Print("\n  Checking the key... ")
	c := model.NewFor(p, key, "")
	if err := c.Validate(ctx); err != nil {
		fmt.Println("failed")
		fmt.Fprintf(os.Stderr, "\n  %v\n", err)
		fmt.Fprintln(os.Stderr, "\n  Nothing was saved. Run `amnesia setup` again to retry.")
		return 1
	}
	fmt.Println("works")

	chosen := preferID
	if chosen != "" {
		fmt.Printf("  Using model...      %s (your choice)\n", chosen)
	} else {
		fmt.Print("  Picking a model...  ")
		discovered, derr := c.Discover(ctx)
		if derr != nil || discovered == "" {
			// Not fatal: the key is good, so fall back to the provider's
			// default rather than making the user start over.
			chosen = p.Fallback
			fmt.Printf("%s (default)\n", chosen)
		} else {
			chosen = discovered
			fmt.Println(chosen)
		}
	}

	spec := p.Name + "/" + chosen
	path, err := config.Save(spec, key, baseURL, -1)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amnesia:", err)
		return 1
	}

	fmt.Printf("\n  Saved to %s\n", path)
	fmt.Printf("  amnesia will use %s when the corpus is not confident.\n", spec)
	fmt.Println()
	fmt.Println("  " + dim("Change the model:   amnesia model "+p.Name+"/<model-id>"))
	fmt.Println("  " + dim("Turn it off again:  amnesia model none"))
	return 0
}

// prompt reads one line. It returns ok=false on EOF, so a closed stdin can
// never be mistaken for an answer.
func prompt(label string) (string, bool) {
	fmt.Print(label)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Println()
		return "", false
	}
	return strings.TrimSpace(line), true
}

func humanCount(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%d,%03d", n/1000, n%1000)
}
