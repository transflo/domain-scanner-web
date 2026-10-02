// Package logbus is the application log hub: an in-memory ring for the UI, live fan-out to SSE
// subscribers, and batched asynchronous persistence through a sink.
package logbus

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"domain_scanner/internal/store"
)

const (
	subBuffer     = 256
	flushInterval = 300 * time.Millisecond
	flushBatch    = 100
)

type Bus struct {
	mu      sync.Mutex
	ring    []store.LogEntry
	size    int
	next    int64
	subs    map[int]chan store.LogEntry
	subSeq  int
	pending []store.LogEntry
	sink    func([]store.LogEntry)

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// New creates a Bus. sink may be nil; ringSize is the number of recent entries kept in memory.
func New(sink func([]store.LogEntry), ringSize int) *Bus {
	if ringSize < 1 {
		ringSize = 1000
	}
	b := &Bus{size: ringSize, subs: map[int]chan store.LogEntry{}, sink: sink, done: make(chan struct{})}
	if sink != nil {
		b.wg.Add(1)
		go b.flusher()
	}
	return b
}

// SetStartID makes the next entry id n+1 so in-memory ids continue after persisted ones.
func (b *Bus) SetStartID(n int64) {
	b.mu.Lock()
	b.next = n
	b.mu.Unlock()
}

// Log records an entry. It never blocks on subscribers or the sink.
func (b *Bus) Log(level string, jobID int64, format string, args ...any) {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	b.mu.Lock()
	b.next++
	e := store.LogEntry{ID: b.next, JobID: jobID, Level: level, Message: msg, Time: time.Now()}
	if len(b.ring) < b.size {
		b.ring = append(b.ring, e)
	} else {
		copy(b.ring, b.ring[1:])
		b.ring[len(b.ring)-1] = e
	}
	if b.sink != nil {
		b.pending = append(b.pending, e)
	}
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default: // slow subscriber: drop rather than block scanning
		}
	}
	b.mu.Unlock()
}

// Subscribe returns a live channel and an idempotent cancel func that closes it.
func (b *Bus) Subscribe() (<-chan store.LogEntry, func()) {
	ch := make(chan store.LogEntry, subBuffer)
	b.mu.Lock()
	id := b.subSeq
	b.subSeq++
	b.subs[id] = ch
	b.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			close(ch)
			b.mu.Unlock()
		})
	}
}

// Recent returns up to n entries (oldest first) at or above minLevel, optionally for one job.
func (b *Bus) Recent(n int, minLevel string, jobID int64) []store.LogEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []store.LogEntry
	for _, e := range b.ring {
		if minLevel != "" && store.LevelRank(e.Level) < store.LevelRank(minLevel) {
			continue
		}
		if jobID != 0 && e.JobID != jobID {
			continue
		}
		out = append(out, e)
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// Close flushes pending entries to the sink and stops the background flusher.
func (b *Bus) Close() {
	b.closeOnce.Do(func() {
		close(b.done)
		b.wg.Wait()
		b.flush()
	})
}

func (b *Bus) flusher() {
	defer b.wg.Done()
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-b.done:
			return
		case <-t.C:
			b.flush()
		}
	}
}

func (b *Bus) flush() {
	b.mu.Lock()
	batch := b.pending
	b.pending = nil
	b.mu.Unlock()
	for len(batch) > 0 {
		n := len(batch)
		if n > flushBatch*10 {
			n = flushBatch * 10
		}
		b.sink(batch[:n])
		batch = batch[n:]
	}
}

var botTokenRe = regexp.MustCompile(`\d{6,}:[A-Za-z0-9_-]{25,}`)

// RedactSecrets removes the given secrets and anything shaped like a Telegram bot token.
func RedactSecrets(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return botTokenRe.ReplaceAllString(s, "***")
}
