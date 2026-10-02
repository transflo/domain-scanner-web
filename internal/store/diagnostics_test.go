package store

import (
	"context"
	"testing"
	"time"
)

func seedChecks(t *testing.T, s *Store) {
	t.Helper()
	now := time.Now()
	mk := func(domain, egress, status, kind string, dur int64) LogEntry {
		return LogEntry{Level: "debug", Component: "check", Event: "done", Domain: domain, Egress: egress, DurationMS: dur, Time: now,
			Message: domain + " → " + status, Fields: map[string]any{"status": status, "err_kind": kind}}
	}
	es := []LogEntry{
		mk("a.com", "direct", "registered", "", 100),
		mk("b.com", "direct", "available", "", 300),
		mk("c.com", "direct", "unknown", "rate_limited", 50),
		mk("d.li", "proxy-1", "available", "", 200),
		mk("e.li", "proxy-1", "unknown", "timeout", 1000),
		mk("f.co.uk", "proxy-1", "registered", "", 400),
		// noise that must not be counted
		{Level: "debug", Component: "check", Event: "step", Domain: "a.com", Egress: "direct", Time: now, Message: "step",
			Fields: map[string]any{"step": "rdap.throttle"}},
		{Level: "debug", Component: "check", Event: "step", Domain: "c.com", Egress: "direct", Time: now, Message: "step",
			Fields: map[string]any{"step": "rdap.throttle"}},
		{Level: "debug", Component: "check", Event: "step", Domain: "c.com", Egress: "direct", Time: now, Message: "step",
			Fields: map[string]any{"step": "dns"}},
		{Level: "warn", Component: "egress", Event: "storm", Time: now, Message: "storm", Fields: map[string]any{"from": "direct", "switched": true}},
		{Level: "warn", Component: "egress", Event: "storm", Time: now, Message: "storm", Fields: map[string]any{"from": "direct", "switched": false}},
		{Level: "info", Component: "check", Event: "found", Domain: "b.com", Time: now, Message: "found"},
	}
	if err := s.InsertLogs(context.Background(), es); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosticsByEgress(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	seedChecks(t, s)
	d, err := s.Diagnostics(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if d.Checks != 6 {
		t.Fatalf("checks = %d, want 6 (only check.done lines count)", d.Checks)
	}
	by := map[string]EgressDiag{}
	for _, e := range d.ByEgress {
		by[e.Egress] = e
	}
	dir, p1 := by["direct"], by["proxy-1"]
	if dir.Checks != 3 || dir.Unknown != 1 || dir.RateLimited != 1 || dir.Available != 1 || dir.Registered != 1 {
		t.Fatalf("direct = %+v", dir)
	}
	if dir.Throttles != 2 {
		t.Fatalf("direct throttles = %d, want the 2 rdap.throttle steps", dir.Throttles)
	}
	if dir.Storms != 2 {
		t.Fatalf("direct storms = %d, want 2", dir.Storms)
	}
	if p1.Checks != 3 || p1.Timeouts != 1 || p1.Unknown != 1 {
		t.Fatalf("proxy-1 = %+v", p1)
	}
	if dir.AvgMS < 149 || dir.AvgMS > 151 || dir.MaxMS != 300 {
		t.Fatalf("direct timings avg=%v max=%d, want avg 150 max 300", dir.AvgMS, dir.MaxMS)
	}
	if dir.SuccessRate < 0.66 || dir.SuccessRate > 0.67 {
		t.Fatalf("direct success rate = %v, want 2/3 (unknown is not a success)", dir.SuccessRate)
	}
}

func TestDiagnosticsByTLD(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	seedChecks(t, s)
	d, _ := s.Diagnostics(ctx, time.Now().Add(-time.Hour))
	by := map[string]TLDDiag{}
	for _, x := range d.ByTLD {
		by[x.TLD] = x
	}
	if by["com"].Checks != 3 || by["com"].Available != 1 || by["com"].Registered != 1 || by["com"].Unknown != 1 {
		t.Fatalf("com = %+v", by["com"])
	}
	if by["li"].Checks != 2 || by["li"].Available != 1 || by["li"].Unknown != 1 {
		t.Fatalf("li = %+v", by["li"])
	}
	if by["co.uk"].Checks != 1 {
		t.Fatalf("multi-level suffixes keep everything after the first dot: %+v", by)
	}
}

func TestDiagnosticsRespectsTheWindowAndEmptyData(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	d, err := s.Diagnostics(ctx, time.Now().Add(-time.Hour))
	if err != nil || d.Checks != 0 || len(d.ByEgress) != 0 || len(d.ByTLD) != 0 {
		t.Fatalf("empty db: %+v err=%v", d, err)
	}
	seedChecks(t, s)
	d, _ = s.Diagnostics(ctx, time.Now().Add(time.Hour)) // window starts in the future
	if d.Checks != 0 {
		t.Fatalf("checks outside the window were counted: %d", d.Checks)
	}
}
