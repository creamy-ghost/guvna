package adaptors

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/creamy-ghost/guvna/internal/config"
)

func testUpstream(t *testing.T, wantPath, wantKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath {
			http.Error(w, "bad path "+r.URL.Path, http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+wantKey {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			http.Error(w, "bad content type", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"echo":` + string(body) + `}`))
	}))
}

func TestOpenAICompatForwards(t *testing.T) {
	up := testUpstream(t, "/v1/chat/completions", "sk-test")
	defer up.Close()

	a := NewOpenAICompat(config.Provider{Name: "groq", BaseURL: up.URL}, "sk-test")
	resp, err := a.Chat(context.Background(), []byte(`{"model":"llama-3.3-70b-versatile"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), "llama-3.3-70b-versatile") {
		t.Errorf("body not forwarded: %s", got)
	}
}

func TestGeminiUsesOpenAICompatEndpoint(t *testing.T) {
	up := testUpstream(t, "/v1beta/openai/v1/chat/completions", "gk-test")
	defer up.Close()

	a := NewGemini(config.Provider{Name: "gemini", BaseURL: up.URL}, "gk-test")
	resp, err := a.Chat(context.Background(), []byte(`{"model":"gemini-2.5-flash"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestNewByType(t *testing.T) {
	if _, err := New(config.Provider{Name: "x", Type: "openai", BaseURL: "http://x"}, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(config.Provider{Name: "x", Type: "gemini", BaseURL: "http://x"}, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(config.Provider{Name: "x", Type: "anthropic", BaseURL: "http://x"}, "k"); err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestGeminiFailsWithoutKey(t *testing.T) {
	up := testUpstream(t, "/v1beta/openai/v1/chat/completions", "gk-test")
	defer up.Close()
	a := NewGemini(config.Provider{Name: "gemini", BaseURL: up.URL}, "")
	resp, err := a.Chat(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}
