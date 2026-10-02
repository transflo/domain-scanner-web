package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"domain_scanner/internal/egress"
	"domain_scanner/internal/logbus"
	"domain_scanner/internal/store"
)

const (
	defaultInterval = 5 * time.Minute
	testConcurrency = 8
)

// Service ties together the stored outbounds, the xray process serving them, the shared egress
// registry the scheduler consults, and periodic liveness tests whose results decide which
// proxies a job may fail over to.
type Service struct {
	St  *store.Store
	Mgr *Manager
	Reg *egress.Registry
	Log *logbus.Logger

	// TestURL returns the URL probed through each proxy (a setting, read on every test).
	TestURL      func() string
	TraceURL     string        // default DefaultTraceURL
	ProbeTimeout time.Duration // default 10s
	Interval     time.Duration // default 5 minutes

	reload sync.Mutex
}

func (s *Service) probeOptions() ProbeOptions {
	o := ProbeOptions{TraceURL: s.TraceURL, Timeout: s.ProbeTimeout}
	if s.TestURL != nil {
		o.TestURL = s.TestURL()
	}
	if o.TraceURL == "" {
		o.TraceURL = DefaultTraceURL
	}
	return o
}

// Reload rebuilds the xray configuration from the enabled outbounds and publishes them to the
// egress registry. If xray cannot run, the registry is emptied so no job is routed into a dead
// listener; the error is returned (and kept in Status).
func (s *Service) Reload(ctx context.Context) error {
	s.reload.Lock()
	defer s.reload.Unlock()
	list, err := s.St.ListOutbounds(ctx)
	if err != nil {
		return err
	}
	var entries []Entry
	byID := map[int64]store.Outbound{}
	for _, o := range list {
		if o.Enabled {
			entries = append(entries, Entry{ID: o.ID, Config: o.Config})
			byID[o.ID] = o
		}
	}
	ports, err := s.Mgr.Apply(ctx, entries)
	var cfgErr *ConfigError
	if errors.As(err, &cfgErr) && len(entries) > 1 {
		// xray refused the combined configuration: find the culprit(s), keep the rest running
		entries, err = s.dropRejected(ctx, entries, byID)
		if err == nil {
			ports, err = s.Mgr.Apply(ctx, entries)
		}
	} else if errors.As(err, &cfgErr) {
		_ = s.St.SaveOutboundTest(ctx, entries[0].ID, false, 0, cfgErr.Message, "", "")
	}
	if err != nil {
		s.Reg.Replace(nil)
		s.Log.Error("reload_failed", 0, "出站代理重载失败,代理池已清空:"+err.Error(), logbus.Fields{"error": err.Error(), "outbounds": len(entries)})
		return err
	}
	var pe []egress.ProxyEntry
	for id, port := range ports {
		pe = append(pe, egress.ProxyEntry{ID: egress.ProxyID(id), Name: byID[id].Name,
			Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), Enabled: true})
	}
	s.Reg.Replace(pe)
	s.Log.Info("reloaded", 0, fmt.Sprintf("出站代理已重载:%d 个启用(共 %d 个)", len(entries), len(list)),
		logbus.Fields{"enabled": len(entries), "total": len(list)})
	return nil
}

// dropRejected checks every entry on its own with xray's config test, records why the rejected
// ones failed, and returns the entries xray accepts.
func (s *Service) dropRejected(ctx context.Context, entries []Entry, byID map[int64]store.Outbound) ([]Entry, error) {
	var good []Entry
	for _, e := range entries {
		err := s.Mgr.CheckConfig(ctx, e.Config)
		if err == nil {
			good = append(good, e)
			continue
		}
		msg := err.Error()
		var ce *ConfigError
		if errors.As(err, &ce) {
			msg = ce.Message
		}
		_ = s.St.SaveOutboundTest(ctx, e.ID, false, 0, "xray 拒绝了配置:"+msg, "", "")
		s.Reg.Replace(nil) // never leave a stale view behind while the set changes
		s.Log.Warn("rejected", 0, fmt.Sprintf("出站代理「%s」被 xray 拒绝,已跳过:%s", byID[e.ID].Name, msg),
			logbus.Fields{"id": e.ID, "error": msg})
	}
	return good, nil
}

