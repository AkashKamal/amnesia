package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// A transport is one provider's wire format.
//
// There are three, not one. Groq, DeepSeek, OpenRouter, Together, Ollama and
// anything self-hosted all speak OpenAI's /v1/chat/completions, so they share a
// transport. Anthropic and Google do not: different endpoints, different auth
// headers, different request and response shapes. Routing them through an
// OpenAI-compatibility shim would work until it quietly did not, and a wrong
// command is the one thing this tool must not produce.
//
// These are ~60 lines each against a documented contract, which is the right
// trade for a binary whose whole distribution story is having no dependencies.
// Three vendor SDKs would end that for one request and one response.
type transport interface {
	// complete sends one prompt and returns the model's raw text.
	complete(ctx context.Context, c *Client, system, user string) (string, error)
	// validate checks a key without spending tokens, so `amnesia setup` can say
	// "that key works" instead of failing later on a real query.
	validate(ctx context.Context, c *Client) error
	// discover lists the models this key can actually use and returns the best
	// cheap one. Hardcoding a model id is how integrations rot: ids change, and
	// the id that shipped is wrong for someone on day one.
	discover(ctx context.Context, c *Client) (string, error)
}

const maxOutputTokens = 1024 // one shell command and a one-line description

func (c *Client) post(ctx context.Context, url string, headers map[string]string, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.http.Do(req)
}

func (c *Client) get(ctx context.Context, url string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.http.Do(req)
}

// decodeOrFail reads a JSON body, turning a non-200 into an explained error.
func decodeOrFail(c *Client, resp *http.Response, out any) error {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return c.explainAPI(resp.StatusCode, e.Error.Message)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: unreadable response: %w", c.Name(), err)
	}
	return nil
}

// ----------------------------------------------------------------- openai --

// openAITransport covers OpenAI itself plus every provider that copied its
// shape: Groq, DeepSeek, OpenRouter, Together, Ollama, LM Studio, llama.cpp,
// vLLM, LiteLLM.
type openAITransport struct{}

func (openAITransport) auth(c *Client) map[string]string {
	if c.apiKey == "" {
		return nil // Ollama and other local servers take no key
	}
	return map[string]string{"Authorization": "Bearer " + c.apiKey}
}

