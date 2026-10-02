// Package verify is the last gate before a domain is announced: it asks Cloudflare's registrar
// whether each RDAP/WHOIS "available" domain can really be registered, records the verdict, and
// only then hands the domain to the notifier — with the price and a register button when
// confirmed, with a warning label when Cloudflare cannot say.
package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"domain_scanner/internal/appsettings"
	"domain_scanner/internal/cloudflare"
	"domain_scanner/internal/logbus"
	"domain_scanner/internal/notifier"
	"domain_scanner/internal/scheduler"
	"domain_scanner/internal/store"
)

const maxBatch = 20

// CF is the part of the Cloudflare client the verifier uses.
type CF interface {
	Configured() bool
	Check(ctx context.Context, domains []string) ([]cloudflare.Domain, error)
}

// Sink receives the domains that should be announced.
type Sink interface {
	NotifyItems(job string, items []notifier.Item)
}

// Verifier batches hits, checks them with Cloudflare and announces the outcome.
type Verifier struct {
	St     *store.Store
	CF     CF
	Sink   Sink
	Policy func() appsettings.RegisterPolicy
	Log    *logbus.Logger

	FlushEvery  time.Duration // default 2s
	RetryBase   time.Duration // default 2s; attempt n waits n × RetryBase (or the server's Retry-After)
	MaxAttempts int           // default 3

	mu      sync.Mutex
	queue   []scheduler.Hit
	wake    chan struct{}
	stopCh  chan struct{}
	done    chan struct{}
	started bool
	stopped sync.Once
}

func (v *Verifier) defaults() {
	if v.FlushEvery <= 0 {
		v.FlushEvery = 2 * time.Second
	}
	if v.RetryBase <= 0 {
		v.RetryBase = 2 * time.Second
	}
	if v.MaxAttempts <= 0 {
		v.MaxAttempts = 3
	}
}

// Start launches the batching goroutine.
func (v *Verifier) Start(ctx context.Context) {
	v.defaults()
	v.mu.Lock()
	v.wake, v.stopCh, v.done, v.started = make(chan struct{}, 1), make(chan struct{}), make(chan struct{}), true
	v.mu.Unlock()
	go v.loop(ctx)
}

// Stop flushes whatever is queued, then ends the goroutine.
func (v *Verifier) Stop() {
	v.stopped.Do(func() {
		v.mu.Lock()
		started := v.started
		v.mu.Unlock()
		if !started {
			return
		}
		close(v.stopCh)
		<-v.done
	})
}

// Found implements scheduler's notifier: the hit is verified, then announced. It never blocks.
func (v *Verifier) Found(h scheduler.Hit) {
	if !v.CF.Configured() {
		// legacy behaviour: nothing to verify against, announce right away with a label
		v.Log.Debug("skip", h.JobID, fmt.Sprintf("未配置 Cloudflare,%s 不做终检,直接推送", h.Domain), logbus.Fields{"domain": h.Domain})
		v.Sink.NotifyItems(h.JobName, []notifier.Item{{ResultID: h.ResultID, Domain: h.Domain,
			Status: notifier.StatusUnconfirmed, Note: "未配置 Cloudflare 校验"}})
		return
	}
	_ = v.St.UpdateResultCF(context.Background(), h.ResultID, store.CFUpdate{Status: "pending"})
	v.mu.Lock()
	v.queue = append(v.queue, h)
	full := len(v.queue) >= maxBatch
	wake := v.wake
	v.mu.Unlock()
	if full && wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (v *Verifier) take(n int) []scheduler.Hit {
	v.mu.Lock()
	defer v.mu.Unlock()
	if n > len(v.queue) {
		n = len(v.queue)
	}
	batch := append([]scheduler.Hit(nil), v.queue[:n]...)
	v.queue = v.queue[n:]
	return batch
}

func (v *Verifier) queued() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.queue)
}

func (v *Verifier) loop(ctx context.Context) {
	defer close(v.done)
	t := time.NewTicker(v.FlushEvery)
	defer t.Stop()
	drain := func(onlyFull bool) {
		for v.queued() >= maxBatch || (!onlyFull && v.queued() > 0) {
			v.process(ctx, v.take(maxBatch))
		}
	}
	for {
		select {
		case <-v.stopCh:
			drain(false)
			return
		case <-ctx.Done():
			drain(false)
			return
		case <-v.wake:
			drain(true)
		case <-t.C:
			drain(false)
		}
	}
}

