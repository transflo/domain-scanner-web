package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// InsertLogs stores a batch of entries. A non-zero ID is kept (the log bus assigns ids); zero
// lets SQLite pick one.
func (s *Store) InsertLogs(ctx context.Context, entries []LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO logs
		(id,job_id,level,component,event,message,domain,egress,duration_ms,fields,time)
		VALUES (NULLIF(?,0),?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range entries {
		fields := ""
		if len(e.Fields) > 0 {
			if b, err := json.Marshal(e.Fields); err == nil {
				fields = string(b)
			}
		}
		if _, err := stmt.ExecContext(ctx, e.ID, e.JobID, e.Level, e.Component, e.Event, e.Message, e.Domain,
			e.Egress, e.DurationMS, fields, ms(e.Time)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MaxLogID returns the highest persisted log id (0 when empty).
func (s *Store) MaxLogID(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM logs`).Scan(&id)
	return id, err
}

const logCols = `id,job_id,level,component,event,message,domain,egress,duration_ms,fields,time`

func scanLog(sc interface{ Scan(...any) error }) (LogEntry, error) {
	var e LogEntry
	var fields string
	var t int64
	if err := sc.Scan(&e.ID, &e.JobID, &e.Level, &e.Component, &e.Event, &e.Message, &e.Domain, &e.Egress,
		&e.DurationMS, &fields, &t); err != nil {
		return e, err
	}
	e.Time = fromMS(t)
	if fields != "" {
		_ = json.Unmarshal([]byte(fields), &e.Fields)
	}
	return e, nil
}

func logWhere(f LogFilter) (string, []any) {
	var where []string
	var args []any
	if f.Level != "" {
		var in []string
		for _, l := range levels {
			if LevelRank(l) >= LevelRank(f.Level) {
				in = append(in, "'"+l+"'")
			}
		}
		where = append(where, "level IN ("+strings.Join(in, ",")+")")
	}
	add := func(col, val string) {
		if val != "" {
			where = append(where, col+"=?")
			args = append(args, val)
		}
	}
	if f.JobID != 0 {
		where = append(where, "job_id=?")
		args = append(args, f.JobID)
	}
	add("component", f.Component)
	add("event", f.Event)
	add("domain", f.Domain)
	add("egress", f.Egress)
	if f.Q != "" {
		like := "%" + likeEscape(f.Q) + "%"
		where = append(where, `(message LIKE ? ESCAPE '\' OR domain LIKE ? ESCAPE '\' OR event LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like)
	}
	if f.BeforeID != 0 {
		where = append(where, "id<?")
		args = append(args, f.BeforeID)
	}
	if len(where) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

// ListLogs returns up to f.Limit rows, newest first.
func (s *Store) ListLogs(ctx context.Context, f LogFilter) ([]LogEntry, error) {
	w, args := logWhere(f)
	limit := f.Limit
	if limit <= 0 || limit > 5000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+logCols+` FROM logs`+w+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LogEntry{}
	for rows.Next() {
		e, err := scanLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EachLog calls fn for every matching row in chronological order, reading in pages so a long
// export never holds the database connection for more than one page at a time.
func (s *Store) EachLog(ctx context.Context, f LogFilter, fn func(LogEntry) error) error {
	const page = 2000
	f.BeforeID = 0
	w, args := logWhere(f)
	var last int64
	for {
		q := `SELECT ` + logCols + ` FROM logs`
		qargs := append([]any{}, args...)
		if w == "" {
			q += ` WHERE id>?`
		} else {
			q += w + ` AND id>?`
		}
		qargs = append(qargs, last, page)
		batch, err := s.queryLogPage(ctx, q+` ORDER BY id ASC LIMIT ?`, qargs)
		if err != nil {
			return err
		}
		for _, e := range batch {
			if err := fn(e); err != nil {
				return err
			}
			last = e.ID
		}
		if len(batch) < page {
			return nil
		}
	}
}

func (s *Store) queryLogPage(ctx context.Context, q string, args []any) ([]LogEntry, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogEntry
	for rows.Next() {
		e, err := scanLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneLogs keeps only the newest `keep` rows.
func (s *Store) PruneLogs(ctx context.Context, keep int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM logs WHERE id <= (SELECT COALESCE(MAX(id),0) FROM logs) - ?`, keep)
	return err
}

// PruneLogsByLevel keeps the newest keepDebug debug rows and, separately, the newest keepOther
// rows of every other level, so a flood of per-step debug lines cannot push out warnings/errors.
func (s *Store) PruneLogsByLevel(ctx context.Context, keepDebug, keepOther int) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM logs WHERE level='debug' AND id NOT IN
		(SELECT id FROM logs WHERE level='debug' ORDER BY id DESC LIMIT ?)`, keepDebug); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM logs WHERE level<>'debug' AND id NOT IN
		(SELECT id FROM logs WHERE level<>'debug' ORDER BY id DESC LIMIT ?)`, keepOther)
	return err
}

// LogComponents lists the distinct component names present in the logs.
func (s *Store) LogComponents(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT component FROM logs WHERE component<>'' ORDER BY component`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LogRange reports the id and time bounds of the stored logs (for the diagnostics view).
func (s *Store) LogRange(ctx context.Context) (count int64, oldest, newest time.Time, err error) {
	var lo, hi sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*),MIN(time),MAX(time) FROM logs`).Scan(&count, &lo, &hi)
	if lo.Valid {
		oldest = fromMS(lo.Int64)
	}
	if hi.Valid {
		newest = fromMS(hi.Int64)
	}
	return
}
