// Package scheduler owns scan jobs: validation, the queued/running/paused/done state machine,
// egress selection with error-storm failover, and resuming interrupted jobs after a restart.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"domain_scanner/internal/domain"
	"domain_scanner/internal/egress"
	"domain_scanner/internal/enumerate"
	"domain_scanner/internal/logbus"
	"domain_scanner/internal/store"
)

var (
	// ErrInvalid wraps every job-parameter validation failure.
	ErrInvalid = errors.New("invalid job")
	// ErrState is returned for a transition the current job state does not allow.
	ErrState = errors.New("invalid state transition")
)

// CheckOpts are the per-check choices the scheduler makes.
type CheckOpts struct {
	UseReserved bool
	Egress      string // egress id the check must use
	// MaxRDAPWait, when > 0, caps how long an RDAP lookup waits out registry rate limits so a
	// failing egress is noticed quickly (a failover target exists). 0 keeps the long default.
	MaxRDAPWait time.Duration
}

type Checker interface {
	Check(ctx context.Context, domain string, o CheckOpts) domain.Verdict
}

type Notifier interface {
	Notify(job string, domains []string)
}

type Words interface {
	Words(id string) ([]string, error)
}

type Params struct {
	Name        string `json:"name"`
	Suffix      string `json:"suffix"`
	Pattern     string `json:"pattern"`
	Regex       string `json:"regex"`
	Wordlist    string `json:"wordlist"`
	Length      int    `json:"length"`
	DelayMS     int    `json:"delay_ms"`
	Workers     int    `json:"workers"`
	UseReserved bool   `json:"use_reserved"`
	Force       bool   `json:"force"`

	// EgressMode is direct (default), proxy (ProxyID) or pool (any healthy proxy). Failover
	// (default true) lets the job move to another egress when its own keeps failing.
	EgressMode string `json:"egress_mode"`
	ProxyID    int64  `json:"proxy_id"`
	Failover   *bool  `json:"failover"`
}

type Options struct {
	// MaxParallelJobs caps how many jobs routed through proxies run at once. Jobs on the direct
	// connection are never limited. 0 means no cap at all.
	MaxParallelJobs int
	MaxSpace        int64         // candidates allowed without Force (default 5,000,000)
	HardMaxSpace    int64         // absolute ceiling, even with Force (default 10,000,000,000)
	SaveEvery       time.Duration // progress persistence interval (default 2s)
	// Registry is the shared view of egresses (direct + proxies). Created empty when nil.
	Registry *egress.Registry
	Health   healthConfig // zero value = defaults
}

type runner struct {
	id     int64
	cancel context.CancelFunc
	done   chan struct{}
	reason string // "", pause, cancel, delete — guarded by Scheduler.mu
}

type Scheduler struct {
	st   *store.Store
	bus  *logbus.Bus
	sl   *logbus.Logger // component "scheduler"
	cl   *logbus.Logger // component "check"
	el   *logbus.Logger // component "egress"
	nf   Notifier
	ck   Checker
	wl   Words
	opts Options
	reg  *egress.Registry

	rootCtx    context.Context
	rootCancel context.CancelFunc
	sem        chan struct{} // nil = unlimited

	mu      sync.Mutex
	runners map[int64]*runner
	wg      sync.WaitGroup
}