// TestOne tests one stored outbound, saves the result and updates the registry's health view.
// An enabled outbound is tested through the running xray; a disabled one on a throw-away
// instance, so it can be checked before being switched on.
func (s *Service) TestOne(ctx context.Context, id int64) (ProbeResult, error) {
	o, err := s.St.GetOutbound(ctx, id)
	if err != nil {
		return ProbeResult{}, err
	}
	opts := s.probeOptions()
	var res ProbeResult
	started := time.Now()
	if port, ok := s.Mgr.Port(id); ok && o.Enabled {
		res = s.Mgr.explain(Probe(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), opts), o.Address, started)
	} else {
		res = s.Mgr.TestConfig(ctx, o.Config, opts)
	}
	_ = s.St.SaveOutboundTest(ctx, id, res.OK, res.DelayMS, res.Error, res.IP, res.Country)
	if o.Enabled {
		s.Reg.SetHealth(egress.ProxyID(id), res.OK)
	}
	f := logbus.Fields{"egress": egress.ProxyID(id), "ok": res.OK, "delay_ms": res.DelayMS, "cold_ms": res.ColdMS,
		"status": res.Status, "ip": res.IP, "country": res.Country, "name": o.Name, "protocol": o.Protocol, "address": o.Address}
	if res.OK {
		s.Log.Info("test_ok", 0, fmt.Sprintf("代理「%s」可用:延迟 %dms(冷启动 %dms),出口 %s %s", o.Name, res.DelayMS, res.ColdMS, res.IP, res.Country), f)
	} else {
		f["error"] = res.Error
		s.Log.Warn("test_failed", 0, fmt.Sprintf("代理「%s」测活失败:%s", o.Name, res.Error), f)
	}
	return res, nil
}

// TestAll tests every enabled outbound (several at once) and returns the results by id.
func (s *Service) TestAll(ctx context.Context) map[int64]ProbeResult {
	list, err := s.St.ListOutbounds(ctx)
	if err != nil {
		return nil
	}
	out := map[int64]ProbeResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, testConcurrency)
	for _, o := range list {
		if !o.Enabled {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(id int64) {
			defer wg.Done()
			defer func() { <-sem }()
			if res, err := s.TestOne(ctx, id); err == nil {
				mu.Lock()
				out[id] = res
				mu.Unlock()
			}
		}(o.ID)
	}
	wg.Wait()
	return out
}

// TestConfig tests an arbitrary outbound config (not stored) on a throw-away instance.
func (s *Service) TestConfig(ctx context.Context, cfg []byte) ProbeResult {
	return s.Mgr.TestConfig(ctx, cfg, s.probeOptions())
}

// Run loads the proxies, tests them once, then keeps re-testing on an interval until ctx ends.
func (s *Service) Run(ctx context.Context) {
	if err := s.Reload(ctx); err == nil {
		s.TestAll(ctx)
	}
	every := s.Interval
	if every <= 0 {
		every = defaultInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Log.Debug("periodic_test", 0, "开始周期性测活", nil)
			s.TestAll(ctx)
		}
	}
}

// Status is a snapshot for the UI.
type Status struct {
	XrayAvailable bool            `json:"xray_available"`
	Running       bool            `json:"running"`
	Error         string          `json:"error"`
	Egresses      []egress.Status `json:"egresses"`
}

func (s *Service) Status() Status {
	return Status{XrayAvailable: s.Mgr.Available(), Running: s.Mgr.Running(), Error: s.Mgr.LastError(), Egresses: s.Reg.Snapshot()}
}
