package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func insertLogsAt(t *testing.T, s *Store, level string, n int, at time.Time) {
	t.Helper()
	var es []LogEntry
	for i := 0; i < n; i++ {
		es = append(es, LogEntry{Level: level, Component: "x", Event: "e", Message: strings.Repeat("m", 200), Time: at})
	}
	if err := s.InsertLogs(context.Background(), es); err != nil {
		t.Fatal(err)
	}
}

func logCount(t *testing.T, s *Store, level string) int {
	t.Helper()
	got, err := s.ListLogs(context.Background(), LogFilter{Level: level, Limit: 100000})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range got {
		if e.Level == level {
			n++
		}
	}
	return n
}

func TestDeleteLogsBeforeOnlyTouchesTheGivenLevelsAndAge(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	old, fresh := time.Now().Add(-72*time.Hour), time.Now()
	insertLogsAt(t, s, "debug", 50, old)
	insertLogsAt(t, s, "debug", 5, fresh)
	insertLogsAt(t, s, "info", 7, old)
	n, err := s.DeleteLogsBefore(ctx, []string{"debug"}, time.Now().Add(-24*time.Hour), 20) // small chunks
	if err != nil || n != 50 {
		t.Fatalf("deleted %d err %v, want 50", n, err)
	}
	if logCount(t, s, "debug") != 5 || logCount(t, s, "info") != 7 {
		t.Fatalf("debug=%d info=%d", logCount(t, s, "debug"), logCount(t, s, "info"))
	}
}

func TestTrimLogsKeepsTheNewestOfTheGivenLevels(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	insertLogsAt(t, s, "debug", 30, time.Now())
	insertLogsAt(t, s, "warn", 30, time.Now())
	n, err := s.TrimLogs(ctx, []string{"debug"}, 10)
	if err != nil || n != 20 {
		t.Fatalf("deleted %d err %v", n, err)
	}
	if logCount(t, s, "debug") != 10 || logCount(t, s, "warn") != 30 {
		t.Fatalf("debug=%d warn=%d", logCount(t, s, "debug"), logCount(t, s, "warn"))
	}
	// keeping more than exist is a no-op, keeping 0 clears the level
	if n, _ := s.TrimLogs(ctx, []string{"debug"}, 100); n != 0 {
		t.Fatalf("nothing to trim, deleted %d", n)
	}
	if n, _ := s.TrimLogs(ctx, []string{"debug"}, 0); n != 10 {
		t.Fatalf("keep 0 should clear the level, deleted %d", n)
	}
}

func TestDeleteOldestLogsGoesInIdOrderWithinLevels(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	insertLogsAt(t, s, "debug", 10, time.Now())
	insertLogsAt(t, s, "error", 10, time.Now())
	n, err := s.DeleteOldestLogs(ctx, []string{"debug", "info"}, 4)
	if err != nil || n != 4 {
		t.Fatalf("deleted %d err %v", n, err)
	}
	if logCount(t, s, "debug") != 6 || logCount(t, s, "error") != 10 {
		t.Fatal("wrong rows removed")
	}
}

func TestDeleteUnknownResultsBeforeKeepsAvailableOnes(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	jid, _ := s.CreateJob(ctx, newJob("j"))
	for _, r := range []Result{
		{JobID: jid, Domain: "a.com", Status: "unknown"},
		{JobID: jid, Domain: "b.com", Status: "available"},
		{JobID: jid, Domain: "c.com", Status: "unknown"},
	} {
		r := r
		if _, err := s.InsertResult(ctx, &r); err != nil {
			t.Fatal(err)
		}
	}
	// age two of them (InsertResult stamps "now")
	if _, err := s.db.ExecContext(ctx, `UPDATE results SET created_at=? WHERE domain IN ('a.com','b.com')`,
		ms(time.Now().Add(-48*time.Hour))); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteUnknownResultsBefore(ctx, time.Now().Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("deleted %d err %v", n, err)
	}
	rows, total, _ := s.ListResults(ctx, ResultFilter{Limit: 10})
	if total != 2 || len(rows) != 2 {
		t.Fatalf("remaining %d", total)
	}
}

func TestStorageStatsCountsRowsAndSizes(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	insertLogsAt(t, s, "debug", 3, time.Now())
	insertLogsAt(t, s, "error", 2, time.Now())
	st, err := s.StorageStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Logs["debug"] != 3 || st.Logs["error"] != 2 || st.PageSize <= 0 || st.PageCount <= 0 {
		t.Fatalf("stats = %+v", st)
	}
	if st.UsedBytes() <= 0 || st.UsedBytes() > st.FileBytes() {
		t.Fatalf("used %d file %d", st.UsedBytes(), st.FileBytes())
	}
}

func TestReclaimShrinksTheFileAfterBigDeletes(t *testing.T) {
	path := t.TempDir() + "/big.db"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	insertLogsAt(t, s, "debug", 20000, time.Now())
	big, _ := os.Stat(path)
	if _, err := s.TrimLogs(ctx, []string{"debug"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableAutoVacuum(ctx); err != nil { // first run on a pre-existing database
		t.Fatal(err)
	}
	if err := s.Reclaim(ctx, 1<<30); err != nil {
		t.Fatal(err)
	}
	small, _ := os.Stat(path)
	if small.Size() >= big.Size()/2 {
		t.Fatalf("file did not shrink: %d -> %d", big.Size(), small.Size())
	}
	if mode, _ := s.AutoVacuumMode(ctx); mode != 2 {
		t.Fatalf("auto_vacuum mode = %d, want 2 (incremental)", mode)
	}
}
