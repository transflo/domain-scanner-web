package verify

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"domain_scanner/internal/appsettings"
	"domain_scanner/internal/cloudflare"
	"domain_scanner/internal/logbus"
	"domain_scanner/internal/notifier"
	"domain_scanner/internal/scheduler"
	"domain_scanner/internal/store"
)

type fakeCF struct {
	mu         sync.Mutex
	configured bool
	results    map[string]cloudflare.Domain
	errs       []error // returned one per call, then nil
	calls      [][]string
}

func (f *fakeCF) Configured() bool { return f.configured }

func (f *fakeCF) Check(_ context.Context, domains []string) ([]cloudflare.Domain, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), domains...))
	if len(f.errs) > 0 {
		e := f.errs[0]
		f.errs = f.errs[1:]
		if e != nil {
			return nil, e
		}
	}
	var out []cloudflare.Domain
	for _, d := range domains {
		if r, ok := f.results[d]; ok {
			r.Name = d
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeCF) callCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

type sentBatch struct {
	job   string
	items []notifier.Item
}

type fakeSink struct {
	mu   sync.Mutex
	sent []sentBatch
}

func (s *fakeSink) NotifyItems(job string, items []notifier.Item) {
	s.mu.Lock()
	s.sent = append(s.sent, sentBatch{job, items})
	s.mu.Unlock()
}

func (s *fakeSink) all() []notifier.Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []notifier.Item
	for _, b := range s.sent {
		out = append(out, b.items...)
	}
	return out
}

type env struct {
	v      *Verifier
	st     *store.Store
	cf     *fakeCF
	sink   *fakeSink
	policy *appsettings.RegisterPolicy
}

func newEnv(t *testing.T, tune ...func(*Verifier)) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bus := logbus.New(nil, 100)
	t.Cleanup(bus.Close)
	cf := &fakeCF{configured: true, results: map[string]cloudflare.Domain{}}
	sink := &fakeSink{}
	pol := &appsettings.RegisterPolicy{Confirm: true, MaxPrice: 30, DailyCap: 5, PushUnconfirmed: true}
	v := &Verifier{St: st, CF: cf, Sink: sink, Policy: func() appsettings.RegisterPolicy { return *pol }, Log: bus.Logger("cloudflare"),
		FlushEvery: 20 * time.Millisecond, RetryBase: 5 * time.Millisecond}
	for _, f := range tune {
		f(v)
	}
	v.Start(context.Background())
	t.Cleanup(v.Stop)
	return &env{v: v, st: st, cf: cf, sink: sink, policy: pol}
}

