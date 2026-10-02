package scheduler

import (
	"testing"
	"time"

	"domain_scanner/internal/egress"
)

type clk struct{ t time.Time }

func (c *clk) now() time.Time { return c.t }

func testCfg() healthConfig {
	return healthConfig{Window: 20, MinSamples: 10, Threshold: 0.6, BackoffBase: 30 * time.Second,
		BackoffMax: 10 * time.Minute, HealthyReset: 2 * time.Minute, ReturnAfter: 5 * time.Minute}
}

func setup(t *testing.T, mode string, proxyID int64, failover bool, healthy ...string) (*egressState, *egress.Registry, *clk) {
	t.Helper()
	c := &clk{time.Unix(2_000_000, 0)}
	reg := egress.NewRegistry()
	reg.SetClock(c.now)
	var entries []egress.ProxyEntry
	for _, id := range []string{"proxy-1", "proxy-2", "proxy-3"} {
		entries = append(entries, egress.ProxyEntry{ID: id, Name: id, Addr: "127.0.0.1:1", Enabled: true})
	}
	reg.Replace(entries)
	for _, id := range healthy {
		reg.SetHealth(id, true)
	}
	st := newEgressState(reg, testCfg(), mode, proxyID, failover)
	st.now = c.now
	return st, reg, c
}

func feed(st *egressState, failures, successes int) *decision {
	var d *decision
	for i := 0; i < failures; i++ {
		if x := st.Record(true); x != nil {
			d = x
		}
	}
	for i := 0; i < successes; i++ {
		if x := st.Record(false); x != nil {
			d = x
		}
	}
	return d
}

func TestNoTriggerBeforeEnoughSamples(t *testing.T) {
	st, _, _ := setup(t, "direct", 0, true, "proxy-1")
	if d := feed(st, 9, 0); d != nil {
		t.Fatalf("triggered with only 9 samples: %+v", d)
	}
}

func TestNoTriggerBelowThreshold(t *testing.T) {
	st, _, _ := setup(t, "direct", 0, true, "proxy-1")
	// 5 failures in 20 samples = 25%
	if d := feed(st, 5, 15); d != nil {
		t.Fatalf("triggered at 25%% failures: %+v", d)
	}
}

func TestStormBacksOffPenalisesAndSwitchesToAHealthyProxy(t *testing.T) {
	st, reg, c := setup(t, "direct", 0, true, "proxy-1", "proxy-2")
	if st.Current() != egress.DirectID {
		t.Fatalf("direct job starts on %q", st.Current())
	}
	d := feed(st, 10, 0)
	if d == nil {
		t.Fatal("10/10 failures must trigger")
	}
	if d.From != egress.DirectID || d.To != "proxy-1" || !d.Switched || d.Backoff != 30*time.Second {
		t.Fatalf("decision = %+v", d)
	}
	if st.Current() != "proxy-1" {
		t.Fatalf("current = %q", st.Current())
	}
	if _, pen := reg.Penalized(egress.DirectID); !pen {
		t.Fatal("the failing egress must be put into cooldown for every job")
	}
	if got := st.BlockedUntil(); !got.Equal(c.now().Add(30 * time.Second)) {
		t.Fatalf("workers must pause until %v, got %v", c.now().Add(30*time.Second), got)
	}
	if d.FailureRate < 0.99 {
		t.Fatalf("failure rate = %v", d.FailureRate)
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	st, _, c := setup(t, "direct", 0, false) // no failover: only backoff
	want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 240 * time.Second, 480 * time.Second, 600 * time.Second, 600 * time.Second}
	for i, w := range want {
		d := feed(st, 10, 0)
		if d == nil || d.Backoff != w {
			t.Fatalf("trigger %d: decision = %+v, want backoff %v", i+1, d, w)
		}
		c.t = c.t.Add(d.Backoff) // keep going after the pause
	}
}

func TestFailoverDisabledOnlyBacksOff(t *testing.T) {
	st, _, _ := setup(t, "direct", 0, false, "proxy-1")
	d := feed(st, 10, 0)
	if d == nil || d.Switched || st.Current() != egress.DirectID || d.Backoff == 0 {
		t.Fatalf("decision = %+v current=%q; without failover the job must stay put and back off", d, st.Current())
	}
}

func TestNoHealthyCandidateMeansBackoffWithoutSwitch(t *testing.T) {
	st, _, _ := setup(t, "direct", 0, true) // nothing healthy
	d := feed(st, 10, 0)
	if d == nil || d.Switched || st.Current() != egress.DirectID {
		t.Fatalf("decision = %+v", d)
	}
}

