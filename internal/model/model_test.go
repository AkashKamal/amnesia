package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

	m := New()
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
	t.Setenv("AMNESIA_OFFLINE", "1")
	t.Setenv("AMNESIA_MODEL", "groq/some-model")
	t.Setenv("GROQ_API_KEY", "x")
	if m := New(); m != nil {
		t.Fatalf("AMNESIA_OFFLINE must beat every other signal, got %s", m.Name())
	}
}