// process checks one batch (retrying temporary failures) and announces the outcome.
func (v *Verifier) process(ctx context.Context, batch []scheduler.Hit) {
	if len(batch) == 0 {
		return
	}
	names := make([]string, len(batch))
	for i, h := range batch {
		names[i] = strings.ToLower(h.Domain)
	}
	t0 := time.Now()
	var doms []cloudflare.Domain
	var err error
	attempts := 0
	for attempts < v.MaxAttempts {
		attempts++
		doms, err = v.CF.Check(ctx, names)
		if err == nil {
			break
		}
		var ae *cloudflare.APIError
		retryable := errors.As(err, &ae) && ae.Temporary()
		v.Log.Warn("batch_error", 0, fmt.Sprintf("Cloudflare 批量校验第 %d 次失败:%v", attempts, err),
			logbus.Fields{"attempt": attempts, "domains": len(names), "error": err.Error(), "retryable": retryable})
		if !retryable || attempts >= v.MaxAttempts {
			break
		}
		wait := v.RetryBase * time.Duration(attempts)
		if ae != nil && ae.RetryAfter > wait {
			wait = ae.RetryAfter
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
	byName := map[string]cloudflare.Domain{}
	for _, d := range doms {
		byName[strings.ToLower(d.Name)] = d
	}

	pol := v.Policy()
	perJob := map[string][]notifier.Item{}
	var order []string
	counts := map[string]int{}
	add := func(h scheduler.Hit, it notifier.Item) {
		if _, ok := perJob[h.JobName]; !ok {
			order = append(order, h.JobName)
		}
		perJob[h.JobName] = append(perJob[h.JobName], it)
	}
	for _, h := range batch {
		upd, item, announce := v.decide(h, byName[strings.ToLower(h.Domain)], err != nil, err, doms, pol)
		_ = v.St.UpdateResultCF(context.Background(), h.ResultID, upd)
		counts[upd.Status]++
		v.Log.Info("verdict", h.JobID, fmt.Sprintf("%s → %s %s", h.Domain, upd.Status, first(upd.Reason, upd.Price)), logbus.Fields{
			"domain": h.Domain, "cf_status": upd.Status, "reason": upd.Reason, "price": upd.Price, "currency": upd.Currency})
		if announce {
			add(h, item)
		}
	}
	v.Log.Info("batch", 0, fmt.Sprintf("Cloudflare 校验 %d 个域名:%v(%d 次请求,耗时 %s)", len(batch), counts, attempts, time.Since(t0).Round(time.Millisecond)),
		logbus.Fields{"domains": len(batch), "attempts": attempts, "duration_ms": time.Since(t0), "confirmed": counts["confirmed"],
			"rejected": counts["rejected"], "unsupported": counts["unsupported"], "error": counts["error"]})
	for _, job := range order {
		v.Sink.NotifyItems(job, perJob[job])
	}
}

// decide turns Cloudflare's answer for one hit into the stored verdict and the announcement.
func (v *Verifier) decide(h scheduler.Hit, d cloudflare.Domain, batchFailed bool, batchErr error, got []cloudflare.Domain, pol appsettings.RegisterPolicy) (store.CFUpdate, notifier.Item, bool) {
	item := notifier.Item{ResultID: h.ResultID, Domain: h.Domain}
	switch {
	case batchFailed:
		reason := batchErr.Error()
		item.Status, item.Note = notifier.StatusUnconfirmed, "Cloudflare 校验失败:"+reason
		return store.CFUpdate{Status: "error", Reason: reason}, item, pol.PushUnconfirmed
	case d.Name == "":
		reason := "Cloudflare 没有返回该域名的结果"
		item.Status, item.Note = notifier.StatusUnconfirmed, "Cloudflare 校验失败:"+reason
		return store.CFUpdate{Status: "error", Reason: reason}, item, pol.PushUnconfirmed
	case d.Registrable:
		item.Status, item.Price, item.Currency = notifier.StatusConfirmed, d.RegistrationCost, d.Currency
		return store.CFUpdate{Status: "confirmed", Price: d.RegistrationCost, Currency: d.Currency}, item, true
	case d.Unsupported():
		item.Status, item.Note = notifier.StatusUnconfirmed, "Cloudflare 不支持该后缀,无法确认"
		return store.CFUpdate{Status: "unsupported", Reason: d.Reason}, item, pol.PushUnconfirmed
	default: // domain_unavailable, domain_premium, extension_disallows_registration, ...
		return store.CFUpdate{Status: "rejected", Reason: d.Reason}, item, false
	}
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