func TestFailingProxySwitchesToTheNextAndThenBackToDirectWhenItRecovers(t *testing.T) {
	st, reg, c := setup(t, "direct", 0, true, "proxy-1")
	feed(st, 10, 0) // direct -> proxy-1
	c.t = c.t.Add(time.Minute)
	d := feed(st, 10, 0) // proxy-1 storms too; no other proxy; direct still cooling down
	if d == nil || d.Switched {
		t.Fatalf("nothing to switch to yet: %+v", d)
	}
	c.t = c.t.Add(6 * time.Minute) // direct's 5 minute cooldown has passed
	d = feed(st, 10, 0)
	if d == nil || !d.Switched || d.To != egress.DirectID {
		t.Fatalf("a proxy failing while direct recovered must fall back to direct: %+v", d)
	}
	if _, pen := reg.Penalized("proxy-1"); !pen {
		t.Fatal("the failing proxy must be cooling down")
	}
}

func TestReturnsToPreferredEgressAfterCooldown(t *testing.T) {
	st, _, c := setup(t, "direct", 0, true, "proxy-1")
	feed(st, 10, 0)
	if st.Current() != "proxy-1" {
		t.Fatal("setup: expected a switch")
	}
	if _, ok := st.MaybeReturn(); ok {
		t.Fatal("must not return while direct is still cooling down")
	}
	c.t = c.t.Add(6 * time.Minute)
	to, ok := st.MaybeReturn()
	if !ok || to != egress.DirectID || st.Current() != egress.DirectID {
		t.Fatalf("expected a return to direct, got %q %v (current %q)", to, ok, st.Current())
	}
}

func TestBackoffResetsAfterAHealthyStretch(t *testing.T) {
	st, _, c := setup(t, "direct", 0, false)
	feed(st, 10, 0)
	c.t = c.t.Add(31 * time.Second)
	feed(st, 0, 20) // window is healthy again
	c.t = c.t.Add(3 * time.Minute)
	feed(st, 0, 5) // healthy for longer than HealthyReset
	d := feed(st, 20, 0)
	if d == nil || d.Backoff != 30*time.Second {
		t.Fatalf("backoff after recovery = %+v, want it to restart at 30s", d)
	}
}

func TestPoolModeStartsOnAHealthyProxyAndRotates(t *testing.T) {
	st, _, _ := setup(t, "pool", 0, true, "proxy-2", "proxy-3")
	if st.Current() != "proxy-2" {
		t.Fatalf("pool starts on %q, want the first healthy proxy-2", st.Current())
	}
	d := feed(st, 10, 0)
	if d == nil || d.To != "proxy-3" {
		t.Fatalf("pool rotation: %+v", d)
	}
}

func TestPoolWithoutHealthyProxiesFallsBackToDirect(t *testing.T) {
	st, _, _ := setup(t, "pool", 0, true)
	if st.Current() != egress.DirectID {
		t.Fatalf("empty pool should fall back to direct, got %q", st.Current())
	}
}

func TestFixedProxyStaysUnlessFailoverIsOn(t *testing.T) {
	st, _, _ := setup(t, "proxy", 2, false, "proxy-1", "proxy-2")
	if st.Current() != "proxy-2" {
		t.Fatalf("fixed proxy job on %q", st.Current())
	}
	if d := feed(st, 10, 0); d == nil || d.Switched || st.Current() != "proxy-2" {
		t.Fatalf("fixed proxy without failover must not switch: %+v", d)
	}
	st2, _, _ := setup(t, "proxy", 2, true, "proxy-1", "proxy-2")
	if d := feed(st2, 10, 0); d == nil || !d.Switched || st2.Current() != "proxy-1" {
		t.Fatalf("fixed proxy with failover should move to another proxy: %+v", d)
	}
}

func TestOneJobsStormSteersTheOtherJobsAway(t *testing.T) {
	a, reg, c := setup(t, "direct", 0, true, "proxy-1")
	b := newEgressState(reg, testCfg(), "direct", 0, true)
	b.now = c.now
	if _, _, ok := b.Steer(); ok {
		t.Fatal("nothing is cooling down yet")
	}
	feed(a, 10, 0) // job A finds direct failing: direct cools down for everyone
	from, to, ok := b.Steer()
	if !ok || from != egress.DirectID || to != "proxy-1" || b.Current() != "proxy-1" {
		t.Fatalf("Steer = %q -> %q ok=%v, current %q", from, to, ok, b.Current())
	}
	if !b.BlockedUntil().IsZero() && b.BlockedUntil().After(c.now()) {
		t.Fatal("steering is not a storm: it must not pause the job")
	}
	noFailover := newEgressState(reg, testCfg(), "direct", 0, false)
	noFailover.now = c.now
	if _, _, ok := noFailover.Steer(); ok {
		t.Fatal("a job with failover disabled must not be moved")
	}
}

func TestFailoverAvailableReflectsCandidates(t *testing.T) {
	st, reg, _ := setup(t, "direct", 0, true)
	if st.FailoverAvailable() {
		t.Fatal("no healthy proxies: failover not available")
	}
	reg.SetHealth("proxy-1", true)
	if !st.FailoverAvailable() {
		t.Fatal("a healthy proxy makes failover available")
	}
	off, _, _ := setup(t, "direct", 0, false, "proxy-1")
	if off.FailoverAvailable() {
		t.Fatal("failover disabled")
	}
}
