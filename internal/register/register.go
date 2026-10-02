// Package register spends money: it turns a Cloudflare-confirmed result into a real domain
// registration. Everything here is built to fail closed — a fresh availability check, a price
// cap, a daily cap and an atomic claim stand between a button press and the charge.
package register

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"domain_scanner/internal/appsettings"
	"domain_scanner/internal/cloudflare"
	"domain_scanner/internal/logbus"
	"domain_scanner/internal/store"
)

var (
	ErrNotFound          = errors.New("结果不存在(可能任务已被删除)")
	ErrAlreadyRegistered = errors.New("该域名已经注册成功")
	ErrInProgress        = errors.New("该域名正在注册中,请稍候")
)

// BlockedError is a refusal with a reason meant to be shown to the user as is.
type BlockedError struct{ Reason string }

func (e *BlockedError) Error() string { return e.Reason }

func blocked(format string, args ...any) error {
	return &BlockedError{Reason: fmt.Sprintf(format, args...)}
}

// CF is the part of the Cloudflare client this package needs.
type CF interface {
	Check(ctx context.Context, domains []string) ([]cloudflare.Domain, error)
	Register(ctx context.Context, domain string) (cloudflare.RegisterResult, error)
}

// Service performs previews and registrations.
type Service struct {
	St     *store.Store
	CF     CF
	Policy func() appsettings.RegisterPolicy
	Log    *logbus.Logger
	Now    func() time.Time // default time.Now
}

// Preview is what the user is asked to confirm.
type Preview struct {
	ResultID int64
	Domain   string
	Price    string
	Currency string
	// Confirm says whether the policy wants an explicit second confirmation before charging.
	Confirm bool
}

