// Package model is the last stage of the resolution cascade: an LLM that turns
// a query the corpus could not answer into a command.
//
// Every provider worth supporting - Ollama, Groq, DeepSeek, OpenRouter,
// Together, OpenAI - speaks the same /v1/chat/completions shape. So there is
// one client and providers are just a (base URL, key, model) triple. Vendoring
// four SDKs to send the same JSON would be four dependencies and four
// breakages.
//
// The guiding rule here is: never assume anything about the user's machine that
// can be asked instead. A hardcoded default model id is wrong for most people
// on the day it ships and wrong for everyone a year later.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/AkashKamal/amnesia/internal/config"
	"github.com/AkashKamal/amnesia/internal/resolve"
)

// Provider is a known endpoint. Anything not listed still works by setting a
// base URL: the point is zero-config for the common cases, not a registry that
// has to be complete.
type Provider struct {
	Name    string
	BaseURL string
	EnvKey  string
	// Fallback is used only when the provider cannot be asked what it has.
	// For Ollama it is a suggestion to print, never a model we assume exists.
	Fallback string
	Local    bool
}

var providers = map[string]Provider{
	"ollama":     {Name: "ollama", BaseURL: "http://localhost:11434/v1", Fallback: "llama3.2", Local: true},
	"groq":       {Name: "groq", BaseURL: "https://api.groq.com/openai/v1", EnvKey: "GROQ_API_KEY", Fallback: "llama-3.3-70b-versatile"},
	"deepseek":   {Name: "deepseek", BaseURL: "https://api.deepseek.com/v1", EnvKey: "DEEPSEEK_API_KEY", Fallback: "deepseek-chat"},
	"openai":     {Name: "openai", BaseURL: "https://api.openai.com/v1", EnvKey: "OPENAI_API_KEY", Fallback: "gpt-4o-mini"},
	"openrouter": {Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", EnvKey: "OPENROUTER_API_KEY", Fallback: "meta-llama/llama-3.3-70b-instruct"},
	"together":   {Name: "together", BaseURL: "https://api.together.xyz/v1", EnvKey: "TOGETHER_API_KEY", Fallback: "meta-llama/Llama-3.3-70B-Instruct-Turbo"},
	// A catch-all for LM Studio, llama.cpp, vLLM, LiteLLM and anything else
	// that speaks the OpenAI shape. The user supplies the base URL.
	"custom": {Name: "custom", Local: false},
}

// Names lists configurable providers, for help text and error messages.
func Names() []string {
	out := make([]string, 0, len(providers))
	for n := range providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

type Client struct {
	provider string
	baseURL  string
	apiKey   string
	model    string
	http     *http.Client
}

// Status explains what amnesia found, so `amnesia doctor` can give an
// instruction rather than a shrug. Ready is false when there is nothing usable;
// Hint is then the single next command the user should run.
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
	c, _ := resolve2(cfg)
	return c
}

// Describe reports what New would do and why, without performing a resolution.
func Describe(cfg config.Config) Status {
	_, st := resolve2(cfg)
	return st
}

func resolve2(cfg config.Config) (resolve.Model, Status) {
	if os.Getenv("AMNESIA_OFFLINE") != "" {
		return nil, Status{Hint: "AMNESIA_OFFLINE is set; unset it to use a model"}
	}

	// 1. An explicit choice always wins, from config file or environment.
	if cfg.Model != "" && cfg.Model != "none" {
		name, id, _ := strings.Cut(cfg.Model, "/")
		p, ok := providers[name]
		if !ok {
			return nil, Status{Hint: fmt.Sprintf("unknown provider %q; try one of: %s",
				name, strings.Join(Names(), ", "))}
		}
		p = applyOverrides(p, cfg)

		if id == "" {
			// Ask the provider what it has rather than guessing an id.
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
			return nil, Status{Hint: "provider \"custom\" needs a base URL: set AMNESIA_BASE_URL"}
		}
		if !p.Local && p.EnvKey != "" && key(p, cfg) == "" {
			return nil, Status{Hint: fmt.Sprintf("%s needs an API key: amnesia model %s <api-key>", p.Name, cfg.Model)}
		}
		return build(p, id, cfg), Status{Ready: true, Provider: p.Name, Model: id, Origin: cfg.ModelFrom}
	}

	// 2. Nothing chosen. A key sitting in the environment is an unambiguous
	//    signal, and it beats a local Ollama the user may run for other things.
	for _, name := range []string{"groq", "deepseek", "openai", "openrouter", "together"} {
		p := providers[name]
		if os.Getenv(p.EnvKey) != "" {
			return build(p, p.Fallback, cfg), Status{
				Ready: true, Provider: p.Name, Model: p.Fallback, Origin: config.FromDetected,
			}
		}
	}

	// 3. A running Ollama, using a model it actually has. This is the whole
	//    zero-configuration path, and it must never name a model on faith.
	p := providers["ollama"]
	p = applyOverrides(p, cfg)
	if installed := ollamaModels(p.BaseURL); len(installed) > 0 {
		return build(p, installed[0], cfg), Status{
			Ready: true, Provider: p.Name, Model: installed[0], Origin: config.FromDetected,
		}
	}

	return nil, Status{} // nothing configured and nothing detected; the caller prints the setup guide
}

func hintNoModels(p Provider) string {
	if reachable(p.BaseURL) {
		return fmt.Sprintf("%s is running but has no chat models. Pull one, smallest first:\n"+
			"    ollama pull %s", p.Name, p.Fallback)
	}
	return fmt.Sprintf("%s is not reachable at %s. Start it, or pick a hosted provider with `amnesia model`.",
		p.Name, p.BaseURL)
}

func applyOverrides(p Provider, cfg config.Config) Provider {
	if cfg.BaseURL != "" {
		p.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
		// A user-supplied endpoint on loopback is still a local model, and gets
		// the long cold-start timeout. This is what makes LM Studio, llama.cpp
		// and vLLM behave sensibly without amnesia knowing they exist.
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
		http:     &http.Client{Timeout: timeout(p)},
	}
}

// timeout is generous for anything on loopback and tight for anything remote.
// A local model may have to be read off disk into VRAM before it answers a
// single token, which takes tens of seconds for a large one; a hosted API that
// has not replied in 20s is not going to. One number cannot serve both.
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

func (c *Client) Name() string { return c.provider + "/" + c.model }

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

// ollamaModels returns installed chat models, smallest first.
//
// Smallest first because turning one English sentence into one shell command is
// an easy task, and on the miss path of a CLI that promises to feel instant, a
// 1B model answering in 300ms beats a 70B model answering in 20s. Anyone who
// disagrees names a model explicitly and is never second-guessed.
func ollamaModels(baseURL string) []string {
	client := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get(root(baseURL) + "/api/tags")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var payload struct {
		Models []struct {
			Name    string `json:"name"`
			Size    int64  `json:"size"`
			Details struct {
				Family string `json:"family"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil
	}

	type m struct {
		name string
		size int64
	}
	var usable []m
	for _, x := range payload.Models {
		if isEmbedding(x.Name, x.Details.Family) {
			continue
		}
		usable = append(usable, m{x.Name, x.Size})
	}
	sort.Slice(usable, func(i, j int) bool {
		if usable[i].size != usable[j].size {
			return usable[i].size < usable[j].size
		}
		return usable[i].name < usable[j].name // deterministic across runs
	})

	out := make([]string, 0, len(usable))
	for _, x := range usable {
		out = append(out, x.name)
	}
	return out
}

// isEmbedding filters models that cannot hold a conversation. Ollama does not
// flag them directly, so this matches the naming conventions every embedding
// model on the registry actually follows, plus the BERT families they are built
// on. A false negative costs one clear error; a false positive costs nothing,
// because nobody picks an embedding model to write shell commands.
func isEmbedding(name, family string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"embed", "bge-", "bge:", "gte-", "gte:", "e5-", "minilm"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return strings.Contains(strings.ToLower(family), "bert")
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

type chatReq struct {
	Model          string    `json:"model"`
	Messages       []message `json:"messages"`
	Temperature    float64   `json:"temperature"`
	ResponseFormat *rformat  `json:"response_format,omitempty"`
}

type rformat struct {
	Type string `json:"type"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResp struct {
	Choices []struct {
		Message message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type suggestion struct {
	Command     string `json:"command"`
	Description string `json:"description"`
	Tool        string `json:"tool"`
}

func (c *Client) Suggest(ctx context.Context, query string, env resolve.Env) ([]resolve.Result, error) {
	body, err := json.Marshal(chatReq{
		Model:       c.model,
		Temperature: 0,
		// Not every gateway honours this, which is why parseSuggestions also
		// copes with a JSON object wrapped in prose.
		ResponseFormat: &rformat{Type: "json_object"},
		Messages: []message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: fmt.Sprintf("OS: %s\nShell: %s\n\nRequest: %s", env.Platform, env.Shell, query)},
		},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.explain(err)
	}
	defer resp.Body.Close()

	var out chatResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: unreadable response: %w", c.Name(), err)
	}
	if out.Error != nil {
		return nil, c.explainAPI(resp.StatusCode, out.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.explainAPI(resp.StatusCode, "")
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("%s: empty response", c.Name())
	}
	return parseSuggestions(out.Choices[0].Message.Content)
}

// explain turns a transport failure into something the user can act on. "context
// deadline exceeded" tells a reader nothing about what to do next.
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
			return fmt.Errorf("nothing is listening at %s. Start it (`ollama serve`) or run `amnesia model` "+
				"to pick a hosted provider", c.baseURL)
		}
		return fmt.Errorf("%s is unreachable at %s", c.Name(), c.baseURL)
	}
	return fmt.Errorf("%s: %w", c.Name(), err)
}

func (c *Client) explainAPI(status int, msg string) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s rejected the API key. Set a working one: amnesia model %s <api-key>", c.Name(), c.Name())
	case http.StatusNotFound:
		if isLoopback(c.baseURL) {
			return fmt.Errorf("%s does not have model %q. Pull it (`ollama pull %s`), or run "+
				"`amnesia model ollama` to use whichever model you already have", c.provider, c.model, c.model)
		}
		return fmt.Errorf("%s does not have model %q", c.provider, c.model)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%s rate limit reached; try again shortly", c.Name())
	}
	if msg != "" {
		return fmt.Errorf("%s: %s", c.Name(), msg)
	}
	return fmt.Errorf("%s: http %d", c.Name(), status)
}

// parseSuggestions is deliberately strict about structure and forgiving about
// packaging: models wrap JSON in prose or fences often enough that failing on
// it would be a bad experience, but regexing commands out of free text would
// mean executing something we never really parsed.
func parseSuggestions(content string) ([]resolve.Result, error) {
	raw := strings.TrimSpace(content)
	if i := strings.Index(raw, "{"); i >= 0 {
		if j := strings.LastIndex(raw, "}"); j > i {
			raw = raw[i : j+1]
		}
	}

	var payload struct {
		Suggestions []suggestion `json:"suggestions"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, fmt.Errorf("model did not return usable JSON: %w", err)
	}

	out := make([]resolve.Result, 0, len(payload.Suggestions))
	for _, s := range payload.Suggestions {
		cmd := strings.TrimSpace(strings.Trim(s.Command, "`"))
		// A multi-line command cannot be shown in a one-line confirm prompt, so
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
