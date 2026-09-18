// Package model is the last stage of the resolution cascade: an LLM that turns
// a query the corpus could not answer into a command.
//
// Every provider worth supporting - Ollama, Groq, DeepSeek, OpenRouter,
// Together, OpenAI - speaks the same /v1/chat/completions shape. So there is
// one client and providers are just a (base URL, key, model) triple. Vendoring
// four SDKs to send the same JSON would be four dependencies and four
// breakages.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/AkashKamal/amnesia/internal/resolve"
)

// Provider is a known endpoint. Anything not listed here still works via
// AMNESIA_BASE_URL - the point is zero-config for the common cases, not an
// exhaustive registry.
type Provider struct {
	Name    string
	BaseURL string
	EnvKey  string
	Default string // default model id
}

var providers = map[string]Provider{
	"ollama":     {"ollama", "http://localhost:11434/v1", "", "llama3.2"},
	"groq":       {"groq", "https://api.groq.com/openai/v1", "GROQ_API_KEY", "llama-3.3-70b-versatile"},
	"deepseek":   {"deepseek", "https://api.deepseek.com/v1", "DEEPSEEK_API_KEY", "deepseek-chat"},
	"openai":     {"openai", "https://api.openai.com/v1", "OPENAI_API_KEY", "gpt-4o-mini"},
	"openrouter": {"openrouter", "https://openrouter.ai/api/v1", "OPENROUTER_API_KEY", "meta-llama/llama-3.3-70b-instruct"},
	"together":   {"together", "https://api.together.xyz/v1", "TOGETHER_API_KEY", "meta-llama/Llama-3.3-70B-Instruct-Turbo"},
}

type Client struct {
	provider string
	baseURL  string
	apiKey   string
	model    string
	http     *http.Client
}

// New picks a provider from the environment and returns nil when none is
// configured. A nil Model is not an error: it is offline mode, and the corpus
// still answers most queries.
//
//	AMNESIA_MODEL=groq/llama-3.3-70b-versatile   explicit provider/model
//	AMNESIA_MODEL=ollama                         provider only, default model
//	GROQ_API_KEY=...                             inferred from whichever key is set
//	(nothing set)                                Ollama, if it answers locally
func New() resolve.Model {
	if os.Getenv("AMNESIA_OFFLINE") != "" {
		return nil
	}

	if spec := os.Getenv("AMNESIA_MODEL"); spec != "" {
		name, id, _ := strings.Cut(spec, "/")
		p, ok := providers[name]
		if !ok {
			return nil
		}
		if id == "" {
			id = p.Default
		}
		return build(p, id)
	}

	// An explicitly set API key is an unambiguous signal of intent, so it wins
	// over a local Ollama the user may have running for something else.
	for _, name := range []string{"groq", "deepseek", "openai", "openrouter", "together"} {
		p := providers[name]
		if os.Getenv(p.EnvKey) != "" {
			return build(p, p.Default)
		}
	}

	// Last: Ollama, but only if it is actually listening. The probe is a TCP
	// dial with a tight deadline, because this runs on the miss path of a CLI
	// that promises to feel instant.
	p := providers["ollama"]
	if conn, err := net.DialTimeout("tcp", "localhost:11434", 150*time.Millisecond); err == nil {
		conn.Close()
		return build(p, p.Default)
	}
	return nil
}

func build(p Provider, id string) *Client {
	base := p.BaseURL
	if v := os.Getenv("AMNESIA_BASE_URL"); v != "" {
		base = v
	}
	key := os.Getenv("AMNESIA_API_KEY")
	if key == "" && p.EnvKey != "" {
		key = os.Getenv(p.EnvKey)
	}
	return &Client{
		provider: p.Name,
		baseURL:  strings.TrimSuffix(base, "/"),
		apiKey:   key,
		model:    id,
		// Bounded on purpose: a CLI that hangs is worse than one that says no.
		http: &http.Client{Timeout: 8 * time.Second},
	}
}

func (c *Client) Name() string { return c.provider + "/" + c.model }

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
		return nil, fmt.Errorf("%s: %w", c.provider, err)
	}
	defer resp.Body.Close()

	var out chatResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: bad response: %w", c.provider, err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("%s: %s", c.provider, out.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: http %d", c.provider, resp.StatusCode)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("%s: empty response", c.provider)
	}
	return parseSuggestions(out.Choices[0].Message.Content)
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
