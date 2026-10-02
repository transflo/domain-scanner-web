package domain

import (
	"context"
	"sync"
	"time"
)

// Step is one observable action inside a check (a DNS lookup, an RDAP request, a WHOIS attempt,
// the TLS probe, the final verdict). Steps make every check explainable after the fact.
type Step struct {
	Name       string `json:"name"`
	Detail     string `json:"detail"`
	DurationMS int64  `json:"duration_ms"`
	OK         bool   `json:"ok"`
}

// ErrKind classifies why a check ended as StatusUnknown. The scheduler uses it to decide when
// an egress is misbehaving (rate limits, timeouts) as opposed to merely returning odd answers.
type ErrKind string

const (
	ErrNone         ErrKind = ""
	ErrRateLimited  ErrKind = "rate_limited"
	ErrTimeout      ErrKind = "timeout"
	ErrNetwork      ErrKind = "network"
	ErrUnrecognised ErrKind = "unrecognised"
	ErrCancelled    ErrKind = "cancelled"
	ErrInternal     ErrKind = "internal"
)

// Failure reports whether the kind says the path to the registry is unhealthy.
func (k ErrKind) Failure() bool {
	return k == ErrRateLimited || k == ErrTimeout || k == ErrNetwork
}

type tracer struct {
	mu    sync.Mutex
	steps []Step
	fn    func(Step)
}

func (t *tracer) add(s Step) {
	t.mu.Lock()
	t.steps = append(t.steps, s)
	fn := t.fn
	t.mu.Unlock()
	if fn != nil {
		fn(s)
	}
}

func (t *tracer) snapshot() []Step {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Step(nil), t.steps...)
}

type ctxKey int

const (
	traceKey ctxKey = iota
	maxWaitKey
)

// WithTrace returns a context whose checks report each step to fn as it happens.
func WithTrace(ctx context.Context, fn func(Step)) context.Context {
	return context.WithValue(ctx, traceKey, &tracer{fn: fn})
}

func traceFrom(ctx context.Context) *tracer {
	t, _ := ctx.Value(traceKey).(*tracer)
	return t
}

// step records one step on the tracer carried by ctx (no-op without one).
func step(ctx context.Context, name, detail string, start time.Time, ok bool) {
	if t := traceFrom(ctx); t != nil {
		t.add(Step{Name: name, Detail: detail, DurationMS: time.Since(start).Milliseconds(), OK: ok})
	}
}

// WithRDAPMaxWait overrides, for checks made with the returned context, how long an RDAP lookup
// keeps waiting out rate limits before giving up.
func WithRDAPMaxWait(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, maxWaitKey, d)
}
