package register

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"domain_scanner/internal/appsettings"
	"domain_scanner/internal/cloudflare"
	"domain_scanner/internal/logbus"
	"domain_scanner/internal/store"
)

type fakeCF struct {
	mu          sync.Mutex
	checkErr    error
	checkResult cloudflare.Domain
	checks      int
	registers   []string
	regResult   cloudflare.RegisterResult
	regErr      error
	regDelay    time.Duration
}

func (f *fakeCF) Check(_ context.Context, domains []string) ([]cloudflare.Domain, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	if f.checkErr != nil {
		return nil, f.checkErr
	}
	d := f.checkResult
	d.Name = domains[0]
	return []cloudflare.Domain{d}, nil
}

func (f *fakeCF) Register(_ context.Context, domain string) (cloudflare.RegisterResult, error) {
	f.mu.Lock()
	f.registers = append(f.registers, domain)
	res, err, delay := f.regResult, f.regErr, f.regDelay
	f.mu.Unlock()
	time.Sleep(delay)
	res.Domain = domain
	return res, err
}

func (f *fakeCF) registerCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.registers) }

type env struct {
	svc    *Service
	st     *store.Store
	cf     *fakeCF
	policy *appsettings.RegisterPolicy
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bus := logbus.New(nil, 100)
	t.Cleanup(bus.Close)
	cf := &fakeCF{
		checkResult: cloudflare.Domain{Registrable: true, Tier: "standard", Currency: "USD", RegistrationCost: "10.46", RenewalCost: "10.46"},
		regResult:   cloudflare.RegisterResult{State: "succeeded", Completed: true, RegistrationStatus: "active"},
	}
	pol := &appsettings.RegisterPolicy{Confirm: true, MaxPrice: 30, DailyCap: 5, PushUnconfirmed: true}
	svc := &Service{St: st, CF: cf, Policy: func() appsettings.RegisterPolicy { return *pol }, Log: bus.Logger("register")}
	return &env{svc: svc, st: st, cf: cf, policy: pol}
}

func (e *env) confirmed(t *testing.T, domain string) int64 {
	t.Helper()
	ctx := context.Background()
	jid, _ := e.st.CreateJob(ctx, &store.Job{Name: "j", Suffix: ".com", Pattern: "d", Length: 1, Workers: 1, Status: "done", Total: 1})
	r := &store.Result{JobID: jid, Domain: domain, Status: "available"}
	if ok, err := e.st.InsertResult(ctx, r); err != nil || !ok {
		t.Fatal(err)
	}
	e.st.UpdateResultCF(ctx, r.ID, store.CFUpdate{Status: "confirmed", Price: "10.46", Currency: "USD"})
	return r.ID
}

func isBlocked(err error) (*BlockedError, bool) {
	var b *BlockedError
	return b, errors.As(err, &b)
}

func TestPreviewReturnsAFreshPrice(t *testing.T) {
	e := newEnv(t)
	id := e.confirmed(t, "free.com")
	e.cf.checkResult.RegistrationCost = "11.00" // price moved since the first check
	p, err := e.svc.Preview(context.Background(), id)
	if err != nil || p.Domain != "free.com" || p.Price != "11.00" || p.Currency != "USD" || !p.Confirm {
		t.Fatalf("preview = %+v err=%v", p, err)
	}
	got, _ := e.st.GetResult(context.Background(), id)
	if got.CFPrice != "11.00" {
		t.Fatalf("the stored price must follow the fresh check, got %q", got.CFPrice)
	}
	if e.cf.registerCount() != 0 {
		t.Fatal("a preview must never register anything")
	}
}

