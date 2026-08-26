// Package keypool manages a provider's pool of API keys: selection strategy,
// class-based quarantine and per-key usage counters. Keys are identified by
// their env var name; values are resolved lazily by the caller so env changes
// take effect without a restart.
//
// Selection never hard-blocks: if every key is quarantined, Pick falls back to
// the key whose cool-off expires first (most recovered). Persistent failure
// still surfaces via the step health tracker, which fails the chain over.
package keypool

import (
	"fmt"
	"sync"
	"time"

	"github.com/creamy-ghost/guvna/internal/config"
)

// Failure classes mirror upstream status semantics.
type Class string

const (
	ClassAuth      Class = "auth"       // 401/403: key dead or quota gone
	ClassRateLimit Class = "rate_limit" // 429: temporarily rate-limited
	ClassTransient Class = "transient"  // 5xx / network errors
)

// Strategy selects the next key among healthy ones.
type Strategy string

const (
	StrategyRoundRobin Strategy = "round_robin"
	StrategyLeastUsed  Strategy = "least_used"
	StrategySequential Strategy = "sequential"
)

// KeyStatus is one key's observable state for the admin surface.
type KeyStatus struct {
	Provider  string        `json:"provider"`
	Index     int           `json:"index"`
	Env       string        `json:"env"`
	State     string        `json:"state"`  // healthy | quarantined
	Reason    string        `json:"reason"` // auth | rate_limit | transient | network | ""
	Failures  int           `json:"failures"`
	DownFor   time.Duration `json:"down_for_ns"`
	Requests  int64         `json:"requests"`
	TokensIn  int64         `json:"tokens_in"`
	TokensOut int64         `json:"tokens_out"`
}

type keyState struct {
	env       string
	failures  int
	downUntil time.Time
	lastAt    time.Time
	lastClass Class
	requests  int64
	tokensIn  int64
	tokensOut int64
}

func (k *keyState) healthy(now time.Time) bool {
	return k.failures == 0 || now.After(k.downUntil)
}

// Pool is safe for concurrent use.
type Pool struct {
	name   string
	keys   []*keyState
	strat  Strategy
	qc     config.QuarantineCfg
	now    func() time.Time
	rrNext int
	mu     sync.Mutex
}

// New builds a pool for the provider. Strategy "" and zero quarantine fields
// fall back to round_robin and config.DefaultQuarantine.
func New(provider string, envs []string, strategy string, qc config.QuarantineCfg) (*Pool, error) {
	if len(envs) == 0 {
		return nil, fmt.Errorf("provider %q: empty key pool", provider)
	}
	s := Strategy(strategy)
	switch s {
	case "", StrategyRoundRobin:
		s = StrategyRoundRobin
	case StrategyLeastUsed, StrategySequential:
	default:
		return nil, fmt.Errorf("provider %q: unknown rotation %q", provider, strategy)
	}
	if qc.Auth.Duration == 0 {
		qc.Auth = config.DefaultQuarantine.Auth
	}
	if qc.RateLimit.Duration == 0 {
		qc.RateLimit = config.DefaultQuarantine.RateLimit
	}
	if qc.Transient.Duration == 0 {
		qc.Transient = config.DefaultQuarantine.Transient
	}
	keys := make([]*keyState, 0, len(envs))
	for _, e := range envs {
		keys = append(keys, &keyState{env: e})
	}
	return &Pool{name: provider, keys: keys, strat: s, qc: qc, now: time.Now}, nil
}

// Pick returns the env var name of the key to use for the next attempt.
// Prefers healthy keys per the strategy; if none are healthy, falls back to
// the key with the oldest cool-off expiry (most recovered). Never returns "".
func (p *Pool) Pick() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	best := -1
	switch p.strat {
	case StrategySequential:
		for i, k := range p.keys {
			if k.healthy(now) {
				best = i
				break
			}
		}
	case StrategyLeastUsed:
		for i, k := range p.keys {
			if !k.healthy(now) {
				continue
			}
			if best == -1 || k.lastAt.Before(p.keys[best].lastAt) {
				best = i
			}
		}
	default: // round_robin
		n := len(p.keys)
		for i := 0; i < n; i++ {
			idx := (p.rrNext + i) % n
			if p.keys[idx].healthy(now) {
				best = idx
				break
			}
		}
	}
	if best == -1 {
		// All quarantined: use the key that recovers soonest.
		for i, k := range p.keys {
			if best == -1 || k.downUntil.Before(p.keys[best].downUntil) {
				best = i
			}
		}
	}
	p.keys[best].lastAt = now
	if p.strat == StrategyRoundRobin {
		p.rrNext = (best + 1) % len(p.keys)
	}
	return p.keys[best].env
}

// ClassFor maps an upstream status to a failure class. Returns "" for
// non-quarantinable statuses (client errors, successes).
func ClassFor(status int, err error) Class {
	if err != nil {
		return ClassTransient
	}
	switch {
	case status == 401 || status == 403:
		return ClassAuth
	case status == 429:
		return ClassRateLimit
	case status >= 500:
		return ClassTransient
	}
	return ""
}

// Mark records a failure for the key and quarantines it for the class's
// cool-off, doubling per consecutive failure. Returns the class applied.
func (p *Pool) Mark(env string, class Class) Class {
	if class == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.byEnv(env)
	if k == nil {
		return ""
	}
	d := p.durationFor(class)
	for i := 0; i < k.failures; i++ {
		if d >= config.DefaultQuarantine.Auth.Duration {
			break
		}
		d *= 2
	}
	if d > config.DefaultQuarantine.Auth.Duration {
		d = config.DefaultQuarantine.Auth.Duration
	}
	k.failures++
	k.lastAt = p.now()
	k.lastClass = class
	k.downUntil = k.lastAt.Add(d)
	return class
}

// MarkSuccess resets the key's failure state.
func (p *Pool) MarkSuccess(env string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if k := p.byEnv(env); k != nil {
		k.failures = 0
		k.downUntil = time.Time{}
		k.lastClass = ""
	}
}

// Record adds usage counters for the key.
func (p *Pool) Record(env string, in, out int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if k := p.byEnv(env); k != nil {
		k.requests++
		k.tokensIn += in
		k.tokensOut += out
	}
}

// Keys returns the current state of every key, for the admin surface.
func (p *Pool) Keys() []KeyStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]KeyStatus, 0, len(p.keys))
	for i, k := range p.keys {
		ks := KeyStatus{
			Provider:  p.name,
			Index:     i,
			Env:       k.env,
			Failures:  k.failures,
			Requests:  k.requests,
			TokensIn:  k.tokensIn,
			TokensOut: k.tokensOut,
		}
		if !k.healthy(now) {
			ks.State = "quarantined"
			ks.Reason = string(k.lastClass)
			ks.DownFor = k.downUntil.Sub(now)
		} else {
			ks.State = "healthy"
		}
		out = append(out, ks)
	}
	return out
}

func (p *Pool) byEnv(env string) *keyState {
	for _, k := range p.keys {
		if k.env == env {
			return k
		}
	}
	return nil
}

func (p *Pool) durationFor(class Class) time.Duration {
	switch class {
	case ClassAuth:
		return p.qc.Auth.Duration
	case ClassRateLimit:
		return p.qc.RateLimit.Duration
	default:
		return p.qc.Transient.Duration
	}
}
