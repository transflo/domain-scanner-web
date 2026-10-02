// Package housekeeping keeps a long-running scanner from filling its disk: it ages out logs and
// stale results, caps the database size, gives freed space back to the filesystem, and reacts when
// the disk itself is running low.
package housekeeping

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"domain_scanner/internal/logbus"
	"domain_scanner/internal/store"
)

// Disk-state names reported to the UI.
const (
	StateOK       = "ok"
	StateLow      = "low"
	StateCritical = "critical"
)

// Policy is the retention configuration. Zero means "no limit" for every field.
type Policy struct {
	DebugDays, InfoDays, WarnDays int   // how long logs of each class are kept (warn covers error too)
	MaxDebugRows, MaxOtherRows    int   // safety caps on row counts
	MaxDBBytes                    int64 // cap on the database's used bytes
	UnknownResultDays             int   // "could not decide" results older than this are dropped
	MinFreeBytes                  int64 // below this much free disk the keeper starts shedding data
}

// Disk reports free and total bytes of the filesystem holding dir.
type Disk func(dir string) (free, total int64, ok bool)

// Report is a snapshot of what the keeper found and did in its latest run.
type Report struct {
	At             time.Time        `json:"at"`
	State          string           `json:"state"`
	LevelForced    bool             `json:"level_forced"`
	DiskFreeBytes  int64            `json:"disk_free_bytes"`
	DiskTotalBytes int64            `json:"disk_total_bytes"`
	DBBytes        int64            `json:"db_bytes"`
	DBUsedBytes    int64            `json:"db_used_bytes"`
	WALBytes       int64            `json:"wal_bytes"`
	AutoVacuum     int              `json:"auto_vacuum"`
	Logs           map[string]int64 `json:"logs"`
	Results        map[string]int64 `json:"results"`
	Jobs           int64            `json:"jobs"`
	DeletedLogs    int64            `json:"deleted_logs"`
	DeletedUnknown int64            `json:"deleted_unknown"`
	Policy         Policy           `json:"policy"`
	Error          string           `json:"error,omitempty"`
}

type Keeper struct {
	St     *store.Store
	Bus    *logbus.Bus
	Log    *logbus.Logger
	Policy Policy
	Dir    string // data directory (for the free-space check)
	DBPath string // database file (for the WAL size)
	Disk   Disk
	// UserLogLevel returns the level chosen in settings; it is restored once the disk recovers.
	UserLogLevel func() string
	Now          func() time.Time

	mu     sync.Mutex // serialises runs
	last   Report
	forced bool
}

func (k *Keeper) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

// Last returns the most recent report (zero before the first run).
func (k *Keeper) Last() Report {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.last
}

var (
	lowLevels      = []string{"debug"}
	chattyLevels   = []string{"debug", "info"}
	importantLevel = []string{"warn", "error"}
	allLevels      = []string{"debug", "info", "warn", "error"}
)

func (k *Keeper) diskState() (state string, free, total int64) {
	if k.Disk == nil {
		return StateOK, 0, 0
	}
	f, t, ok := k.Disk(k.Dir)
	if !ok || k.Policy.MinFreeBytes <= 0 {
		return StateOK, f, t
	}
	switch {
	case f < k.Policy.MinFreeBytes/5:
		return StateCritical, f, t
	case f < k.Policy.MinFreeBytes:
		return StateLow, f, t
	}
	return StateOK, f, t
}

// Prepare switches an existing database to incremental auto-vacuum so freed pages can be returned
// to the disk. Rebuilding the file needs about its size in free space, so it only happens when
// that is available.
func (k *Keeper) Prepare(ctx context.Context) {
	mode, err := k.St.AutoVacuumMode(ctx)
	if err != nil || mode == 2 {
		return
	}
	var size int64
	if fi, err := os.Stat(k.DBPath); err == nil {
		size = fi.Size()
	}
	if k.Disk != nil {
		if free, _, ok := k.Disk(k.Dir); ok && free < 2*size+k.Policy.MinFreeBytes {
			k.Log.Warn("vacuum_skipped", 0, fmt.Sprintf("磁盘剩余空间不足以整理数据库(需要约 %d MB),稍后再试", (2*size+k.Policy.MinFreeBytes)>>20), nil)
			return
		}
	}
	t0 := time.Now()
	if err := k.St.EnableAutoVacuum(ctx); err != nil {
		k.Log.Warn("vacuum_failed", 0, "启用数据库自动回收失败:"+err.Error(), logbus.Fields{"error": err.Error()})
		return
	}
	k.Log.Info("vacuum_enabled", 0, "数据库已启用增量回收,删除日志后空间会归还给磁盘",
		logbus.Fields{"duration_ms": time.Since(t0).Milliseconds(), "size_bytes": size})
}

