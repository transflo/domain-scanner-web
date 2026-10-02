// Package scheduler owns scan jobs: validation, the queued/running/paused/done state machine,
// and resuming interrupted jobs after a restart.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"domain_scanner/internal/domain"
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

type Checker interface {
	Check(ctx context.Context, domain string, useReserved bool) domain.Verdict
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
}

type Options struct {
	MaxParallelJobs int           // jobs running at once (default 2)
	MaxSpace        int64         // candidates allowed without Force (default 5,000,000)
	HardMaxSpace    int64         // absolute ceiling, even with Force (default 10,000,000,000)
	SaveEvery       time.Duration // progress persistence interval (default 2s)
}

type runner struct {
	id     int64
	cancel context.CancelFunc
	done   chan struct{}
	reason string // "", pause, cancel, delete — guarded by Scheduler.mu
}

type Scheduler struct {
	st   *store.Store
	log  *logbus.Bus
	nf   Notifier
	ck   Checker
	wl   Words
	opts Options

	rootCtx    context.Context
	rootCancel context.CancelFunc
	sem        chan struct{}

	mu      sync.Mutex
	runners map[int64]*runner
	wg      sync.WaitGroup
}

func New(st *store.Store, log *logbus.Bus, nf Notifier, ck Checker, wl Words, opts Options) *Scheduler {
	if opts.MaxParallelJobs <= 0 {
		opts.MaxParallelJobs = 2
	}
	if opts.MaxSpace <= 0 {
		opts.MaxSpace = 5_000_000
	}
	if opts.HardMaxSpace <= 0 {
		opts.HardMaxSpace = 10_000_000_000
	}
	if opts.SaveEvery <= 0 {
		opts.SaveEvery = 2 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		st: st, log: log, nf: nf, ck: ck, wl: wl, opts: opts,
		rootCtx: ctx, rootCancel: cancel,
		sem:     make(chan struct{}, opts.MaxParallelJobs),
		runners: map[int64]*runner{},
	}
}

// Start re-queues every job that was queued or running when the process last stopped.
func (s *Scheduler) Start(ctx context.Context) error {
	jobs, err := s.st.RecoverableJobs(ctx)
	if err != nil {
		return err
	}
	for i := range jobs {
		s.log.Log("info", jobs[i].ID, "恢复任务 #%d「%s」,从第 %d/%d 个候选继续", jobs[i].ID, jobs[i].Name, jobs[i].Cursor, jobs[i].Total)
		s.launch(jobs[i].ID)
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
	j := &store.Job{
		Name: strings.TrimSpace(p.Name), Suffix: suffix, Pattern: p.Pattern, Regex: p.Regex, Wordlist: p.Wordlist,
		Length: p.Length, DelayMS: p.DelayMS, Workers: p.Workers, UseReserved: p.UseReserved, Status: "queued",
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
	s.log.Log("info", id, "创建任务 #%d「%s」:%d 个候选,%d 并发,间隔 %dms", id, j.Name, j.Total, j.Workers, j.DelayMS)
	s.launch(id)
	return s.st.GetJob(ctx, id)
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
	if !s.stop(id, "cancel") {
		s.log.Log("info", id, "任务 #%d 已取消", id)
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
	s.log.Log("info", id, "任务 #%d 继续", id)
	s.launch(id)
	return nil
}

// Delete stops the job (if running), waits for it to wind down, then removes it with its data.
func (s *Scheduler) Delete(ctx context.Context, id int64) error {
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
