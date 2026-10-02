package scheduler

import (
	"context"
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

const (
	statsEvery      = 30 * time.Second
	failoverRDAPCap = 5 * time.Second // RDAP rate-limit wait when a failover target exists
)

type counters struct{ checked, available, unknown, registered int64 }

func (c *counters) add(d counters) {
	c.checked += d.checked
	c.available += d.available
	c.unknown += d.unknown
	c.registered += d.registered
}

type task struct {
	idx    int64
	domain string
}

type event struct {
	idx     int64
	skipped bool
	verdict domain.Verdict
	egress  string
}

// run executes one job from its saved cursor until it finishes or ctx is cancelled.
func (s *Scheduler) run(ctx context.Context, r *runner) {
	id := r.id
	job, err := s.st.GetJob(context.Background(), id)
	if err != nil {
		s.sl.Error("load_failed", id, fmt.Sprintf("任务 #%d 无法读取:%v", id, err), nil)
		return
	}
	// Only jobs that route through proxies compete for a slot; direct jobs are never limited.
	if s.sem != nil && job.EgressMode != "direct" {
		queuedAt := time.Now()
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
			if w := time.Since(queuedAt); w > 100*time.Millisecond {
				s.sl.Info("slot_acquired", id, fmt.Sprintf("任务 #%d 等待并发名额 %s 后开始", id, w.Round(time.Millisecond)), nil)
			}
		case <-ctx.Done():
			s.finalize(r, nil, 0)
			return
		}
	}

	plan, err := s.buildPlan(job, true)
	if err != nil {
		s.sl.Error("start_failed", id, fmt.Sprintf("任务 #%d 启动失败:%v", id, err), logbus.Fields{"error": err.Error()})
		_ = s.st.SetJobStatus(context.Background(), id, "failed", err.Error())
		return
	}
	if job.EgressMode == "proxy" {
		if _, ok := s.reg.Get(egress.ProxyID(job.ProxyID)); !ok && (!job.Failover || len(s.reg.Candidates(false)) == 0) {
			msg := fmt.Sprintf("指定的代理 #%d 已被删除或停用,且没有可切换的可用代理", job.ProxyID)
			s.sl.Error("start_failed", id, fmt.Sprintf("任务 #%d 启动失败:%s", id, msg), logbus.Fields{"proxy_id": job.ProxyID})
			_ = s.st.SetJobStatus(context.Background(), id, "failed", msg)
			return
		}
	}
	_ = s.st.SetJobStatus(context.Background(), id, "running", "")
	s.sl.Info("start", id, fmt.Sprintf("任务 #%d「%s」开始:共 %d 个候选,从 %d 继续", id, job.Name, plan.Total(), job.Cursor),
		logbus.Fields{"total": plan.Total(), "from": job.Cursor, "workers": job.Workers, "egress_mode": job.EgressMode,
			"failover": job.Failover})

	snap := s.scan(ctx, job, plan)
	s.finalize(r, &snap, plan.Total())
}

type snapshot struct {
	cursor int64
	c      counters
}

// finalize records the job's end state according to why the runner stopped.
func (s *Scheduler) finalize(r *runner, snap *snapshot, total int64) {
	id := r.id
	reason := s.reasonOf(r)
	if reason == "delete" {
		return // the caller removes the job; write nothing
	}
	bg := context.Background()
	if snap != nil {
		_ = s.st.UpdateJobProgress(bg, id, snap.cursor, snap.c.checked, snap.c.available, snap.c.unknown, snap.c.registered)
		if snap.cursor >= total {
			_ = s.st.SetJobStatus(bg, id, "done", "")
			s.sl.Info("done", id, fmt.Sprintf("任务 #%d 完成:检查 %d,可注册 %d,未知 %d,已注册/保留 %d",
				id, snap.c.checked, snap.c.available, snap.c.unknown, snap.c.registered),
				logbus.Fields{"checked": snap.c.checked, "available": snap.c.available, "unknown": snap.c.unknown,
					"registered": snap.c.registered, "cursor": snap.cursor})
			return
		}
	}
	switch reason {
	case "pause":
		_ = s.st.SetJobStatus(bg, id, "paused", "")
		s.sl.Info("paused", id, fmt.Sprintf("任务 #%d 已暂停", id), cursorFields(snap))
	case "cancel":
		_ = s.st.SetJobStatus(bg, id, "cancelled", "")
		s.sl.Info("cancelled", id, fmt.Sprintf("任务 #%d 已取消", id), cursorFields(snap))
	default:
		// Process shutdown: leave status as-is so Start() resumes it.
		s.sl.Info("interrupted", id, fmt.Sprintf("任务 #%d 因服务停止而中断,进度已保存,重启后自动继续", id), cursorFields(snap))
	}
}

