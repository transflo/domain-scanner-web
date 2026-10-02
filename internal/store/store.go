// Package store is the SQLite persistence layer (jobs, results, logs, settings).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

type Job struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Suffix      string `json:"suffix"`
	Pattern     string `json:"pattern"`
	Regex       string `json:"regex"`
	Wordlist    string `json:"wordlist"`
	Length      int    `json:"length"`
	DelayMS     int    `json:"delay_ms"`
	Workers     int    `json:"workers"`
	UseReserved bool   `json:"use_reserved"`
	// Egress selection: direct, proxy (ProxyID) or pool; Failover lets the scheduler switch
	// to another healthy egress when this one keeps failing.
	EgressMode string    `json:"egress_mode"`
	ProxyID    int64     `json:"proxy_id"`
	Failover   bool      `json:"failover"`
	Status     string    `json:"status"`
	Cursor     int64     `json:"cursor"`
	Total      int64     `json:"total"`
	Checked    int64     `json:"checked"`
	Available  int64     `json:"available"`
	Unknown    int64     `json:"unknown"`
	Registered int64     `json:"registered"`
	Error      string    `json:"error"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Result struct {
	ID         int64     `json:"id"`
	JobID      int64     `json:"job_id"`
	Domain     string    `json:"domain"`
	Status     string    `json:"status"`
	Signatures string    `json:"signatures"`
	CreatedAt  time.Time `json:"created_at"`

	// Cloudflare final check: "" (not applicable / not configured), pending, confirmed,
	// rejected, unsupported, error.
	CFStatus    string     `json:"cf_status"`
	CFReason    string     `json:"cf_reason"`
	CFPrice     string     `json:"cf_price"`
	CFCurrency  string     `json:"cf_currency"`
	CFCheckedAt *time.Time `json:"cf_checked_at,omitempty"`
	// Registration through the Telegram button: "", registering, succeeded, failed.
	RegisterStatus string `json:"register_status"`
	RegisterNote   string `json:"register_note"`
}

// LogEntry is one structured log line. Domain, Egress and DurationMS are first-class columns
// (indexed / aggregated); everything else lives in Fields.
type LogEntry struct {
	ID         int64          `json:"id"`
	JobID      int64          `json:"job_id"`
	Level      string         `json:"level"`
	Component  string         `json:"component"`
	Event      string         `json:"event"`
	Message    string         `json:"message"`
	Domain     string         `json:"domain,omitempty"`
	Egress     string         `json:"egress,omitempty"`
	DurationMS int64          `json:"duration_ms,omitempty"`
	Fields     map[string]any `json:"fields,omitempty"`
	Time       time.Time      `json:"time"`
}

type ResultFilter struct {
	JobID         int64
	Status, Q     string
	CFStatus      string
	Limit, Offset int
}

// LogFilter selects log rows. Empty fields are ignored; Q is a literal substring match over the
// message, domain and event.
type LogFilter struct {
	Level     string // minimum level
	JobID     int64
	Component string
	Event     string
	Domain    string
	Egress    string
	Q         string
	Limit     int
	BeforeID  int64
}

type Stats struct {
	Jobs        int64 `json:"jobs"`
	RunningJobs int64 `json:"running_jobs"`
	Checked     int64 `json:"checked"`
	Available   int64 `json:"available"`
	Unknown     int64 `json:"unknown"`
	Registered  int64 `json:"registered"`
}

type Store struct{ db *sql.DB }

// LevelRank orders log levels; unknown levels rank as info.
func LevelRank(l string) int {
	switch l {
	case "debug":
		return 0
	case "warn":
		return 2
	case "error":
		return 3
	}
	return 1
}

var levels = []string{"debug", "info", "warn", "error"}

// Open opens (and migrates) the database at path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serialises writes, which is plenty for this workload and avoids
	// "database is locked" under concurrent workers.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMS(v int64) time.Time { return time.UnixMilli(v) }
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- jobs ----

const jobCols = `id,name,suffix,pattern,regex,wordlist,length,delay_ms,workers,use_reserved,egress_mode,proxy_id,failover,status,cursor,total,checked,available,unknown,registered,error,created_at,updated_at`

func scanJob(sc interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var reserved, failover int
	var created, updated int64
	err := sc.Scan(&j.ID, &j.Name, &j.Suffix, &j.Pattern, &j.Regex, &j.Wordlist, &j.Length, &j.DelayMS, &j.Workers,
		&reserved, &j.EgressMode, &j.ProxyID, &failover, &j.Status, &j.Cursor, &j.Total, &j.Checked, &j.Available,
		&j.Unknown, &j.Registered, &j.Error, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.UseReserved = reserved != 0
	j.Failover = failover != 0
	j.CreatedAt, j.UpdatedAt = fromMS(created), fromMS(updated)
	return &j, nil
}

func (s *Store) CreateJob(ctx context.Context, j *Job) (int64, error) {
	now := ms(time.Now())
	mode := j.EgressMode
	if mode == "" {
		mode = "direct"
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO jobs
		(name,suffix,pattern,regex,wordlist,length,delay_ms,workers,use_reserved,egress_mode,proxy_id,failover,status,cursor,total,checked,available,unknown,registered,error,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,0,0,0,0,'',?,?)`,
		j.Name, j.Suffix, j.Pattern, j.Regex, j.Wordlist, j.Length, j.DelayMS, j.Workers, b2i(j.UseReserved),
		mode, j.ProxyID, b2i(j.Failover), j.Status, j.Total, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetJob(ctx context.Context, id int64) (*Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
}

