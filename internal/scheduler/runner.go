package scheduler

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"domain_scanner/internal/domain"
	"domain_scanner/internal/enumerate"
	"domain_scanner/internal/store"
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
}

// run executes one job from its saved cursor until it finishes or ctx is cancelled.
func (s *Scheduler) run(ctx context.Context, r *runner) {
	id := r.id
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		s.finalize(r, nil, 0)
		return
	}

	job, err := s.st.GetJob(context.Background(), id)
	if err != nil {
		s.log.Log("error", id, "任务 #%d 无法读取:%v", id, err)
		return
	}
	plan, err := s.buildPlan(job, true)
	if err != nil {
		s.log.Log("error", id, "任务 #%d 启动失败:%v", id, err)
		_ = s.st.SetJobStatus(context.Background(), id, "failed", err.Error())
		return
	}
	_ = s.st.SetJobStatus(context.Background(), id, "running", "")
	s.log.Log("info", id, "任务 #%d「%s」开始:共 %d 个候选,从 %d 继续", id, job.Name, plan.Total(), job.Cursor)

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
	finished := false
	if snap != nil {
		_ = s.st.UpdateJobProgress(bg, id, snap.cursor, snap.c.checked, snap.c.available, snap.c.unknown, snap.c.registered)
		finished = snap.cursor >= total
		if finished {
			_ = s.st.SetJobStatus(bg, id, "done", "")
			s.log.Log("info", id, "任务 #%d 完成:检查 %d,可注册 %d,未知 %d,已注册/保留 %d",
				id, snap.c.checked, snap.c.available, snap.c.unknown, snap.c.registered)
			return
		}
	}
	switch reason {
	case "pause":
		_ = s.st.SetJobStatus(bg, id, "paused", "")
		s.log.Log("info", id, "任务 #%d 已暂停", id)
	case "cancel":
		_ = s.st.SetJobStatus(bg, id, "cancelled", "")
		s.log.Log("info", id, "任务 #%d 已取消", id)
	default:
		// Process shutdown: leave status as-is so Start() resumes it.
		s.log.Log("info", id, "任务 #%d 因服务停止而中断,进度已保存,重启后自动继续", id)
	}
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
				v, ok := s.check(ctx, job.ID, t.domain, job.UseReserved)
				if !ok {
					return // cancelled mid-check: drop it, it will be redone on resume
				}
				events <- event{idx: t.idx, verdict: v}
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
	save := func() {
		_ = s.st.UpdateJobProgress(context.Background(), job.ID, cursor,
			committed.checked, committed.available, committed.unknown, committed.registered)
		lastSave, lastSaved = time.Now(), cursor
	}
	for e := range events {
		var d counters
		if !e.skipped {
			d = s.record(job, e.verdict)
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
	}
	return snapshot{cursor: cursor, c: committed}
}

// check runs the checker in its own goroutine so a stuck WHOIS call cannot delay cancellation.
// ok=false means ctx was cancelled before a verdict arrived.
func (s *Scheduler) check(ctx context.Context, jobID int64, d string, useReserved bool) (domain.Verdict, bool) {
	ch := make(chan domain.Verdict, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				s.log.Log("error", jobID, "检测 %s 时发生内部错误:%v", d, p)
				ch <- domain.Verdict{Domain: d, Status: domain.StatusUnknown, Reason: fmt.Sprintf("internal error: %v", p)}
			}
		}()
		ch <- s.ck.Check(ctx, d, useReserved)
	}()
	select {
	case v := <-ch:
		if ctx.Err() != nil && v.Status == domain.StatusUnknown && strings.Contains(v.Reason, "cancelled") {
			return v, false
		}
		return v, true
	case <-ctx.Done():
		return domain.Verdict{}, false
	}
}

// record persists/announces one verdict and returns its counter delta.
func (s *Scheduler) record(job *store.Job, v domain.Verdict) counters {
	ctx := context.Background()
	d := counters{checked: 1}
	switch v.Status {
	case domain.StatusAvailable:
		d.available = 1
		inserted, err := s.st.InsertResult(ctx, &store.Result{JobID: job.ID, Domain: v.Domain, Status: "available",
			Signatures: strings.Join(v.Signatures, ",")})
		if err != nil {
			s.log.Log("error", job.ID, "保存结果 %s 失败:%v", v.Domain, err)
			break
		}
		if inserted {
			s.log.Log("info", job.ID, "可注册:%s", v.Domain)
			s.nf.Notify(job.Name, []string{v.Domain})
		}
	case domain.StatusUnknown:
		d.unknown = 1
		if _, err := s.st.InsertResult(ctx, &store.Result{JobID: job.ID, Domain: v.Domain, Status: "unknown",
			Signatures: v.Reason}); err != nil {
			s.log.Log("error", job.ID, "保存结果 %s 失败:%v", v.Domain, err)
		}
		s.log.Log("warn", job.ID, "无法确定 %s:%s", v.Domain, v.Reason)
	default: // registered, reserved
		d.registered = 1
		s.log.Log("debug", job.ID, "%s → %s %s", v.Domain, v.Status, strings.Join(v.Signatures, ","))
	}
	return d
}
