package main

import (
	"context"
	"sync"

	"domain_scanner/internal/domain"
	"domain_scanner/internal/egress"
	"domain_scanner/internal/scheduler"
)

// checkerPool gives every egress its own domain.Checker. RDAP rate limits are per source
// address, so throttle state, pooled connections and WHOIS dialers must not be shared between
// the direct connection and the proxies.
type checkerPool struct {
	reg    *egress.Registry
	extra  map[string]string
	notice func(format string, args ...any)

	mu sync.Mutex
	by map[*egress.Egress]*domain.Checker
}

func newCheckerPool(reg *egress.Registry, extraRDAP map[string]string, notice func(format string, args ...any)) *checkerPool {
	return &checkerPool{reg: reg, extra: extraRDAP, notice: notice, by: map[*egress.Egress]*domain.Checker{}}
}

func (p *checkerPool) forEgress(eg *egress.Egress) *domain.Checker {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.by[eg]; ok {
		return c
	}
	c := domain.NewChecker(p.extra, p.notice, eg)
	p.by[eg] = c
	// forget checkers of egresses the registry no longer knows
	for old := range p.by {
		if got, ok := p.reg.Get(old.ID); !ok || got != old {
			delete(p.by, old)
		}
	}
	return c
}

// Check implements scheduler.Checker.
func (p *checkerPool) Check(ctx context.Context, d string, o scheduler.CheckOpts) domain.Verdict {
	eg, ok := p.reg.Get(o.Egress)
	if !ok {
		return domain.Verdict{Domain: d, Status: domain.StatusUnknown, ErrKind: domain.ErrInternal,
			Reason: "egress " + o.Egress + " is not available (proxy removed or disabled?)"}
	}
	cc := *p.forEgress(eg) // copy so the per-job UseReserved flag does not leak between jobs
	cc.UseReserved = o.UseReserved
	return cc.Check(ctx, d)
}
