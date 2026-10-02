package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newJob(name string) *Job {
	return &Job{Name: name, Suffix: ".li", Pattern: "d", Length: 3, DelayMS: 100, Workers: 2, Status: "queued", Total: 1000}
}

func TestJobRoundTrip(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	id, err := s.CreateJob(ctx, newJob("a"))
	if err != nil || id == 0 {
		t.Fatalf("CreateJob: %d %v", id, err)
	}
	got, err := s.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "a" || got.Suffix != ".li" || got.Total != 1000 || got.Status != "queued" || got.Workers != 2 {
		t.Fatalf("unexpected job %+v", got)
	}
	if err := s.UpdateJobProgress(ctx, id, 10, 9, 2, 1, 6); err != nil {
		t.Fatal(err)
	}
	if err := s.SetJobStatus(ctx, id, "failed", "boom"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetJob(ctx, id)
	if got.Cursor != 10 || got.Checked != 9 || got.Available != 2 || got.Unknown != 1 || got.Registered != 6 ||
		got.Status != "failed" || got.Error != "boom" {
		t.Fatalf("progress not saved: %+v", got)
	}
	if _, err := s.GetJob(ctx, 9999); err != ErrNotFound {
		t.Fatalf("missing job err = %v, want ErrNotFound", err)
	}
}

func TestRecoverableJobsOnlyQueuedOrRunning(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	for _, st := range []string{"queued", "running", "paused", "done", "failed", "cancelled"} {
		id, _ := s.CreateJob(ctx, newJob(st))
		s.SetJobStatus(ctx, id, st, "")
	}
	jobs, err := s.RecoverableJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("recoverable = %d, want 2", len(jobs))
	}
}

func TestInsertResultDeduplicates(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	id, _ := s.CreateJob(ctx, newJob("a"))
	ins, err := s.InsertResult(ctx, &Result{JobID: id, Domain: "x.li", Status: "available", Signatures: ""})
	if err != nil || !ins {
		t.Fatalf("first insert: %v %v", ins, err)
	}
	ins, err = s.InsertResult(ctx, &Result{JobID: id, Domain: "x.li", Status: "available"})
	if err != nil || ins {
		t.Fatalf("duplicate insert should report inserted=false, got %v %v", ins, err)
	}
}

func TestListResultsFilters(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	a, _ := s.CreateJob(ctx, newJob("a"))
	b, _ := s.CreateJob(ctx, newJob("b"))
	for i := 0; i < 5; i++ {
		s.InsertResult(ctx, &Result{JobID: a, Domain: fmt.Sprintf("a%d.li", i), Status: "available"})
	}
	s.InsertResult(ctx, &Result{JobID: a, Domain: "weird.li", Status: "unknown"})
	s.InsertResult(ctx, &Result{JobID: b, Domain: "b1.li", Status: "available"})

	rows, total, err := s.ListResults(ctx, ResultFilter{JobID: a, Status: "available", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || len(rows) != 2 {
		t.Fatalf("total=%d rows=%d, want 5/2", total, len(rows))
	}
	rows, total, _ = s.ListResults(ctx, ResultFilter{Q: "wei", Limit: 10})
	if total != 1 || len(rows) != 1 || rows[0].Domain != "weird.li" {
		t.Fatalf("q filter: total=%d rows=%v", total, rows)
	}
	rows, total, _ = s.ListResults(ctx, ResultFilter{Limit: 100, Offset: 5})
	if total != 7 || len(rows) != 2 {
		t.Fatalf("offset: total=%d rows=%d, want 7/2", total, len(rows))
	}
}

func TestDeleteJobCascades(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	id, _ := s.CreateJob(ctx, newJob("a"))
	s.InsertResult(ctx, &Result{JobID: id, Domain: "x.li", Status: "available"})
	s.InsertLogs(ctx, []LogEntry{{JobID: id, Level: "info", Message: "hi", Time: time.Now()}})
	if err := s.DeleteJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, total, _ := s.ListResults(ctx, ResultFilter{Limit: 10}); total != 0 {
		t.Fatalf("results not deleted: %d", total)
	}
	if logs, _ := s.ListLogs(ctx, LogFilter{Limit: 10}); len(logs) != 0 {
		t.Fatalf("logs not deleted: %d", len(logs))
	}
}

func TestLogsLevelFilterPagingAndPrune(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	var entries []LogEntry
	for i := 0; i < 10; i++ {
		lvl := "info"
		if i%5 == 0 {
			lvl = "debug"
		}
		entries = append(entries, LogEntry{Level: lvl, Message: fmt.Sprintf("m%d", i), Time: time.Now()})
	}
	entries = append(entries, LogEntry{Level: "error", Message: "bad", Time: time.Now()})
	if err := s.InsertLogs(ctx, entries); err != nil {
		t.Fatal(err)
	}
	info, _ := s.ListLogs(ctx, LogFilter{Level: "info", Limit: 100})
	if len(info) != 9 {
		t.Fatalf("info+ = %d, want 9 (debug excluded)", len(info))
	}
	errs, _ := s.ListLogs(ctx, LogFilter{Level: "error", Limit: 100})
	if len(errs) != 1 || errs[0].Message != "bad" {
		t.Fatalf("error filter: %+v", errs)
	}
	page1, _ := s.ListLogs(ctx, LogFilter{Limit: 4})
	page2, _ := s.ListLogs(ctx, LogFilter{Limit: 4, BeforeID: page1[len(page1)-1].ID})
	if len(page1) != 4 || len(page2) != 4 || page2[0].ID >= page1[len(page1)-1].ID {
		t.Fatalf("paging broken: %v / %v", page1, page2)
	}
	if err := s.PruneLogs(ctx, 3); err != nil {
		t.Fatal(err)
	}
	left, _ := s.ListLogs(ctx, LogFilter{Limit: 100})
	if len(left) != 3 || left[0].Message != "bad" {
		t.Fatalf("prune kept %d, first=%q", len(left), left[0].Message)
	}
}

func TestSettings(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	if _, ok, _ := s.GetSetting(ctx, "k"); ok {
		t.Fatal("missing setting reported present")
	}
	s.SetSetting(ctx, "k", "v1")
	s.SetSetting(ctx, "k", "v2")
	if v, ok, _ := s.GetSetting(ctx, "k"); !ok || v != "v2" {
		t.Fatalf("setting = %q,%v want v2", v, ok)
	}
}

func TestConcurrentInsertsDoNotLock(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	id, _ := s.CreateJob(ctx, newJob("a"))
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.InsertResult(ctx, &Result{JobID: id, Domain: fmt.Sprintf("d%d.li", i), Status: "available"}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent insert failed: %v", err)
	}
	if _, total, _ := s.ListResults(ctx, ResultFilter{Limit: 1}); total != 50 {
		t.Fatalf("total = %d, want 50", total)
	}
}

func TestStats(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	a, _ := s.CreateJob(ctx, newJob("a"))
	s.SetJobStatus(ctx, a, "running", "")
	s.UpdateJobProgress(ctx, a, 5, 5, 2, 1, 2)
	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Jobs != 1 || st.RunningJobs != 1 || st.Checked != 5 || st.Available != 2 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	s, _ := Open(path)
	id, _ := s.CreateJob(context.Background(), newJob("keep"))
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if j, err := s2.GetJob(context.Background(), id); err != nil || j.Name != "keep" {
		t.Fatalf("job lost across reopen: %v %v", j, err)
	}
}
