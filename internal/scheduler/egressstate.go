package scheduler

import (
	"context"
	"sync"
	"time"

	"domain_scanner/internal/egress"
)

// healthConfig tunes the per-job error-storm detector.
type healthConfig struct {
	Window       int           // number of most recent checks looked at
	MinSamples   int           // never trigger on fewer samples than this
	Threshold    float64       // failure ratio that counts as a storm
	BackoffBase  time.Duration // first pause; doubles per consecutive storm
	BackoffMax   time.Duration
	HealthyReset time.Duration // this long without a storm resets the backoff
	ReturnAfter  time.Duration // a failed-over job tries its preferred egress again after this
}

func defaultHealthConfig() healthConfig {
	return healthConfig{Window: 20, MinSamples: 10, Threshold: 0.6, BackoffBase: 30 * time.Second,
		BackoffMax: 10 * time.Minute, HealthyReset: 2 * time.Minute, ReturnAfter: 5 * time.Minute}
}

// decision describes what the detector did when it saw a storm.
type decision struct {
	From, To    string
	Switched    bool
	Backoff     time.Duration
	Penalty     time.Duration
	FailureRate float64
	Samples     int
}

// egressState owns one job's choice of egress and its health window. All workers of the job
// report each check's outcome through Record; when most recent checks failed it
//  1. pauses every worker (BlockedUntil, exponential backoff),
//  2. puts the failing egress into a cooldown shared by all jobs, and
//  3. if failover is on, moves the job to another healthy egress.
type egressState struct {
	mu  sync.Mutex
	now func() time.Time
	cfg healthConfig
	reg *egress.Registry

	mode      string // direct | proxy | pool
	preferred string // egress the job was configured for ("" for pool)
	failover  bool
	current   string

	win   []bool // ring of recent outcomes, true = failure
	pos   int
	count int

	k            int // consecutive storm count (backoff exponent)
	blockedUntil time.Time
	lastTrouble  time.Time
	switchedAt   time.Time
}

func newEgressState(reg *egress.Registry, cfg healthConfig, mode string, proxyID int64, failover bool) *egressState {
	s := &egressState{now: time.Now, cfg: cfg, reg: reg, mode: mode, failover: failover, win: make([]bool, cfg.Window)}
	switch mode {
	case "proxy":
		s.preferred = egress.ProxyID(proxyID)
		s.current = s.preferred
	case "pool":
		if c := reg.Candidates(false); len(c) > 0 {
			s.current = c[0]
		} else {
			s.current = egress.DirectID
		}
	default:
		s.mode = "direct"
		s.preferred = egress.DirectID
		s.current = egress.DirectID
	}
	return s
}

// Current is the egress id the next check should use.
func (s *egressState) Current() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// BlockedUntil is the time before which workers must not start new checks.
func (s *egressState) BlockedUntil() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blockedUntil
}

// Gate blocks until any active backoff pause is over (or ctx ends).
func (s *egressState) Gate(ctx context.Context) error {
	for {
		until := s.BlockedUntil()
		d := until.Sub(s.now())
		if d <= 0 {
			return ctx.Err()
		}
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
}

// FailoverAvailable reports whether a storm right now could be answered by a switch. It lets the
// scheduler shorten RDAP rate-limit waits so a bad egress shows up quickly.
func (s *egressState) FailoverAvailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failover && len(s.candidatesLocked()) > 0
}

func (s *egressState) candidatesLocked() []string {
	// direct is a legitimate target only for jobs that were set up on direct
	return s.reg.Candidates(s.mode == "direct", s.current)
}

func (s *egressState) resetWindowLocked() {
	for i := range s.win {
		s.win[i] = false
	}
	s.pos, s.count = 0, 0
}

// Record reports one finished check (failure = the egress looked unhealthy: rate limit, timeout,
// network error). It returns a decision when this outcome completed a storm.
func (s *egressState) Record(failure bool) *decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()

	s.win[s.pos] = failure
	s.pos = (s.pos + 1) % len(s.win)
	if s.count < len(s.win) {
		s.count++
	}
	fails := 0
	for i := 0; i < s.count; i++ {
		if s.win[i] {
			fails++
		}
	}
	rate := float64(fails) / float64(s.count)

	if s.count < s.cfg.MinSamples || rate < s.cfg.Threshold {
		if !failure && s.k > 0 && now.Sub(s.lastTrouble) >= s.cfg.HealthyReset {
			s.k = 0
			s.reg.Recover(s.current)
		}
		return nil
	}

	d := &decision{From: s.current, To: s.current, FailureRate: rate, Samples: s.count}
	d.Backoff = s.cfg.BackoffBase
	for i := 0; i < s.k && d.Backoff < s.cfg.BackoffMax; i++ {
		d.Backoff *= 2
	}
	if d.Backoff > s.cfg.BackoffMax {
		d.Backoff = s.cfg.BackoffMax
	}
	s.k++
	s.blockedUntil = now.Add(d.Backoff)
	s.lastTrouble = now
	d.Penalty = s.reg.Penalize(s.current)

	if s.failover {
		if c := s.candidatesLocked(); len(c) > 0 {
			d.To, d.Switched = c[0], true
			s.current = d.To
			s.switchedAt = now
		}
	}
	s.resetWindowLocked()
	return d
}

// Steer moves the job off its current egress when ANOTHER job has already put that egress into
// cooldown, so one job's discovery protects all the others without each having to be hit by its
// own storm. It needs failover and a healthy alternative; it applies no backoff.
func (s *egressState) Steer() (from, to string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.failover {
		return "", "", false
	}
	if _, cooling := s.reg.Penalized(s.current); !cooling {
		return "", "", false
	}
	c := s.candidatesLocked()
	if len(c) == 0 {
		return "", "", false
	}
	from, to = s.current, c[0]
	s.current = to
	s.switchedAt = s.now()
	s.resetWindowLocked()
	return from, to, true
}

// MaybeReturn moves a failed-over job back to its preferred egress once that egress has cooled
// down, so a temporary rate limit does not pin the job to a proxy forever.
func (s *egressState) MaybeReturn() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.failover || s.preferred == "" || s.current == s.preferred ||
		s.now().Sub(s.switchedAt) < s.cfg.ReturnAfter {
		return "", false
	}
	if _, cooling := s.reg.Penalized(s.preferred); cooling {
		return "", false
	}
	if s.preferred != egress.DirectID {
		ok := false
		for _, id := range s.reg.Candidates(false) {
			if id == s.preferred {
				ok = true
			}
		}
		if !ok {
			return "", false
		}
	}
	s.current = s.preferred
	s.resetWindowLocked()
	return s.current, true
}