// Outcome is the end state of Register. Status is succeeded, pending (Cloudflare is still
// processing it) or failed.
type Outcome struct {
	Status  string
	Message string
	Domain  string
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// precheck re-validates everything right before money could move and returns the fresh price.
func (s *Service) precheck(ctx context.Context, id int64) (*store.Result, cloudflare.Domain, appsettings.RegisterPolicy, error) {
	pol := s.Policy()
	r, err := s.St.GetResult(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, cloudflare.Domain{}, pol, ErrNotFound
	}
	if err != nil {
		return nil, cloudflare.Domain{}, pol, err
	}
	switch r.RegisterStatus {
	case "succeeded":
		return r, cloudflare.Domain{}, pol, ErrAlreadyRegistered
	case "registering":
		return r, cloudflare.Domain{}, pol, ErrInProgress
	}
	if r.CFStatus != "confirmed" {
		return r, cloudflare.Domain{}, pol, blocked("该域名没有经过 Cloudflare 确认,不能一键注册")
	}

	// the earlier verdict may be hours old: ask again
	doms, err := s.CF.Check(ctx, []string{r.Domain})
	if err != nil {
		s.Log.Warn("precheck_failed", r.JobID, fmt.Sprintf("注册前重新校验 %s 失败:%v", r.Domain, err), logbus.Fields{"domain": r.Domain, "error": err.Error()})
		return r, cloudflare.Domain{}, pol, blocked("注册前重新校验失败,未执行注册:%v", err)
	}
	if len(doms) == 0 {
		return r, cloudflare.Domain{}, pol, blocked("Cloudflare 没有返回该域名的校验结果,未执行注册")
	}
	d := doms[0]
	if !d.Registrable {
		_ = s.St.UpdateResultCF(ctx, id, store.CFUpdate{Status: "rejected", Reason: d.Reason})
		s.Log.Warn("no_longer_registrable", r.JobID, fmt.Sprintf("%s 已不可注册(%s)", r.Domain, d.Reason), logbus.Fields{"domain": r.Domain, "reason": d.Reason})
		return r, d, pol, blocked("该域名已不可注册(%s)", d.Reason)
	}
	if d.RegistrationCost != "" && d.RegistrationCost != r.CFPrice {
		_ = s.St.UpdateResultCF(ctx, id, store.CFUpdate{Status: "confirmed", Price: d.RegistrationCost, Currency: d.Currency})
	}

	if pol.MaxPrice > 0 {
		price, perr := strconv.ParseFloat(strings.TrimSpace(d.RegistrationCost), 64)
		if perr != nil {
			return r, d, pol, blocked("无法获得该域名的价格,无法核对单价上限(%.2f),未执行注册", pol.MaxPrice)
		}
		if price > pol.MaxPrice {
			s.Log.Warn("price_cap", r.JobID, fmt.Sprintf("%s 价格 %s %s 超过上限 %.2f", r.Domain, d.RegistrationCost, d.Currency, pol.MaxPrice),
				logbus.Fields{"domain": r.Domain, "price": d.RegistrationCost, "cap": pol.MaxPrice})
			return r, d, pol, blocked("价格 %s %s 超过单价上限 %.2f(可在设置里调整)", d.RegistrationCost, d.Currency, pol.MaxPrice)
		}
	}
	if pol.DailyCap > 0 {
		n, err := s.St.CountRegistrationsSince(ctx, s.now().Add(-24*time.Hour))
		if err != nil {
			return r, d, pol, err
		}
		if n >= pol.DailyCap {
			s.Log.Warn("daily_cap", r.JobID, fmt.Sprintf("已达每日注册上限 %d", pol.DailyCap), logbus.Fields{"domain": r.Domain, "count": n, "cap": pol.DailyCap})
			return r, d, pol, blocked("过去 24 小时已注册 %d 个,达到每日上限 %d(可在设置里调整)", n, pol.DailyCap)
		}
	}
	return r, d, pol, nil
}

// Preview validates a registration request and returns the fresh price to show the user. It
// never registers anything.
func (s *Service) Preview(ctx context.Context, id int64) (Preview, error) {
	r, d, pol, err := s.precheck(ctx, id)
	if err != nil {
		return Preview{}, err
	}
	s.Log.Info("preview", r.JobID, fmt.Sprintf("注册预览 %s:%s %s", r.Domain, d.RegistrationCost, d.Currency),
		logbus.Fields{"domain": r.Domain, "price": d.RegistrationCost, "currency": d.Currency, "result_id": id})
	return Preview{ResultID: id, Domain: r.Domain, Price: d.RegistrationCost, Currency: d.Currency, Confirm: pol.Confirm}, nil
}

// Register charges the account and registers the domain. Only one caller can ever get past
// the atomic claim for a given result; others see ErrInProgress / ErrAlreadyRegistered.
func (s *Service) Register(ctx context.Context, id int64) (Outcome, error) {
	r, d, _, err := s.precheck(ctx, id)
	if err != nil {
		return Outcome{}, err
	}
	won, err := s.St.ClaimRegistration(ctx, id)
	if err != nil {
		return Outcome{}, err
	}
	if !won { // lost a race (or the state changed between the check and the claim)
		cur, gerr := s.St.GetResult(ctx, id)
		if gerr == nil && cur.RegisterStatus == "succeeded" {
			return Outcome{}, ErrAlreadyRegistered
		}
		return Outcome{}, ErrInProgress
	}

	f := logbus.Fields{"domain": r.Domain, "price": d.RegistrationCost, "currency": d.Currency, "result_id": id}
	s.Log.Warn("claim", r.JobID, fmt.Sprintf("开始注册 %s(%s %s)——将产生真实扣费", r.Domain, d.RegistrationCost, d.Currency), f)
	t0 := time.Now()
	rctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	res, err := s.CF.Register(rctx, r.Domain)
	f["duration_ms"] = time.Since(t0).Milliseconds()
	if err != nil {
		msg := err.Error()
		_ = s.St.SetRegisterOutcome(context.Background(), id, "failed", msg)
		f["error"] = msg
		s.Log.Error("failed", r.JobID, fmt.Sprintf("注册 %s 失败:%s", r.Domain, msg), f)
		return Outcome{Status: "failed", Message: msg, Domain: r.Domain}, nil
	}
	f["state"] = res.State
	switch {
	case res.Succeeded():
		_ = s.St.SetRegisterOutcome(context.Background(), id, "succeeded", "Cloudflare 注册成功")
		s.Log.Warn("succeeded", r.JobID, fmt.Sprintf("注册成功:%s", r.Domain), f)
		return Outcome{Status: "succeeded", Message: "注册成功", Domain: r.Domain}, nil
	case res.Completed: // failed / blocked terminal states
		msg := res.ErrorMessage
		if msg == "" {
			msg = "Cloudflare 报告状态 " + res.State
		}
		_ = s.St.SetRegisterOutcome(context.Background(), id, "failed", msg)
		f["error"] = msg
		s.Log.Error("failed", r.JobID, fmt.Sprintf("注册 %s 失败:%s", r.Domain, msg), f)
		return Outcome{Status: "failed", Message: msg, Domain: r.Domain}, nil
	default:
		note := "Cloudflare 仍在处理(" + res.State + "),请稍后在 Cloudflare 面板查看"
		_ = s.St.SetRegisterOutcome(context.Background(), id, "registering", note)
		s.Log.Warn("pending", r.JobID, fmt.Sprintf("%s 注册仍在处理中:%s", r.Domain, res.State), f)
		return Outcome{Status: "pending", Message: note, Domain: r.Domain}, nil
	}
}
