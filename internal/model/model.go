// Package model is the last stage of the resolution cascade: an LLM that turns
// a query the corpus could not answer into a command.
//
// The guiding rule is: never assume anything about the user's setup that can be
// asked instead. A hardcoded model id is wrong for many people on the day it
// ships and wrong for everyone a year later, so amnesia asks each provider what
// it actually has and pins the answer at setup time.
//
// Wire formats live in transport.go. There are three, because Anthropic and
// Google do not speak OpenAI's shape and pretending otherwise would eventually
// produce a wrong command.
package model

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/AkashKamal/amnesia/internal/config"
	"github.com/AkashKamal/amnesia/internal/resolve"
)

// Provider is one endpoint amnesia knows how to talk to.
type Provider struct {
	Name    string
	Label   string // what a human calls it
	BaseURL string
	EnvKey  string // conventional environment variable for its key
	KeyURL  string // where to get a key, printed during setup
	Note    string // one line shown in the setup list
	Kind    string // which transport: openai | anthropic | gemini
	Local   bool
	// Fallback is used only when the provider cannot be asked what it has.
	Fallback string
}

var providers = map[string]Provider{
	"ollama": {
		Name: "ollama", Label: "Ollama", Kind: "openai", Local: true,
		BaseURL: "http://localhost:11434/v1", Fallback: "llama3.2",
		Note: "free, runs on your machine, nothing leaves it",
	},
	"claude": {
		Name: "claude", Label: "Claude (Anthropic)", Kind: "anthropic",
		BaseURL: "https://api.anthropic.com/v1", EnvKey: "ANTHROPIC_API_KEY",
		KeyURL:   "https://console.anthropic.com/settings/keys",
		Fallback: "claude-haiku-4-5", Note: "strong on shell and code",
	},
	"gemini": {
		Name: "gemini", Label: "Gemini (Google)", Kind: "gemini",
		BaseURL: "https://generativelanguage.googleapis.com/v1beta", EnvKey: "GEMINI_API_KEY",
		KeyURL:   "https://aistudio.google.com/apikey",
		Fallback: "gemini-2.5-flash", Note: "generous free tier",
	},
	"openai": {
		Name: "openai", Label: "ChatGPT (OpenAI)", Kind: "openai",
		BaseURL: "https://api.openai.com/v1", EnvKey: "OPENAI_API_KEY",
		KeyURL:   "https://platform.openai.com/api-keys",
		Fallback: "gpt-4o-mini", Note: "",
	},
	"groq": {
		Name: "groq", Label: "Groq", Kind: "openai",
		BaseURL: "https://api.groq.com/openai/v1", EnvKey: "GROQ_API_KEY",
		KeyURL:   "https://console.groq.com/keys",
		Fallback: "llama-3.3-70b-versatile", Note: "fastest hosted, free tier",
	},
	"deepseek": {
		Name: "deepseek", Label: "DeepSeek", Kind: "openai",
		BaseURL: "https://api.deepseek.com/v1", EnvKey: "DEEPSEEK_API_KEY",
		KeyURL:   "https://platform.deepseek.com/api_keys",
		Fallback: "deepseek-chat", Note: "cheapest hosted",
	},
	"openrouter": {
		Name: "openrouter", Label: "OpenRouter", Kind: "openai",
		BaseURL: "https://openrouter.ai/api/v1", EnvKey: "OPENROUTER_API_KEY",
		KeyURL:   "https://openrouter.ai/keys",
		Fallback: "meta-llama/llama-3.3-70b-instruct", Note: "one key, many models",
	},
	"together": {
		Name: "together", Label: "Together", Kind: "openai",
		BaseURL: "https://api.together.xyz/v1", EnvKey: "TOGETHER_API_KEY",
		KeyURL:   "https://api.together.xyz/settings/api-keys",
		Fallback: "meta-llama/Llama-3.3-70B-Instruct-Turbo",
	},
	"custom": {
		Name: "custom", Label: "Other (OpenAI-compatible)", Kind: "openai",
		Note: "LM Studio, llama.cpp, vLLM, LiteLLM - set AMNESIA_BASE_URL",
	},
}

