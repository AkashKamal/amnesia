package model

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

// jsonUnmarshal exists so model.go does not import encoding/json twice under
// different names after the transport split.
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// ollamaModels returns installed chat models, smallest first.
//
// Smallest first because turning one English sentence into one shell command is
// an easy task, and on the miss path of a CLI that promises to feel instant a
// 1B model answering in 300ms beats a 70B answering in 20s. Anyone who
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

// OllamaModels is the exported form, for `amnesia setup` to show what is there.
func OllamaModels() []string { return ollamaModels(providers["ollama"].BaseURL) }

// isEmbedding filters models that cannot hold a conversation. Ollama does not
// flag them directly, so this matches the naming conventions every embedding
// model on the registry follows, plus the BERT families they are built on. A
// false negative costs one clear error; a false positive costs nothing, because
// nobody picks an embedding model to write shell commands.
func isEmbedding(name, family string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"embed", "bge-", "bge:", "gte-", "gte:", "e5-", "minilm"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return strings.Contains(strings.ToLower(family), "bert")
}
