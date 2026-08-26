package keypool

import (
	"errors"
	"testing"
	"time"

	"github.com/creamy-ghost/guvna/internal/config"
)

func TestClassFor(t *testing.T) {
	cases := []struct {
		status int
		err    error
		want   Class
	}{
		{401, nil, ClassAuth},
		{403, nil, ClassAuth},
		{429, nil, ClassRateLimit},
		{500, nil, ClassTransient},
		{502, nil, ClassTransient},
		{400, nil, ""},
		{404, nil, ""},
		{200, nil, ""},
		{0, errors.New("boom"), ClassTransient},
	}
	for _, c := range cases {
		if got := ClassFor(c.status, c.err); got != c.want {
			t.Errorf("ClassFor(%d, %v) = %q, want %q", c.status, c.err, got, c.want)
		}
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New("p", nil, "", config.QuarantineCfg{}); err == nil {
		t.Error("empty envs must error")
	}
	if _, err := New("p", []string{"K1"}, "bogus", config.QuarantineCfg{}); err == nil {
		t.Error("unknown strategy must error")
	}
	p, err := New("p", []string{"K1", "K2"}, "", config.QuarantineCfg{})
	if err != nil {
		t.Fatal(err)
	}
	if p.strat != StrategyRoundRobin {
		t.Errorf("default strategy = %q, want round_robin", p.strat)
	}
	if p.qc.Auth.Duration != config.DefaultQuarantine.Auth.Duration ||
		p.qc.RateLimit.Duration != config.DefaultQuarantine.RateLimit.Duration ||
		p.qc.Transient.Duration != config.DefaultQuarantine.Transient.Duration {
		t.Error("zero quarantine fields must fall back to defaults")
	}
}

func TestRoundRobinRotation(t *testing.T) {
	p, _ := New("p", []string{"K1", "K2", "K3"}, "round_robin", config.QuarantineCfg{})
	for i := 0; i < 3; i++ {
		if got := p.Pick(); got != "K"+string(rune('1'+i)) {
			t.Fatalf("pick %d = %s, want K%d", i, got, i+1)
		}
	}
	if got := p.Pick(); got != "K1" {
		t.Fatalf("pick after wrap = %s, want K1", got)
	}
}

func TestRoundRobinSkipsQuarantined(t *testing.T) {
	p, _ := New("p", []string{"K1", "K2", "K3"}, "round_robin", config.QuarantineCfg{})
	now := time.Now()
	p.now = func() time.Time { return now }
	p.Mark("K1", ClassAuth)
	p.Mark("K2", ClassAuth)
	if got := p.Pick(); got != "K3" {
		t.Fatalf("pick with K1/K2 down = %s, want K3", got)
	}
	if got := p.Pick(); got != "K3" {
		t.Fatalf("pick again = %s, want K3 (rrNext must not advance past quarantine)", got)
	}
}

func TestSequentialPrefersFirstHealthy(t *testing.T) {
	p, _ := New("p", []string{"K1", "K2", "K3"}, "sequential", config.QuarantineCfg{})
	now := time.Now()
	p.now = func() time.Time { return now }
	if got := p.Pick(); got != "K1" {
		t.Fatalf("first pick = %s, want K1", got)
	}
	p.Mark("K1", ClassRateLimit)
	if got := p.Pick(); got != "K2" {
		t.Fatalf("pick with K1 down = %s, want K2", got)
	}
}

func TestLeastUsedPicksOldest(t *testing.T) {
	p, _ := New("p", []string{"K1", "K2", "K3"}, "least_used", config.QuarantineCfg{})
	now := time.Now()
	p.now = func() time.Time { return now }
	p.Pick() // K1 lastAt = now
	now = now.Add(time.Second)
	if got := p.Pick(); got != "K2" {
		t.Fatalf("pick = %s, want K2 (K1 used most recently)", got)
	}
}

func TestAllQuarantinedFallsBackToMostRecovered(t *testing.T) {
	p, _ := New("p", []string{"K1", "K2"}, "round_robin", config.QuarantineCfg{})
	now := time.Now()
	p.now = func() time.Time { return now }
	p.Mark("K1", ClassAuth) // down until now+24h
	now = now.Add(time.Minute)
	p.Mark("K2", ClassAuth) // down until now+24h (1 minute later than K1)
	if got := p.Pick(); got != "K1" {
		t.Fatalf("fallback pick = %s, want K1 (earliest expiry)", got)
	}
}

func TestMarkDoublesAndCaps(t *testing.T) {
	p, _ := New("p", []string{"K"}, "round_robin", config.QuarantineCfg{})
	now := time.Now()
	p.now = func() time.Time { return now }
	p.Mark("K", ClassRateLimit)
	if got := p.keys[0].downUntil.Sub(now); got != 60*time.Second {
		t.Errorf("1st failure cool-off = %v, want 60s", got)
	}
	now = now.Add(time.Hour)
	p.Mark("K", ClassRateLimit)
	if got := p.keys[0].downUntil.Sub(now); got != 120*time.Second {
		t.Errorf("2nd failure cool-off = %v, want 120s", got)
	}
	now = now.Add(time.Hour)
	p.Mark("K", ClassRateLimit)
	if got := p.keys[0].downUntil.Sub(now); got != 240*time.Second {
		t.Errorf("3rd failure cool-off = %v, want 240s", got)
	}
	for i := 0; i < 15; i++ {
		now = now.Add(time.Hour)
		p.Mark("K", ClassRateLimit)
	}
	if got := p.keys[0].downUntil.Sub(now); got > config.DefaultQuarantine.Auth.Duration {
		t.Errorf("cool-off = %v, must cap at %v", got, config.DefaultQuarantine.Auth.Duration)
	}
}

func TestMarkSuccessResets(t *testing.T) {
	p, _ := New("p", []string{"K"}, "round_robin", config.QuarantineCfg{})
	now := time.Now()
	p.now = func() time.Time { return now }
	p.Mark("K", ClassAuth)
	if p.keys[0].healthy(now) {
		t.Fatal("key should be quarantined after mark")
	}
	p.MarkSuccess("K")
	if !p.keys[0].healthy(now) {
		t.Error("key should be healthy after success")
	}
	if p.keys[0].failures != 0 || p.keys[0].lastClass != "" {
		t.Error("failures/lastClass must reset")
	}
}

func TestRecoveryAfterCoolOffExpires(t *testing.T) {
	p, _ := New("p", []string{"K"}, "round_robin", config.QuarantineCfg{})
	now := time.Now()
	p.now = func() time.Time { return now }
	p.Mark("K", ClassRateLimit)
	now = now.Add(61 * time.Second)
	if !p.keys[0].healthy(now) {
		t.Error("key must recover after cool-off expires")
	}
	if got := p.Pick(); got != "K" {
		t.Fatalf("pick after recovery = %s, want K", got)
	}
}

func TestRecordCounters(t *testing.T) {
	p, _ := New("p", []string{"K"}, "round_robin", config.QuarantineCfg{})
	p.Record("K", 10, 20)
	p.Record("K", 5, 7)
	p.Record("NOPE", 100, 100) // unknown key ignored
	ks := p.Keys()[0]
	if ks.Requests != 2 || ks.TokensIn != 15 || ks.TokensOut != 27 {
		t.Errorf("counters = req %d in %d out %d, want 2/15/27", ks.Requests, ks.TokensIn, ks.TokensOut)
	}
}

func TestKeysSnapshot(t *testing.T) {
	p, _ := New("p", []string{"K1", "K2"}, "round_robin", config.QuarantineCfg{})
	now := time.Now()
	p.now = func() time.Time { return now }
	ks := p.Keys()
	if ks[0].State != "healthy" || ks[0].Reason != "" {
		t.Errorf("healthy key state = %q/%q", ks[0].State, ks[0].Reason)
	}
	p.Mark("K1", ClassAuth)
	ks = p.Keys()
	if ks[0].State != "quarantined" || ks[0].Reason != "auth" {
		t.Errorf("quarantined key state = %q/%q, want quarantined/auth", ks[0].State, ks[0].Reason)
	}
	if ks[0].DownFor != 24*time.Hour {
		t.Errorf("down_for_ns = %v, want 24h", ks[0].DownFor)
	}
	if ks[0].Env != "K1" || ks[0].Index != 0 || ks[0].Provider != "p" {
		t.Errorf("identity fields wrong: %+v", ks[0])
	}
}

func TestMarkEmptyClassNoop(t *testing.T) {
	p, _ := New("p", []string{"K"}, "round_robin", config.QuarantineCfg{})
	p.Mark("K", "")
	if p.keys[0].failures != 0 {
		t.Error("empty class must not mark")
	}
}
