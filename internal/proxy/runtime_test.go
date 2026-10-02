package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"domain_scanner/internal/logbus"
)

// fakeProc stands in for the xray process: it serves SOCKS5 on every inbound port found in the
// config file, so the manager and the probe can be tested end to end without the real binary.
type fakeProc struct {
	servers []*socksServer
	stopped chan struct{}
	once    sync.Once
	crash   chan struct{}
	exitErr error
}

func (p *fakeProc) Wait() error {
	select {
	case <-p.stopped:
		return nil
	case <-p.crash:
		return errors.New("exit status 1")
	}
}

func (p *fakeProc) Stop() {
	p.once.Do(func() {
		close(p.stopped)
		for _, s := range p.servers {
			s.ln.Close()
		}
	})
}

type fakeStarter struct {
	t       *testing.T
	started atomic.Int32
	fail    atomic.Bool
	mu      sync.Mutex
	procs   []*fakeProc
	configs []string
}

func (f *fakeStarter) start(ctx context.Context, bin, cfgPath string, logf func(level, line string)) (Process, error) {
	if f.fail.Load() {
		return nil, errors.New("exec: xray: not found")
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, err
	}
	var c struct {
		Inbounds []struct {
			Port int `json:"port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	p := &fakeProc{stopped: make(chan struct{}), crash: make(chan struct{})}
	for _, in := range c.Inbounds {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(in.Port)))
		if err != nil {
			return nil, err
		}
		s := &socksServer{ln: ln}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go s.handle(c)
			}
		}()
		p.servers = append(p.servers, s)
	}
	f.started.Add(1)
	f.mu.Lock()
	f.procs = append(f.procs, p)
	f.configs = append(f.configs, string(raw))
	f.mu.Unlock()
	logf("warn", "[Warning] fake xray started")
	return p, nil
}

func (f *fakeStarter) last() *fakeProc {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.procs[len(f.procs)-1]
}

func newManager(t *testing.T) (*Manager, *fakeStarter) {
	t.Helper()
	bus := logbus.New(nil, 100)
	t.Cleanup(bus.Close)
	f := &fakeStarter{t: t}
	m := NewManager("/usr/local/bin/xray", t.TempDir(), bus.Logger("xray"))
	m.Starter = f.start
	m.Tester = nil // no xray binary in unit tests; individual tests install a fake Tester
	m.BasePort = freeBase(t)
	m.RestartBackoff = 20 * time.Millisecond
	m.SkipBinaryCheck = true
	t.Cleanup(m.Stop)
	return m, f
}

// freeBase finds a port base with a run of free ports so parallel tests do not collide.
func freeBase(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if p+PortSpan > 65000 {
		return p - PortSpan - 1000
	}
	return p
}

func TestApplyStartsOneProcessWithAListenerPerOutbound(t *testing.T) {
	m, f := newManager(t)
	ports, err := m.Apply(context.Background(), []Entry{entry(1, vlessCfg), entry(2, vlessCfg)})
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 2 || f.started.Load() != 1 || !m.Running() {
		t.Fatalf("ports=%v started=%d running=%v", ports, f.started.Load(), m.Running())
	}
	for id, port := range ports {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
		if err != nil {
			t.Fatalf("outbound %d: nothing listens on %d: %v", id, port, err)
		}
		c.Close()
		if p, ok := m.Port(id); !ok || p != port {
			t.Fatalf("Port(%d) = %d,%v", id, p, ok)
		}
	}
	if _, ok := m.Port(99); ok {
		t.Fatal("unknown outbound must have no port")
	}
}

func TestApplyAgainRestartsWithTheNewSet(t *testing.T) {
	m, f := newManager(t)
	m.Apply(context.Background(), []Entry{entry(1, vlessCfg)})
	first := f.last()
	ports, err := m.Apply(context.Background(), []Entry{entry(2, vlessCfg)})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.stopped:
	case <-time.After(time.Second):
		t.Fatal("the old process was not stopped")
	}
	if f.started.Load() != 2 || len(ports) != 1 {
		t.Fatalf("started=%d ports=%v", f.started.Load(), ports)
	}
	if _, ok := m.Port(1); ok {
		t.Fatal("removed outbound still has a port")
	}
}

func TestApplyWithNothingStopsXray(t *testing.T) {
	m, f := newManager(t)
	m.Apply(context.Background(), []Entry{entry(1, vlessCfg)})
	proc := f.last()
	ports, err := m.Apply(context.Background(), nil)
	if err != nil || len(ports) != 0 || m.Running() {
		t.Fatalf("ports=%v err=%v running=%v", ports, err, m.Running())
	}
	select {
	case <-proc.stopped:
	case <-time.After(time.Second):
		t.Fatal("xray still running with no outbounds")
	}
}

func TestStartFailureIsReportedAndLeavesNothingRunning(t *testing.T) {
	m, f := newManager(t)
	f.fail.Store(true)
	if _, err := m.Apply(context.Background(), []Entry{entry(1, vlessCfg)}); err == nil {
		t.Fatal("expected an error")
	}
	if m.Running() {
		t.Fatal("must not report running after a failed start")
	}
	if m.LastError() == "" {
		t.Fatal("LastError should explain why xray is not running")
	}
	f.fail.Store(false)
	if _, err := m.Apply(context.Background(), []Entry{entry(1, vlessCfg)}); err != nil || m.LastError() != "" {
		t.Fatalf("recovery: err=%v lastErr=%q", err, m.LastError())
	}
}

func TestCrashedProcessIsRestarted(t *testing.T) {
	m, f := newManager(t)
	ports, _ := m.Apply(context.Background(), []Entry{entry(1, vlessCfg)})
	port := ports[1]
	// simulate xray dying; the old listener must go away so the replacement can bind again
	proc := f.last()
	for _, s := range proc.servers {
		s.ln.Close()
	}
	close(proc.crash)
	deadline := time.Now().Add(3 * time.Second)
	for f.started.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.started.Load() < 2 {
		t.Fatal("manager did not restart the crashed process")
	}
	if c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second); err != nil {
		t.Fatalf("restarted process does not listen: %v", err)
	} else {
		c.Close()
	}
}

func TestStopDoesNotTriggerARestart(t *testing.T) {
	m, f := newManager(t)
	m.Apply(context.Background(), []Entry{entry(1, vlessCfg)})
	m.Stop()
	time.Sleep(150 * time.Millisecond)
	if f.started.Load() != 1 {
		t.Fatalf("started %d times; an intentional Stop must not be treated as a crash", f.started.Load())
	}
}

func TestApplyRejectsAnInvalidConfigBeforeStartingAnything(t *testing.T) {
	m, f := newManager(t)
	if _, err := m.Apply(context.Background(), []Entry{entry(1, `{broken`)}); err == nil {
		t.Fatal("expected an error")
	}
	if f.started.Load() != 0 {
		t.Fatal("a broken config must not start a process")
	}
}

func TestTestConfigUsesATemporaryInstance(t *testing.T) {
	m, f := newManager(t)
	site := testSite(t, 204, 0)
	r := m.TestConfig(context.Background(), json.RawMessage(vlessCfg), ProbeOptions{TestURL: site.URL + "/x", TraceURL: site.URL + "/trace", Timeout: 2 * time.Second})
	if !r.OK || r.IP != "203.0.113.7" {
		t.Fatalf("result = %+v", r)
	}
	if f.started.Load() != 1 {
		t.Fatalf("started %d", f.started.Load())
	}
	select {
	case <-f.last().stopped:
	case <-time.After(time.Second):
		t.Fatal("the temporary instance was left running")
	}
	if m.Running() {
		t.Fatal("testing a config must not disturb the main instance")
	}
}

// rejectingTester fails any config containing the marker, like `xray run -test` would.
func rejectingTester(marker, message string) Tester {
	return func(ctx context.Context, bin, cfgPath string) (string, error) {
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			return "", err
		}
		if strings.Contains(string(raw), marker) {
			return message, errors.New("exit status 23")
		}
		return "Configuration OK.", nil
	}
}

func TestApplyPrechecksTheConfigAndReportsXraysOwnMessage(t *testing.T) {
	m, f := newManager(t)
	m.Tester = rejectingTester("BADUUID", "Failed to start: infra/conf: invalid UUID: BADUUID")
	bad := `{"protocol":"vless","settings":{"address":"a.example","port":443,"id":"BADUUID"}}`
	_, err := m.Apply(context.Background(), []Entry{entry(1, bad)})
	var ce *ConfigError
	if !errors.As(err, &ce) || !strings.Contains(ce.Message, "invalid UUID") {
		t.Fatalf("err = %v, want a *ConfigError carrying xray's message", err)
	}
	if f.started.Load() != 0 {
		t.Fatal("a rejected config must never be started")
	}
	if !strings.Contains(m.LastError(), "invalid UUID") {
		t.Fatalf("LastError = %q", m.LastError())
	}
}

func TestTestConfigFailsFastWithXraysMessage(t *testing.T) {
	m, f := newManager(t)
	m.Tester = rejectingTester("BADUUID", "Failed to start: infra/conf: invalid UUID: BADUUID")
	bad := `{"protocol":"vless","settings":{"address":"a.example","port":443,"id":"BADUUID"}}`
	start := time.Now()
	r := m.TestConfig(context.Background(), json.RawMessage(bad), ProbeOptions{})
	if r.OK || !strings.Contains(r.Error, "invalid UUID") {
		t.Fatalf("result = %+v", r)
	}
	if time.Since(start) > 2*time.Second || f.started.Load() != 0 {
		t.Fatalf("took %v / started %d; the precheck must reject before any process is spawned", time.Since(start), f.started.Load())
	}
}

func TestCheckConfigAcceptsGoodAndRejectsBad(t *testing.T) {
	m, _ := newManager(t)
	m.Tester = rejectingTester("BADUUID", "nope")
	if err := m.CheckConfig(context.Background(), json.RawMessage(vlessCfg)); err != nil {
		t.Fatalf("good config: %v", err)
	}
	if err := m.CheckConfig(context.Background(), json.RawMessage(`{"protocol":"vless","settings":{"id":"BADUUID"}}`)); err == nil {
		t.Fatal("bad config accepted")
	}
}

func TestTestConfigReportsStartFailureAndBadJSON(t *testing.T) {
	m, f := newManager(t)
	f.fail.Store(true)
	if r := m.TestConfig(context.Background(), json.RawMessage(vlessCfg), ProbeOptions{}); r.OK || r.Error == "" {
		t.Fatalf("start failure: %+v", r)
	}
	f.fail.Store(false)
	if r := m.TestConfig(context.Background(), json.RawMessage(`{x`), ProbeOptions{}); r.OK || r.Error == "" {
		t.Fatalf("bad json: %+v", r)
	}
}
