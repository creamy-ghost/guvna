package router

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/creamy-ghost/guvna/internal/config"
)

var ErrNotFound = errors.New("no chain or model matches")

type Step struct {
	Provider string
	Model    string
	Params   map[string]any
}

type Router struct {
	mu         sync.RWMutex
	chains     map[string][]Step
	modelIndex map[string]string
	providers  map[string]config.Provider
	sources    map[string]string // chain name -> "config" | "runtime"
}

func New(cfg *config.Config) *Router {
	r := &Router{
		chains:     make(map[string][]Step, len(cfg.Chains)),
		modelIndex: make(map[string]string),
		providers:  make(map[string]config.Provider, len(cfg.Providers)),
		sources:    make(map[string]string),
	}
	for _, p := range cfg.Providers {
		r.providers[p.Name] = p
	}
	for _, ch := range cfg.Chains {
		r.addLocked(ch.Name, stepsFromConfig(ch.Steps), "config")
	}
	return r
}

func stepsFromConfig(steps []config.Step) []Step {
	out := make([]Step, 0, len(steps))
	for _, s := range steps {
		out = append(out, Step{Provider: s.Provider, Model: s.Model, Params: s.Params})
	}
	return out
}

func (r *Router) addLocked(name string, steps []Step, source string) {
	r.chains[name] = steps
	r.sources[name] = source
	for _, s := range steps {
		r.modelIndex[s.Model] = name
	}
}

// AddChain adds a runtime chain. Loose validation by design: the provider must
// exist in config and every step needs a non-empty model. Model existence is
// NOT checked — the upstream provider is the source of truth; a bad model
// fails at request time and the chain falls through to the next step.
func (r *Router) AddChain(name string, steps []Step) error {
	if err := ValidateChainName(name); err != nil {
		return err
	}
	if len(steps) == 0 {
		return errors.New("chain needs at least one step")
	}
	for _, s := range steps {
		if _, ok := r.providers[s.Provider]; !ok {
			return fmt.Errorf("unknown provider %q", s.Provider)
		}
		if s.Model == "" {
			return fmt.Errorf("step for %q has no model", s.Provider)
		}
		if err := config.ValidateStepParams(s.Params); err != nil {
			return fmt.Errorf("step for %q: %w", s.Provider, err)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.chains[name]; exists {
		return fmt.Errorf("chain %q already exists (source: %s)", name, r.sources[name])
	}
	r.addLocked(name, steps, "runtime")
	return nil
}

func (r *Router) RemoveChain(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	src, exists := r.sources[name]
	if !exists {
		return fmt.Errorf("chain %q not found", name)
	}
	if src == "config" {
		return fmt.Errorf("chain %q is defined in config.yaml — edit the config, not the runtime store", name)
	}
	steps := r.chains[name]
	for _, s := range steps {
		delete(r.modelIndex, s.Model)
	}
	delete(r.chains, name)
	delete(r.sources, name)
	return nil
}

// Resolve maps a requested model name to a chain or a direct provider step.
// Precedence: exact chain name -> model inside a chain's steps ->
// provider-prefix passthrough -> ErrNotFound.
func (r *Router) Resolve(model string) (name string, steps []Step, err error) {
	r.mu.RLock()
	if _, found := r.chains[model]; found {
		name, steps, err = model, r.chains[model], nil
		r.mu.RUnlock()
		return
	}
	if chain, found := r.modelIndex[model]; found {
		name, steps, err = chain, r.chains[chain], nil
		r.mu.RUnlock()
		return
	}
	r.mu.RUnlock()
	if provider, upstreamModel, ok := r.passthrough(model); ok {
		return "", []Step{{Provider: provider, Model: upstreamModel}}, nil
	}
	return "", nil, ErrNotFound
}

// passthrough routes a model directly to a provider by prefix. For each
// provider in config order, for each of its prefixes (provider name plus any
// configured extras): if the model starts with "<prefix>/", route it there.
// If the stripped name appears in the provider's static models list, the
// stripped name is sent (covers bare-name catalogs like groq); otherwise the
// full original name is sent (covers namespaced catalogs like orcarouter's
// "openai/gpt-5.6-luna").
func (r *Router) passthrough(model string) (string, string, bool) {
	for _, p := range r.providers {
		prefixes := p.Prefixes
		if len(prefixes) == 0 {
			prefixes = []string{p.Name}
		}
		for _, prefix := range prefixes {
			rest, found := strings.CutPrefix(model, prefix+"/")
			if !found {
				continue
			}
			if contains(p.Models, rest) {
				return p.Name, rest, true
			}
			return p.Name, model, true
		}
	}
	return "", "", false
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

type ChainInfo struct {
	Name   string
	Source string
	Steps  []Step
}

func (r *Router) ChainNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.chains))
	for name := range r.chains {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Router) ListChains() []ChainInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ChainInfo, 0, len(r.chains))
	for name, steps := range r.chains {
		cp := make([]Step, len(steps))
		copy(cp, steps)
		out = append(out, ChainInfo{Name: name, Source: r.sources[name], Steps: cp})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RuntimeChains returns the chains owned by the runtime store (for persistence).
func (r *Router) RuntimeChains() map[string][]Step {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string][]Step)
	for name, steps := range r.chains {
		if r.sources[name] != "runtime" {
			continue
		}
		cp := make([]Step, len(steps))
		copy(cp, steps)
		out[name] = cp
	}
	return out
}
