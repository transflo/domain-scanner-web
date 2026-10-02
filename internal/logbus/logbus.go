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

	minRank  int
	minLevel string
	secrets  []string

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

// Fields carries structured attributes. The keys "domain", "egress" and "duration_ms" are
// promoted to first-class columns; everything else stays in the entry's Fields.
type Fields map[string]any

// Log records a free-form line under the "system" component. Prefer Emit/Logger for new code.
func (b *Bus) Log(level string, jobID int64, format string, args ...any) {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	b.Emit(level, "system", "log", jobID, msg, nil)
}

// SetMinLevel drops everything below level (ring, subscribers and sink alike).
func (b *Bus) SetMinLevel(level string) {
	b.mu.Lock()
	b.minRank = store.LevelRank(level)
	b.minLevel = level
	b.mu.Unlock()
}

// MinLevel returns the current minimum level ("debug" by default).
func (b *Bus) MinLevel() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.minLevel == "" {
		return "debug"
	}
	return b.minLevel
}

// SetSecrets registers values that must never appear in logs; they are masked in messages and
// string fields. Empty strings are ignored.
func (b *Bus) SetSecrets(secrets ...string) {
	var keep []string
	for _, s := range secrets {
		if len(s) >= 6 {
			keep = append(keep, s)
		}
	}
	b.mu.Lock()
	b.secrets = keep
	b.mu.Unlock()
}

// Logger is a Bus bound to one component name.
type Logger struct {
	b         *Bus
	component string
}

func (b *Bus) Logger(component string) *Logger { return &Logger{b: b, component: component} }

func (l *Logger) Emit(level, event string, jobID int64, msg string, f Fields) {
	l.b.Emit(level, l.component, event, jobID, msg, f)
}
func (l *Logger) Debug(event string, jobID int64, msg string, f Fields) {
	l.Emit("debug", event, jobID, msg, f)
}
func (l *Logger) Info(event string, jobID int64, msg string, f Fields) {
	l.Emit("info", event, jobID, msg, f)
}
func (l *Logger) Warn(event string, jobID int64, msg string, f Fields) {
	l.Emit("warn", event, jobID, msg, f)
}
func (l *Logger) Error(event string, jobID int64, msg string, f Fields) {
	l.Emit("error", event, jobID, msg, f)
}

func toMillis(v any) (int64, bool) {
	switch d := v.(type) {
	case time.Duration:
		return d.Milliseconds(), true
	case int:
		return int64(d), true
	case int64:
		return d, true
	case float64:
		return int64(d), true
	}
	return 0, false
}

// Emit records a structured entry. It never blocks on subscribers or the sink.
func (b *Bus) Emit(level, component, event string, jobID int64, msg string, f Fields) {
	b.mu.Lock()
	if store.LevelRank(level) < b.minRank {
		b.mu.Unlock()
		return
	}
	secrets := b.secrets
	b.mu.Unlock()

	clean := func(s string) string { return RedactSecrets(s, secrets...) }
	e := store.LogEntry{JobID: jobID, Level: level, Component: component, Event: event, Message: clean(msg), Time: time.Now()}
	if len(f) > 0 {
		extra := make(map[string]any, len(f))
		for k, v := range f {
			switch k {
			case "domain":
				e.Domain, _ = v.(string)
			case "egress":
				e.Egress, _ = v.(string)
			case "duration_ms":
				if n, ok := toMillis(v); ok {
					e.DurationMS = n
				}
			default:
				if s, ok := v.(string); ok {
					v = clean(s)
				}
				extra[k] = v
			}
		}
		if len(extra) > 0 {
			e.Fields = extra
		}
	}
	b.mu.Lock()
	b.next++
	e.ID = b.next
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