// setupOrder is the order `amnesia setup` lists providers: free and local
// first, then the ones most people have heard of.
var setupOrder = []string{"ollama", "claude", "gemini", "openai", "groq", "deepseek", "openrouter", "together", "custom"}

// Catalog returns providers in the order setup presents them.
func Catalog() []Provider {
	out := make([]Provider, 0, len(setupOrder))
	for _, n := range setupOrder {
		out = append(out, providers[n])
	}
	return out
}

func Lookup(name string) (Provider, bool) {
	p, ok := providers[name]
	return p, ok
}

// Names lists provider ids, for help text and error messages.
func Names() []string {
	out := make([]string, 0, len(providers))
	for n := range providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func transportFor(kind string) transport {
	switch kind {
	case "anthropic":
		return anthropicTransport{}
	case "gemini":
		return geminiTransport{}
	default:
		return openAITransport{}
	}
}

type Client struct {
	provider string
	baseURL  string
	apiKey   string
	model    string
	tr       transport
	http     *http.Client
}

func (c *Client) Name() string { return c.provider + "/" + c.model }

// Status explains what amnesia found, so the CLI can give an instruction rather
// than a shrug. Ready is false when there is nothing usable; Hint is then the
// single next thing the user should do.
type Status struct {
	Ready    bool
	Provider string
	Model    string
	Origin   config.Origin
	Hint     string
}

// New returns a model client, or nil for offline. Offline is a supported mode,
// not a failure: the corpus answers most queries without any of this.
func New(cfg config.Config) resolve.Model {
	c, _ := resolveClient(cfg)
	if c == nil {
		return nil // typed nil would satisfy the interface and break `!= nil`
	}
	return c
}

// Describe reports what New would do and why, without sending a request.
func Describe(cfg config.Config) Status {
	_, st := resolveClient(cfg)
	return st
}

func resolveClient(cfg config.Config) (*Client, Status) {
	if os.Getenv("AMNESIA_OFFLINE") != "" {
		return nil, Status{Hint: "AMNESIA_OFFLINE is set; unset it to use a model"}
	}

	// 1. An explicit choice wins, from config file or environment.
	if cfg.Model != "" && cfg.Model != "none" {
		name, id, _ := strings.Cut(cfg.Model, "/")
		p, ok := providers[name]
		if !ok {
			return nil, Status{Hint: fmt.Sprintf("unknown provider %q. Run `amnesia setup`, or pick one of: %s",
				name, strings.Join(Names(), ", "))}
		}
		p = applyOverrides(p, cfg)

		if id == "" {
			if p.Local {
				installed := ollamaModels(p.BaseURL)
				if len(installed) == 0 {
					return nil, Status{Hint: hintNoModels(p)}
				}
				id = installed[0]
			} else {
				id = p.Fallback
			}
		}
		if p.BaseURL == "" {
			return nil, Status{Hint: "provider \"custom\" needs an endpoint: set AMNESIA_BASE_URL"}
		}
		if !p.Local && p.EnvKey != "" && key(p, cfg) == "" {
			return nil, Status{Hint: fmt.Sprintf("%s needs an API key. Run `amnesia setup`.", p.Label)}
		}
		return build(p, id, cfg), Status{Ready: true, Provider: p.Name, Model: id, Origin: cfg.ModelFrom}
	}

	// 2. Nothing chosen. A key already in the environment is unambiguous
	//    intent, and beats a local Ollama the user may run for other things.
	for _, name := range []string{"claude", "gemini", "openai", "groq", "deepseek", "openrouter", "together"} {
		p := providers[name]
		if os.Getenv(p.EnvKey) != "" {
			return build(p, p.Fallback, cfg), Status{
				Ready: true, Provider: p.Name, Model: p.Fallback, Origin: config.FromDetected,
			}
		}
	}

	// 3. A running Ollama, using a model it actually has. This is the whole
	//    zero-configuration path, and it must never name a model on faith.
	p := applyOverrides(providers["ollama"], cfg)
	if installed := ollamaModels(p.BaseURL); len(installed) > 0 {
		return build(p, installed[0], cfg), Status{
			Ready: true, Provider: p.Name, Model: installed[0], Origin: config.FromDetected,
		}
	}

	return nil, Status{} // nothing set up; the caller prints the setup guide
}

func hintNoModels(p Provider) string {
	if reachable(p.BaseURL) {
		return fmt.Sprintf("Ollama is running but has no chat models. Pull a small one:\n" +
			"    ollama pull llama3.2:1b")
	}
	return fmt.Sprintf("Ollama is not reachable at %s. Start it, or run `amnesia setup` to use a hosted provider.", p.BaseURL)
}

func applyOverrides(p Provider, cfg config.Config) Provider {
	if cfg.BaseURL != "" {
		p.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
		// A user-supplied endpoint on loopback is a local model and gets the
		// long cold-start timeout. This is what makes LM Studio, llama.cpp and
		// vLLM behave sensibly without amnesia knowing they exist.
		p.Local = isLoopback(p.BaseURL)
	}
	return p
}

func isLoopback(url string) bool {
	return strings.Contains(url, "localhost") || strings.Contains(url, "127.0.0.1") || strings.Contains(url, "[::1]")
}

func key(p Provider, cfg config.Config) string {
	if cfg.APIKey != "" {
		return cfg.APIKey
	}
	if p.EnvKey != "" {
		return os.Getenv(p.EnvKey)
	}
	return ""
}

func build(p Provider, id string, cfg config.Config) *Client {
	return &Client{
		provider: p.Name,
		baseURL:  strings.TrimSuffix(p.BaseURL, "/"),
		apiKey:   key(p, cfg),
		model:    id,
		tr:       transportFor(p.Kind),
		http:     &http.Client{Timeout: timeout(p)},
	}
}

// NewFor builds a client for setup: an explicit provider, key and model, with
// no config lookup. Used to validate a key the moment it is pasted.
func NewFor(p Provider, apiKey, id string) *Client {
	base := p.BaseURL
	if v := os.Getenv("AMNESIA_BASE_URL"); v != "" {
		base = v
	}
	if id == "" {
		id = p.Fallback
	}
	return &Client{
		provider: p.Name,
		baseURL:  strings.TrimSuffix(base, "/"),
		apiKey:   apiKey,
		model:    id,
		tr:       transportFor(p.Kind),
		http:     &http.Client{Timeout: 20 * time.Second},
	}
}

// Validate checks a key without spending tokens.
func (c *Client) Validate(ctx context.Context) error { return c.tr.validate(ctx, c) }

// Discover asks the provider which model to use, so nothing is hardcoded.
func (c *Client) Discover(ctx context.Context) (string, error) { return c.tr.discover(ctx, c) }

// Model reports the id this client will send.
func (c *Client) Model() string { return c.model }

// timeout is generous on loopback and tight for anything remote. A local model
// may be read off disk into VRAM before it emits a token, which takes tens of
// seconds; a hosted API that has not replied in 20s is not going to.
func timeout(p Provider) time.Duration {
	d := 20 * time.Second
	if p.Local {
		d = 3 * time.Minute
	}
	if v := os.Getenv("AMNESIA_TIMEOUT"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			d = parsed
		}
	}
	return d
}

