package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"domain_scanner/internal/auth"
	"domain_scanner/internal/logbus"
	"domain_scanner/internal/notifier"
	"domain_scanner/internal/scheduler"
	"domain_scanner/internal/store"
	"domain_scanner/internal/wordlists"
)

const password = "correct-horse-battery"

type fakeSched struct {
	createErr error
	stateErr  error
	created   []scheduler.Params
	st        *store.Store
}

func (f *fakeSched) Create(ctx context.Context, p scheduler.Params) (*store.Job, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, p)
	id, err := f.st.CreateJob(ctx, &store.Job{Name: p.Name, Suffix: p.Suffix, Pattern: p.Pattern, Length: p.Length, Workers: 1, Status: "queued", Total: 10})
	if err != nil {
		return nil, err
	}
	return f.st.GetJob(ctx, id)
}
func (f *fakeSched) Pause(id int64) error  { return f.act(id) }
func (f *fakeSched) Resume(id int64) error { return f.act(id) }
func (f *fakeSched) Cancel(id int64) error { return f.act(id) }
func (f *fakeSched) act(id int64) error {
	if f.stateErr != nil {
		return f.stateErr
	}
	if _, err := f.st.GetJob(context.Background(), id); err != nil {
		return err
	}
	return nil
}
func (f *fakeSched) Delete(ctx context.Context, id int64) error { return f.st.DeleteJob(ctx, id) }

type fakeTG struct{ err error }

func (f fakeTG) SendTest(context.Context) error { return f.err }

type fixture struct {
	srv   *httptest.Server
	st    *store.Store
	bus   *logbus.Bus
	sched *fakeSched
	tg    *fakeTG
	auth  *auth.Auth
	words *wordlists.Manager
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	bus := logbus.New(nil, 100)
	a, err := auth.New(password, []byte("stored-secret-0123456789abcdef"), time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{st: st, bus: bus, auth: a, sched: &fakeSched{st: st}, tg: &fakeTG{},
		words: wordlists.NewManager(filepath.Join(dir, "builtin"), filepath.Join(dir, "user"))}
	h := New(Deps{Store: st, Bus: bus, Sched: f.sched, Words: f.words, Telegram: f.tg, Auth: a,
		TelegramEnv: notifier.Config{}, MaxWordlistBytes: 1 << 20})
	f.srv = httptest.NewServer(h)
	t.Cleanup(func() { f.srv.Close(); bus.Close(); st.Close() })
	return f
}

func (f *fixture) client(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (f *fixture) login(t *testing.T) *http.Client {
	t.Helper()
	c := f.client(t)
	resp := f.do(t, c, "POST", "/api/auth/login", map[string]string{"password": password})
	if resp.StatusCode != 200 {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	return c
}

func (f *fixture) do(t *testing.T, c *http.Client, method, path string, body any) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestEverythingExceptHealthRequiresLogin(t *testing.T) {
	f := newFixture(t)
	c := f.client(t)
	for _, p := range []string{"/api/jobs", "/api/jobs/1", "/api/results", "/api/results/export", "/api/logs",
		"/api/logs/stream", "/api/wordlists", "/api/settings", "/api/stats"} {
		resp := f.do(t, c, "GET", p, nil)
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("GET %s unauthenticated = %d, want 401", p, resp.StatusCode)
		}
	}
	for _, p := range []string{"/api/jobs", "/api/settings/telegram/test", "/api/jobs/1/pause"} {
		resp := f.do(t, c, "POST", p, map[string]string{})
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("POST %s unauthenticated = %d, want 401", p, resp.StatusCode)
		}
	}
	resp := f.do(t, c, "GET", "/api/health", nil)
	var h map[string]any
	decode(t, resp, &h)
	if resp.StatusCode != 200 || h["ok"] != true {
		t.Fatalf("health = %d %v", resp.StatusCode, h)
	}
}

func TestLoginLogoutAndMe(t *testing.T) {
	f := newFixture(t)
	c := f.client(t)

	resp := f.do(t, c, "GET", "/api/auth/me", nil)
	var me map[string]bool
	decode(t, resp, &me)
	if me["authenticated"] {
		t.Fatal("me before login must be unauthenticated")
	}

	resp = f.do(t, c, "POST", "/api/auth/login", map[string]string{"password": "wrong"})
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("wrong password = %d, want 401", resp.StatusCode)
	}

	resp = f.do(t, c, "POST", "/api/auth/login", map[string]string{"password": password})
	resp.Body.Close()
	var sess *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == auth.CookieName {
			sess = ck
		}
	}
	if resp.StatusCode != 200 || sess == nil || !sess.HttpOnly || sess.SameSite != http.SameSiteStrictMode {
		t.Fatalf("login = %d cookie=%+v", resp.StatusCode, sess)
	}
	resp = f.do(t, c, "GET", "/api/jobs", nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("authenticated GET /api/jobs = %d", resp.StatusCode)
	}

	resp = f.do(t, c, "POST", "/api/auth/logout", nil)
	resp.Body.Close()
	// replay the old cookie: must be dead server-side
	req, _ := http.NewRequest("GET", f.srv.URL+"/api/jobs", nil)
	req.AddCookie(sess)
	resp2, _ := http.DefaultClient.Do(req)
	resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Fatalf("replayed session after logout = %d, want 401", resp2.StatusCode)
	}
}

