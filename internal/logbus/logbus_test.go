package logbus

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"domain_scanner/internal/store"
)

func TestRingKeepsNewest(t *testing.T) {
	b := New(nil, 3)
	defer b.Close()
	for i := 0; i < 5; i++ {
		b.Log("info", 0, "m%d", i)
	}
	got := b.Recent(10, "", 0)
	if len(got) != 3 || got[0].Message != "m2" || got[2].Message != "m4" {
		t.Fatalf("ring = %+v, want m2..m4 oldest-first", got)
	}
}

func TestIDsAreMonotonicFromStartID(t *testing.T) {
	b := New(nil, 10)
	defer b.Close()
	b.SetStartID(100)
	b.Log("info", 0, "a")
	b.Log("info", 0, "b")
	got := b.Recent(10, "", 0)
	if got[0].ID != 101 || got[1].ID != 102 {
		t.Fatalf("ids = %d,%d want 101,102", got[0].ID, got[1].ID)
	}
}

func TestSubscribeReceivesAndCancelCloses(t *testing.T) {
	b := New(nil, 10)
	defer b.Close()
	ch, cancel := b.Subscribe()
	b.Log("warn", 7, "hello %s", "world")
	select {
	case e := <-ch:
		if e.Message != "hello world" || e.Level != "warn" || e.JobID != 7 {
			t.Fatalf("got %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no event received")
	}
	cancel()
	cancel() // idempotent
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after cancel")
	}
	b.Log("info", 0, "after cancel") // must not panic
}

