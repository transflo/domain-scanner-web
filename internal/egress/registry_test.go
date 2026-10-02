package egress

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newReg(c *fakeClock) *Registry {
	r := NewRegistry()
	r.now = c.now
	r.SetCooldown(5*time.Minute, 30*time.Minute)
	return r
}

func TestRegistryAlwaysHasDirect(t *testing.T) {
	r := NewRegistry()
	e, ok := r.Get(DirectID)
	if !ok || !e.IsDirect() {
		t.Fatalf("direct egress missing: %v %v", e, ok)
	}
	if _, ok := r.Get("proxy-9"); ok {
		t.Fatal("unknown id must not resolve")
	}
}

func TestReplaceKeepsExistingEgressObjectsAndHealth(t *testing.T) {
	r := NewRegistry()
	r.Replace([]ProxyEntry{{ID: "proxy-1", Name: "a", Addr: "127.0.0.1:20001", Enabled: true}})
	first, _ := r.Get("proxy-1")
	r.SetHealth("proxy-1", true)
	r.Replace([]ProxyEntry{
		{ID: "proxy-1", Name: "renamed", Addr: "127.0.0.1:20001", Enabled: true},
		{ID: "proxy-2", Name: "b", Addr: "127.0.0.1:20002", Enabled: true},
	})
	again, _ := r.Get("proxy-1")
	if again != first {
		t.Fatal("same address must keep the same Egress (pooled connections, throttle state)")
	}
	if got := r.Candidates(false); len(got) != 1 || got[0] != "proxy-1" {
		t.Fatalf("candidates = %v; proxy-1 keeps its health, proxy-2 is untested", got)
	}
	r.Replace([]ProxyEntry{{ID: "proxy-1", Name: "a", Addr: "127.0.0.1:29999", Enabled: true}})
	moved, _ := r.Get("proxy-1")
	if moved == first || moved.SocksAddr != "127.0.0.1:29999" {
		t.Fatal("a changed address must produce a new Egress")
	}
	if _, ok := r.Get("proxy-2"); ok {
		t.Fatal("removed proxy still resolvable")
	}
}

func TestCandidatesOnlyHealthyEnabledAndUnpenalised(t *testing.T) {
	c := &fakeClock{time.Unix(1_000_000, 0)}
	r := newReg(c)
	r.Replace([]ProxyEntry{
		{ID: "proxy-1", Addr: "a:1", Enabled: true},
		{ID: "proxy-2", Addr: "a:2", Enabled: true},
		{ID: "proxy-3", Addr: "a:3", Enabled: false},
		{ID: "proxy-4", Addr: "a:4", Enabled: true},
	})
	r.SetHealth("proxy-1", true)
	r.SetHealth("proxy-2", true)
	r.SetHealth("proxy-3", true)
	r.SetHealth("proxy-4", false)
	if got := r.Candidates(false); len(got) != 2 || got[0] != "proxy-1" || got[1] != "proxy-2" {
		t.Fatalf("candidates = %v, want [proxy-1 proxy-2]", got)
	}
	if got := r.Candidates(false, "proxy-1"); len(got) != 1 || got[0] != "proxy-2" {
		t.Fatalf("with exclusion = %v", got)
	}
	if got := r.Candidates(true); len(got) != 3 || got[0] != DirectID {
		t.Fatalf("with direct = %v, want direct first", got)
	}
	r.Penalize("proxy-1")
	if got := r.Candidates(false); len(got) != 1 || got[0] != "proxy-2" {
		t.Fatalf("penalised proxy still offered: %v", got)
	}
}

func TestPenaltyEscalatesAndExpires(t *testing.T) {
	c := &fakeClock{time.Unix(1_000_000, 0)}
	r := newReg(c)
	if d := r.Penalize(DirectID); d != 5*time.Minute {
		t.Fatalf("first penalty = %v, want 5m", d)
	}
	if _, pen := r.Penalized(DirectID); !pen {
		t.Fatal("direct should be penalised")
	}
	c.t = c.t.Add(5*time.Minute + time.Second)
	if _, pen := r.Penalized(DirectID); pen {
		t.Fatal("penalty should have expired")
	}
	if d := r.Penalize(DirectID); d != 10*time.Minute {
		t.Fatalf("second penalty = %v, want 10m (strikes persist until Recover)", d)
	}
	for i := 0; i < 6; i++ {
		c.t = c.t.Add(time.Hour)
		r.Penalize(DirectID)
	}
	c.t = c.t.Add(time.Hour)
	if d := r.Penalize(DirectID); d != 30*time.Minute {
		t.Fatalf("penalty must cap at 30m, got %v", d)
	}
	r.Recover(DirectID)
	c.t = c.t.Add(time.Hour)
	if d := r.Penalize(DirectID); d != 5*time.Minute {
		t.Fatalf("after Recover the penalty restarts at 5m, got %v", d)
	}
}

func TestSnapshotDescribesEveryEgress(t *testing.T) {
	r := NewRegistry()
	r.Replace([]ProxyEntry{{ID: "proxy-1", Name: "hk", Addr: "a:1", Enabled: true}})
	r.SetHealth("proxy-1", true)
	r.Penalize("proxy-1")
	snap := r.Snapshot()
	if len(snap) != 2 || snap[0].ID != DirectID || snap[1].ID != "proxy-1" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if !snap[1].Healthy || snap[1].PenalizedUntil.IsZero() || snap[1].Strikes != 1 || snap[1].Name != "hk" {
		t.Fatalf("proxy snapshot = %+v", snap[1])
	}
}