func (s *Store) queryJobs(ctx context.Context, where string, args ...any) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

func (s *Store) ListJobs(ctx context.Context) ([]Job, error) {
	return s.queryJobs(ctx, `ORDER BY id DESC`)
}

func (s *Store) RecoverableJobs(ctx context.Context) ([]Job, error) {
	return s.queryJobs(ctx, `WHERE status IN ('queued','running') ORDER BY id`)
}

func (s *Store) UpdateJobProgress(ctx context.Context, id, cursor, checked, available, unknown, registered int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET cursor=?,checked=?,available=?,unknown=?,registered=?,updated_at=? WHERE id=?`,
		cursor, checked, available, unknown, registered, ms(time.Now()), id)
	return err
}

func (s *Store) SetJobStatus(ctx context.Context, id int64, status, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status=?,error=?,updated_at=? WHERE id=?`,
		status, errMsg, ms(time.Now()), id)
	return err
}

func (s *Store) DeleteJob(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM logs WHERE job_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE id=?`, id); err != nil { // results cascade
		return err
	}
	return tx.Commit()
}

// ---- results ----

// InsertResult stores a result; inserted=false means (job, domain) already existed.
func (s *Store) InsertResult(ctx context.Context, r *Result) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO results (job_id,domain,status,signatures,created_at) VALUES (?,?,?,?,?)`,
		r.JobID, r.Domain, r.Status, r.Signatures, ms(time.Now()))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err == nil && n > 0 {
		if id, err := res.LastInsertId(); err == nil {
			r.ID = id
		}
	}
	return n > 0, err
}

func (s *Store) ListResults(ctx context.Context, f ResultFilter) ([]Result, int64, error) {
	var where []string
	var args []any
	if f.JobID != 0 {
		where = append(where, "job_id=?")
		args = append(args, f.JobID)
	}
	if f.Status != "" {
		where = append(where, "status=?")
		args = append(args, f.Status)
	}
	if f.Q != "" {
		where = append(where, `domain LIKE ? ESCAPE '\'`)
		args = append(args, "%"+likeEscape(f.Q)+"%")
	}
	if f.CFStatus != "" {
		where = append(where, "cf_status=?")
		args = append(args, f.CFStatus)
	}
	w := ""
	if len(where) > 0 {
		w = " WHERE " + strings.Join(where, " AND ")
	}
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM results`+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+resultCols+` FROM results`+w+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Result{}
	for rows.Next() {
		r, err := scanResult(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *r)
	}
	return out, total, rows.Err()
}

const resultCols = `id,job_id,domain,status,signatures,created_at,cf_status,cf_reason,cf_price,cf_currency,cf_checked_at,register_status,register_note`

func scanResult(sc interface{ Scan(...any) error }) (*Result, error) {
	var r Result
	var created, cfChecked int64
	err := sc.Scan(&r.ID, &r.JobID, &r.Domain, &r.Status, &r.Signatures, &created, &r.CFStatus, &r.CFReason,
		&r.CFPrice, &r.CFCurrency, &cfChecked, &r.RegisterStatus, &r.RegisterNote)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt = fromMS(created)
	if cfChecked > 0 {
		t := fromMS(cfChecked)
		r.CFCheckedAt = &t
	}
	return &r, nil
}

// GetResult returns one result by id.
func (s *Store) GetResult(ctx context.Context, id int64) (*Result, error) {
	return scanResult(s.db.QueryRowContext(ctx, `SELECT `+resultCols+` FROM results WHERE id=?`, id))
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ---- settings & stats ----

func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(status='running'),0),COALESCE(SUM(checked),0),COALESCE(SUM(available),0),
		COALESCE(SUM(unknown),0),COALESCE(SUM(registered),0) FROM jobs`).
		Scan(&st.Jobs, &st.RunningJobs, &st.Checked, &st.Available, &st.Unknown, &st.Registered)
	return st, err
}