func reachable(baseURL string) bool {
	client := &http.Client{Timeout: 400 * time.Millisecond}
	resp, err := client.Get(root(baseURL))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// root strips the OpenAI-compatibility suffix: Ollama's native endpoints live
// beside /v1, not under it.
func root(baseURL string) string {
	return strings.TrimSuffix(strings.TrimSuffix(baseURL, "/"), "/v1")
}

const systemPrompt = `You translate a developer's request into a single shell command.

Rules:
- Reply with JSON only: {"suggestions":[{"command":"...","description":"...","tool":"..."}]}
- At most 3 suggestions, best first. "tool" is the executable name, e.g. "docker".
- "description" is one short line, no markdown, no backticks.
- Target the stated OS and shell exactly. Never suggest a command for another platform.
- Use <angle-bracket> placeholders for values you cannot know. Never invent a real
  container name, pod name, path or host.
- If the request is not a shell task, reply {"suggestions":[]}.`

func (c *Client) Suggest(ctx context.Context, query string, env resolve.Env) ([]resolve.Result, error) {
	user := fmt.Sprintf("OS: %s\nShell: %s\n\nRequest: %s", env.Platform, env.Shell, query)
	raw, err := c.tr.complete(ctx, c, systemPrompt, user)
	if err != nil {
		return nil, err
	}
	return parseSuggestions(raw)
}

// explain turns a transport failure into something the user can act on.
// "context deadline exceeded" tells a reader nothing about what to do next.
func (c *Client) explain(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "Timeout"):
		if isLoopback(c.baseURL) {
			return fmt.Errorf("%s timed out. A local model can take a while to load the first time; "+
				"retry, or raise it with AMNESIA_TIMEOUT=5m", c.Name())
		}
		return fmt.Errorf("%s timed out. Check your connection, or raise AMNESIA_TIMEOUT", c.Name())
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host"):
		if isLoopback(c.baseURL) {
			return fmt.Errorf("nothing is listening at %s. Start it (`ollama serve`), or run "+
				"`amnesia setup` to use a hosted provider", c.baseURL)
		}
		return fmt.Errorf("%s is unreachable at %s", c.Name(), c.baseURL)
	}
	return fmt.Errorf("%s: %w", c.Name(), err)
}

