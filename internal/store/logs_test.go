package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestStructuredLogFieldsRoundTrip(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	err := s.InsertLogs(ctx, []LogEntry{{
		Level: "debug", Component: "rdap", Event: "request", JobID: 3, Message: "GET ok",
		Domain: "foo.com", Egress: "direct", DurationMS: 42, Time: time.Now(),
		Fields: map[string]any{"status": float64(404), "server": "https://rdap.example/"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListLogs(ctx, LogFilter{Limit: 10})
	if len(got) != 1 {
		t.Fatalf("got %d rows", len(got))
	}
	e := got[0]
	if e.Component != "rdap" || e.Event != "request" || e.Domain != "foo.com" || e.Egress != "direct" ||
		e.DurationMS != 42 || e.Fields["status"] != float64(404) || e.Fields["server"] != "https://rdap.example/" {
		t.Fatalf("lost data: %+v", e)
	}
}

func seedLogs(t *testing.T, s *Store) {
	t.Helper()
	now := time.Now()
	err := s.InsertLogs(context.Background(), []LogEntry{
		{Level: "debug", Component: "dns", Event: "lookup", Message: "ns lookup foo.com", Domain: "foo.com", Egress: "direct", Time: now},
		{Level: "info", Component: "check", Event: "done", Message: "foo.com registered", Domain: "foo.com", Egress: "direct", Time: now},
		{Level: "warn", Component: "rdap", Event: "throttle", Message: "429 from rdap.nic.ch", Egress: "proxy-2", Time: now},
		{Level: "error", Component: "notifier", Event: "send_failed", Message: "telegram chat not found", Time: now},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestListLogsFiltersByComponentDomainEgressAndText(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	seedLogs(t, s)
	cases := []struct {
		name string
		f    LogFilter
		want int
	}{
		{"component", LogFilter{Component: "rdap", Limit: 50}, 1},
		{"domain", LogFilter{Domain: "foo.com", Limit: 50}, 2},
		{"egress", LogFilter{Egress: "proxy-2", Limit: 50}, 1},
		{"event", LogFilter{Event: "send_failed", Limit: 50}, 1},
		{"text in message", LogFilter{Q: "chat not found", Limit: 50}, 1},
		{"text in domain", LogFilter{Q: "foo.com", Limit: 50}, 2},
		{"text with LIKE wildcard is literal", LogFilter{Q: "%", Limit: 50}, 0},
		{"level and component", LogFilter{Level: "info", Component: "dns", Limit: 50}, 0},
	}
	for _, c := range cases {
		got, err := s.ListLogs(ctx, c.f)
		if err != nil || len(got) != c.want {
			t.Errorf("%s: got %d rows (err %v), want %d", c.name, len(got), err, c.want)
		}
	}
}

func TestPruneLogsByLevelKeepsImportantOnes(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	var es []LogEntry
	for i := 0; i < 30; i++ {
		es = append(es, LogEntry{Level: "debug", Message: "d", Time: time.Now()})
	}
	for i := 0; i < 10; i++ {
		es = append(es, LogEntry{Level: "info", Message: "i", Time: time.Now()})
	}
	es = append(es, LogEntry{Level: "error", Message: "e", Time: time.Now()})
	if err := s.InsertLogs(ctx, es); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneLogsByLevel(ctx, 5, 8); err != nil {
		t.Fatal(err)
	}
	dbg, _ := s.ListLogs(ctx, LogFilter{Level: "debug", Limit: 500})
	counts := map[string]int{}
	for _, e := range dbg {
		counts[e.Level]++
	}
	if counts["debug"] != 5 {
		t.Fatalf("debug kept = %d, want 5", counts["debug"])
	}
	if counts["info"]+counts["error"] != 8 {
		t.Fatalf("info+error kept = %d, want 8 (newest)", counts["info"]+counts["error"])
	}
	if counts["error"] != 1 {
		t.Fatalf("newest error was pruned")
	}
}

func TestEachLogStreamsInChronologicalOrder(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	seedLogs(t, s)
	var order []string
	err := s.EachLog(ctx, LogFilter{Level: "debug"}, func(e LogEntry) error {
		order = append(order, e.Level)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"debug", "info", "warn", "error"}
	if len(order) != 4 {
		t.Fatalf("got %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestOpenUpgradesAnOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	// schema as shipped in the first release (no structured columns, no egress/cloudflare columns)
	_, err = db.Exec(`
CREATE TABLE jobs (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL DEFAULT '', suffix TEXT NOT NULL,
 pattern TEXT NOT NULL DEFAULT '', regex TEXT NOT NULL DEFAULT '', wordlist TEXT NOT NULL DEFAULT '',
 length INTEGER NOT NULL DEFAULT 0, delay_ms INTEGER NOT NULL DEFAULT 0, workers INTEGER NOT NULL DEFAULT 1,
 use_reserved INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL, cursor INTEGER NOT NULL DEFAULT 0,
 total INTEGER NOT NULL DEFAULT 0, checked INTEGER NOT NULL DEFAULT 0, available INTEGER NOT NULL DEFAULT 0,
 unknown INTEGER NOT NULL DEFAULT 0, registered INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE results (id INTEGER PRIMARY KEY AUTOINCREMENT, job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
 domain TEXT NOT NULL, status TEXT NOT NULL, signatures TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, UNIQUE(job_id, domain));
CREATE TABLE logs (id INTEGER PRIMARY KEY AUTOINCREMENT, job_id INTEGER NOT NULL DEFAULT 0, level TEXT NOT NULL,
 message TEXT NOT NULL, time INTEGER NOT NULL);
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO jobs (name,suffix,status,created_at,updated_at) VALUES ('old','.li','done',1,1);
INSERT INTO results (job_id,domain,status,created_at) VALUES (1,'x.li','available',1);
INSERT INTO logs (level,message,time) VALUES ('info','legacy line',1);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on old schema: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	j, err := s.GetJob(ctx, 1)
	if err != nil || j.EgressMode != "direct" || !j.Failover {
		t.Fatalf("old job after upgrade: %+v err=%v (want egress_mode=direct, failover=true)", j, err)
	}
	rows, _, err := s.ListResults(ctx, ResultFilter{Limit: 5})
	if err != nil || len(rows) != 1 || rows[0].CFStatus != "" {
		t.Fatalf("old result after upgrade: %+v err=%v", rows, err)
	}
	logs, err := s.ListLogs(ctx, LogFilter{Limit: 5})
	if err != nil || len(logs) != 1 || logs[0].Message != "legacy line" {
		t.Fatalf("old log after upgrade: %+v err=%v", logs, err)
	}
	// and a second Open must be a no-op
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	s2.Close()
}
