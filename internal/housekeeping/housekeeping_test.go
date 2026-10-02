package housekeeping

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"domain_scanner/internal/logbus"
	"domain_scanner/internal/store"
)

const gb = int64(1) << 30

type env struct {
	st   *store.Store
	bus  *logbus.Bus
	k    *Keeper
	mu   sync.Mutex
	free int64
	user string // the level the user chose in settings
}

func newEnv(t *testing.T, p Policy) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bus := logbus.New(nil, 100)
	t.Cleanup(bus.Close)
	e := &env{st: st, bus: bus, free: 100 * gb, user: "debug"}
	bus.SetMinLevel("debug")
	e.k = &Keeper{St: st, Bus: bus, Log: bus.Logger("housekeeping"), Policy: p, Dir: dir, DBPath: filepath.Join(dir, "s.db"),
		Disk:         func(string) (int64, int64, bool) { e.mu.Lock(); defer e.mu.Unlock(); return e.free, 200 * gb, true },
		UserLogLevel: func() string { return e.user }}
	return e
}

func (e *env) setFree(b int64) { e.mu.Lock(); e.free = b; e.mu.Unlock() }

func (e *env) logs(t *testing.T, at time.Time, level string, n int) {
	t.Helper()
	var es []store.LogEntry
	for i := 0; i < n; i++ {
		es = append(es, store.LogEntry{Level: level, Component: "c", Event: "e", Message: strings.Repeat("x", 150), Time: at})
	}
	if err := e.st.InsertLogs(context.Background(), es); err != nil {
		t.Fatal(err)
	}
}

func (e *env) count(t *testing.T, level string) int64 {
	t.Helper()
	st, err := e.st.StorageStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st.Logs[level]
}

var roomy = Policy{DebugDays: 3, InfoDays: 14, WarnDays: 90, MaxDebugRows: 1_000_000, MaxOtherRows: 1_000_000, MaxDBBytes: 10 * gb, UnknownResultDays: 30, MinFreeBytes: gb}

func TestRunDeletesLogsOlderThanTheirLevelsRetention(t *testing.T) {
	e := newEnv(t, roomy)
	now := time.Now()
	e.logs(t, now.Add(-4*24*time.Hour), "debug", 10)  // past 3 days
	e.logs(t, now.Add(-2*24*time.Hour), "debug", 5)   // kept
	e.logs(t, now.Add(-15*24*time.Hour), "info", 10)  // past 14 days
	e.logs(t, now.Add(-10*24*time.Hour), "info", 5)   // kept
	e.logs(t, now.Add(-30*24*time.Hour), "warn", 4)   // warn keeps 90 days
	e.logs(t, now.Add(-100*24*time.Hour), "error", 3) // past 90 days
	rep := e.k.Run(context.Background())
	if e.count(t, "debug") != 5 || e.count(t, "info") != 5 || e.count(t, "warn") != 4 || e.count(t, "error") != 0 {
		t.Fatalf("debug=%d info=%d warn=%d error=%d", e.count(t, "debug"), e.count(t, "info"), e.count(t, "warn"), e.count(t, "error"))
	}
	if rep.DeletedLogs != 23 || rep.State != StateOK {
		t.Fatalf("report = %+v", rep)
	}
}

func TestRunEnforcesRowCapsPerLevelClass(t *testing.T) {
	p := roomy
	p.MaxDebugRows, p.MaxOtherRows = 20, 8
	e := newEnv(t, p)
	e.logs(t, time.Now(), "debug", 50)
	e.logs(t, time.Now(), "info", 10)
	e.logs(t, time.Now(), "error", 10)
	e.k.Run(context.Background())
	if e.count(t, "debug") != 20 {
		t.Fatalf("debug rows = %d", e.count(t, "debug"))
	}
	if e.count(t, "info")+e.count(t, "error") != 8 {
		t.Fatalf("other rows = %d", e.count(t, "info")+e.count(t, "error"))
	}
}

func TestRunShrinksTheDatabaseBelowItsSizeCap(t *testing.T) {
	p := roomy
	p.MaxDBBytes = 400 << 10 // 400 KB
	e := newEnv(t, p)
	e.logs(t, time.Now(), "debug", 6000) // ~1.5 MB of rows
	e.logs(t, time.Now(), "error", 10)
	rep := e.k.Run(context.Background())
	st, _ := e.st.StorageStats(context.Background())
	if st.UsedBytes() > p.MaxDBBytes+(64<<10) {
		t.Fatalf("still %d bytes used after the run (cap %d); report %+v", st.UsedBytes(), p.MaxDBBytes, rep)
	}
	if e.count(t, "error") != 10 {
		t.Fatal("errors must outlive debug lines when trimming for size")
	}
}