func cursorFields(snap *snapshot) logbus.Fields {
	if snap == nil {
		return nil
	}
	return logbus.Fields{"cursor": snap.cursor, "checked": snap.c.checked}
}

// scan runs the dispatcher, workers and the single collector. The returned cursor is the number
// of leading candidates that are fully processed, and the counters cover exactly those, so a
// resumed run never double counts.
func (s *Scheduler) scan(ctx context.Context, job *store.Job, plan *enumerate.Plan) snapshot {
	total := plan.Total()
	workers := job.Workers
	if workers < 1 {
		workers = 1
	}
	st := newEgressState(s.reg, s.opts.Health, job.EgressMode, job.ProxyID, job.Failover)
	s.el.Info("selected", job.ID, fmt.Sprintf("任务 #%d 使用出口 %s(模式 %s,故障切换 %v)", job.ID, st.Current(), job.EgressMode, job.Failover),
		logbus.Fields{"egress": st.Current(), "mode": job.EgressMode, "failover": job.Failover})

	tasks := make(chan task, workers)
	events := make(chan event, 256)
	delay := time.Duration(job.DelayMS) * time.Millisecond

	var producers sync.WaitGroup
	producers.Add(1)
	go func() { // dispatcher
		defer producers.Done()
		defer close(tasks)
		for i := job.Cursor; i < total; i++ {
			if ctx.Err() != nil {
				return
			}
			d, ok := plan.At(i)
			if !ok {
				select {
				case events <- event{idx: i, skipped: true}:
				case <-ctx.Done():
					return
				}
				continue
			}
			select {
			case tasks <- task{idx: i, domain: d}:
			case <-ctx.Done():
				return
			}
		}
	}()
	for w := 0; w < workers; w++ {
		producers.Add(1)
		go func() {
			defer producers.Done()
			for t := range tasks {
				if err := st.Gate(ctx); err != nil {
					return // cancelled while paused by a backoff
				}
				s.steer(job, st)
				egID := st.Current()
				v, ok := s.check(ctx, job, t.domain, egID, st)
				if !ok {
					return // cancelled mid-check: drop it, it will be redone on resume
				}
				failure := v.Status == domain.StatusUnknown && v.ErrKind.Failure()
				if d := st.Record(failure); d != nil {
					s.logStorm(job, d)
				}
				events <- event{idx: t.idx, verdict: v, egress: egID}
				if delay > 0 {
					select {
					case <-time.After(delay):
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	go func() { producers.Wait(); close(events) }()

	cursor := job.Cursor
	committed := counters{job.Checked, job.Available, job.Unknown, job.Registered}
	pending := map[int64]counters{}
	lastSave, lastSaved := time.Now(), cursor
	statAt, statCursor, statChecked := time.Now(), cursor, int64(0)
	var statFail int64
	save := func() {
		_ = s.st.UpdateJobProgress(context.Background(), job.ID, cursor,
			committed.checked, committed.available, committed.unknown, committed.registered)
		lastSave, lastSaved = time.Now(), cursor
	}
	for e := range events {
		var d counters
		if !e.skipped {
			d = s.record(job, e)
			statChecked++
			if e.verdict.Status == domain.StatusUnknown && e.verdict.ErrKind.Failure() {
				statFail++
			}
		}
		pending[e.idx] = d
		for {
			dd, ok := pending[cursor]
			if !ok {
				break
			}
			delete(pending, cursor)
			cursor++
			committed.add(dd)
		}
		if cursor != lastSaved && (time.Since(lastSave) >= s.opts.SaveEvery || cursor-lastSaved >= 500) {
			save()
		}
		if since := time.Since(statAt); since >= statsEvery {
			s.logStats(job, st, cursor, total, statCursor, statChecked, statFail, since)
			statAt, statCursor, statChecked, statFail = time.Now(), cursor, 0, 0
		}
	}
	return snapshot{cursor: cursor, c: committed}
}

// logStats writes the periodic progress line: rate, ETA, failure ratio and current egress.
func (s *Scheduler) logStats(job *store.Job, st *egressState, cursor, total, fromCursor, checked, failures int64, since time.Duration) {
	rate := float64(cursor-fromCursor) / since.Seconds()
	eta := "—"
	if rate > 0 {
		eta = (time.Duration(float64(total-cursor)/rate) * time.Second).Round(time.Minute).String()
	}
	ratio := 0.0
	if checked > 0 {
		ratio = float64(failures) / float64(checked)
	}
	s.sl.Info("progress", job.ID,
		fmt.Sprintf("任务 #%d 进度 %d/%d(%.1f%%),近 %s 速率 %.2f/s,预计剩余 %s,失败率 %.0f%%,出口 %s",
			job.ID, cursor, total, 100*float64(cursor)/float64(max64(total, 1)), since.Round(time.Second), rate, eta, ratio*100, st.Current()),
		logbus.Fields{"cursor": cursor, "total": total, "rate_per_s": fmt.Sprintf("%.2f", rate), "eta": eta,
			"failure_ratio": fmt.Sprintf("%.2f", ratio), "egress": st.Current(), "checked_in_window": checked})
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// steer moves the job off an egress that another job already found to be failing, and back to
// its preferred egress after the cooldown.
func (s *Scheduler) steer(job *store.Job, st *egressState) {
	if from, to, ok := st.Steer(); ok {
		s.el.Info("steer", job.ID, fmt.Sprintf("任务 #%d:出口 %s 正在冷却(其他任务已发现它不可用),改用 %s", job.ID, from, to),
			logbus.Fields{"from": from, "to": to, "reason": "egress cooling down"})
	}
	if to, ok := st.MaybeReturn(); ok {
		s.el.Info("return", job.ID, fmt.Sprintf("任务 #%d:首选出口 %s 已恢复,切回", job.ID, to), logbus.Fields{"to": to})
	}
}

func (s *Scheduler) logStorm(job *store.Job, d *decision) {
	msg := fmt.Sprintf("任务 #%d 在出口 %s 上遇到大量错误(近 %d 次失败率 %.0f%%):全体 worker 暂停 %s,该出口冷却 %s",
		job.ID, d.From, d.Samples, d.FailureRate*100, d.Backoff.Round(time.Second), d.Penalty.Round(time.Second))
	if d.Switched {
		msg += fmt.Sprintf(",并切换到 %s", d.To)
	} else if job.Failover {
		msg += ",没有可切换的可用出口,原地退避"
	} else {
		msg += "(未启用故障切换,原地退避)"
	}
	s.el.Warn("storm", job.ID, msg, logbus.Fields{"from": d.From, "to": d.To, "switched": d.Switched,
		"backoff_s": d.Backoff.Seconds(), "penalty_s": d.Penalty.Seconds(), "failure_ratio": fmt.Sprintf("%.2f", d.FailureRate),
		"samples": d.Samples, "failover": job.Failover})
}

// check runs the checker in its own goroutine so a stuck WHOIS call cannot delay cancellation.
// ok=false means ctx was cancelled before a verdict arrived.
func (s *Scheduler) check(ctx context.Context, job *store.Job, d, egressID string, st *egressState) (domain.Verdict, bool) {
	opts := CheckOpts{UseReserved: job.UseReserved, Egress: egressID}
	if st.FailoverAvailable() {
		opts.MaxRDAPWait = failoverRDAPCap
	}
	tctx := domain.WithTrace(ctx, func(step domain.Step) {
		s.cl.Debug("step", job.ID, fmt.Sprintf("%s %s: %s", d, step.Name, step.Detail), logbus.Fields{
			"domain": d, "egress": egressID, "step": step.Name, "ok": step.OK, "duration_ms": step.DurationMS, "detail": step.Detail})
	})
	if opts.MaxRDAPWait > 0 {
		tctx = domain.WithRDAPMaxWait(tctx, opts.MaxRDAPWait)
	}
	s.cl.Debug("start", job.ID, fmt.Sprintf("开始检测 %s(出口 %s)", d, egressID), logbus.Fields{"domain": d, "egress": egressID})

	ch := make(chan domain.Verdict, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				s.cl.Error("panic", job.ID, fmt.Sprintf("检测 %s 时发生内部错误:%v", d, p), logbus.Fields{"domain": d, "egress": egressID})
				ch <- domain.Verdict{Domain: d, Status: domain.StatusUnknown, Reason: fmt.Sprintf("internal error: %v", p), ErrKind: domain.ErrInternal}
			}
		}()
		ch <- s.ck.Check(tctx, d, opts)
	}()
	select {
	case v := <-ch:
		if ctx.Err() != nil && v.Status == domain.StatusUnknown && (v.ErrKind == domain.ErrCancelled || strings.Contains(v.Reason, "cancelled")) {
			return v, false
		}
		return v, true
	case <-ctx.Done():
		return domain.Verdict{}, false
	}
}

// record persists/announces one verdict and returns its counter delta.
func (s *Scheduler) record(job *store.Job, e event) counters {
	ctx := context.Background()
	v := e.verdict
	d := counters{checked: 1}
	f := logbus.Fields{"domain": v.Domain, "egress": e.egress, "status": string(v.Status), "reason": v.Reason,
		"signatures": strings.Join(v.Signatures, ","), "err_kind": string(v.ErrKind), "duration_ms": v.DurationMS, "steps": len(v.Steps)}
	s.cl.Debug("done", job.ID, fmt.Sprintf("%s → %s(%s)", v.Domain, v.Status, v.Reason), f)

	switch v.Status {
	case domain.StatusAvailable:
		d.available = 1
		res := &store.Result{JobID: job.ID, Domain: v.Domain, Status: "available", Signatures: strings.Join(v.Signatures, ",")}
		inserted, err := s.st.InsertResult(ctx, res)
		if err != nil {
			s.cl.Error("save_failed", job.ID, fmt.Sprintf("保存结果 %s 失败:%v", v.Domain, err), logbus.Fields{"domain": v.Domain, "error": err.Error()})
			break
		}
		if inserted {
			s.cl.Info("found", job.ID, fmt.Sprintf("可注册:%s", v.Domain), logbus.Fields{"domain": v.Domain, "egress": e.egress, "reason": v.Reason})
			s.nf.Found(Hit{ResultID: res.ID, JobID: job.ID, JobName: job.Name, Domain: v.Domain})
		} else {
			s.cl.Debug("duplicate", job.ID, fmt.Sprintf("%s 已存在于结果中,不重复推送", v.Domain), logbus.Fields{"domain": v.Domain})
		}
	case domain.StatusUnknown:
		d.unknown = 1
		if _, err := s.st.InsertResult(ctx, &store.Result{JobID: job.ID, Domain: v.Domain, Status: "unknown",
			Signatures: v.Reason}); err != nil {
			s.cl.Error("save_failed", job.ID, fmt.Sprintf("保存结果 %s 失败:%v", v.Domain, err), logbus.Fields{"domain": v.Domain, "error": err.Error()})
		}
		s.cl.Warn("unknown", job.ID, fmt.Sprintf("无法确定 %s:%s", v.Domain, v.Reason),
			logbus.Fields{"domain": v.Domain, "egress": e.egress, "err_kind": string(v.ErrKind), "reason": v.Reason})
	default: // registered, reserved
		d.registered = 1
	}
	return d
}