func TestPreviewRefusesAnUnconfirmedResult(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	jid, _ := e.st.CreateJob(ctx, &store.Job{Name: "j", Suffix: ".li", Pattern: "d", Length: 1, Workers: 1, Status: "done", Total: 1})
	r := &store.Result{JobID: jid, Domain: "x.li", Status: "available"}
	e.st.InsertResult(ctx, r)
	e.st.UpdateResultCF(ctx, r.ID, store.CFUpdate{Status: "unsupported", Reason: "extension_not_supported"})
	if _, err := e.svc.Preview(ctx, r.ID); err == nil {
		t.Fatal("expected a refusal")
	} else if b, ok := isBlocked(err); !ok || !strings.Contains(b.Reason, "Cloudflare") {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.svc.Preview(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing result: err = %v", err)
	}
}

func TestPreviewDetectsADomainThatIsNoLongerRegistrable(t *testing.T) {
	e := newEnv(t)
	id := e.confirmed(t, "free.com")
	e.cf.checkResult = cloudflare.Domain{Registrable: false, Reason: "domain_unavailable"}
	_, err := e.svc.Preview(context.Background(), id)
	if b, ok := isBlocked(err); !ok || !strings.Contains(b.Reason, "domain_unavailable") {
		t.Fatalf("err = %v", err)
	}
	got, _ := e.st.GetResult(context.Background(), id)
	if got.CFStatus != "rejected" || got.CFReason != "domain_unavailable" {
		t.Fatalf("the stored verdict must be corrected: %+v", got)
	}
}

func TestCheckFailureBlocksRatherThanGuesses(t *testing.T) {
	e := newEnv(t)
	id := e.confirmed(t, "free.com")
	e.cf.checkErr = errors.New("boom")
	if _, err := e.svc.Preview(context.Background(), id); err == nil {
		t.Fatal("when the fresh check fails nothing may be registered")
	} else if _, ok := isBlocked(err); !ok {
		t.Fatalf("err = %v, want a BlockedError", err)
	}
	if _, err := e.svc.Register(context.Background(), id); err == nil || e.cf.registerCount() != 0 {
		t.Fatalf("register with a failing check: err=%v registers=%d", err, e.cf.registerCount())
	}
}

func TestPriceCapBlocksRegistration(t *testing.T) {
	e := newEnv(t)
	id := e.confirmed(t, "free.com")
	e.policy.MaxPrice = 10
	if _, err := e.svc.Register(context.Background(), id); err == nil {
		t.Fatal("price 10.46 > cap 10 must be refused")
	} else if b, ok := isBlocked(err); !ok || !strings.Contains(b.Reason, "10.46") {
		t.Fatalf("err = %v", err)
	}
	if e.cf.registerCount() != 0 {
		t.Fatal("no registration may be attempted over the cap")
	}
	e.policy.MaxPrice = 0 // 0 = unlimited
	if _, err := e.svc.Register(context.Background(), id); err != nil {
		t.Fatalf("with no cap: %v", err)
	}
}

func TestUnknownPriceIsRefusedWhenACapIsSet(t *testing.T) {
	e := newEnv(t)
	id := e.confirmed(t, "free.com")
	e.cf.checkResult.RegistrationCost = ""
	if _, err := e.svc.Register(context.Background(), id); err == nil {
		t.Fatal("without a price the cap cannot be enforced")
	}
}

func TestDailyCapBlocksTheNextRegistration(t *testing.T) {
	e := newEnv(t)
	e.policy.DailyCap = 2
	for i, d := range []string{"a.com", "b.com"} {
		if out, err := e.svc.Register(context.Background(), e.confirmed(t, d)); err != nil || out.Status != "succeeded" {
			t.Fatalf("registration %d: %+v %v", i, out, err)
		}
	}
	c := e.confirmed(t, "c.com")
	_, err := e.svc.Register(context.Background(), c)
	if b, ok := isBlocked(err); !ok || !strings.Contains(b.Reason, "上限") {
		t.Fatalf("third registration: err = %v", err)
	}
	if e.cf.registerCount() != 2 {
		t.Fatalf("registers = %d, want 2", e.cf.registerCount())
	}
}

func TestRegisterSuccessIsRecordedAndNotRepeatable(t *testing.T) {
	e := newEnv(t)
	id := e.confirmed(t, "free.com")
	out, err := e.svc.Register(context.Background(), id)
	if err != nil || out.Status != "succeeded" || out.Domain != "free.com" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	got, _ := e.st.GetResult(context.Background(), id)
	if got.RegisterStatus != "succeeded" {
		t.Fatalf("status = %q", got.RegisterStatus)
	}
	if _, err := e.svc.Register(context.Background(), id); !errors.Is(err, ErrAlreadyRegistered) {
		t.Fatalf("second register: err = %v, want ErrAlreadyRegistered", err)
	}
	if e.cf.registerCount() != 1 {
		t.Fatalf("the charge happened %d times", e.cf.registerCount())
	}
}

func TestDoubleTapChargesOnce(t *testing.T) {
	e := newEnv(t)
	id := e.confirmed(t, "free.com")
	e.cf.regDelay = 80 * time.Millisecond
	var wg sync.WaitGroup
	var ok, busy atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := e.svc.Register(context.Background(), id)
			switch {
			case err == nil && out.Status == "succeeded":
				ok.Add(1)
			case errors.Is(err, ErrInProgress), errors.Is(err, ErrAlreadyRegistered):
				busy.Add(1)
			default:
				t.Errorf("unexpected: %+v %v", out, err)
			}
		}()
	}
	wg.Wait()
	if e.cf.registerCount() != 1 || ok.Load() != 1 {
		t.Fatalf("registers=%d ok=%d busy=%d; exactly one request may spend money", e.cf.registerCount(), ok.Load(), busy.Load())
	}
}

func TestFailedRegistrationIsReportedAndRetryable(t *testing.T) {
	e := newEnv(t)
	id := e.confirmed(t, "free.com")
	e.cf.regErr = &cloudflare.APIError{Status: 400, Code: 10000, Message: "no default payment method"}
	out, err := e.svc.Register(context.Background(), id)
	if err != nil || out.Status != "failed" || !strings.Contains(out.Message, "payment method") {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	got, _ := e.st.GetResult(context.Background(), id)
	if got.RegisterStatus != "failed" || !strings.Contains(got.RegisterNote, "payment method") {
		t.Fatalf("stored = %+v", got)
	}
	e.cf.regErr = nil
	if out, err = e.svc.Register(context.Background(), id); err != nil || out.Status != "succeeded" {
		t.Fatalf("retry: %+v %v", out, err)
	}
}

func TestAWorkflowThatFailsAtCloudflareIsAFailure(t *testing.T) {
	e := newEnv(t)
	id := e.confirmed(t, "free.com")
	e.cf.regResult = cloudflare.RegisterResult{State: "failed", Completed: true, ErrorMessage: "registry rejected"}
	out, err := e.svc.Register(context.Background(), id)
	if err != nil || out.Status != "failed" || !strings.Contains(out.Message, "registry rejected") {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestPendingRegistrationStaysRegistering(t *testing.T) {
	e := newEnv(t)
	id := e.confirmed(t, "free.com")
	e.cf.regResult = cloudflare.RegisterResult{State: "in_progress", Completed: false}
	out, err := e.svc.Register(context.Background(), id)
	if err != nil || out.Status != "pending" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	got, _ := e.st.GetResult(context.Background(), id)
	if got.RegisterStatus != "registering" {
		t.Fatalf("status = %q; an unfinished registration must keep blocking duplicates", got.RegisterStatus)
	}
	if _, err := e.svc.Register(context.Background(), id); !errors.Is(err, ErrInProgress) {
		t.Fatalf("second register while pending: %v", err)
	}
}
