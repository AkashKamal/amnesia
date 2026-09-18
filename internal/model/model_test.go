package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AkashKamal/amnesia/internal/config"
	"github.com/AkashKamal/amnesia/internal/resolve"
)

const jsonAnswer = `{"suggestions":[{"command":"brew list","description":"list packages","tool":"brew"}]}`

var osxEnv = resolve.Env{Platform: "osx", Shell: "zsh"}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestParseSuggestions(t *testing.T) {
	t.Run("plain json", func(t *testing.T) {
		got, err := parseSuggestions(`{"suggestions":[{"command":"az vm list","tool":"az"}]}`)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Command != "az vm list" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("json wrapped in prose and fences", func(t *testing.T) {
		got, err := parseSuggestions("Sure!\n```json\n{\"suggestions\":[{\"command\":\"ls -la\"}]}\n```\nDone.")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Command != "ls -la" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("multi-line command is dropped", func(t *testing.T) {
		// There is no honest way to show a script on a one-line confirm prompt.
		got, err := parseSuggestions(`{"suggestions":[{"command":"echo one\necho two"},{"command":"ls"}]}`)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Command != "ls" {
			t.Fatalf("got %+v, want only the single-line command", got)
		}
	})

	t.Run("prose with no json is an error, never a guess", func(t *testing.T) {
		if _, err := parseSuggestions("I think you want `rm -rf /`"); err == nil {
			t.Fatal("want an error; regexing a command out of prose executes something nobody parsed")
		}
	})
}

// --- transports ------------------------------------------------------------
//
// Each provider is checked against its documented contract: the right path, the
// right auth header, the right request shape, and the right place to find the
// answer. An OpenAI-compat shim would collapse these three into one and would
// be wrong the day any of them diverged - which is exactly the failure mode a
// tool that emits shell commands cannot afford.

func TestAnthropicTransport(t *testing.T) {
	var gotPath, gotKey, gotVersion, gotAuth string
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.Header.Get("x-api-key")
		gotVersion, gotAuth = r.Header.Get("anthropic-version"), r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "application/json")
		// content is an array of blocks; only the text ones carry the answer.
		_, _ = w.Write([]byte(`{"content":[{"type":"thinking","thinking":"..."},` +
			`{"type":"text","text":` + quote(jsonAnswer) + `}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	p, _ := Lookup("claude")
	p.BaseURL = srv.URL
	got, err := NewFor(p, "sk-ant-test", "claude-haiku-4-5").
		Suggest(context.Background(), "list installed packages", osxEnv)
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}
	if len(got) != 1 || got[0].Command != "brew list" {
		t.Fatalf("got %+v", got)
	}
	if gotPath != "/messages" {
		t.Errorf("path = %q, want /messages", gotPath)
	}
	if gotKey != "sk-ant-test" {
		t.Errorf("x-api-key = %q", gotKey)
	}
	if gotVersion != anthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", gotVersion, anthropicVersion)
	}
	if gotAuth != "" {
		t.Errorf("Anthropic takes x-api-key, not Authorization: %q", gotAuth)
	}
	if _, ok := body["system"].(string); !ok {
		t.Errorf("system must be a top-level string, got %T", body["system"])
	}
	if _, ok := body["max_tokens"]; !ok {
		t.Error("max_tokens is required by the Messages API")
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("want exactly one user message, got %d", len(msgs))
	}
	if m := msgs[0].(map[string]any); m["role"] != "user" || !strings.Contains(m["content"].(string), "osx") {
		t.Errorf("user message wrong: %+v", m)
	}
}

func TestAnthropicRefusalIsNotAnAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[],"stop_reason":"refusal"}`))
	}))
	defer srv.Close()

	p, _ := Lookup("claude")
	p.BaseURL = srv.URL
	if _, err := NewFor(p, "k", "m").Suggest(context.Background(), "x", osxEnv); err == nil {
		t.Fatal("a refusal must be an error, not an empty command list")
	}
}

