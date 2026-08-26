package chains

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/creamy-ghost/guvna/internal/router"
	"gopkg.in/yaml.v3"
)

type fileFormat struct {
	Version int         `yaml:"version,omitempty"`
	Chains  []fileChain `yaml:"chains"`
}

type fileStep struct {
	Provider string         `yaml:"provider"`
	Model    string         `yaml:"model"`
	Params   map[string]any `yaml:"params,omitempty"`
}

type fileChain struct {
	Name  string     `yaml:"name"`
	Steps []fileStep `yaml:"steps"`
}

// Store persists runtime chains to <data-dir>/chains.yaml with atomic rewrites.
type Store struct {
	path string
}

func New(dataDir string) *Store {
	return &Store{path: filepath.Join(dataDir, "chains.yaml")}
}

// Load reads runtime chains and applies them to the router.
// Conflicts with existing (config-defined) chains are skipped with a warning.
func (s *Store) Load(r *router.Router) error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read chains: %w", err)
	}
	var f fileFormat
	if err := yaml.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parse chains.yaml: %w", err)
	}
	for _, ch := range f.Chains {
		steps := make([]router.Step, 0, len(ch.Steps))
		for _, st := range ch.Steps {
			steps = append(steps, router.Step{Provider: st.Provider, Model: st.Model, Params: st.Params})
		}
		if err := r.AddChain(ch.Name, steps); err != nil {
			fmt.Printf("chains: skipping %q from chains.yaml: %v\n", ch.Name, err)
		}
	}
	return nil
}

// Save writes the current runtime chains atomically (tmp file + rename).
func (s *Store) Save(r *router.Router) error {
	runtime := r.RuntimeChains()
	f := fileFormat{Version: 2, Chains: make([]fileChain, 0, len(runtime))}
	for name, steps := range runtime {
		ch := fileChain{Name: name, Steps: make([]fileStep, 0, len(steps))}
		for _, st := range steps {
			ch.Steps = append(ch.Steps, fileStep{Provider: st.Provider, Model: st.Model, Params: st.Params})
		}
		f.Chains = append(f.Chains, ch)
	}
	data, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("marshal chains: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("chains dir: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write chains: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("rename chains: %w", err)
	}
	return nil
}
