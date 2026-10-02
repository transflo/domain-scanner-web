package proxy

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"domain_scanner/internal/egress"
	"domain_scanner/internal/logbus"
	"domain_scanner/internal/store"
)

type svcEnv struct {
	svc  *Service
	st   *store.Store
	reg  *egress.Registry
	f    *fakeStarter
	site string
}

func newSvc(t *testing.T) *svcEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m, f := newManager(t)
	reg := egress.NewRegistry()
	bus := logbus.New(nil, 200)
	t.Cleanup(bus.Close)
	site := testSite(t, 204, 0)
	svc := &Service{St: st, Mgr: m, Reg: reg, Log: bus.Logger("proxy"),
		TestURL: func() string { return site.URL + "/gen204" }, TraceURL: site.URL + "/trace", ProbeTimeout: 2 * time.Second}
	return &svcEnv{svc: svc, st: st, reg: reg, f: f, site: site.URL}
}

func (e *svcEnv) add(t *testing.T, name string, enabled bool) int64 {
	t.Helper()
	id, err := e.st.CreateOutbound(context.Background(), &store.Outbound{Name: name, Protocol: "vless", Address: "a.example", Port: 443,
		Config: json.RawMessage(vlessCfg), Enabled: enabled})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestReloadRunsEnabledOutboundsAndRegistersThem(t *testing.T) {
	e := newSvc(t)
	a := e.add(t, "one", true)
	b := e.add(t, "two", true)
	c := e.add(t, "off", false)
	if err := e.svc.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{a, b} {
		eg, ok := e.reg.Get(egress.ProxyID(id))
		if !ok || eg.SocksAddr == "" {
			t.Fatalf("outbound %d not registered: %v %v", id, eg, ok)
		}
		if port, _ := e.svc.Mgr.Port(id); eg.SocksAddr != "127.0.0.1:"+strconv.Itoa(port) {
			t.Fatalf("registered address %q does not match xray's port %d", eg.SocksAddr, port)
		}
	}
	if _, ok := e.reg.Get(egress.ProxyID(c)); ok {
		t.Fatal("a disabled outbound must not be registered")
	}
	if e.f.started.Load() != 1 {
		t.Fatalf("xray started %d times, want one shared process", e.f.started.Load())
	}
}

func TestTestAllSavesResultsAndUpdatesHealth(t *testing.T) {
	e := newSvc(t)
	a := e.add(t, "one", true)
	e.svc.Reload(context.Background())
	e.svc.TestAll(context.Background())

	o, _ := e.st.GetOutbound(context.Background(), a)
	if o.LastTestAt == nil || !o.LastOK || o.LastIP != "203.0.113.7" || o.LastCountry != "HK" {
		t.Fatalf("stored result = %+v", o)
	}
	if got := e.reg.Candidates(false); len(got) != 1 || got[0] != egress.ProxyID(a) {
		t.Fatalf("a passing proxy must become a failover candidate: %v", got)
	}

	// the proxy stops working (test URL now unreachable): it must drop out of the candidates
	e.svc.TestURL = func() string { return "http://127.0.0.1:1/dead" }
	e.svc.TestAll(context.Background())
	o, _ = e.st.GetOutbound(context.Background(), a)
	if o.LastOK || o.LastError == "" {
		t.Fatalf("failed test not stored: %+v", o)
	}
	if got := e.reg.Candidates(false); len(got) != 0 {
		t.Fatalf("a failing proxy must not be offered: %v", got)
	}
}

func TestReloadIsolatesAnOutboundXrayRejects(t *testing.T) {
	e := newSvc(t)
	e.svc.Mgr.Tester = rejectingTester("BADUUID", "Failed to start: infra/conf: invalid UUID: BADUUID")
	good1 := e.add(t, "good-1", true)
	bad, _ := e.st.CreateOutbound(context.Background(), &store.Outbound{Name: "bad", Protocol: "vless", Address: "b.example", Port: 443,
		Config: json.RawMessage(`{"protocol":"vless","settings":{"address":"b.example","port":443,"id":"BADUUID"}}`), Enabled: true})
	good2 := e.add(t, "good-2", true)

	if err := e.svc.Reload(context.Background()); err != nil {
		t.Fatalf("one bad outbound must not break the others: %v", err)
	}
	for _, id := range []int64{good1, good2} {
		if _, ok := e.reg.Get(egress.ProxyID(id)); !ok {
			t.Fatalf("good outbound %d is not usable", id)
		}
	}
	if _, ok := e.reg.Get(egress.ProxyID(bad)); ok {
		t.Fatal("the rejected outbound must not be registered")
	}
	o, _ := e.st.GetOutbound(context.Background(), bad)
	if o.LastOK || !strings.Contains(o.LastError, "invalid UUID") {
		t.Fatalf("the reason must be stored so the UI can show it: %+v", o)
	}
	if e.f.started.Load() != 1 {
		t.Fatalf("xray started %d times, want one process for the good outbounds", e.f.started.Load())
	}
}

func TestReloadFailureEmptiesTheRegistry(t *testing.T) {
	e := newSvc(t)
	a := e.add(t, "one", true)
	e.svc.Reload(context.Background())
	e.f.fail.Store(true)
	e.add(t, "two", true)
	if err := e.svc.Reload(context.Background()); err == nil {
		t.Fatal("expected the xray start failure to surface")
	}
	if _, ok := e.reg.Get(egress.ProxyID(a)); ok {
		t.Fatal("no proxy can be used while xray is down")
	}
	st := e.svc.Status()
	if st.Error == "" || st.Running {
		t.Fatalf("status = %+v", st)
	}
}

func TestTestOneOnADisabledOutboundUsesATemporaryInstance(t *testing.T) {
	e := newSvc(t)
	id := e.add(t, "off", false)
	res, err := e.svc.TestOne(context.Background(), id)
	if err != nil || !res.OK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if e.f.started.Load() != 1 || e.svc.Mgr.Running() {
		t.Fatalf("started=%d running=%v: a disabled outbound is tested on a throw-away instance", e.f.started.Load(), e.svc.Mgr.Running())
	}
	o, _ := e.st.GetOutbound(context.Background(), id)
	if o.LastTestAt == nil || !o.LastOK {
		t.Fatalf("result not stored: %+v", o)
	}
	if _, err := e.svc.TestOne(context.Background(), 999); err != store.ErrNotFound {
		t.Fatalf("missing outbound: err = %v", err)
	}
}

func TestStatusListsEgresses(t *testing.T) {
	e := newSvc(t)
	e.add(t, "one", true)
	e.svc.Reload(context.Background())
	st := e.svc.Status()
	if !st.XrayAvailable || !st.Running || len(st.Egresses) != 2 || st.Egresses[0].ID != egress.DirectID {
		t.Fatalf("status = %+v", st)
	}
}
