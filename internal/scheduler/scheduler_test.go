package scheduler

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"domain_scanner/internal/domain"
	"domain_scanner/internal/egress"
	"domain_scanner/internal/logbus"
	"domain_scanner/internal/store"
)

// ---- fakes ----

// fakeChecker: labels with an even numeric value are available, multiples of 7 are registered,
// everything else is registered too. A label listed in unknown is "unknown"; one in panicOn panics.
type fakeChecker struct {
	mu      sync.Mutex
	calls   map[string]int
	total   atomic.Int32
	cur     atomic.Int32
	maxConc atomic.Int32

	blockAfter int32 // when > 0, calls beyond this many block until release is closed
	release    chan struct{}
	unknown    map[string]bool
	panicOn    string
	reserved   atomic.Bool // records the useReserved flag seen

	egressCalls map[string]int  // egress id -> number of checks routed there
	failEgress  map[string]bool // egress ids that answer every check with a rate limit
	lastOpts    atomic.Value    // CheckOpts of the latest call
}

func newFake() *fakeChecker {
	return &fakeChecker{calls: map[string]int{}, release: make(chan struct{}), unknown: map[string]bool{},
		egressCalls: map[string]int{}, failEgress: map[string]bool{}}
}

func (f *fakeChecker) egressCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.egressCalls[id]
}

func (f *fakeChecker) Check(ctx context.Context, d string, o CheckOpts) domain.Verdict {
	n := f.total.Add(1)
	c := f.cur.Add(1)
	defer f.cur.Add(-1)
	for {
		m := f.maxConc.Load()
		if c <= m || f.maxConc.CompareAndSwap(m, c) {
			break
		}
	}
	f.reserved.Store(o.UseReserved)
	f.lastOpts.Store(o)
	f.mu.Lock()
	f.calls[d]++
	f.egressCalls[o.Egress]++
	failing := f.failEgress[o.Egress]
	f.mu.Unlock()
	if f.blockAfter > 0 && n > f.blockAfter {
		<-f.release
	}
	if failing {
		return domain.Verdict{Domain: d, Status: domain.StatusUnknown, Reason: "429", ErrKind: domain.ErrRateLimited}
	}
	label := strings.SplitN(d, ".", 2)[0]
	if label == f.panicOn {
		panic("boom in checker")
	}
	if f.unknown[label] {
		return domain.Verdict{Domain: d, Status: domain.StatusUnknown, Reason: "rate limited"}
	}
	if v, err := strconv.Atoi(label); err == nil && v%2 == 0 {
		return domain.Verdict{Domain: d, Status: domain.StatusAvailable}
	}
	return domain.Verdict{Domain: d, Status: domain.StatusRegistered, Signatures: []string{"DNS_NS"}}
}

type fakeNotifier struct {
	mu   sync.Mutex
	seen map[string]int
}

func (n *fakeNotifier) Notify(job string, ds []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.seen == nil {
		n.seen = map[string]int{}
	}
	for _, d := range ds {
		n.seen[d]++
	}
}

func (n *fakeNotifier) count() (distinct, dups int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, c := range n.seen {
		distinct++
		if c > 1 {
			dups++
		}
	}
	return
}

type fakeWords map[string][]string

func (w fakeWords) Words(id string) ([]string, error) {
	if v, ok := w[id]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("wordlist %q not found", id)
}

type env struct {
	st  *store.Store
	bus *logbus.Bus
	nf  *fakeNotifier
	ck  *fakeChecker
	reg *egress.Registry
	s   *Scheduler
	dir string
}

func newEnv(t *testing.T, opts Options) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{st: st, bus: logbus.New(nil, 500), nf: &fakeNotifier{}, ck: newFake(), dir: dir, reg: egress.NewRegistry()}
	e.s = e.newScheduler(opts)
	t.Cleanup(func() {
		close2(e.ck.release)
		e.s.Shutdown(context.Background())
		e.bus.Close()
		st.Close()
	})
	return e
}

func close2(ch chan struct{}) {
	defer func() { recover() }()
	close(ch)
}

func (e *env) newScheduler(opts Options) *Scheduler {
	if opts.SaveEvery == 0 {
		opts.SaveEvery = 10 * time.Millisecond
	}
	opts.Registry = e.reg
	if opts.Health.Window == 0 { // fast, small windows so storms are easy to provoke
		opts.Health = healthConfig{Window: 10, MinSamples: 5, Threshold: 0.6, BackoffBase: 5 * time.Millisecond,
			BackoffMax: 20 * time.Millisecond, HealthyReset: 50 * time.Millisecond, ReturnAfter: time.Hour}
	}
	return New(e.st, e.bus, e.nf, e.ck, fakeWords{"builtin:tiny": {"alpha", "beta", "gamma"}, "builtin:empty": {}}, opts)
}