func (e *env) hit(t *testing.T, job, domain string) scheduler.Hit {
	t.Helper()
	ctx := context.Background()
	jid, err := e.st.CreateJob(ctx, &store.Job{Name: job, Suffix: ".com", Pattern: "d", Length: 1, Workers: 1, Status: "running", Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	r := &store.Result{JobID: jid, Domain: domain, Status: "available"}
	if ok, err := e.st.InsertResult(ctx, r); err != nil || !ok {
		t.Fatalf("insert: %v %v", ok, err)
	}
	return scheduler.Hit{ResultID: r.ID, JobID: jid, JobName: job, Domain: domain}
}

func (e *env) waitSent(t *testing.T, n int) []notifier.Item {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if items := e.sink.all(); len(items) >= n {
			return items
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d notified items, got %v", n, e.sink.all())
	return nil
}

func (e *env) cfStatus(t *testing.T, id int64) *store.Result {
	t.Helper()
	r, err := e.st.GetResult(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) waitCF(t *testing.T, id int64, want string) *store.Result {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r := e.cfStatus(t, id); r.CFStatus == want {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("result %d never reached cf_status %q (now %q)", id, want, e.cfStatus(t, id).CFStatus)
	return nil
}

func TestWithoutCloudflareTheHitIsAnnouncedImmediatelyAndLabelled(t *testing.T) {
	e := newEnv(t)
	e.cf.configured = false
	h := e.hit(t, "job", "free.com")
	e.v.Found(h)
	items := e.waitSent(t, 1)
	if items[0].Domain != "free.com" || items[0].Status != notifier.StatusUnconfirmed || !strings.Contains(items[0].Note, "未配置") || items[0].ResultID != h.ResultID {
		t.Fatalf("item = %+v", items[0])
	}
	if e.cf.callCount() != 0 {
		t.Fatal("no Cloudflare call without credentials")
	}
}

func TestConfirmedDomainIsStoredAndAnnouncedWithItsPrice(t *testing.T) {
	e := newEnv(t)
	e.cf.results["free.com"] = cloudflare.Domain{Registrable: true, Tier: "standard", Currency: "USD", RegistrationCost: "10.46", RenewalCost: "10.46"}
	h := e.hit(t, "job", "free.com")
	e.v.Found(h)
	items := e.waitSent(t, 1)
	if items[0].Status != notifier.StatusConfirmed || items[0].Price != "10.46" || items[0].Currency != "USD" || items[0].ResultID != h.ResultID {
		t.Fatalf("item = %+v", items[0])
	}
	r := e.waitCF(t, h.ResultID, "confirmed")
	if r.CFPrice != "10.46" || r.CFCheckedAt == nil {
		t.Fatalf("stored = %+v", r)
	}
}

func TestPendingIsRecordedAtOnce(t *testing.T) {
	e := newEnv(t)
	e.cf.errs = []error{errors.New("slow")} // keep the check from finishing immediately
	h := e.hit(t, "job", "free.com")
	e.v.Found(h)
	if got := e.cfStatus(t, h.ResultID).CFStatus; got != "pending" {
		t.Fatalf("cf_status right after Found = %q, want pending", got)
	}
}

func TestRejectedDomainIsNeverAnnounced(t *testing.T) {
	e := newEnv(t)
	e.cf.results["taken.com"] = cloudflare.Domain{Registrable: false, Reason: "domain_unavailable"}
	h := e.hit(t, "job", "taken.com")
	e.v.Found(h)
	r := e.waitCF(t, h.ResultID, "rejected")
	if r.CFReason != "domain_unavailable" {
		t.Fatalf("reason = %q", r.CFReason)
	}
	time.Sleep(80 * time.Millisecond)
	if got := e.sink.all(); len(got) != 0 {
		t.Fatalf("a rejected domain was announced: %+v", got)
	}
}

func TestPremiumDomainsAreRejectedToo(t *testing.T) {
	e := newEnv(t)
	e.cf.results["gem.com"] = cloudflare.Domain{Registrable: false, Tier: "premium", Reason: "domain_premium"}
	h := e.hit(t, "job", "gem.com")
	e.v.Found(h)
	e.waitCF(t, h.ResultID, "rejected")
	time.Sleep(60 * time.Millisecond)
	if len(e.sink.all()) != 0 {
		t.Fatal("premium domains cannot be registered through the API; do not announce them")
	}
}

func TestUnsupportedExtensionIsAnnouncedUnconfirmedOnlyWhenAllowed(t *testing.T) {
	e := newEnv(t)
	e.cf.results["x.li"] = cloudflare.Domain{Registrable: false, Reason: "extension_not_supported"}
	h := e.hit(t, "job", "x.li")
	e.v.Found(h)
	items := e.waitSent(t, 1)
	if items[0].Status != notifier.StatusUnconfirmed || !strings.Contains(items[0].Note, "不支持") {
		t.Fatalf("item = %+v", items[0])
	}
	e.waitCF(t, h.ResultID, "unsupported")

	e.policy.PushUnconfirmed = false
	h2 := e.hit(t, "job", "y.li")
	e.cf.results["y.li"] = cloudflare.Domain{Registrable: false, Reason: "extension_not_supported_via_api"}
	e.v.Found(h2)
	e.waitCF(t, h2.ResultID, "unsupported")
	time.Sleep(60 * time.Millisecond)
	if len(e.sink.all()) != 1 {
		t.Fatalf("with push_unconfirmed off nothing more may be announced: %+v", e.sink.all())
	}
}

func TestBatchesNeverExceedTwentyDomains(t *testing.T) {
	e := newEnv(t, func(v *Verifier) { v.FlushEvery = time.Hour }) // only a full batch triggers early
	var hits []scheduler.Hit
	for i := 0; i < 45; i++ {
		d := "d" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".com"
		e.cf.results[d] = cloudflare.Domain{Registrable: true, Currency: "USD", RegistrationCost: "9"}
		hits = append(hits, e.hit(t, "job", d))
	}
	for _, h := range hits {
		e.v.Found(h)
	}
	e.waitSent(t, 40) // two full batches go out by themselves
	e.v.Stop()        // the remaining 5 are flushed on shutdown
	if got := len(e.sink.all()); got != 45 {
		t.Fatalf("announced %d of 45", got)
	}
	e.cf.mu.Lock()
	defer e.cf.mu.Unlock()
	for i, c := range e.cf.calls {
		if len(c) > 20 {
			t.Fatalf("call %d carried %d domains; the API accepts at most 20", i, len(c))
		}
	}
	if len(e.cf.calls) != 3 {
		t.Fatalf("calls = %d, want 3 (20 + 20 + 5)", len(e.cf.calls))
	}
}

func TestTemporaryErrorsAreRetried(t *testing.T) {
	e := newEnv(t)
	e.cf.results["free.com"] = cloudflare.Domain{Registrable: true, Currency: "USD", RegistrationCost: "9"}
	e.cf.errs = []error{&cloudflare.APIError{Status: 429, Message: "slow down"}, &cloudflare.APIError{Status: 502, Message: "bad gateway"}}
	h := e.hit(t, "job", "free.com")
	e.v.Found(h)
	e.waitCF(t, h.ResultID, "confirmed")
	if e.cf.callCount() != 3 {
		t.Fatalf("calls = %d, want 3 (two failures, then success)", e.cf.callCount())
	}
}

func TestPersistentTemporaryErrorsEndAsAnErrorStatus(t *testing.T) {
	e := newEnv(t)
	e.cf.errs = []error{&cloudflare.APIError{Status: 503}, &cloudflare.APIError{Status: 503}, &cloudflare.APIError{Status: 503}, &cloudflare.APIError{Status: 503}}
	h := e.hit(t, "job", "free.com")
	e.v.Found(h)
	r := e.waitCF(t, h.ResultID, "error")
	if r.CFReason == "" {
		t.Fatal("the failure reason must be stored")
	}
	items := e.waitSent(t, 1)
	if items[0].Status != notifier.StatusUnconfirmed || !strings.Contains(items[0].Note, "校验失败") {
		t.Fatalf("item = %+v", items[0])
	}
}

func TestAuthErrorsAreNotRetried(t *testing.T) {
	e := newEnv(t)
	e.cf.errs = []error{&cloudflare.APIError{Status: 403, Code: 10000, Message: "Authentication error"}}
	h := e.hit(t, "job", "free.com")
	e.v.Found(h)
	r := e.waitCF(t, h.ResultID, "error")
	if e.cf.callCount() != 1 {
		t.Fatalf("calls = %d; a rejected token will not become valid by retrying", e.cf.callCount())
	}
	if !strings.Contains(r.CFReason, "Authentication") {
		t.Fatalf("reason = %q", r.CFReason)
	}
}

func TestMissingDomainInTheResponseIsAnError(t *testing.T) {
	e := newEnv(t)
	h := e.hit(t, "job", "ghost.com") // the fake returns nothing for it
	e.v.Found(h)
	e.waitCF(t, h.ResultID, "error")
}

func TestHitsAreGroupedByJobWhenAnnounced(t *testing.T) {
	e := newEnv(t, func(v *Verifier) { v.FlushEvery = 50 * time.Millisecond })
	for _, d := range []string{"a.com", "b.com", "c.com"} {
		e.cf.results[d] = cloudflare.Domain{Registrable: true, Currency: "USD", RegistrationCost: "9"}
	}
	e.v.Found(e.hit(t, "alpha", "a.com"))
	e.v.Found(e.hit(t, "beta", "b.com"))
	e.v.Found(e.hit(t, "alpha", "c.com"))
	e.waitSent(t, 3)
	e.sink.mu.Lock()
	defer e.sink.mu.Unlock()
	perJob := map[string]int{}
	for _, b := range e.sink.sent {
		perJob[b.job] += len(b.items)
	}
	if perJob["alpha"] != 2 || perJob["beta"] != 1 {
		t.Fatalf("grouping = %v", perJob)
	}
}

func TestStopFlushesWhatIsQueued(t *testing.T) {
	e := newEnv(t, func(v *Verifier) { v.FlushEvery = time.Hour })
	e.cf.results["free.com"] = cloudflare.Domain{Registrable: true, Currency: "USD", RegistrationCost: "9"}
	e.v.Found(e.hit(t, "job", "free.com"))
	e.v.Stop()
	if len(e.sink.all()) != 1 {
		t.Fatalf("queued hit lost on shutdown: %+v", e.sink.all())
	}
}