func TestSlowSubscriberNeverBlocksLogging(t *testing.T) {
	b := New(nil, 10)
	defer b.Close()
	_, cancel := b.Subscribe() // never read
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ {
			b.Log("info", 0, "x%d", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Log blocked on a slow subscriber")
	}
}

func TestRecentFilters(t *testing.T) {
	b := New(nil, 50)
	defer b.Close()
	b.Log("debug", 1, "d")
	b.Log("info", 1, "i1")
	b.Log("info", 2, "i2")
	b.Log("error", 2, "e2")
	if got := b.Recent(50, "info", 0); len(got) != 3 {
		t.Fatalf("info+ = %d, want 3", len(got))
	}
	if got := b.Recent(50, "", 2); len(got) != 2 {
		t.Fatalf("job 2 = %d, want 2", len(got))
	}
	if got := b.Recent(1, "", 0); len(got) != 1 || got[0].Message != "e2" {
		t.Fatalf("limit keeps newest: %+v", got)
	}
}

func TestSinkReceivesEverythingOnClose(t *testing.T) {
	var mu sync.Mutex
	var seen []store.LogEntry
	b := New(func(es []store.LogEntry) {
		mu.Lock()
		seen = append(seen, es...)
		mu.Unlock()
	}, 10)
	for i := 0; i < 250; i++ {
		b.Log("info", 0, "n%d", i)
	}
	b.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 250 {
		t.Fatalf("sink saw %d, want 250", len(seen))
	}
	for i, e := range seen {
		if e.Message != fmt.Sprintf("n%d", i) {
			t.Fatalf("order broken at %d: %q", i, e.Message)
		}
	}
}

func TestRedactSecrets(t *testing.T) {
	in := "POST https://api.telegram.org/bot123456789:AAHdummyDUMMYdummyDUMMYdummyDUMMY12345/sendMessage failed; secret=hunter22"
	out := RedactSecrets(in, "hunter22")
	if strings.Contains(out, "AAHdummy") || strings.Contains(out, "hunter22") {
		t.Fatalf("not redacted: %s", out)
	}
	if RedactSecrets("keep me", "", "x") != "keep me" {
		t.Fatalf("empty secret must be ignored")
	}
}

func TestEmitExtractsFirstClassFields(t *testing.T) {
	b := New(nil, 10)
	defer b.Close()
	b.Emit("debug", "rdap", "request", 7, "GET ok", Fields{
		"domain": "foo.com", "egress": "proxy-3", "duration_ms": 125, "status": 404,
	})
	got := b.Recent(5, "", 0)
	if len(got) != 1 {
		t.Fatalf("got %d entries", len(got))
	}
	e := got[0]
	if e.Component != "rdap" || e.Event != "request" || e.JobID != 7 || e.Domain != "foo.com" ||
		e.Egress != "proxy-3" || e.DurationMS != 125 {
		t.Fatalf("first-class fields not extracted: %+v", e)
	}
	if _, dup := e.Fields["domain"]; dup {
		t.Fatalf("domain must not be duplicated inside Fields: %v", e.Fields)
	}
	if e.Fields["status"] != 404 {
		t.Fatalf("extra fields lost: %v", e.Fields)
	}
}

func TestDurationFieldAcceptsTimeDuration(t *testing.T) {
	b := New(nil, 10)
	defer b.Close()
	b.Emit("info", "check", "done", 0, "x", Fields{"duration_ms": 1500 * time.Millisecond})
	if got := b.Recent(1, "", 0)[0].DurationMS; got != 1500 {
		t.Fatalf("duration = %d, want 1500", got)
	}
}

func TestLoggerSetsComponent(t *testing.T) {
	b := New(nil, 10)
	defer b.Close()
	lg := b.Logger("egress")
	lg.Warn("cooldown", 2, "proxy-1 cooling down", Fields{"seconds": 300})
	e := b.Recent(1, "", 0)[0]
	if e.Component != "egress" || e.Event != "cooldown" || e.Level != "warn" || e.JobID != 2 {
		t.Fatalf("logger entry = %+v", e)
	}
}

func TestMinLevelDropsLowerLevelsEverywhere(t *testing.T) {
	var mu sync.Mutex
	var persisted []store.LogEntry
	b := New(func(es []store.LogEntry) { mu.Lock(); persisted = append(persisted, es...); mu.Unlock() }, 10)
	ch, cancel := b.Subscribe()
	defer cancel()
	b.SetMinLevel("info")
	b.Emit("debug", "check", "step", 0, "noise", nil)
	b.Emit("info", "check", "done", 0, "kept", nil)
	b.Close()
	if got := b.Recent(10, "", 0); len(got) != 1 || got[0].Message != "kept" {
		t.Fatalf("ring = %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(persisted) != 1 {
		t.Fatalf("sink got %d entries, want 1", len(persisted))
	}
	select {
	case e := <-ch:
		if e.Message != "kept" {
			t.Fatalf("subscriber got %q", e.Message)
		}
	default:
		t.Fatal("subscriber got nothing")
	}
	if b.MinLevel() != "info" {
		t.Fatalf("MinLevel = %q", b.MinLevel())
	}
}

func TestSecretsAreRedactedInMessageAndFields(t *testing.T) {
	b := New(nil, 10)
	defer b.Close()
	b.SetSecrets("cfat_supersecrettoken", "")
	b.Emit("info", "cloudflare", "auth", 0, "using token cfat_supersecrettoken now", Fields{
		"header": "Bearer cfat_supersecrettoken", "n": 3,
	})
	e := b.Recent(1, "", 0)[0]
	if strings.Contains(e.Message, "supersecret") || strings.Contains(e.Fields["header"].(string), "supersecret") {
		t.Fatalf("secret leaked: %+v", e)
	}
	if e.Fields["n"] != 3 {
		t.Fatalf("non-string field mangled: %v", e.Fields)
	}
}

func TestConcurrentUseIsRaceFree(t *testing.T) {
	b := New(func([]store.LogEntry) {}, 100)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, cancel := b.Subscribe()
			for j := 0; j < 200; j++ {
				b.Log("info", 0, "x")
				select {
				case <-ch:
				default:
				}
				b.Recent(5, "", 0)
			}
			cancel()
		}()
	}
	wg.Wait()
	b.Close()
}
