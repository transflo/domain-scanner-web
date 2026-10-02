package notifier

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"domain_scanner/internal/logbus"
	"domain_scanner/internal/store"
)

const testToken = "123456789:AAHdummyDUMMYdummyDUMMYdummyDUMMY12345"

type tgMsg struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

type fakeTG struct {
	srv      *httptest.Server
	msgs     chan tgMsg
	requests atomic.Int32
	// failFirst makes the first N requests return HTTP 500.
	failFirst atomic.Int32
	failAll   atomic.Bool
	badChat   atomic.Bool
}

func newFakeTG(t *testing.T) *fakeTG {
	f := &fakeTG{msgs: make(chan tgMsg, 100)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.URL.Path != "/bot"+testToken+"/sendMessage" {
			http.NotFound(w, r)
			return
		}
		if f.failAll.Load() || f.failFirst.Add(-1) >= 0 {
			http.Error(w, "boom", 500)
			return
		}
		if f.badChat.Load() {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`)
			return
		}
		var m tgMsg
		json.NewDecoder(r.Body).Decode(&m)
		f.msgs <- m
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTG) wait(t *testing.T, d time.Duration) tgMsg {
	t.Helper()
	select {
	case m := <-f.msgs:
		return m
	case <-time.After(d):
		t.Fatal("timed out waiting for telegram message")
		return tgMsg{}
	}
}

func (f *fakeTG) expectNone(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m := <-f.msgs:
		t.Fatalf("unexpected message: %q", m.Text)
	case <-time.After(d):
	}
}

type logs struct {
	mu sync.Mutex
	es []store.LogEntry
}

func (l *logs) sink(es []store.LogEntry) { l.mu.Lock(); l.es = append(l.es, es...); l.mu.Unlock() }
func (l *logs) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var sb strings.Builder
	for _, e := range l.es {
		sb.WriteString(e.Level + ": " + e.Message + "\n")
	}
	return sb.String()
}

func setup(t *testing.T, f *fakeTG, opts Options, cfg Config) (*Notifier, *logs) {
	t.Helper()
	l := &logs{}
	bus := logbus.New(l.sink, 100)
	opts.BaseURL = f.srv.URL
	if opts.Backoff == 0 {
		opts.Backoff = time.Millisecond
	}
	n := New(func() Config { return cfg }, bus, opts)
	n.Start(context.Background())
	t.Cleanup(func() { n.Stop(); bus.Close() })
	return n, l
}

func domains(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("d%03d.li", i)
	}
	return out
}

func TestFullBatchSendsImmediately(t *testing.T) {
	f := newFakeTG(t)
	n, _ := setup(t, f, Options{FlushEvery: time.Hour, MaxBatch: 20}, Config{testToken, "42"})
	n.Notify("job1", domains(20))
	m := f.wait(t, 2*time.Second)
	if m.ChatID != "42" || m.ParseMode != "HTML" || !strings.Contains(m.Text, "d000.li") || !strings.Contains(m.Text, "d019.li") {
		t.Fatalf("bad message: %+v", m)
	}
}

func TestPartialBatchSendsOnInterval(t *testing.T) {
	f := newFakeTG(t)
	n, _ := setup(t, f, Options{FlushEvery: 50 * time.Millisecond, MaxBatch: 20}, Config{testToken, "42"})
	n.Notify("job1", []string{"a.li"})
	m := f.wait(t, 2*time.Second)
	if !strings.Contains(m.Text, "a.li") {
		t.Fatalf("missing domain: %q", m.Text)
	}
}

func TestTwoJobsAreSeparateMessages(t *testing.T) {
	f := newFakeTG(t)
	n, _ := setup(t, f, Options{FlushEvery: 50 * time.Millisecond, MaxBatch: 100}, Config{testToken, "42"})
	n.Notify("alpha", []string{"a.li"})
	n.Notify("beta", []string{"b.li"})
	m1, m2 := f.wait(t, 2*time.Second), f.wait(t, 2*time.Second)
	all := m1.Text + "|" + m2.Text
	if !strings.Contains(all, "alpha") || !strings.Contains(all, "beta") || strings.Contains(m1.Text, "alpha") && strings.Contains(m1.Text, "beta") {
		t.Fatalf("jobs not split: %q", all)
	}
}

func TestHTMLIsEscaped(t *testing.T) {
	f := newFakeTG(t)
	n, _ := setup(t, f, Options{FlushEvery: 50 * time.Millisecond, MaxBatch: 100}, Config{testToken, "42"})
	n.Notify("a<b>&", []string{"<script>&.li"})
	m := f.wait(t, 2*time.Second)
	if strings.Contains(m.Text, "<script>") || strings.Contains(m.Text, "a<b>") || !strings.Contains(m.Text, "&lt;script&gt;&amp;.li") {
		t.Fatalf("not escaped: %q", m.Text)
	}
}

func TestLongMessageIsSplit(t *testing.T) {
	f := newFakeTG(t)
	n, _ := setup(t, f, Options{FlushEvery: 50 * time.Millisecond, MaxBatch: 100000}, Config{testToken, "42"})
	ds := make([]string, 400)
	for i := range ds {
		ds[i] = fmt.Sprintf("very-long-candidate-name-%04d.example", i)
	}
	n.Notify("big", ds)
	var combined strings.Builder
	count := 0
	deadline := time.After(3 * time.Second)
loop:
	for {
		select {
		case m := <-f.msgs:
			if len([]rune(m.Text)) > 4096 {
				t.Fatalf("message of %d chars exceeds 4096", len([]rune(m.Text)))
			}
			combined.WriteString(m.Text)
			count++
			if strings.Contains(combined.String(), "0399.example") {
				break loop
			}
		case <-deadline:
			t.Fatalf("did not receive all parts (got %d messages)", count)
		}
	}
	if count < 2 {
		t.Fatalf("expected the long text to be split, got %d message(s)", count)
	}
	for i := range ds {
		if !strings.Contains(combined.String(), ds[i]) {
			t.Fatalf("domain %s lost while splitting", ds[i])
		}
	}
}

func TestRetriesThenSucceeds(t *testing.T) {
	f := newFakeTG(t)
	f.failFirst.Store(2)
	n, l := setup(t, f, Options{FlushEvery: 30 * time.Millisecond, MaxBatch: 100}, Config{testToken, "42"})
	n.Notify("j", []string{"a.li"})
	f.wait(t, 2*time.Second)
	if got := f.requests.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
	if strings.Contains(l.text(), "error:") {
		t.Fatalf("no error expected after eventual success:\n%s", l.text())
	}
}

func TestGivesUpAfterThreeAttemptsWithoutLeakingToken(t *testing.T) {
	f := newFakeTG(t)
	f.failAll.Store(true)
	n, l := setup(t, f, Options{FlushEvery: 30 * time.Millisecond, MaxBatch: 100}, Config{testToken, "42"})
	n.Notify("j", []string{"a.li"})
	time.Sleep(400 * time.Millisecond)
	n.Stop()
	if got := f.requests.Load(); got != 3 {
		t.Fatalf("requests = %d, want exactly 3 attempts", got)
	}
	time.Sleep(400 * time.Millisecond) // let the bus flush
	txt := l.text()
	if !strings.Contains(txt, "error:") {
		t.Fatalf("expected an error log, got:\n%s", txt)
	}
	if strings.Contains(txt, "AAHdummy") || strings.Contains(txt, testToken) {
		t.Fatalf("token leaked into logs:\n%s", txt)
	}
	if strings.Contains(txt, "已推送") {
		t.Fatalf("a failed delivery must not be logged as pushed:\n%s", txt)
	}
}

func TestUnconfiguredSendsNothing(t *testing.T) {
	f := newFakeTG(t)
	n, l := setup(t, f, Options{FlushEvery: 30 * time.Millisecond, MaxBatch: 100}, Config{})
	n.Notify("j", []string{"a.li"})
	f.expectNone(t, 300*time.Millisecond)
	if f.requests.Load() != 0 {
		t.Fatal("request made without configuration")
	}
	_ = l
}

func TestSendTest(t *testing.T) {
	f := newFakeTG(t)
	n, _ := setup(t, f, Options{FlushEvery: time.Hour}, Config{testToken, "42"})
	if err := n.SendTest(context.Background()); err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	f.wait(t, time.Second)

	f.badChat.Store(true)
	err := n.SendTest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("err = %v, want chat-not-found description", err)
	}
	if strings.Contains(err.Error(), "AAHdummy") {
		t.Fatalf("token leaked in error: %v", err)
	}

	n2, _ := setup(t, f, Options{FlushEvery: time.Hour}, Config{})
	if err := n2.SendTest(context.Background()); err == nil {
		t.Fatal("unconfigured SendTest must fail")
	}
}

func TestStopFlushesPending(t *testing.T) {
	f := newFakeTG(t)
	n, _ := setup(t, f, Options{FlushEvery: time.Hour, MaxBatch: 100}, Config{testToken, "42"})
	n.Notify("j", []string{"left-over.li"})
	n.Stop()
	m := f.wait(t, time.Second)
	if !strings.Contains(m.Text, "left-over.li") {
		t.Fatalf("pending not flushed: %q", m.Text)
	}
}