func (c *Client) explainAPI(status int, msg string) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s rejected the API key. Run `amnesia setup` to replace it", c.provider)
	case http.StatusNotFound:
		if isLoopback(c.baseURL) {
			return fmt.Errorf("%s does not have model %q. Pull it (`ollama pull %s`), or run "+
				"`amnesia setup`", c.provider, c.model, c.model)
		}
		return fmt.Errorf("%s does not have model %q. Run `amnesia setup` to pick one it does", c.provider, c.model)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%s rate limit reached; try again shortly", c.Name())
	case http.StatusPaymentRequired:
		return fmt.Errorf("%s reports no credit on this account", c.provider)
	}
	if msg != "" {
		return fmt.Errorf("%s: %s", c.Name(), msg)
	}
	return fmt.Errorf("%s: http %d", c.Name(), status)
}

// parseSuggestions is strict about structure and forgiving about packaging:
// models wrap JSON in prose or fences often enough that failing on it would be
// a bad experience, but regexing commands out of free text would mean executing
// something we never really parsed.
func parseSuggestions(content string) ([]resolve.Result, error) {
	raw := strings.TrimSpace(content)
	if i := strings.Index(raw, "{"); i >= 0 {
		if j := strings.LastIndex(raw, "}"); j > i {
			raw = raw[i : j+1]
		}
	}

	var payload struct {
		Suggestions []struct {
			Command     string `json:"command"`
			Description string `json:"description"`
			Tool        string `json:"tool"`
		} `json:"suggestions"`
	}
	if err := jsonUnmarshal([]byte(raw), &payload); err != nil {
		return nil, fmt.Errorf("model did not return usable JSON: %w", err)
	}

	out := make([]resolve.Result, 0, len(payload.Suggestions))
	for _, s := range payload.Suggestions {
		cmd := strings.TrimSpace(strings.Trim(s.Command, "`"))
		// A multi-line command cannot be shown on a one-line confirm prompt, so
		// there is no safe way to present it. Drop it rather than truncate.
		if cmd == "" || strings.ContainsAny(cmd, "\n\r") {
			continue
		}
		// Models do not give calibrated probabilities, so confidence here is
		// just rank, capped below an exact corpus hit. It orders the list; it
		// is not a claim about correctness.
		out = append(out, resolve.Result{
			Command:    cmd,
			Desc:       strings.TrimSpace(s.Description),
			Tool:       strings.TrimSpace(s.Tool),
			Confidence: 0.9 - 0.1*float64(len(out)),
		})
	}
	return out, nil
}