func (e *env) start(t *testing.T) {
	t.Helper()
	if err := e.s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func digits2(workers int) Params {
	return Params{Name: "t", Suffix: ".li", Pattern: "d", Length: 2, Workers: workers}
}

func (e *env) waitStatus(t *testing.T, id int64, want string) *store.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, err := e.st.GetJob(context.Background(), id)
		if err == nil && j.Status == want {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	j, _ := e.st.GetJob(context.Background(), id)
	t.Fatalf("job %d never reached %q (now %+v)", id, want, j)
	return nil
}

func (e *env) results(t *testing.T) (total int64) {
	t.Helper()
	_, total, err := e.st.ListResults(context.Background(), store.ResultFilter{Status: "available", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

// ---- tests ----

func TestJobRunsToCompletion(t *testing.T) {
	e := newEnv(t, Options{})
	e.start(t)
	j, err := e.s.Create(context.Background(), digits2(5))
	if err != nil {
		t.Fatal(err)
	}
	got := e.waitStatus(t, j.ID, "done")
	if got.Checked != 100 || got.Available != 50 || got.Registered != 50 || got.Cursor != 100 || got.Total != 100 {
		t.Fatalf("job = %+v", got)
	}
	if n := e.results(t); n != 50 {
		t.Fatalf("available results = %d, want 50", n)
	}
	if distinct, dups := e.nf.count(); distinct != 50 || dups != 0 {
		t.Fatalf("notified distinct=%d dups=%d, want 50/0", distinct, dups)
	}
}

func TestRespectsWorkerLimit(t *testing.T) {
	e := newEnv(t, Options{})
	e.start(t)
	j, _ := e.s.Create(context.Background(), digits2(3))
	e.waitStatus(t, j.ID, "done")
	if m := e.ck.maxConc.Load(); m > 3 {
		t.Fatalf("max concurrent checks = %d, want <= 3", m)
	}
}

func TestPauseStopsAndResumeFinishesWithoutDuplicates(t *testing.T) {
	e := newEnv(t, Options{})
	e.ck.blockAfter = 20
	e.start(t)
	j, _ := e.s.Create(context.Background(), digits2(4))
	for e.ck.total.Load() < 20 {
		time.Sleep(2 * time.Millisecond)
	}
	if err := e.s.Pause(j.ID); err != nil {
		t.Fatal(err)
	}
	paused := e.waitStatus(t, j.ID, "paused")
	calls := e.ck.total.Load()
	time.Sleep(60 * time.Millisecond)
	if e.ck.total.Load() != calls {
		t.Fatalf("checks continued after pause: %d -> %d", calls, e.ck.total.Load())
	}
	close2(e.ck.release) // stop blocking
	if err := e.s.Resume(j.ID); err != nil {
		t.Fatal(err)
	}
	done := e.waitStatus(t, j.ID, "done")
	if done.Checked != 100 || done.Available != 50 || done.Cursor != 100 {
		t.Fatalf("after resume: %+v (paused at cursor %d)", done, paused.Cursor)
	}
	if n := e.results(t); n != 50 {
		t.Fatalf("results = %d, want 50 (no duplicates)", n)
	}
	if _, dups := e.nf.count(); dups != 0 {
		t.Fatalf("%d domains notified twice", dups)
	}
}

func TestRestartResumesFromSavedCursor(t *testing.T) {
	e := newEnv(t, Options{})
	e.ck.blockAfter = 30
	e.start(t)
	j, _ := e.s.Create(context.Background(), digits2(4))
	for e.ck.total.Load() < 30 {
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	e.s.Shutdown(context.Background()) // simulates container stop

	mid, _ := e.st.GetJob(context.Background(), j.ID)
	if mid.Status != "running" && mid.Status != "queued" {
		t.Fatalf("interrupted job must stay recoverable, status = %q", mid.Status)
	}
	if mid.Cursor >= 100 || mid.Cursor == 0 {
		t.Fatalf("cursor = %d, want partial progress", mid.Cursor)
	}

	// "new process": fresh scheduler and a checker that never blocks
	close2(e.ck.release) // let the abandoned in-flight checks of the "old process" finish
	e.ck = newFake()
	s2 := e.newScheduler(Options{})
	e.s = s2
	if err := s2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := e.waitStatus(t, j.ID, "done")
	if done.Checked != 100 || done.Available != 50 || done.Registered != 50 {
		t.Fatalf("after restart: %+v", done)
	}
	if got := int(e.ck.total.Load()); got > 100-int(mid.Cursor)+4+1 {
		t.Fatalf("restart re-checked too much: %d checks for %d remaining", got, 100-mid.Cursor)
	}
	if n := e.results(t); n != 50 {
		t.Fatalf("results = %d, want 50", n)
	}
	if _, dups := e.nf.count(); dups != 0 {
		t.Fatalf("%d domains notified twice after restart", dups)
	}
}

func TestUnknownIsStoredButNeverNotifiedOrCountedAvailable(t *testing.T) {
	e := newEnv(t, Options{})
	e.ck.unknown["10"] = true // would be available if it were known
	e.start(t)
	j, _ := e.s.Create(context.Background(), digits2(4))
	got := e.waitStatus(t, j.ID, "done")
	if got.Unknown != 1 || got.Available != 49 {
		t.Fatalf("job = %+v, want unknown=1 available=49", got)
	}
	e.nf.mu.Lock()
	_, notified := e.nf.seen["10.li"]
	e.nf.mu.Unlock()
	if notified {
		t.Fatal("unknown domain was pushed to the notifier")
	}
	rows, _, _ := e.st.ListResults(context.Background(), store.ResultFilter{Status: "unknown", Limit: 10})
	if len(rows) != 1 || rows[0].Domain != "10.li" {
		t.Fatalf("unknown rows = %+v", rows)
	}
}

func TestCreateValidation(t *testing.T) {
	e := newEnv(t, Options{MaxSpace: 1000, HardMaxSpace: 100000})
	ctx := context.Background()
	bad := map[string]Params{
		"bad pattern":        {Suffix: ".li", Pattern: "x", Length: 2},
		"dangerous regex":    {Suffix: ".li", Pattern: "D", Length: 2, Regex: "(a+)+"},
		"broken regex":       {Suffix: ".li", Pattern: "D", Length: 2, Regex: "(["},
		"missing wordlist":   {Suffix: ".li", Wordlist: "builtin:nope"},
		"empty wordlist":     {Suffix: ".li", Wordlist: "builtin:empty"},
		"no suffix":          {Pattern: "d", Length: 2},
		"too large no force": {Suffix: ".li", Pattern: "d", Length: 4, Force: false}, // 10^4 > 1000
		"beyond hard limit":  {Suffix: ".li", Pattern: "a", Length: 8, Force: true},
	}
	for name, p := range bad {
		if _, err := e.s.Create(ctx, p); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if jobs, _ := e.st.ListJobs(ctx); len(jobs) != 0 {
		t.Fatalf("invalid creates wrote %d job(s)", len(jobs))
	}
	if _, err := e.s.Create(ctx, Params{Suffix: ".li", Pattern: "d", Length: 4, Force: true}); err != nil {
		t.Fatalf("force within hard limit should be accepted: %v", err)
	}
}

func TestDictionaryJobAndReservedFlag(t *testing.T) {
	e := newEnv(t, Options{})
	e.start(t)
	j, err := e.s.Create(context.Background(), Params{Name: "words", Suffix: "li", Wordlist: "builtin:tiny", Workers: 2, UseReserved: true})
	if err != nil {
		t.Fatal(err)
	}
	got := e.waitStatus(t, j.ID, "done")
	if got.Total != 3 || got.Checked != 3 {
		t.Fatalf("dictionary job = %+v", got)
	}
	if !e.ck.reserved.Load() {
		t.Fatal("UseReserved was not passed to the checker")
	}
}

func TestCancelAndInvalidTransitions(t *testing.T) {
	e := newEnv(t, Options{})
	e.ck.blockAfter = 5
	e.start(t)
	j, _ := e.s.Create(context.Background(), digits2(2))
	for e.ck.total.Load() < 5 {
		time.Sleep(2 * time.Millisecond)
	}
	if err := e.s.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(t, j.ID, "cancelled")
	if err := e.s.Pause(j.ID); !errors.Is(err, ErrState) {
		t.Fatalf("Pause on cancelled job: err = %v, want ErrState", err)
	}
	if err := e.s.Resume(j.ID); !errors.Is(err, ErrState) {
		t.Fatalf("Resume on cancelled job: err = %v, want ErrState", err)
	}
	if err := e.s.Pause(99999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Pause on missing job: err = %v, want ErrNotFound", err)
	}
}

func TestDeleteRunningJobStopsWritingAndRemovesEverything(t *testing.T) {
	e := newEnv(t, Options{})
	e.ck.blockAfter = 15
	e.start(t)
	j, _ := e.s.Create(context.Background(), digits2(3))
	for e.ck.total.Load() < 15 {
		time.Sleep(2 * time.Millisecond)
	}
	if err := e.s.Delete(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	close2(e.ck.release)
	time.Sleep(100 * time.Millisecond)
	if _, err := e.st.GetJob(context.Background(), j.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("job still present: %v", err)
	}
	if _, total, _ := e.st.ListResults(context.Background(), store.ResultFilter{Limit: 5}); total != 0 {
		t.Fatalf("orphan results after delete: %d", total)
	}
}

func TestCheckerPanicBecomesUnknown(t *testing.T) {
	e := newEnv(t, Options{})
	e.ck.panicOn = "10"
	e.start(t)
	j, _ := e.s.Create(context.Background(), digits2(2))
	got := e.waitStatus(t, j.ID, "done")
	if got.Unknown != 1 || got.Checked != 100 {
		t.Fatalf("job = %+v, want 1 unknown and all 100 checked", got)
	}
}

func (e *env) addProxies(ids ...int64) {
	var entries []egress.ProxyEntry
	for _, id := range ids {
		entries = append(entries, egress.ProxyEntry{ID: egress.ProxyID(id), Name: fmt.Sprintf("p%d", id), Addr: "127.0.0.1:1", Enabled: true})
	}
	e.reg.Replace(entries)
	for _, id := range ids {
		e.reg.SetHealth(egress.ProxyID(id), true)
	}
}

func proxyParams(id int64) Params {
	p := digits2(2)
	p.EgressMode, p.ProxyID = "proxy", id
	return p
}

func TestMaxParallelJobsKeepsProxyJobsQueued(t *testing.T) {
	e := newEnv(t, Options{MaxParallelJobs: 1})
	e.addProxies(1)
	e.ck.blockAfter = 3
	e.start(t)
	a, err := e.s.Create(context.Background(), proxyParams(1))
	if err != nil {
		t.Fatal(err)
	}
	for e.ck.total.Load() < 3 {
		time.Sleep(2 * time.Millisecond)
	}
	b, _ := e.s.Create(context.Background(), proxyParams(1))
	time.Sleep(80 * time.Millisecond)
	jb, _ := e.st.GetJob(context.Background(), b.ID)
	if jb.Status != "queued" {
		t.Fatalf("second proxy job status = %q, want queued while the first holds the only slot", jb.Status)
	}
	close2(e.ck.release)
	e.waitStatus(t, a.ID, "done")
	e.waitStatus(t, b.ID, "done")
}

func TestDirectJobsAreNotLimitedByMaxParallelJobs(t *testing.T) {
	e := newEnv(t, Options{MaxParallelJobs: 1})
	e.ck.blockAfter = 2 // everything beyond the first two checks blocks, keeping both jobs "running"
	e.start(t)
	a, _ := e.s.Create(context.Background(), digits2(1))
	b, _ := e.s.Create(context.Background(), digits2(1))
	c, _ := e.s.Create(context.Background(), digits2(1))
	for _, j := range []*store.Job{a, b, c} {
		e.waitStatus(t, j.ID, "running")
	}
}

func TestStormSwitchesADirectJobToAHealthyProxyAndFinishes(t *testing.T) {
	e := newEnv(t, Options{})
	e.addProxies(1)
	e.ck.failEgress[egress.DirectID] = true
	e.start(t)
	j, err := e.s.Create(context.Background(), digits2(2))
	if err != nil {
		t.Fatal(err)
	}
	got := e.waitStatus(t, j.ID, "done")
	if got.Checked != 100 || got.Cursor != 100 {
		t.Fatalf("job = %+v", got)
	}
	if e.ck.egressCount(egress.ProxyID(1)) < 60 {
		t.Fatalf("proxy handled %d checks; the job should have moved over after the storm", e.ck.egressCount(egress.ProxyID(1)))
	}
	if e.ck.egressCount(egress.DirectID) > 30 {
		t.Fatalf("direct handled %d checks; the job kept hammering the failing egress", e.ck.egressCount(egress.DirectID))
	}
	if _, cooling := e.reg.Penalized(egress.DirectID); !cooling {
		t.Fatal("direct must be cooling down after the storm")
	}
}

func TestStormWithoutFailoverStaysOnDirectButBacksOff(t *testing.T) {
	e := newEnv(t, Options{})
	e.addProxies(1)
	e.ck.failEgress[egress.DirectID] = true
	e.start(t)
	p := digits2(2)
	off := false
	p.Failover = &off
	j, _ := e.s.Create(context.Background(), p)
	got := e.waitStatus(t, j.ID, "done")
	if e.ck.egressCount(egress.ProxyID(1)) != 0 {
		t.Fatal("failover is off: the proxy must never be used")
	}
	if got.Unknown != 100 {
		t.Fatalf("unknown = %d, every check failed", got.Unknown)
	}
}

func TestFixedProxyJobUsesOnlyThatProxy(t *testing.T) {
	e := newEnv(t, Options{})
	e.addProxies(1, 2)
	e.start(t)
	j, err := e.s.Create(context.Background(), proxyParams(2))
	if err != nil {
		t.Fatal(err)
	}
	e.waitStatus(t, j.ID, "done")
	if e.ck.egressCount(egress.ProxyID(2)) != 100 || e.ck.egressCount(egress.DirectID) != 0 || e.ck.egressCount(egress.ProxyID(1)) != 0 {
		t.Fatalf("egress usage: %v", e.ck.egressCalls)
	}
}

func TestPoolJobUsesOnlyHealthyProxies(t *testing.T) {
	e := newEnv(t, Options{})
	e.addProxies(1, 2)
	e.reg.SetHealth(egress.ProxyID(1), false)
	e.start(t)
	p := digits2(2)
	p.EgressMode = "pool"
	j, err := e.s.Create(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	e.waitStatus(t, j.ID, "done")
	if e.ck.egressCount(egress.ProxyID(2)) != 100 || e.ck.egressCount(egress.ProxyID(1)) != 0 {
		t.Fatalf("egress usage: %v", e.ck.egressCalls)
	}
}

func TestCreateValidatesEgressSettings(t *testing.T) {
	e := newEnv(t, Options{})
	ctx := context.Background()
	bad := map[string]func(*Params){
		"unknown mode":           func(p *Params) { p.EgressMode = "teleport" },
		"proxy without id":       func(p *Params) { p.EgressMode = "proxy" },
		"proxy that is missing":  func(p *Params) { p.EgressMode, p.ProxyID = "proxy", 42 },
		"pool without any proxy": func(p *Params) { p.EgressMode = "pool" },
	}
	for name, mutate := range bad {
		p := digits2(2)
		mutate(&p)
		if _, err := e.s.Create(ctx, p); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	e.addProxies(1)
	j, err := e.s.Create(ctx, proxyParams(1))
	if err != nil || j.EgressMode != "proxy" || j.ProxyID != 1 || !j.Failover {
		t.Fatalf("valid proxy job = %+v err=%v (failover must default to on)", j, err)
	}
	d, err := e.s.Create(ctx, digits2(2))
	if err != nil || d.EgressMode != "direct" || !d.Failover {
		t.Fatalf("default job = %+v err=%v", d, err)
	}
}

func TestEveryCheckIsLoggedStepByStep(t *testing.T) {
	e := newEnv(t, Options{})
	e.start(t)
	j, _ := e.s.Create(context.Background(), digits2(2)) // 100 candidates, 50 of them available
	e.waitStatus(t, j.ID, "done")
	time.Sleep(50 * time.Millisecond)
	logs := e.bus.Recent(500, "debug", j.ID)
	var done, found, lifecycle int
	for _, l := range logs {
		switch {
		case l.Component == "check" && l.Event == "done":
			done++
			if l.Domain == "" || l.Egress != egress.DirectID {
				t.Fatalf("check.done lacks domain/egress: %+v", l)
			}
		case l.Component == "check" && l.Event == "found":
			found++
		case l.Component == "scheduler":
			lifecycle++
		}
	}
	if done != 100 || found != 50 || lifecycle < 2 {
		t.Fatalf("check.done=%d found=%d scheduler lines=%d (want 100, 50, >=2)", done, found, lifecycle)
	}
}

func TestFilteredCandidatesStillAdvanceCursor(t *testing.T) {
	e := newEnv(t, Options{})
	e.start(t)
	p := digits2(3)
	p.Regex = "^1" // only 10..19 survive
	j, err := e.s.Create(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	got := e.waitStatus(t, j.ID, "done")
	if got.Checked != 10 || got.Cursor != 100 || got.Available != 5 {
		t.Fatalf("job = %+v, want checked=10 cursor=100 available=5", got)
	}
}