func (t openAITransport) complete(ctx context.Context, c *Client, system, user string) (string, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	body := map[string]any{
		"model":       c.model,
		"temperature": 0,
		"max_tokens":  maxOutputTokens,
		// Not every gateway honours this, which is why parseSuggestions also
		// copes with JSON wrapped in prose.
		"response_format": map[string]string{"type": "json_object"},
		"messages":        []msg{{"system", system}, {"user", user}},
	}

	resp, err := c.post(ctx, c.baseURL+"/chat/completions", t.auth(c), body)
	if err != nil {
		return "", c.explain(err)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := decodeOrFail(c, resp, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("%s: empty response", c.Name())
	}
	return out.Choices[0].Message.Content, nil
}

func (t openAITransport) validate(ctx context.Context, c *Client) error {
	resp, err := c.get(ctx, c.baseURL+"/models", t.auth(c))
	if err != nil {
		return c.explain(err)
	}
	var out struct{}
	return decodeOrFail(c, resp, &out)
}

func (t openAITransport) discover(ctx context.Context, c *Client) (string, error) {
	resp, err := c.get(ctx, c.baseURL+"/models", t.auth(c))
	if err != nil {
		return "", c.explain(err)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := decodeOrFail(c, resp, &out); err != nil {
		return "", err
	}

	// A provider's /models list mixes chat models with embeddings, audio and
	// image endpoints. Prefer a name that looks small and fast, since turning
	// one sentence into one command is an easy task.
	var names []string
	for _, m := range out.Data {
		if !looksLikeChatModel(m.ID) {
			continue
		}
		names = append(names, m.ID)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("%s returned no usable chat models", c.provider)
	}
	sort.Slice(names, func(i, j int) bool {
		pi, pj := cheapRank(names[i]), cheapRank(names[j])
		if pi != pj {
			return pi < pj
		}
		return names[i] < names[j]
	})
	return names[0], nil
}

func looksLikeChatModel(id string) bool {
	l := strings.ToLower(id)
	for _, bad := range []string{
		"embed", "whisper", "tts", "dall-e", "image", "audio", "moderation",
		"rerank", "guard", "vision-only", "realtime", "transcribe", "search",
	} {
		if strings.Contains(l, bad) {
			return false
		}
	}
	return true
}

// cheapRank prefers models whose names advertise being small or fast. It is a
// heuristic over free-form ids, so it is only a tiebreak - an explicit choice
// always wins and is never second-guessed.
// It matches whole tokens, never substrings. "gemini" contains "mini", so a
// substring test ranked every Gemini model as the cheap one and then picked
// gemini-2.5-pro over gemini-2.5-flash on the tiebreak.
func cheapRank(id string) int {
	cheap := map[string]bool{"mini": true, "flash": true, "haiku": true, "small": true,
		"nano": true, "tiny": true, "1b": true, "3b": true, "7b": true, "8b": true}
	mid := map[string]bool{"instant": true, "turbo": true, "lite": true, "fast": true}

	rank := 2
	for _, tok := range strings.FieldsFunc(strings.ToLower(id), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if cheap[tok] {
			return 0
		}
		if mid[tok] {
			rank = 1
		}
	}
	return rank
}

// -------------------------------------------------------------- anthropic --

// anthropicTransport speaks the native Messages API.
//
//	POST https://api.anthropic.com/v1/messages
//	x-api-key: <key>
//	anthropic-version: 2023-06-01
type anthropicTransport struct{}

const anthropicVersion = "2023-06-01"

func (anthropicTransport) auth(c *Client) map[string]string {
	return map[string]string{
		"x-api-key":         c.apiKey,
		"anthropic-version": anthropicVersion,
	}
}

func (t anthropicTransport) complete(ctx context.Context, c *Client, system, user string) (string, error) {
	body := map[string]any{
		"model":      c.model,
		"max_tokens": maxOutputTokens,
		// System is a top-level field here, not a message with role "system".
		"system":   system,
		"messages": []map[string]string{{"role": "user", "content": user}},
	}

	resp, err := c.post(ctx, c.baseURL+"/messages", t.auth(c), body)
	if err != nil {
		return "", c.explain(err)
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := decodeOrFail(c, resp, &out); err != nil {
		return "", err
	}
	if out.StopReason == "refusal" {
		return "", fmt.Errorf("%s declined this request", c.Name())
	}
	// content is an array of blocks; only the text ones carry the answer.
	var b strings.Builder
	for _, blk := range out.Content {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("%s: empty response", c.Name())
	}
	return b.String(), nil
}

// validate uses the Models endpoint, which costs no tokens.
func (t anthropicTransport) validate(ctx context.Context, c *Client) error {
	resp, err := c.get(ctx, c.baseURL+"/models", t.auth(c))
	if err != nil {
		return c.explain(err)
	}
	var out struct{}
	return decodeOrFail(c, resp, &out)
}

func (t anthropicTransport) discover(ctx context.Context, c *Client) (string, error) {
	resp, err := c.get(ctx, c.baseURL+"/models", t.auth(c))
	if err != nil {
		return "", c.explain(err)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := decodeOrFail(c, resp, &out); err != nil {
		return "", err
	}
	var names []string
	for _, m := range out.Data {
		names = append(names, m.ID)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("anthropic returned no models")
	}
	sort.Slice(names, func(i, j int) bool {
		pi, pj := cheapRank(names[i]), cheapRank(names[j])
		if pi != pj {
			return pi < pj
		}
		return names[i] > names[j] // newer ids sort later, so prefer the tail
	})
	return names[0], nil
}

// ----------------------------------------------------------------- gemini --

// geminiTransport speaks Google's generateContent API.
//
//	POST https://generativelanguage.googleapis.com/v1beta/models/<model>:generateContent
//	x-goog-api-key: <key>
type geminiTransport struct{}

func (geminiTransport) auth(c *Client) map[string]string {
	return map[string]string{"x-goog-api-key": c.apiKey}
}

func (t geminiTransport) complete(ctx context.Context, c *Client, system, user string) (string, error) {
	body := map[string]any{
		"system_instruction": map[string]any{
			"parts": []map[string]string{{"text": system}},
		},
		"contents": []map[string]any{{
			"role":  "user",
			"parts": []map[string]string{{"text": user}},
		}},
		"generationConfig": map[string]any{
			"temperature":      0,
			"maxOutputTokens":  maxOutputTokens,
			"responseMimeType": "application/json",
		},
	}

	url := c.baseURL + "/models/" + c.model + ":generateContent"
	resp, err := c.post(ctx, url, t.auth(c), body)
	if err != nil {
		return "", c.explain(err)
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}
	if err := decodeOrFail(c, resp, &out); err != nil {
		return "", err
	}
	if len(out.Candidates) == 0 {
		return "", fmt.Errorf("%s: empty response (the request may have been filtered)", c.Name())
	}
	var b strings.Builder
	for _, p := range out.Candidates[0].Content.Parts {
		b.WriteString(p.Text)
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("%s: empty response", c.Name())
	}
	return b.String(), nil
}

func (t geminiTransport) validate(ctx context.Context, c *Client) error {
	resp, err := c.get(ctx, c.baseURL+"/models", t.auth(c))
	if err != nil {
		return c.explain(err)
	}
	var out struct{}
	return decodeOrFail(c, resp, &out)
}

func (t geminiTransport) discover(ctx context.Context, c *Client) (string, error) {
	resp, err := c.get(ctx, c.baseURL+"/models", t.auth(c))
	if err != nil {
		return "", c.explain(err)
	}
	var out struct {
		Models []struct {
			Name                       string   `json:"name"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := decodeOrFail(c, resp, &out); err != nil {
		return "", err
	}

	var names []string
	for _, m := range out.Models {
		// Names come back as "models/gemini-x"; the request path adds that back.
		id := strings.TrimPrefix(m.Name, "models/")
		if !looksLikeChatModel(id) {
			continue
		}
		// Only models that can actually answer a prompt.
		ok := len(m.SupportedGenerationMethods) == 0
		for _, meth := range m.SupportedGenerationMethods {
			if meth == "generateContent" {
				ok = true
			}
		}
		if ok {
			names = append(names, id)
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("gemini returned no usable models")
	}
	sort.Slice(names, func(i, j int) bool {
		pi, pj := cheapRank(names[i]), cheapRank(names[j])
		if pi != pj {
			return pi < pj
		}
		return names[i] > names[j]
	})
	return names[0], nil
}
