package chains

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/creamy-ghost/guvna/internal/config"
	"github.com/creamy-ghost/guvna/internal/router"
)

func newRouter() *router.Router {
	return router.New(&config.Config{
		Providers: []config.Provider{
			{Name: "groq", Models: []string{"llama-3.3-70b-versatile"}},
			{Name: "gemini", Models: []string{"gemini-2.5-flash"}},
		},
		Chains: []config.Chain{{Name: "from-config", Steps: []config.Step{{Provider: "groq", Model: "llama-3.3-70b-versatile"}}}},
	})
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := newRouter()
	if err := r.AddChain("custom", []router.Step{{Provider: "gemini", Model: "gemini-2.5-flash"}}); err != nil {
		t.Fatal(err)
	}
	s := New(dir)
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "chains.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("chains.yaml empty")
	}
	if !bytes.Contains(raw, []byte("version: 2")) {
		t.Fatalf("chains.yaml missing version field:\n%s", raw)
	}
	r2 := newRouter()
	if err := s.Load(r2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r2.Resolve("custom"); err != nil {
		t.Fatalf("custom chain not restored: %v", err)
	}
	if _, _, err := r2.Resolve("from-config"); err != nil {
		t.Fatalf("config chain lost: %v", err)
	}
}

func TestSaveLoadParamsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := newRouter()
	params := map[string]any{"reasoning_effort": "high", "max_tokens": 8192, "stop": []any{"END"}}
	if err := r.AddChain("tuned", []router.Step{{Provider: "gemini", Model: "gemini-2.5-flash", Params: params}}); err != nil {
		t.Fatal(err)
	}
	s := New(dir)
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	r2 := newRouter()
	if err := s.Load(r2); err != nil {
		t.Fatal(err)
	}
	_, steps, err := r2.Resolve("tuned")
	if err != nil {
		t.Fatal(err)
	}
	got := steps[0].Params
	if got["reasoning_effort"] != "high" || got["max_tokens"] != 8192 && got["max_tokens"] != float64(8192) {
		t.Fatalf("params not round-tripped: %#v", got)
	}
	if stop, ok := got["stop"].([]any); !ok || len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("non-scalar param not round-tripped: %#v", got["stop"])
	}
}

func TestLoadLegacyFormatNoVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "chains.yaml"), []byte("chains:\n  - name: old\n    steps:\n      - provider: groq\n        model: llama-3.3-70b-versatile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRouter()
	if err := New(dir).Load(r); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Resolve("old"); err != nil {
		t.Fatalf("legacy chain should load: %v", err)
	}
}

func TestLoadMissingFileIsNoop(t *testing.T) {
	r := newRouter()
	s := New(t.TempDir())
	if err := s.Load(r); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConflictSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "chains.yaml"), []byte("chains:\n  - name: from-config\n    steps:\n      - provider: groq\n        model: llama-3.3-70b-versatile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRouter()
	s := New(dir)
	if err := s.Load(r); err != nil {
		t.Fatal(err)
	}
	// config chain must still be intact and still config-sourced (not removable)
	if err := r.RemoveChain("from-config"); err == nil {
		t.Fatal("config chain wrongly replaced by runtime file")
	}
}

func TestLoadUnknownProviderSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "chains.yaml"), []byte("chains:\n  - name: bad\n    steps:\n      - provider: nope\n        model: x\n  - name: good\n    steps:\n      - provider: groq\n        model: llama-3.3-70b-versatile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRouter()
	if err := New(dir).Load(r); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Resolve("bad"); err == nil {
		t.Fatal("invalid chain should have been skipped")
	}
	if _, _, err := r.Resolve("good"); err != nil {
		t.Fatalf("valid chain should load: %v", err)
	}
}

func TestSaveOnlyRuntimeChains(t *testing.T) {
	dir := t.TempDir()
	r := newRouter()
	if err := r.AddChain("custom", []router.Step{{Provider: "gemini", Model: "gemini-2.5-flash"}}); err != nil {
		t.Fatal(err)
	}
	s := New(dir)
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	r2 := newRouter()
	if err := s.Load(r2); err != nil {
		t.Fatal(err)
	}
	if len(r2.RuntimeChains()) != 1 {
		t.Fatalf("runtime chains = %+v", r2.RuntimeChains())
	}
}
