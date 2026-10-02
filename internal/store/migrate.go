package store

import (
	"database/sql"
	"fmt"
)

// baseSchema is the schema of the first release. Columns added later are applied by
// ensureColumn so databases created by older versions are upgraded in place.
const baseSchema = `
CREATE TABLE IF NOT EXISTS jobs (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	name         TEXT    NOT NULL DEFAULT '',
	suffix       TEXT    NOT NULL,
	pattern      TEXT    NOT NULL DEFAULT '',
	regex        TEXT    NOT NULL DEFAULT '',
	wordlist     TEXT    NOT NULL DEFAULT '',
	length       INTEGER NOT NULL DEFAULT 0,
	delay_ms     INTEGER NOT NULL DEFAULT 0,
	workers      INTEGER NOT NULL DEFAULT 1,
	use_reserved INTEGER NOT NULL DEFAULT 0,
	status       TEXT    NOT NULL,
	cursor       INTEGER NOT NULL DEFAULT 0,
	total        INTEGER NOT NULL DEFAULT 0,
	checked      INTEGER NOT NULL DEFAULT 0,
	available    INTEGER NOT NULL DEFAULT 0,
	unknown      INTEGER NOT NULL DEFAULT 0,
	registered   INTEGER NOT NULL DEFAULT 0,
	error        TEXT    NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);

CREATE TABLE IF NOT EXISTS results (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id     INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	domain     TEXT    NOT NULL,
	status     TEXT    NOT NULL,
	signatures TEXT    NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	UNIQUE(job_id, domain)
);
CREATE INDEX IF NOT EXISTS idx_results_status ON results(status);

CREATE TABLE IF NOT EXISTS logs (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id  INTEGER NOT NULL DEFAULT 0,
	level   TEXT    NOT NULL,
	message TEXT    NOT NULL,
	time    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_logs_job ON logs(job_id);

CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// upgrades are idempotent additions on top of baseSchema.
var upgrades = []struct{ table, column, ddl string }{
	// egress selection per job
	{"jobs", "egress_mode", "TEXT NOT NULL DEFAULT 'direct'"},
	{"jobs", "proxy_id", "INTEGER NOT NULL DEFAULT 0"},
	{"jobs", "failover", "INTEGER NOT NULL DEFAULT 1"},
	// Cloudflare final check and registration
	{"results", "cf_status", "TEXT NOT NULL DEFAULT ''"},
	{"results", "cf_reason", "TEXT NOT NULL DEFAULT ''"},
	{"results", "cf_price", "TEXT NOT NULL DEFAULT ''"},
	{"results", "cf_currency", "TEXT NOT NULL DEFAULT ''"},
	{"results", "cf_checked_at", "INTEGER NOT NULL DEFAULT 0"},
	{"results", "register_status", "TEXT NOT NULL DEFAULT ''"},
	{"results", "register_note", "TEXT NOT NULL DEFAULT ''"},
	// structured logs
	{"logs", "component", "TEXT NOT NULL DEFAULT ''"},
	{"logs", "event", "TEXT NOT NULL DEFAULT ''"},
	{"logs", "domain", "TEXT NOT NULL DEFAULT ''"},
	{"logs", "egress", "TEXT NOT NULL DEFAULT ''"},
	{"logs", "duration_ms", "INTEGER NOT NULL DEFAULT 0"},
	{"logs", "fields", "TEXT NOT NULL DEFAULT ''"},
}

const upgradeIndexes = `
CREATE INDEX IF NOT EXISTS idx_logs_component ON logs(component);
CREATE INDEX IF NOT EXISTS idx_logs_domain ON logs(domain);
CREATE INDEX IF NOT EXISTS idx_logs_event ON logs(event);
CREATE INDEX IF NOT EXISTS idx_logs_level ON logs(level);
CREATE INDEX IF NOT EXISTS idx_results_cf ON results(cf_status);
`

const outboundsSchema = `
CREATE TABLE IF NOT EXISTS outbounds (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	name          TEXT    NOT NULL,
	protocol      TEXT    NOT NULL,
	address       TEXT    NOT NULL DEFAULT '',
	port          INTEGER NOT NULL DEFAULT 0,
	config        TEXT    NOT NULL,
	enabled       INTEGER NOT NULL DEFAULT 1,
	last_test_at  INTEGER NOT NULL DEFAULT 0,
	last_ok       INTEGER NOT NULL DEFAULT 0,
	last_delay_ms INTEGER NOT NULL DEFAULT 0,
	last_error    TEXT    NOT NULL DEFAULT '',
	last_ip       TEXT    NOT NULL DEFAULT '',
	last_country  TEXT    NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL
);
`

func migrate(db *sql.DB) error {
	if _, err := db.Exec(baseSchema); err != nil {
		return err
	}
	if _, err := db.Exec(outboundsSchema); err != nil {
		return err
	}
	for _, u := range upgrades {
		if err := ensureColumn(db, u.table, u.column, u.ddl); err != nil {
			return err
		}
	}
	_, err := db.Exec(upgradeIndexes)
	return err
}

// ensureColumn adds a column when it is missing (SQLite has no ADD COLUMN IF NOT EXISTS).
func ensureColumn(db *sql.DB, table, column, ddl string) error {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return err
	}
	has := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			has = true
		}
	}
	rows.Close()
	if has {
		return nil
	}
	_, err = db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, ddl))
	return err
}
