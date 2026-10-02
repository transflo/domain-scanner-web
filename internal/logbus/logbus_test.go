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