func New(st *store.Store, log *logbus.Bus, nf Notifier, ck Checker, wl Words, opts Options) *Scheduler {
	if opts.MaxSpace <= 0 {
		opts.MaxSpace = 5_000_000
	}
	if opts.HardMaxSpace <= 0 {
		opts.HardMaxSpace = 10_000_000_000
	}
	if opts.SaveEvery <= 0 {
		opts.SaveEvery = 2 * time.Second
	}
	if opts.Registry == nil {
		opts.Registry = egress.NewRegistry()
	}
	if opts.Health.Window == 0 {
		opts.Health = defaultHealthConfig()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Scheduler{
		st: st, bus: log, sl: log.Logger("scheduler"), cl: log.Logger("check"), el: log.Logger("egress"),
		nf: nf, ck: ck, wl: wl, opts: opts, reg: opts.Registry,
		rootCtx: ctx, rootCancel: cancel,
		runners: map[int64]*runner{},
	}
	if opts.MaxParallelJobs > 0 {
		s.sem = make(chan struct{}, opts.MaxParallelJobs)
	}
	return s
}

// Start re-queues every job that was queued or running when the process last stopped.
func (s *Scheduler) Start(ctx context.Context) error {
	jobs, err := s.st.RecoverableJobs(ctx)
	if err != nil {
		return err
	}
	for i := range jobs {
		j := jobs[i]
		s.sl.Info("recover", j.ID, fmt.Sprintf("恢复任务 #%d「%s」,从第 %d/%d 个候选继续", j.ID, j.Name, j.Cursor, j.Total),
			logbus.Fields{"cursor": j.Cursor, "total": j.Total, "status": j.Status})
		s.launch(j.ID)
	}
	return nil
}

// Create validates p, stores the job and queues it.
func (s *Scheduler) Create(ctx context.Context, p Params) (*store.Job, error) {
	suffix := strings.ToLower(strings.TrimSpace(p.Suffix))
	if suffix != "" && !strings.HasPrefix(suffix, ".") {
		suffix = "." + suffix
	}
	if p.Workers <= 0 {
		p.Workers = 5
	}
	if p.Workers > 50 {
		p.Workers = 50
	}
	if p.DelayMS < 0 {
		p.DelayMS = 0
	}
	if p.DelayMS > 60000 {
		p.DelayMS = 60000
	}
	failover := true
	if p.Failover != nil {
		failover = *p.Failover
	}
	mode := p.EgressMode
	if mode == "" {
		mode = "direct"
	}
	if err := s.validateEgress(mode, p.ProxyID); err != nil {
		return nil, err
	}
	j := &store.Job{
		Name: strings.TrimSpace(p.Name), Suffix: suffix, Pattern: p.Pattern, Regex: p.Regex, Wordlist: p.Wordlist,
		Length: p.Length, DelayMS: p.DelayMS, Workers: p.Workers, UseReserved: p.UseReserved, Status: "queued",
		EgressMode: mode, ProxyID: p.ProxyID, Failover: failover,
	}
	if mode != "proxy" {
		j.ProxyID = 0
	}
	plan, err := s.buildPlan(j, p.Force)
	if err != nil {
		return nil, err
	}
	j.Total = plan.Total()
	if j.Name == "" {
		if j.Wordlist != "" {
			j.Name = fmt.Sprintf("%s %s", j.Wordlist, j.Suffix)
		} else {
			j.Name = fmt.Sprintf("%s %s×%d", j.Suffix, j.Pattern, j.Length)
		}
	}
	id, err := s.st.CreateJob(ctx, j)
	if err != nil {
		return nil, err
	}
	s.sl.Info("created", id, fmt.Sprintf("创建任务 #%d「%s」:%d 个候选,%d 并发,间隔 %dms,出口 %s", id, j.Name, j.Total, j.Workers, j.DelayMS, mode),
		logbus.Fields{"total": j.Total, "workers": j.Workers, "delay_ms_per_worker": j.DelayMS, "egress_mode": mode,
			"proxy_id": j.ProxyID, "failover": failover, "suffix": j.Suffix, "pattern": j.Pattern, "length": j.Length,
			"wordlist": j.Wordlist, "regex": j.Regex, "use_reserved": j.UseReserved})
	s.launch(id)
	return s.st.GetJob(ctx, id)
}

func (s *Scheduler) validateEgress(mode string, proxyID int64) error {
	switch mode {
	case "direct":
		return nil
	case "proxy":
		if proxyID <= 0 {
			return fmt.Errorf("%w: 请选择一个代理", ErrInvalid)
		}
		if _, ok := s.reg.Get(egress.ProxyID(proxyID)); !ok {
			return fmt.Errorf("%w: 代理 #%d 不存在或未启用", ErrInvalid, proxyID)
		}
	case "pool":
		enabled := 0
		for _, st := range s.reg.Snapshot() {
			if !st.Direct && st.Enabled {
				enabled++
			}
		}
		if enabled == 0 {
			return fmt.Errorf("%w: 代理池为空,请先添加并启用出站代理", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: 未知的出口模式 %q(应为 direct、proxy 或 pool)", ErrInvalid, mode)
	}
	return nil
}

func (s *Scheduler) buildPlan(j *store.Job, force bool) (*enumerate.Plan, error) {
	spec := enumerate.Spec{Length: j.Length, Suffix: j.Suffix, Pattern: j.Pattern, Regex: j.Regex}
	if j.Wordlist != "" {
		words, err := s.wl.Words(j.Wordlist)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if words == nil {
			words = []string{}
		}
		spec.Words = words
	}
	limit := s.opts.MaxSpace
	if force {
		limit = s.opts.HardMaxSpace
	}
	plan, err := enumerate.Compile(spec, limit)
	if err != nil {
		if errors.Is(err, enumerate.ErrTooLarge) && !force {
			return nil, fmt.Errorf("%w: %v(确需扫描请勾选「强制」)", ErrInvalid, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return plan, nil
}

func (s *Scheduler) Pause(id int64) error {
	j, err := s.st.GetJob(s.rootCtx, id)
	if err != nil {
		return err
	}
	if j.Status != "queued" && j.Status != "running" {
		return fmt.Errorf("%w: cannot pause a %s job", ErrState, j.Status)
	}
	s.sl.Info("pause_requested", id, fmt.Sprintf("请求暂停任务 #%d", id), nil)
	if !s.stop(id, "pause") {
		return s.st.SetJobStatus(s.rootCtx, id, "paused", "")
	}
	return nil
}

func (s *Scheduler) Cancel(id int64) error {
	j, err := s.st.GetJob(s.rootCtx, id)
	if err != nil {
		return err
	}
	if j.Status != "queued" && j.Status != "running" && j.Status != "paused" {
		return fmt.Errorf("%w: cannot cancel a %s job", ErrState, j.Status)
	}
	s.sl.Info("cancel_requested", id, fmt.Sprintf("请求取消任务 #%d", id), nil)
	if !s.stop(id, "cancel") {
		s.sl.Info("cancelled", id, fmt.Sprintf("任务 #%d 已取消", id), nil)
		return s.st.SetJobStatus(s.rootCtx, id, "cancelled", "")
	}
	return nil
}

func (s *Scheduler) Resume(id int64) error {
	j, err := s.st.GetJob(s.rootCtx, id)
	if err != nil {
		return err
	}
	if j.Status != "paused" && j.Status != "failed" {
		return fmt.Errorf("%w: cannot resume a %s job", ErrState, j.Status)
	}
	s.waitRunner(id)
	if err := s.st.SetJobStatus(s.rootCtx, id, "queued", ""); err != nil {
		return err
	}
	s.sl.Info("resume", id, fmt.Sprintf("任务 #%d 继续,从第 %d/%d 个候选", id, j.Cursor, j.Total),
		logbus.Fields{"cursor": j.Cursor, "total": j.Total})
	s.launch(id)
	return nil
}

// Delete stops the job (if running), waits for it to wind down, then removes it with its data.
func (s *Scheduler) Delete(ctx context.Context, id int64) error {
	s.sl.Info("delete", id, fmt.Sprintf("删除任务 #%d", id), nil)
	s.stop(id, "delete")
	return s.st.DeleteJob(ctx, id)
}

// Shutdown stops all runners, keeping their progress so Start can resume them next time.
func (s *Scheduler) Shutdown(ctx context.Context) {
	s.rootCancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// stop signals the runner for id and waits (bounded) for it to exit. It reports whether a
// runner existed.
func (s *Scheduler) stop(id int64, reason string) bool {
	s.mu.Lock()
	r := s.runners[id]
	if r != nil && r.reason == "" {
		r.reason = reason
	}
	s.mu.Unlock()
	if r == nil {
		return false
	}
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
	}
	return true
}

func (s *Scheduler) waitRunner(id int64) {
	s.mu.Lock()
	r := s.runners[id]
	s.mu.Unlock()
	if r != nil {
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
		}
	}
}

func (s *Scheduler) launch(id int64) {
	ctx, cancel := context.WithCancel(s.rootCtx)
	r := &runner{id: id, cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	s.runners[id] = r
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		s.run(ctx, r)
		s.mu.Lock()
		if s.runners[id] == r {
			delete(s.runners, id)
		}
		s.mu.Unlock()
		close(r.done)
	}()
}

func (s *Scheduler) reasonOf(r *runner) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return r.reason
}