func TestGeminiTransport(t *testing.T) {
	var gotPath, gotKey string
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.Header.Get("x-goog-api-key")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":` + quote(jsonAnswer) + `}]}}]}`))
	}))
	defer srv.Close()

	p, _ := Lookup("gemini")
	p.BaseURL = srv.URL
	got, err := NewFor(p, "AIza-test", "gemini-2.5-flash").
		Suggest(context.Background(), "list installed packages", osxEnv)
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}
	if len(got) != 1 || got[0].Command != "brew list" {
		t.Fatalf("got %+v", got)
	}
	// The model id goes in the path here, not the body.
	if gotPath != "/models/gemini-2.5-flash:generateContent" {
		t.Errorf("path = %q", gotPath)
	}
	if gotKey != "AIza-test" {
		t.Errorf("x-goog-api-key = %q", gotKey)
	}
	if _, ok := body["system_instruction"]; !ok {
		t.Error("gemini takes system_instruction, not a system message")
	}
	if _, ok := body["contents"]; !ok {
		t.Error("gemini takes contents, not messages")
	}
}

func TestOpenAITransport(t *testing.T) {
	var gotPath, gotAuth string
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + quote(jsonAnswer) + `}}]}`))
	}))
	defer srv.Close()

	p, _ := Lookup("openai")
	p.BaseURL = srv.URL
	got, err := NewFor(p, "sk-test", "gpt-4o-mini").
		Suggest(context.Background(), "list installed packages", osxEnv)
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}
	if len(got) != 1 || got[0].Command != "brew list" {
		t.Fatalf("got %+v", got)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if msgs, _ := body["messages"].([]any); len(msgs) != 2 {
		t.Fatalf("want system + user, got %d", len(msgs))
	}
}

// --- key validation and model discovery ------------------------------------

func TestValidateReportsABadKeyUsefully(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer srv.Close()

	for _, name := range []string{"claude", "gemini", "openai"} {
		p, _ := Lookup(name)
		p.BaseURL = srv.URL
		err := NewFor(p, "wrong", "").Validate(context.Background())
		if err == nil {
			t.Fatalf("%s: a 401 must not validate", name)
		}
		// setup has to tell the user what to do, not print a status code.
		if !strings.Contains(err.Error(), "amnesia setup") {
			t.Errorf("%s: unhelpful error %q", name, err)
		}
	}
}