// Run does one housekeeping pass and returns its report.
func (k *Keeper) Run(ctx context.Context) Report {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.Bus != nil {
		k.Bus.Flush() // persisted rows only: make sure what is in memory counts
	}
	rep := Report{At: k.now(), Policy: k.Policy}
	var errs []error
	note := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	state, free, total := k.diskState()
	rep.State, rep.DiskFreeBytes, rep.DiskTotalBytes = state, free, total
	p := k.Policy

	delLogs := func(n int64, err error) { rep.DeletedLogs += n; note(err) }
	day := 24 * time.Hour
	if p.DebugDays > 0 {
		delLogs(k.St.DeleteLogsBefore(ctx, lowLevels, rep.At.Add(-time.Duration(p.DebugDays)*day), 0))
	}
	if p.InfoDays > 0 {
		delLogs(k.St.DeleteLogsBefore(ctx, []string{"info"}, rep.At.Add(-time.Duration(p.InfoDays)*day), 0))
	}
	if p.WarnDays > 0 {
		delLogs(k.St.DeleteLogsBefore(ctx, importantLevel, rep.At.Add(-time.Duration(p.WarnDays)*day), 0))
	}
	if p.MaxDebugRows > 0 {
		delLogs(k.St.TrimLogs(ctx, lowLevels, p.MaxDebugRows))
	}
	if p.MaxOtherRows > 0 {
		delLogs(k.St.TrimLogs(ctx, []string{"info", "warn", "error"}, p.MaxOtherRows))
	}

	// the disk is running out: shed the cheapest-to-lose data first
	switch state {
	case StateLow:
		delLogs(k.St.TrimLogs(ctx, lowLevels, 0))
		delLogs(k.St.TrimLogs(ctx, []string{"info"}, 50000))
	case StateCritical:
		delLogs(k.St.TrimLogs(ctx, chattyLevels, 0))
		delLogs(k.St.TrimLogs(ctx, importantLevel, 20000))
	}
	k.applyLogLevel(state == StateCritical)
	rep.LevelForced = k.forced

	// stay under the size cap by dropping the oldest rows, least important levels first
	if p.MaxDBBytes > 0 {
		for _, lv := range [][]string{lowLevels, {"info"}, importantLevel} {
			for i := 0; i < 50; i++ {
				st, err := k.St.StorageStats(ctx)
				if err != nil {
					note(err)
					break
				}
				over := st.UsedBytes() - p.MaxDBBytes
				if over <= 0 {
					break
				}
				// ~250 bytes per row on average; always make progress
				n := int(over/250) + 1
				if n < 1000 {
					n = 1000
				}
				d, err := k.St.DeleteOldestLogs(ctx, lv, n)
				delLogs(d, err)
				if err != nil || d == 0 {
					break
				}
			}
		}
	}

	if p.UnknownResultDays > 0 {
		n, err := k.St.DeleteUnknownResultsBefore(ctx, rep.At.Add(-time.Duration(p.UnknownResultDays)*day))
		rep.DeletedUnknown = n
		note(err)
	}

	budget := int64(256 << 20)
	if state != StateOK {
		budget = 4 << 30
	}
	note(k.St.Reclaim(ctx, budget))

	if st, err := k.St.StorageStats(ctx); err == nil {
		rep.DBBytes, rep.DBUsedBytes, rep.AutoVacuum = st.FileBytes(), st.UsedBytes(), st.AutoVacuum
		rep.Logs, rep.Results, rep.Jobs = st.Logs, st.Results, st.Jobs
	} else {
		note(err)
	}
	if fi, err := os.Stat(k.DBPath + "-wal"); err == nil {
		rep.WALBytes = fi.Size()
	}
	if len(errs) > 0 {
		rep.Error = errs[0].Error()
		k.Log.Warn("run_failed", 0, "存储清理出错:"+rep.Error, logbus.Fields{"error": rep.Error})
	}

	prev := k.last.State
	k.last = rep
	f := logbus.Fields{"deleted_logs": rep.DeletedLogs, "deleted_unknown": rep.DeletedUnknown, "db_bytes": rep.DBBytes,
		"db_used_bytes": rep.DBUsedBytes, "wal_bytes": rep.WALBytes, "disk_free_bytes": free, "state": state}
	switch {
	case state != StateOK:
		k.Log.Warn("disk_"+state, 0, fmt.Sprintf("磁盘剩余空间偏低(%d MB),已清理 %d 条日志", free>>20, rep.DeletedLogs), f)
	case prev != "" && prev != StateOK:
		k.Log.Info("disk_recovered", 0, "磁盘空间已恢复", f)
	case rep.DeletedLogs > 0 || rep.DeletedUnknown > 0:
		k.Log.Info("cleaned", 0, fmt.Sprintf("存储清理:删除日志 %d 条、过期未知结果 %d 条", rep.DeletedLogs, rep.DeletedUnknown), f)
	default:
		k.Log.Debug("cleaned", 0, "存储清理:无需删除", f)
	}
	return rep
}

// applyLogLevel raises the persisted log level to warn while the disk is nearly full and puts the
// user's choice back afterwards.
func (k *Keeper) applyLogLevel(critical bool) {
	if k.Bus == nil {
		return
	}
	switch {
	case critical && !k.forced:
		k.forced = true
		k.Bus.SetMinLevel("warn")
	case !critical && k.forced:
		k.forced = false
		lv := "info"
		if k.UserLogLevel != nil {
			lv = k.UserLogLevel()
		}
		k.Bus.SetMinLevel(lv)
	}
}

// Loop runs Prepare once, then Run every interval until ctx ends.
func (k *Keeper) Loop(ctx context.Context, every time.Duration) {
	k.Prepare(ctx)
	k.Run(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			k.Run(ctx)
		}
	}
}