func TestLoginLockoutReturns429(t *testing.T) {
	f := newFixture(t)
	c := f.client(t)
	for i := 0; i < 5; i++ {
		resp := f.do(t, c, "POST", "/api/auth/login", map[string]string{"password": "bad"})
		resp.Body.Close()
	}
	resp := f.do(t, c, "POST", "/api/auth/login", map[string]string{"password": password})
	resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("login while locked = %d, want 429", resp.StatusCode)
	}
}

func TestJobsLifecycleAndErrorMapping(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)

	resp := f.do(t, c, "POST", "/api/jobs", map[string]any{"suffix": ".li", "pattern": "d", "length": 3, "name": "x"})
	var job store.Job
	decode(t, resp, &job)
	if resp.StatusCode != 201 || job.ID == 0 || job.Name != "x" {
		t.Fatalf("create = %d %+v", resp.StatusCode, job)
	}
	if len(f.sched.created) != 1 || f.sched.created[0].Length != 3 {
		t.Fatalf("params not passed through: %+v", f.sched.created)
	}

	resp = f.do(t, c, "GET", fmt.Sprintf("/api/jobs/%d", job.ID), nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("get job = %d", resp.StatusCode)
	}
	resp = f.do(t, c, "POST", fmt.Sprintf("/api/jobs/%d/pause", job.ID), nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("pause = %d", resp.StatusCode)
	}

	f.sched.createErr = fmt.Errorf("%w: bad regex", scheduler.ErrInvalid)
	resp = f.do(t, c, "POST", "/api/jobs", map[string]any{"suffix": ".li"})
	var e map[string]string
	decode(t, resp, &e)
	if resp.StatusCode != 400 || !strings.Contains(e["error"], "bad regex") {
		t.Fatalf("invalid create = %d %v", resp.StatusCode, e)
	}

	f.sched.stateErr = fmt.Errorf("%w: cannot pause a done job", scheduler.ErrState)
	resp = f.do(t, c, "POST", fmt.Sprintf("/api/jobs/%d/pause", job.ID), nil)
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("state error = %d, want 409", resp.StatusCode)
	}
	f.sched.stateErr = nil

	resp = f.do(t, c, "POST", "/api/jobs/9999/pause", nil)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("missing job = %d, want 404", resp.StatusCode)
	}
	resp = f.do(t, c, "POST", fmt.Sprintf("/api/jobs/%d/explode", job.ID), nil)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown action = %d, want 404", resp.StatusCode)
	}
	resp = f.do(t, c, "GET", "/api/jobs/abc", nil)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("non numeric id = %d, want 400", resp.StatusCode)
	}

	resp = f.do(t, c, "DELETE", fmt.Sprintf("/api/jobs/%d", job.ID), nil)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("delete = %d, want 204", resp.StatusCode)
	}
}

func TestMalformedAndOversizedBodiesAreRejected(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	req, _ := http.NewRequest("POST", f.srv.URL+"/api/jobs", strings.NewReader("{not json"))
	resp, _ := c.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("malformed json = %d, want 400", resp.StatusCode)
	}
	req, _ = http.NewRequest("POST", f.srv.URL+"/api/jobs", strings.NewReader(`{"name":"`+strings.Repeat("a", 2<<20)+`"}`))
	resp, _ = c.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 413 && resp.StatusCode != 400 {
		t.Fatalf("oversized json = %d, want 413/400", resp.StatusCode)
	}
}

func TestResultsListAndCSVExport(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	ctx := context.Background()
	jid, _ := f.st.CreateJob(ctx, &store.Job{Name: "j", Suffix: ".li", Workers: 1, Status: "done"})
	f.st.InsertResult(ctx, &store.Result{JobID: jid, Domain: "free.li", Status: "available", Signatures: ""})
	f.st.InsertResult(ctx, &store.Result{JobID: jid, Domain: "odd.li", Status: "unknown", Signatures: "=cmd|calc"})

	resp := f.do(t, c, "GET", "/api/results?status=available", nil)
	var out struct {
		Items []store.Result `json:"items"`
		Total int64          `json:"total"`
	}
	decode(t, resp, &out)
	if out.Total != 1 || len(out.Items) != 1 || out.Items[0].Domain != "free.li" {
		t.Fatalf("results = %+v", out)
	}

	resp = f.do(t, c, "GET", "/api/results/export", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("csv headers: %v", resp.Header)
	}
	csv := string(body)
	if !strings.HasPrefix(csv, "domain,status,job_id,signatures,found_at") ||
		!strings.Contains(csv, "free.li,available") {
		t.Fatalf("csv = %q", csv)
	}
	if strings.Contains(csv, ",=cmd") {
		t.Fatalf("CSV formula injection not neutralised: %q", csv)
	}
}