// Hardcoding a model id is how an integration rots. Ask, and prefer something
// small: turning one sentence into one command is an easy task.
func TestDiscoverPrefersACheapChatModel(t *testing.T) {
	cases := []struct {
		provider string
		body     string
		want     string
	}{
		{"openai", `{"data":[{"id":"gpt-4o"},{"id":"text-embedding-3-large"},{"id":"gpt-4o-mini"},{"id":"whisper-1"}]}`, "gpt-4o-mini"},
		{"claude", `{"data":[{"id":"claude-opus-5"},{"id":"claude-haiku-4-5"},{"id":"claude-sonnet-5"}]}`, "claude-haiku-4-5"},
		{"gemini", `{"models":[{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent"]},` +
			`{"name":"models/text-embedding-004","supportedGenerationMethods":["embedContent"]},` +
			`{"name":"models/gemini-2.5-flash","supportedGenerationMethods":["generateContent"]}]}`, "gemini-2.5-flash"},
	}

	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			p, _ := Lookup(tc.provider)
			p.BaseURL = srv.URL
			got, err := NewFor(p, "k", "").Discover(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- selection -------------------------------------------------------------

func TestOfflineBeatsEverything(t *testing.T) {
	t.Setenv("AMNESIA_HOME", t.TempDir())
	t.Setenv("AMNESIA_OFFLINE", "1")
	t.Setenv("AMNESIA_MODEL", "claude/claude-haiku-4-5")
	t.Setenv("ANTHROPIC_API_KEY", "x")
	if m := New(config.Load()); m != nil {
		t.Fatalf("AMNESIA_OFFLINE must win, got %s", m.Name())
	}
}

func TestHostedProviderWithoutKeyIsExplained(t *testing.T) {
	t.Setenv("AMNESIA_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	st := Describe(config.Config{Model: "claude"})
	if st.Ready {
		t.Fatal("reported ready with no API key")
	}
	if !strings.Contains(st.Hint, "amnesia setup") {
		t.Fatalf("hint %q should name the command that fixes it", st.Hint)
	}
}

func TestAKeyInTheEnvironmentIsDetected(t *testing.T) {
	t.Setenv("AMNESIA_HOME", t.TempDir())
	t.Setenv("GEMINI_API_KEY", "AIza-something")
	st := Describe(config.Load())
	if !st.Ready || st.Provider != "gemini" {
		t.Fatalf("got %+v, want gemini detected from its env var", st)
	}
}

func TestUnknownProviderIsExplained(t *testing.T) {
	t.Setenv("AMNESIA_HOME", t.TempDir())
	st := Describe(config.Config{Model: "definitely-not-a-provider"})
	if st.Ready {
		t.Fatal("unknown provider reported ready")
	}
	if !strings.Contains(st.Hint, "claude") {
		t.Fatalf("hint %q should list valid providers", st.Hint)
	}
}

func TestLocalGetsALongerTimeoutThanHosted(t *testing.T) {
	// A local model may sit on disk until first use; a hosted API that has not
	// answered in 20s never will. One number cannot serve both.
	if local, hosted := timeout(providers["ollama"]), timeout(providers["claude"]); local <= hosted {
		t.Fatalf("local %v must exceed hosted %v", local, hosted)
	}
	t.Setenv("AMNESIA_TIMEOUT", "45s")
	if got := timeout(providers["groq"]); got != 45*time.Second {
		t.Fatalf("AMNESIA_TIMEOUT ignored: got %v", got)
	}
}

func TestLoopbackCustomEndpointCountsAsLocal(t *testing.T) {
	p := applyOverrides(providers["custom"], config.Config{BaseURL: "http://localhost:1234/v1"})
	if !p.Local {
		t.Fatal("a loopback endpoint should get the local timeout")
	}
}

// Every entry in the setup menu has to actually work when picked.
func TestEveryProviderInTheMenuIsUsable(t *testing.T) {
	for _, p := range Catalog() {
		if p.Kind == "" {
			t.Errorf("%s has no transport kind", p.Name)
		}
		if transportFor(p.Kind) == nil {
			t.Errorf("%s: no transport for kind %q", p.Name, p.Kind)
		}
		if p.Label == "" {
			t.Errorf("%s has no human-readable label", p.Name)
		}
		if p.Name == "custom" {
			continue
		}
		if p.BaseURL == "" {
			t.Errorf("%s has no base URL", p.Name)
		}
		if p.Fallback == "" {
			t.Errorf("%s has no fallback model", p.Name)
		}
		if !p.Local && p.KeyURL == "" {
			t.Errorf("%s is hosted but does not say where to get a key", p.Name)
		}
	}
}

func TestOllamaModelsPrefersSmallestUsable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"models":[
			{"name":"qwen3.5:9b","size":6600000000},
			{"name":"nomic-embed-text:latest","size":270000000,"details":{"family":"nomic-bert"}},
			{"name":"llama3.2:1b","size":1300000000}]}`))
	}))
	defer srv.Close()

	got := ollamaModels(srv.URL)
	if len(got) != 2 || got[0] != "llama3.2:1b" {
		t.Fatalf("got %v, want smallest first with embeddings dropped", got)
	}
}

// cheapRank once matched substrings, and "gemini" contains "mini" - so every
// Gemini model looked like the cheap one and discovery picked pro over flash.
func TestCheapRankMatchesWholeTokens(t *testing.T) {
	cases := map[string]int{
		"gemini-2.5-flash":                  0,
		"gemini-2.5-pro":                    2, // must NOT match "mini" inside "gemini"
		"gpt-4o-mini":                       0,
		"gpt-4o":                            2,
		"claude-haiku-4-5":                  0,
		"claude-opus-5":                     2,
		"llama3.2:1b":                       0,
		"meta-llama/Llama-3.3-70B-Instruct": 2,
		"Llama-3.3-70B-Instruct-Turbo":      1,
	}
	for id, want := range cases {
		if got := cheapRank(id); got != want {
			t.Errorf("cheapRank(%q) = %d, want %d", id, got, want)
		}
	}
}
