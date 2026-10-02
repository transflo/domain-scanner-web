package store

import "database/sql"

const schema = `
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

func migrate(db *sql.DB) error {
	_, err := db.Exec(schema)
	return err
}