func TestSettingsMaskTokenAndKeepOnPlaceholder(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	const token = "123456789:AAHdummyDUMMYdummyDUMMYdummyDUMMY12345"

	resp := f.do(t, c, "PUT", "/api/settings", map[string]string{"telegram_token": token, "telegram_chat_id": "42"})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("PUT settings = %d", resp.StatusCode)
	}

	resp = f.do(t, c, "GET", "/api/settings", nil)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(raw), "AAHdummy") {
		t.Fatalf("GET /api/settings leaked the token: %s", raw)
	}
	var s struct {
		TelegramToken  string `json:"telegram_token"`
		TelegramChatID string `json:"telegram_chat_id"`
		Configured     bool   `json:"telegram_configured"`
	}
	json.Unmarshal(raw, &s)
	if !s.Configured || s.TelegramChatID != "42" || !strings.Contains(s.TelegramToken, "****") {
		t.Fatalf("settings = %s", raw)
	}

	// echoing the masked value back must not overwrite the real token
	resp = f.do(t, c, "PUT", "/api/settings", map[string]string{"telegram_token": s.TelegramToken, "telegram_chat_id": "43"})
	resp.Body.Close()
	stored, _, _ := f.st.GetSetting(context.Background(), "telegram_token")
	if stored != token {
		t.Fatalf("masked placeholder overwrote token: %q", stored)
	}
	if id, _, _ := f.st.GetSetting(context.Background(), "telegram_chat_id"); id != "43" {
		t.Fatalf("chat id not updated: %q", id)
	}

	for _, bad := range []map[string]string{
		{"telegram_token": "not a token"},
		{"telegram_chat_id": "abc def"},
	} {
		resp = f.do(t, c, "PUT", "/api/settings", bad)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("PUT %v = %d, want 400", bad, resp.StatusCode)
		}
	}
}

func TestTelegramTestEndpoint(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	resp := f.do(t, c, "POST", "/api/settings/telegram/test", nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("test ok = %d", resp.StatusCode)
	}
	f.tg.err = errors.New("Telegram API 错误 (HTTP 400): Bad Request: chat not found")
	resp = f.do(t, c, "POST", "/api/settings/telegram/test", nil)
	var e map[string]string
	decode(t, resp, &e)
	if resp.StatusCode != 502 || !strings.Contains(e["error"], "chat not found") {
		t.Fatalf("test failure = %d %v", resp.StatusCode, e)
	}
}

func TestLogsHistoryAndStream(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	f.st.InsertLogs(context.Background(), []store.LogEntry{
		{ID: 1, Level: "info", Message: "first", Time: time.Now()},
		{ID: 2, Level: "debug", Message: "noise", Time: time.Now()},
		{ID: 3, Level: "error", Message: "third", Time: time.Now()},
	})
	resp := f.do(t, c, "GET", "/api/logs?level=info", nil)
	var hist struct {
		Items []store.LogEntry `json:"items"`
	}
	decode(t, resp, &hist)
	if len(hist.Items) != 2 || hist.Items[0].Message != "first" || hist.Items[1].Message != "third" {
		t.Fatalf("history (chronological, debug excluded) = %+v", hist.Items)
	}

	req, _ := http.NewRequest("GET", f.srv.URL+"/api/logs/stream", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	lines := make(chan string, 10)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	go func() {
		time.Sleep(150 * time.Millisecond)
		f.bus.Log("warn", 5, "live message")
	}()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("stream closed before the live message")
			}
			if strings.HasPrefix(l, "data:") && strings.Contains(l, "live message") {
				return
			}
		case <-deadline:
			t.Fatal("did not receive the live log over SSE")
		}
	}
}

func TestWordlistUploadAndList(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("name", "My Words")
	fw, _ := mw.CreateFormFile("file", "words.txt")
	fw.Write([]byte("alpha\nbeta\n"))
	mw.Close()
	req, _ := http.NewRequest("POST", f.srv.URL+"/api/wordlists", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var info wordlists.Info
	decode(t, resp, &info)
	if resp.StatusCode != 201 || info.ID != "user:my-words" || info.Count != 2 {
		t.Fatalf("upload = %d %+v", resp.StatusCode, info)
	}
	resp = f.do(t, c, "GET", "/api/wordlists", nil)
	var list struct {
		Items []wordlists.Info `json:"items"`
	}
	decode(t, resp, &list)
	if len(list.Items) != 1 || list.Items[0].ID != "user:my-words" {
		t.Fatalf("list = %+v", list)
	}
}

func TestUnknownRoutesAndMethodsReturnJSON(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	resp := f.do(t, c, "GET", "/api/nope", nil)
	var e map[string]string
	decode(t, resp, &e)
	if resp.StatusCode != 404 || e["error"] == "" {
		t.Fatalf("404 = %d %v", resp.StatusCode, e)
	}
	resp = f.do(t, c, "PATCH", "/api/jobs", nil)
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatalf("405 = %d", resp.StatusCode)
	}
}

func TestStats(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	resp := f.do(t, c, "GET", "/api/stats", nil)
	var s store.Stats
	decode(t, resp, &s)
	if resp.StatusCode != 200 {
		t.Fatalf("stats = %d", resp.StatusCode)
	}
}
