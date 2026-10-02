package egress

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// ProxyID is the egress id of the outbound with database id n.
func ProxyID(n int64) string { return fmt.Sprintf("proxy-%d", n) }

// ProxyEntry describes one proxy egress to the Registry.
type ProxyEntry struct {
	ID      string
	Name    string
	Addr    string // local SOCKS5 listener
	Enabled bool
}

// Status is a read-only view of one egress (for the UI and diagnostics).
type Status struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Direct         bool      `json:"direct"`
	Enabled        bool      `json:"enabled"`
	Healthy        bool      `json:"healthy"`
	Tested         bool      `json:"tested"`
	Strikes        int       `json:"strikes"`
	PenalizedUntil time.Time `json:"penalized_until,omitempty"`
}

type item struct {
	eg      *Egress
	enabled bool
	healthy bool
	tested  bool
	strikes int
	until   time.Time
}

// Registry knows every egress, whether it is healthy, and which ones are cooling down after
// misbehaving. It is shared by all jobs, so a registry that is rate limited on one egress makes
// every job steer around it.
type Registry struct {
	mu       sync.Mutex
	now      func() time.Time
	items    map[string]*item
	baseCool time.Duration
	maxCool  time.Duration
}

func NewRegistry() *Registry {
	r := &Registry{now: time.Now, items: map[string]*item{}, baseCool: 5 * time.Minute, maxCool: 30 * time.Minute}
	r.items[DirectID] = &item{eg: Direct(), enabled: true, healthy: true, tested: true}
	return r
}

// SetClock replaces the time source (tests drive cooldowns with a fake clock).
func (r *Registry) SetClock(now func() time.Time) {
	r.mu.Lock()
	r.now = now
	r.mu.Unlock()
}

// SetCooldown configures the first and the maximum penalty duration.
func (r *Registry) SetCooldown(base, max time.Duration) {
	r.mu.Lock()
	r.baseCool, r.maxCool = base, max
	r.mu.Unlock()
}

// Replace makes the set of proxies equal to entries. Existing proxies with an unchanged address
// keep their Egress object (pooled connections), health and penalty.
func (r *Registry) Replace(entries []ProxyEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	keep := map[string]bool{DirectID: true}
	for _, e := range entries {
		keep[e.ID] = true
		it, ok := r.items[e.ID]
		if ok && it.eg.SocksAddr == e.Addr {
			it.eg.Name = e.Name
			it.enabled = e.Enabled
			continue
		}
		r.items[e.ID] = &item{eg: Socks(e.ID, e.Name, e.Addr), enabled: e.Enabled}
	}
	for id := range r.items {
		if !keep[id] {
			delete(r.items, id)
		}
	}
}

func (r *Registry) Get(id string) (*Egress, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	it, ok := r.items[id]
	if !ok {
		return nil, false
	}
	return it.eg, true
}

// SetHealth records the outcome of the latest liveness test.
func (r *Registry) SetHealth(id string, healthy bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if it, ok := r.items[id]; ok {
		it.healthy, it.tested = healthy, true
	}
}

// Penalize puts id into cooldown and returns how long. Repeated penalties escalate (base,
// 2×base, … up to the maximum) until Recover is called.
func (r *Registry) Penalize(id string) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	it, ok := r.items[id]
	if !ok {
		return 0
	}
	d := r.baseCool
	for i := 0; i < it.strikes && d < r.maxCool; i++ {
		d *= 2
	}
	if d > r.maxCool {
		d = r.maxCool
	}
	it.strikes++
	it.until = r.now().Add(d)
	return d
}

// Recover clears the strike count (the egress proved healthy again).
func (r *Registry) Recover(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if it, ok := r.items[id]; ok {
		it.strikes = 0
		it.until = time.Time{}
	}
}

// Penalized reports whether id is cooling down and until when.
func (r *Registry) Penalized(id string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	it, ok := r.items[id]
	if !ok || !r.now().Before(it.until) {
		return time.Time{}, false
	}
	return it.until, true
}

// Candidates lists egress ids that may be switched to: enabled, healthy and not cooling down,
// in id order (direct first when included), minus the excluded ids.
func (r *Registry) Candidates(includeDirect bool, exclude ...string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	skip := map[string]bool{}
	for _, e := range exclude {
		skip[e] = true
	}
	var out []string
	for id, it := range r.items {
		if skip[id] || !it.enabled || !it.healthy || r.now().Before(it.until) {
			continue
		}
		if id == DirectID && !includeDirect {
			continue
		}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i] == DirectID || out[j] == DirectID {
			return out[i] == DirectID
		}
		return out[i] < out[j]
	})
	return out
}

// Snapshot returns every egress, direct first, then proxies in id order.
func (r *Registry) Snapshot() []Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Status, 0, len(r.items))
	for id, it := range r.items {
		s := Status{ID: id, Name: it.eg.Name, Direct: it.eg.IsDirect(), Enabled: it.enabled,
			Healthy: it.healthy, Tested: it.tested, Strikes: it.strikes}
		if r.now().Before(it.until) {
			s.PenalizedUntil = it.until
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID == DirectID || out[j].ID == DirectID {
			return out[i].ID == DirectID
		}
		return out[i].ID < out[j].ID
	})
	return out
}