func TestLowDiskDropsDebugAndTrimsInfo(t *testing.T) {
	p := roomy
	e := newEnv(t, p)
	e.logs(t, time.Now(), "debug", 100)
	e.logs(t, time.Now(), "info", 60000)
	e.logs(t, time.Now(), "warn", 10)
	e.setFree(gb / 2) // below the 1 GB minimum, above the critical fifth
	rep := e.k.Run(context.Background())
	if rep.State != StateLow {
		t.Fatalf("state = %q", rep.State)
	}
	if e.count(t, "debug") != 0 || e.count(t, "info") > 50000 || e.count(t, "warn") != 10 {
		t.Fatalf("debug=%d info=%d warn=%d", e.count(t, "debug"), e.count(t, "info"), e.count(t, "warn"))
	}
}

func TestCriticalDiskRaisesTheLogLevelUntilSpaceRecovers(t *testing.T) {
	e := newEnv(t, roomy)
	e.logs(t, time.Now(), "info", 100)
	e.setFree(gb / 10) // below a fifth of the minimum
	rep := e.k.Run(context.Background())
	if rep.State != StateCritical || !rep.LevelForced {
		t.Fatalf("report = %+v", rep)
	}
	if e.bus.MinLevel() != "warn" {
		t.Fatalf("bus level = %q, want warn while the disk is nearly full", e.bus.MinLevel())
	}
	if e.count(t, "info") > 10000 {
		t.Fatalf("info rows = %d", e.count(t, "info"))
	}
	// space comes back: the user's own level is restored
	e.setFree(50 * gb)
	rep = e.k.Run(context.Background())
	if rep.State != StateOK || rep.LevelForced || e.bus.MinLevel() != "debug" {
		t.Fatalf("report = %+v, bus level %q", rep, e.bus.MinLevel())
	}
}

func TestRunRemovesOldUnknownResultsOnly(t *testing.T) {
	e := newEnv(t, roomy)
	ctx := context.Background()
	jid, _ := e.st.CreateJob(ctx, &store.Job{Name: "j", Suffix: ".com", Pattern: "d", Length: 2, Workers: 1, Status: "done"})
	for _, d := range []string{"a.com", "b.com"} {
		e.st.InsertResult(ctx, &store.Result{JobID: jid, Domain: d, Status: "unknown"})
	}
	e.st.InsertResult(ctx, &store.Result{JobID: jid, Domain: "c.com", Status: "available"})
	e.k.Policy.UnknownResultDays = 0 // keep forever
	e.k.Run(ctx)
	if st, _ := e.st.StorageStats(ctx); st.Results["unknown"] != 2 {
		t.Fatalf("results = %v", st.Results)
	}
	e.k.Policy.UnknownResultDays = 1
	e.k.Now = func() time.Time { return time.Now().Add(48 * time.Hour) } // the rows are now two days old
	rep := e.k.Run(ctx)
	st, _ := e.st.StorageStats(ctx)
	if st.Results["unknown"] != 0 || st.Results["available"] != 1 || rep.DeletedUnknown != 2 {
		t.Fatalf("results = %v, report %+v", st.Results, rep)
	}
}

func TestReportDescribesTheStorage(t *testing.T) {
	e := newEnv(t, roomy)
	e.logs(t, time.Now(), "info", 3)
	rep := e.k.Run(context.Background())
	if rep.Logs["info"] != 3 || rep.DBBytes <= 0 || rep.DiskTotalBytes != 200*gb || rep.DiskFreeBytes != 100*gb || rep.At.IsZero() {
		t.Fatalf("report = %+v", rep)
	}
	if got := e.k.Last(); got.At != rep.At {
		t.Fatal("Last() should return the most recent report")
	}
}

func TestPrepareEnablesIncrementalVacuumOnlyWhenThereIsRoom(t *testing.T) {
	e := newEnv(t, roomy)
	ctx := context.Background()
	e.logs(t, time.Now(), "info", 10)
	e.setFree(1) // no room for a rebuild
	e.k.Prepare(ctx)
	if m, _ := e.st.AutoVacuumMode(ctx); m == 2 {
		t.Fatal("must not rebuild the database when the disk is full")
	}
	e.setFree(50 * gb)
	e.k.Prepare(ctx)
	if m, _ := e.st.AutoVacuumMode(ctx); m != 2 {
		t.Fatalf("auto_vacuum = %d, want 2", m)
	}
}

func TestLoopRunsPeriodicallyUntilCancelled(t *testing.T) {
	e := newEnv(t, roomy)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.k.Loop(ctx, 20*time.Millisecond); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for e.k.Last().At.IsZero() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.k.Last().At.IsZero() {
		t.Fatal("loop never ran")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop")
	}
}
