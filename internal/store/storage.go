package store

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// Retention and space management. Everything here deletes in chunks: the database has a single
// connection, so one huge DELETE would stall every worker that wants to write a result or a log.

const defaultChunk = 20000

func inList(levels []string) (string, []any) {
	ph := make([]string, len(levels))
	args := make([]any, len(levels))
	for i, l := range levels {
		ph[i], args[i] = "?", l
	}
	return strings.Join(ph, ","), args
}

// deleteChunks runs `DELETE FROM logs WHERE id IN (SELECT id FROM logs WHERE <cond> LIMIT chunk)`
// until nothing is left to delete.
func (s *Store) deleteChunks(ctx context.Context, cond string, args []any, chunk int) (int64, error) {
	if chunk <= 0 {
		chunk = defaultChunk
	}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM logs WHERE id IN (SELECT id FROM logs WHERE `+cond+` LIMIT ?)`, append(append([]any{}, args...), chunk)...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < int64(chunk) {
			return total, nil
		}
	}
}

// DeleteLogsBefore removes log rows of the given levels older than cutoff.
func (s *Store) DeleteLogsBefore(ctx context.Context, levels []string, cutoff time.Time, chunk int) (int64, error) {
	if len(levels) == 0 {
		return 0, nil
	}
	in, args := inList(levels)
	return s.deleteChunks(ctx, `level IN (`+in+`) AND time < ?`, append(args, ms(cutoff)), chunk)
}

// TrimLogs keeps only the newest `keep` rows among the given levels.
func (s *Store) TrimLogs(ctx context.Context, levels []string, keep int) (int64, error) {
	if len(levels) == 0 {
		return 0, nil
	}
	in, args := inList(levels)
	if keep <= 0 {
		return s.deleteChunks(ctx, `level IN (`+in+`)`, args, 0)
	}
	var oldestKept int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM logs WHERE level IN (`+in+`) ORDER BY id DESC LIMIT 1 OFFSET ?`,
		append(append([]any{}, args...), keep-1)...).Scan(&oldestKept)
	if err != nil { // fewer rows than keep: nothing to trim
		return 0, nil
	}
	return s.deleteChunks(ctx, `level IN (`+in+`) AND id < ?`, append(args, oldestKept), 0)
}

// DeleteOldestLogs removes the n oldest rows among the given levels.
func (s *Store) DeleteOldestLogs(ctx context.Context, levels []string, n int) (int64, error) {
	if len(levels) == 0 || n <= 0 {
		return 0, nil
	}
	in, args := inList(levels)
	res, err := s.db.ExecContext(ctx, `DELETE FROM logs WHERE id IN
		(SELECT id FROM logs WHERE level IN (`+in+`) ORDER BY id LIMIT ?)`, append(args, n)...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteUnknownResultsBefore drops old "could not decide" results; confirmed ones are kept.
func (s *Store) DeleteUnknownResultsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM results WHERE status='unknown' AND created_at < ?`, ms(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// StorageStats describes what the database holds and how much space it takes.
type StorageStats struct {
	PageSize   int64            `json:"page_size"`
	PageCount  int64            `json:"page_count"`
	FreePages  int64            `json:"free_pages"`
	AutoVacuum int              `json:"auto_vacuum"`
	Logs       map[string]int64 `json:"logs"`
	Results    map[string]int64 `json:"results"`
	Jobs       int64            `json:"jobs"`
}

// FileBytes is the size of the main database file; UsedBytes excludes pages SQLite can reuse.
func (st StorageStats) FileBytes() int64 { return st.PageCount * st.PageSize }
func (st StorageStats) UsedBytes() int64 { return (st.PageCount - st.FreePages) * st.PageSize }

func (s *Store) StorageStats(ctx context.Context) (StorageStats, error) {
	st := StorageStats{Logs: map[string]int64{}, Results: map[string]int64{}}
	for _, p := range []struct {
		pragma string
		dst    *int64
	}{{"page_size", &st.PageSize}, {"page_count", &st.PageCount}, {"freelist_count", &st.FreePages}} {
		if err := s.db.QueryRowContext(ctx, `PRAGMA `+p.pragma).Scan(p.dst); err != nil {
			return st, err
		}
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&st.AutoVacuum); err != nil {
		return st, err
	}
	for _, q := range []struct {
		sql string
		dst map[string]int64
	}{{`SELECT level, COUNT(*) FROM logs GROUP BY level`, st.Logs}, {`SELECT status, COUNT(*) FROM results GROUP BY status`, st.Results}} {
		rows, err := s.db.QueryContext(ctx, q.sql)
		if err != nil {
			return st, err
		}
		for rows.Next() {
			var k string
			var n int64
			if err := rows.Scan(&k, &n); err != nil {
				rows.Close()
				return st, err
			}
			q.dst[k] = n
		}
		rows.Close()
	}
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&st.Jobs)
	return st, err
}

// AutoVacuumMode returns PRAGMA auto_vacuum (0 none, 1 full, 2 incremental).
func (s *Store) AutoVacuumMode(ctx context.Context) (int, error) {
	var m int
	err := s.db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&m)
	return m, err
}

// EnableAutoVacuum switches the database to incremental auto-vacuum. SQLite only applies the
// change by rebuilding the file (VACUUM), which needs free disk space of about the database size:
// callers check that first.
func (s *Store) EnableAutoVacuum(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA auto_vacuum=INCREMENTAL`); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `VACUUM`)
	return err
}

func (s *Store) checkpoint(ctx context.Context) {
	if rows, err := s.db.QueryContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err == nil {
		rows.Close()
	}
}

// Reclaim gives free pages back to the filesystem (at most maxBytes per call) and truncates the
// write-ahead log. It only releases pages when auto-vacuum is incremental.
func (s *Store) Reclaim(ctx context.Context, maxBytes int64) error {
	s.checkpoint(ctx)
	mode, err := s.AutoVacuumMode(ctx)
	if err != nil || mode != 2 {
		return err
	}
	var pageSize, free int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return err
	}
	budget := maxBytes / pageSize
	for budget > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
			return err
		}
		if free == 0 {
			break
		}
		step := min(free, budget, 2000) // small steps keep the single connection responsive
		rows, err := s.db.QueryContext(ctx, `PRAGMA incremental_vacuum(`+strconv.FormatInt(step, 10)+`)`)
		if err != nil {
			return err
		}
		rows.Close()
		budget -= step
	}
	s.checkpoint(ctx)
	return nil
}
