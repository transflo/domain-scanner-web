package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func seedResult(t *testing.T, s *Store, domain string) *Result {
	t.Helper()
	jid, _ := s.CreateJob(context.Background(), newJob("j"))
	r := &Result{JobID: jid, Domain: domain, Status: "available"}
	if ok, err := s.InsertResult(context.Background(), r); err != nil || !ok {
		t.Fatalf("insert: %v %v", ok, err)
	}
	return r
}

func TestInsertResultReportsTheNewID(t *testing.T) {
	s := openTemp(t)
	r := seedResult(t, s, "a.com")
	if r.ID == 0 {
		t.Fatal("InsertResult must fill in the new row id (the verifier and Telegram buttons refer to it)")
	}
	got, err := s.GetResult(context.Background(), r.ID)
	if err != nil || got.Domain != "a.com" || got.CFStatus != "" || got.RegisterStatus != "" {
		t.Fatalf("get = %+v err=%v", got, err)
	}
}

func TestUpdateResultCF(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	r := seedResult(t, s, "a.com")
	if err := s.UpdateResultCF(ctx, r.ID, CFUpdate{Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetResult(ctx, r.ID)
	if got.CFStatus != "pending" || got.CFCheckedAt != nil {
		t.Fatalf("pending must not claim a check time: %+v", got)
	}
	err := s.UpdateResultCF(ctx, r.ID, CFUpdate{Status: "confirmed", Price: "10.46", Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetResult(ctx, r.ID)
	if got.CFStatus != "confirmed" || got.CFPrice != "10.46" || got.CFCurrency != "USD" || got.CFCheckedAt == nil {
		t.Fatalf("confirmed = %+v", got)
	}
	s.UpdateResultCF(ctx, r.ID, CFUpdate{Status: "rejected", Reason: "domain_unavailable"})
	got, _ = s.GetResult(ctx, r.ID)
	if got.CFReason != "domain_unavailable" || got.CFPrice != "" {
		t.Fatalf("a later verdict must replace the earlier one: %+v", got)
	}
	if err := s.UpdateResultCF(ctx, 9999, CFUpdate{Status: "confirmed"}); err != ErrNotFound {
		t.Fatalf("missing row: err = %v, want ErrNotFound", err)
	}
}

func TestListResultsFiltersByCFStatus(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	a := seedResult(t, s, "a.com")
	b := seedResult(t, s, "b.com")
	s.UpdateResultCF(ctx, a.ID, CFUpdate{Status: "confirmed", Price: "9", Currency: "USD"})
	s.UpdateResultCF(ctx, b.ID, CFUpdate{Status: "rejected", Reason: "domain_unavailable"})
	rows, total, err := s.ListResults(ctx, ResultFilter{CFStatus: "confirmed", Limit: 10})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].Domain != "a.com" {
		t.Fatalf("rows=%+v total=%d err=%v", rows, total, err)
	}
}

func TestClaimRegistrationOnlyForConfirmedAndOnlyOnce(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	r := seedResult(t, s, "a.com")
	if ok, err := s.ClaimRegistration(ctx, r.ID); err != nil || ok {
		t.Fatalf("an unconfirmed result must not be claimable: ok=%v err=%v", ok, err)
	}
	s.UpdateResultCF(ctx, r.ID, CFUpdate{Status: "confirmed", Price: "10", Currency: "USD"})
	if ok, err := s.ClaimRegistration(ctx, r.ID); err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if ok, _ := s.ClaimRegistration(ctx, r.ID); ok {
		t.Fatal("a second claim while registering must fail (double tap)")
	}
	got, _ := s.GetResult(ctx, r.ID)
	if got.RegisterStatus != "registering" {
		t.Fatalf("status = %q", got.RegisterStatus)
	}
	s.SetRegisterOutcome(ctx, r.ID, "succeeded", "ok")
	if ok, _ := s.ClaimRegistration(ctx, r.ID); ok {
		t.Fatal("an already registered domain must not be claimable again")
	}
	// a failed attempt may be retried
	r2 := seedResult(t, s, "b.com")
	s.UpdateResultCF(ctx, r2.ID, CFUpdate{Status: "confirmed"})
	s.ClaimRegistration(ctx, r2.ID)
	s.SetRegisterOutcome(ctx, r2.ID, "failed", "no payment method")
	if ok, _ := s.ClaimRegistration(ctx, r2.ID); !ok {
		t.Fatal("a failed registration must be retryable")
	}
}

func TestClaimRegistrationIsRaceSafe(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	r := seedResult(t, s, "a.com")
	s.UpdateResultCF(ctx, r.ID, CFUpdate{Status: "confirmed"})
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := s.ClaimRegistration(ctx, r.ID); err == nil && ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d concurrent claims won; exactly one may spend the money", wins.Load())
	}
}

func TestCountRegistrationsSince(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	for _, d := range []string{"a.com", "b.com", "c.com"} {
		r := seedResult(t, s, d)
		s.UpdateResultCF(ctx, r.ID, CFUpdate{Status: "confirmed"})
		s.ClaimRegistration(ctx, r.ID)
		if d == "c.com" {
			s.SetRegisterOutcome(ctx, r.ID, "failed", "x") // failures do not count
		}
	}
	n, err := s.CountRegistrationsSince(ctx, time.Now().Add(-time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("count = %d err=%v, want 2 (in flight + succeeded count, failed does not)", n, err)
	}
	if n, _ := s.CountRegistrationsSince(ctx, time.Now().Add(time.Hour)); n != 0 {
		t.Fatalf("count in the future = %d", n)
	}
}
