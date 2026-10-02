package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"domain_scanner/internal/auth"
	"domain_scanner/internal/housekeeping"
	"domain_scanner/internal/logbus"
	"domain_scanner/internal/notifier"
	"domain_scanner/internal/store"
	"domain_scanner/internal/wordlists"
)

type fakeKeeper struct {
	runs atomic.Int32
	rep  housekeeping.Report
}

func (k *fakeKeeper) Last() housekeeping.Report { return k.rep }
func (k *fakeKeeper) Run(context.Context) housekeeping.Report {
	k.runs.Add(1)
	r := k.rep
	r.DeletedLogs = 7
	return r
}

// persistingFixture wires the bus to the database like production does, so rows reach the store
// only when the bus flushes.
func persistingFixture(t *testing.T) (*fixture, *fakeKeeper) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	bus := logbus.New(func(batch []store.LogEntry) { _ = st.InsertLogs(context.Background(), batch) }, 100)
	bus.SetMinLevel("debug")
	a, _ := auth.New(password, []byte("stored-secret-0123456789abcdef"), time.Hour, time.Now)
	k := &fakeKeeper{rep: housekeeping.Report{State: "ok", DBBytes: 1234, Logs: map[string]int64{"debug": 5}, At: time.Now()}}
	f := &fixture{st: st, bus: bus, auth: a, sched: &fakeSched{st: st}, tg: &fakeTG{},
		words: wordlists.NewManager(filepath.Join(dir, "b"), filepath.Join(dir, "u")),
		proxy: &proxyHolder{inner: &fakeProxy{}}, cfTest: &cfHolder{inner: fakeCFTester{}}}
	f.srv = httptest.NewServer(New(Deps{Store: st, Bus: bus, Sched: f.sched, Words: f.words, Telegram: f.tg, Auth: a, Proxy: f.proxy,
		Cloudflare: f.cfTest, Keeper: k, TelegramEnv: notifier.Config{}, MaxWordlistBytes: 1 << 20}))
	t.Cleanup(func() { f.srv.Close(); bus.Close(); st.Close() })
	return f, k
}

func get(t *testing.T, f *fixture, c *http.Client, path string) (*http.Response, string) {
	t.Helper()
	resp := f.do(t, c, "GET", path, nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestExportIncludesEveryLevelAndTheNewestUnflushedLines(t *testing.T) {
	f, _ := persistingFixture(t)
	c := f.login(t)
	lg := f.bus.Logger("dns")
	for i := 0; i < 3000; i++ { // more than one page of the export loop
		lg.Debug("lookup", 0, fmt.Sprintf("line %d", i), nil)
	}
	lg.Info("done", 0, "newest line", nil) // still in the bus buffer, not yet in the database
	lg.Error("boom", 0, "an error", nil)

	resp, body := get(t, f, c, "/api/logs/export") // no filter at all = the whole history
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	lines := strings.Split(strings.TrimSpace(body), "\n")
	// the login and the like may add lines of their own; ours must all be there
	if len(lines) < 3002 {
		t.Fatalf("export has %d lines, want at least 3002", len(lines))
	}
	if !strings.Contains(body, `"message":"newest line"`) || !strings.Contains(body, `"message":"line 0"`) || !strings.Contains(body, `"level":"error"`) {
		t.Fatal("export is missing debug, the newest line or the error")
	}
}

func TestExportAsPlainText(t *testing.T) {
	f, _ := persistingFixture(t)
	c := f.login(t)
	f.bus.Logger("rdap").Warn("throttle", 4, "429 from rdap.nic.ch", map[string]any{"wait_s": 5, "domain": "foo.ch", "egress": "proxy-2"})
	resp, body := get(t, f, c, "/api/logs/export?format=text&level=warn")
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), ".log") {
		t.Fatalf("headers: %v", resp.Header)
	}
	line := strings.TrimSpace(body)
	for _, want := range []string{"WARN", "[job 4]", "rdap/throttle", "429 from rdap.nic.ch", "foo.ch", "proxy-2", "wait_s"} {
		if !strings.Contains(line, want) {
			t.Fatalf("text line %q lacks %q", line, want)
		}
	}
	if strings.Count(line, "\n") != 0 {
		t.Fatalf("expected one line, got %q", line)
	}
}

func TestStorageEndpointsReportAndCleanUp(t *testing.T) {
	f, k := persistingFixture(t)
	c := f.login(t)
	var rep housekeeping.Report
	resp := f.do(t, c, "GET", "/api/storage", nil)
	decode(t, resp, &rep)
	if rep.State != "ok" || rep.DBBytes != 1234 || rep.Logs["debug"] != 5 {
		t.Fatalf("report = %+v", rep)
	}
	resp = f.do(t, c, "POST", "/api/storage/cleanup", nil)
	decode(t, resp, &rep)
	if k.runs.Load() != 1 || rep.DeletedLogs != 7 {
		t.Fatalf("runs=%d report=%+v", k.runs.Load(), rep)
	}
	if r := f.do(t, f.client(t), "GET", "/api/storage", nil); r.StatusCode != 401 {
		t.Fatalf("anonymous access = %d, want 401", r.StatusCode)
	}
}
