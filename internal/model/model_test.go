package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AkashKamal/amnesia/internal/config"
	"github.com/AkashKamal/amnesia/internal/resolve"
)

func TestParseSuggestions(t *testing.T) {
	t.Run("plain json", func(t *testing.T) {
		got, err := parseSuggestions(`{"suggestions":[{"command":"az vm list","description":"list vms","tool":"az"}]}`)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Command != "az vm list" || got[0].Tool != "az" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("json wrapped in prose and fences", func(t *testing.T) {
		got, err := parseSuggestions("Sure! Here you go:\n```json\n{\"suggestions\":[{\"command\":\"ls -la\"}]}\n```\nHope that helps.")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Command != "ls -la" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("multi-line command is dropped", func(t *testing.T) {
		// There is no honest way to show a script on a one-line confirm prompt,
		// so it must not reach one.
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
			t.Fatal("want an error; regexing a command out of prose is how you execute something nobody parsed")
		}
	})

	t.Run("ranked confidence is descending", func(t *testing.T) {
		got, err := parseSuggestions(`{"suggestions":[{"command":"a"},{"command":"b"},{"command":"c"}]}`)
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i < len(got); i++ {
			if got[i].Confidence >= got[i-1].Confidence {
				t.Fatalf("confidence not descending: %+v", got)
			}
		}
		if got[0].Confidence >= 1 {
			t.Fatalf("model confidence %v must stay below an exact corpus hit", got[0].Confidence)
		}
	})
}

func TestSuggestAgainstFakeProvider(t *testing.T) {
	t.Setenv("AMNESIA_HOME", t.TempDir())
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")

		var req chatReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		// The environment must reach the prompt, or we suggest ss(8) on macOS.
		if len(req.Messages) != 2 || !strings.Contains(req.Messages[1].Content, "osx") {
			t.Errorf("env missing from prompt: %+v", req.Messages)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"suggestions\":[{\"command\":\"brew list\",\"tool\":\"brew\"}]}"}}]}`))
	}))
	defer srv.Close()

	t.Setenv("AMNESIA_BASE_URL", srv.URL)
	t.Setenv("AMNESIA_API_KEY", "test-key")
	t.Setenv("AMNESIA_MODEL", "groq/some-model")

	m := New(config.Load())
	if m == nil {
		t.Fatal("New returned nil with AMNESIA_MODEL set")
	}
	got, err := m.Suggest(context.Background(), "list installed packages", resolve.Env{Platform: "osx", Shell: "zsh"})
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}
	if len(got) != 1 || got[0].Command != "brew list" {
		t.Fatalf("got %+v", got)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("auth header = %q", gotAuth)
	}
}

func TestOfflineWins(t *testing.T) {
	t.Setenv("AMNESIA_HOME", t.TempDir())
	t.Setenv("AMNESIA_OFFLINE", "1")
	t.Setenv("AMNESIA_MODEL", "groq/some-model")
	t.Setenv("GROQ_API_KEY", "x")
	if m := New(config.Load()); m != nil {
		t.Fatalf("AMNESIA_OFFLINE must beat every other signal, got %s", m.Name())
	}
}

// fakeOllama serves the /api/tags shape so model discovery can be tested
// without anyone having Ollama installed.
func fakeOllama(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// Hardcoding a model id is wrong for most users on day one and wrong for
// everyone a year later. Ask the server what it has.
func TestOllamaModelsPrefersSmallestUsable(t *testing.T) {
	url := fakeOllama(t, `{"models":[
		{"name":"qwen3.5:9b","size":6600000000},
		{"name":"nomic-embed-text:latest","size":270000000,"details":{"family":"nomic-bert"}},
		{"name":"llama3.2:1b","size":1300000000},
		{"name":"qwen2.5-coder:7b","size":4700000000}
	]}`)

	got := ollamaModels(url)
	want := []string{"llama3.2:1b", "qwen2.5-coder:7b", "qwen3.5:9b"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (smallest first, embeddings dropped)", got, want)
		}
	}
}

func TestOllamaWithNoModelsIsNotReady(t *testing.T) {
	t.Setenv("AMNESIA_HOME", t.TempDir())
	url := fakeOllama(t, `{"models":[]}`)
	t.Setenv("AMNESIA_BASE_URL", url)
	t.Setenv("AMNESIA_MODEL", "ollama")

	st := Describe(config.Load())
	if st.Ready {
		t.Fatal("reported ready with no models installed")
	}
	if !strings.Contains(st.Hint, "ollama pull") {
		t.Fatalf("hint %q does not tell the user what to run", st.Hint)
	}
}

func TestExplicitProviderWithoutModelIdIsDiscovered(t *testing.T) {
	t.Setenv("AMNESIA_HOME", t.TempDir())
	url := fakeOllama(t, `{"models":[{"name":"llama3.2:1b","size":1300000000}]}`)
	t.Setenv("AMNESIA_BASE_URL", url)
	t.Setenv("AMNESIA_MODEL", "ollama")

	st := Describe(config.Load())
	if !st.Ready || st.Model != "llama3.2:1b" {
		t.Fatalf("got %+v, want the installed model", st)
	}
}

// A local model may sit on disk until first use; a hosted API that has not
// answered in 20s never will. One timeout cannot serve both.
func TestLocalGetsALongerTimeoutThanHosted(t *testing.T) {
	local := timeout(providers["ollama"])
	hosted := timeout(providers["groq"])
	if local <= hosted {
		t.Fatalf("local %v must exceed hosted %v", local, hosted)
	}

	t.Setenv("AMNESIA_TIMEOUT", "45s")
	if got := timeout(providers["groq"]); got != 45*time.Second {
		t.Fatalf("AMNESIA_TIMEOUT ignored: got %v", got)
	}
}

// Any OpenAI-compatible server on loopback is a local model, whether or not
// amnesia has heard of it. That is what makes LM Studio and llama.cpp work.
func TestLoopbackCustomEndpointCountsAsLocal(t *testing.T) {
	p := applyOverrides(providers["custom"], config.Config{BaseURL: "http://localhost:1234/v1"})
	if !p.Local {
		t.Fatal("a loopback endpoint should get the local timeout")
	}
	if timeout(p) != timeout(providers["ollama"]) {
		t.Fatal("loopback custom endpoint should share the local timeout")
	}
}

func TestUnknownProviderIsExplained(t *testing.T) {
	t.Setenv("AMNESIA_HOME", t.TempDir())
	st := Describe(config.Config{Model: "definitely-not-a-provider"})
	if st.Ready {
		t.Fatal("unknown provider reported ready")
	}
	if !strings.Contains(st.Hint, "ollama") {
		t.Fatalf("hint %q should list the valid providers", st.Hint)
	}
}

func TestHostedProviderWithoutKeyIsExplained(t *testing.T) {
	t.Setenv("AMNESIA_HOME", t.TempDir())
	t.Setenv("GROQ_API_KEY", "")
	st := Describe(config.Config{Model: "groq/llama-3.3-70b-versatile"})
	if st.Ready {
		t.Fatal("reported ready with no API key")
	}
	if !strings.Contains(st.Hint, "amnesia model") {
		t.Fatalf("hint %q should name the command that fixes it", st.Hint)
	}
}

func TestErrorsTellTheUserWhatToDo(t *testing.T) {
	c := &Client{provider: "ollama", baseURL: "http://localhost:11434/v1", model: "llama3.2"}
	if got := c.explainAPI(404, ""); !strings.Contains(got.Error(), "ollama pull") {
		t.Errorf("404 should suggest pulling the model, got %q", got)
	}
	h := &Client{provider: "groq", baseURL: "https://api.groq.com/openai/v1", model: "x"}
	if got := h.explainAPI(401, ""); !strings.Contains(got.Error(), "amnesia model") {
		t.Errorf("401 should say how to set a key, got %q", got)
	}
}
