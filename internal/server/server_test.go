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
	"sync"
	"testing"
	"time"

	"domain_scanner/internal/auth"
	"domain_scanner/internal/logbus"
	"domain_scanner/internal/notifier"
	"domain_scanner/internal/proxy"
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

// proxyHolder lets a test swap the outbound-proxy service behind the server.
type proxyHolder struct {
	mu    sync.Mutex
	inner ProxyService
}

func (h *proxyHolder) set(p ProxyService)               { h.mu.Lock(); h.inner = p; h.mu.Unlock() }
func (h *proxyHolder) get() ProxyService                { h.mu.Lock(); defer h.mu.Unlock(); return h.inner }
func (h *proxyHolder) Reload(ctx context.Context) error { return h.get().Reload(ctx) }
func (h *proxyHolder) TestOne(ctx context.Context, id int64) (proxy.ProbeResult, error) {
	return h.get().TestOne(ctx, id)
}
func (h *proxyHolder) TestAll(ctx context.Context) map[int64]proxy.ProbeResult {
	return h.get().TestAll(ctx)
}
func (h *proxyHolder) TestConfig(ctx context.Context, cfg []byte) proxy.ProbeResult {
	return h.get().TestConfig(ctx, cfg)
}
func (h *proxyHolder) Status() proxy.Status { return h.get().Status() }

type cfHolder struct {
	mu    sync.Mutex
	inner CloudflareTester
}

func (h *cfHolder) set(c CloudflareTester) { h.mu.Lock(); h.inner = c; h.mu.Unlock() }
func (h *cfHolder) Verify(ctx context.Context) error {
	h.mu.Lock()
	in := h.inner
	h.mu.Unlock()
	return in.Verify(ctx)
}

type fixture struct {
	srv    *httptest.Server
	st     *store.Store
	bus    *logbus.Bus
	sched  *fakeSched
	tg     *fakeTG
	auth   *auth.Auth
	words  *wordlists.Manager
	proxy  *proxyHolder
	cfTest *cfHolder
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
		words: wordlists.NewManager(filepath.Join(dir, "builtin"), filepath.Join(dir, "user")),
		proxy: &proxyHolder{inner: &fakeProxy{}}, cfTest: &cfHolder{inner: fakeCFTester{}}}
	h := New(Deps{Store: st, Bus: bus, Sched: f.sched, Words: f.words, Telegram: f.tg, Auth: a, Proxy: f.proxy, Cloudflare: f.cfTest,
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

func TestSettingsLogLevelAndProxyTestURL(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)

	resp := f.do(t, c, "GET", "/api/settings", nil)
	var s struct {
		LogLevel     string `json:"log_level"`
		ProxyTestURL string `json:"proxy_test_url"`
	}
	decode(t, resp, &s)
	if s.LogLevel != "debug" || s.ProxyTestURL != proxy.DefaultTestURL {
		t.Fatalf("defaults = %+v", s)
	}

	resp = f.do(t, c, "PUT", "/api/settings", map[string]string{"log_level": "warn", "proxy_test_url": "https://example.com/204"})
	decode(t, resp, &s)
	if resp.StatusCode != 200 || s.LogLevel != "warn" || s.ProxyTestURL != "https://example.com/204" {
		t.Fatalf("after PUT = %d %+v", resp.StatusCode, s)
	}
	if f.bus.MinLevel() != "warn" {
		t.Fatalf("the bus must apply the new level at once, got %q", f.bus.MinLevel())
	}
	if v, _, _ := f.st.GetSetting(context.Background(), KeyLogLevel); v != "warn" {
		t.Fatalf("level not persisted: %q", v)
	}

	for _, bad := range []map[string]string{{"log_level": "verbose"}, {"proxy_test_url": "ftp://x"}, {"proxy_test_url": "not a url"}} {
		resp = f.do(t, c, "PUT", "/api/settings", bad)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("PUT %v = %d, want 400", bad, resp.StatusCode)
		}
	}
}

func TestSavedSecretsAreRedactedFromLogs(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	const token = "123456789:AAHdummyDUMMYdummyDUMMYdummyDUMMY12345"
	resp := f.do(t, c, "PUT", "/api/settings", map[string]string{"telegram_token": token, "telegram_chat_id": "42"})
	resp.Body.Close()
	f.bus.Log("info", 0, "leaky line with %s inside", token)
	last := f.bus.Recent(1, "", 0)[0]
	if strings.Contains(last.Message, "AAHdummy") {
		t.Fatalf("a saved token reached the logs: %q", last.Message)
	}
}

func TestCloudflareSettingsAreMaskedValidatedAndRegistrationPolicyRoundTrips(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	const acct = "0123456789abcdef0123456789abcdef"
	const tok = "cfat_SECRETSECRETSECRETSECRETSECRET1234"

	resp := f.do(t, c, "PUT", "/api/settings", map[string]any{
		"cloudflare_account_id": acct, "cloudflare_token": tok,
		"register_confirm": false, "register_max_price": "12.5", "register_daily_cap": 3, "push_unconfirmed": false})
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.Contains(string(raw), "SECRETSECRET") {
		t.Fatalf("PUT = %d, body must not echo the token: %s", resp.StatusCode, raw)
	}
	var s struct {
		Acct       string `json:"cloudflare_account_id"`
		Token      string `json:"cloudflare_token"`
		Configured bool   `json:"cloudflare_configured"`
		Confirm    bool   `json:"register_confirm"`
		MaxPrice   string `json:"register_max_price"`
		Cap        int    `json:"register_daily_cap"`
		Unconfirm  bool   `json:"push_unconfirmed"`
	}
	json.Unmarshal(raw, &s)
	if s.Acct != acct || !strings.HasSuffix(s.Token, "1234") || !strings.HasPrefix(s.Token, "****") || !s.Configured ||
		s.Confirm || s.MaxPrice != "12.5" || s.Cap != 3 || s.Unconfirm {
		t.Fatalf("settings = %+v", s)
	}
	// echoing the mask must keep the stored token
	resp = f.do(t, c, "PUT", "/api/settings", map[string]any{"cloudflare_token": s.Token, "register_daily_cap": 4})
	resp.Body.Close()
	if stored, _, _ := f.st.GetSetting(context.Background(), "cloudflare_token"); stored != tok {
		t.Fatalf("mask overwrote the token: %q", stored)
	}
	// the token must never reach the logs
	f.bus.Log("info", 0, "using "+tok)
	if strings.Contains(f.bus.Recent(1, "", 0)[0].Message, "SECRETSECRET") {
		t.Fatal("cloudflare token reached the logs")
	}

	for name, body := range map[string]map[string]any{
		"short account id":   {"cloudflare_account_id": "abc"},
		"weird token":        {"cloudflare_token": "has spaces and !!"},
		"negative price":     {"register_max_price": "-1"},
		"price not a number": {"register_max_price": "cheap"},
		"cap too large":      {"register_daily_cap": 5000},
		"negative cap":       {"register_daily_cap": -2},
	} {
		resp = f.do(t, c, "PUT", "/api/settings", body)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("%s: PUT = %d, want 400", name, resp.StatusCode)
		}
	}
}

type fakeCFTester struct{ err error }

func (f fakeCFTester) Verify(context.Context) error { return f.err }

func TestCloudflareTestEndpoint(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	f.cfTest.set(fakeCFTester{})
	resp := f.do(t, c, "POST", "/api/settings/cloudflare/test", nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("ok case = %d", resp.StatusCode)
	}
	f.cfTest.set(fakeCFTester{err: errors.New("Token 状态为 disabled")})
	resp = f.do(t, c, "POST", "/api/settings/cloudflare/test", nil)
	var e map[string]string
	decode(t, resp, &e)
	if resp.StatusCode != 502 || !strings.Contains(e["error"], "disabled") {
		t.Fatalf("failure case = %d %v", resp.StatusCode, e)
	}
}

func TestResultsCloudflareFilterAndCSVColumns(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	ctx := context.Background()
	jid, _ := f.st.CreateJob(ctx, &store.Job{Name: "j", Suffix: ".com", Workers: 1, Status: "done"})
	a := &store.Result{JobID: jid, Domain: "ok.com", Status: "available"}
	b := &store.Result{JobID: jid, Domain: "no.com", Status: "available"}
	f.st.InsertResult(ctx, a)
	f.st.InsertResult(ctx, b)
	f.st.UpdateResultCF(ctx, a.ID, store.CFUpdate{Status: "confirmed", Price: "10.46", Currency: "USD"})
	f.st.UpdateResultCF(ctx, b.ID, store.CFUpdate{Status: "rejected", Reason: "domain_unavailable"})

	resp := f.do(t, c, "GET", "/api/results?cf_status=confirmed", nil)
	var out struct {
		Items []store.Result `json:"items"`
		Total int64          `json:"total"`
	}
	decode(t, resp, &out)
	if out.Total != 1 || out.Items[0].Domain != "ok.com" || out.Items[0].CFPrice != "10.46" {
		t.Fatalf("filtered = %+v", out)
	}

	resp = f.do(t, c, "GET", "/api/results/export", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csv := string(body)
	if !strings.HasPrefix(csv, "domain,status,job_id,signatures,found_at,cloudflare,price,currency,registration") ||
		!strings.Contains(csv, "ok.com,available") || !strings.Contains(csv, "confirmed,10.46,USD") ||
		!strings.Contains(csv, "rejected:domain_unavailable") {
		t.Fatalf("csv = %q", csv)
	}
}

func TestAccessLogRecordsRequestsWithoutSecrets(t *testing.T) {
	f := newFixture(t)
	c := f.login(t) // POST /api/auth/login with the real password
	resp := f.do(t, c, "GET", "/api/stats", nil)
	resp.Body.Close()
	resp = f.do(t, f.client(t), "GET", "/api/jobs", nil) // unauthenticated -> 401
	resp.Body.Close()
	resp = f.do(t, c, "GET", "/api/health", nil)
	resp.Body.Close()

	var login, stats, denied *store.LogEntry
	health := 0
	for _, e := range f.bus.Recent(100, "debug", 0) {
		e := e
		if e.Component != "http" {
			continue
		}
		switch {
		case strings.Contains(e.Message, "/api/auth/login"):
			login = &e
		case strings.Contains(e.Message, "/api/stats"):
			stats = &e
		case strings.Contains(e.Message, "/api/jobs"):
			denied = &e
		case strings.Contains(e.Message, "/api/health"):
			health++
		}
	}
	if login == nil || stats == nil || denied == nil {
		t.Fatalf("missing access log lines: login=%v stats=%v denied=%v", login, stats, denied)
	}
	if login.Level != "info" || login.Fields["status"] != 200 || login.Fields["method"] != "POST" {
		t.Fatalf("login line = %+v", login)
	}
	if stats.Level != "debug" || stats.Fields["status"] != 200 {
		t.Fatalf("a polled GET is debug noise: %+v", stats)
	}
	if denied.Level != "warn" || denied.Fields["status"] != 401 {
		t.Fatalf("a 401 deserves a warning: %+v", denied)
	}
	if login.DurationMS < 0 || login.Fields["ip"] == nil {
		t.Fatalf("duration/ip missing: %+v", login)
	}
	if health != 0 {
		t.Fatal("health checks are noise and must not be logged")
	}
	for _, e := range f.bus.Recent(100, "debug", 0) {
		if strings.Contains(e.Message, password) || strings.Contains(fmt.Sprint(e.Fields), password) {
			t.Fatalf("the password reached the logs: %+v", e)
		}
	}
}

func TestDiagnosticsEndpoint(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	now := time.Now()
	f.st.InsertLogs(context.Background(), []store.LogEntry{
		{Level: "debug", Component: "check", Event: "done", Domain: "a.com", Egress: "direct", DurationMS: 120, Time: now, Message: "x",
			Fields: map[string]any{"status": "available", "err_kind": ""}},
		{Level: "debug", Component: "check", Event: "done", Domain: "b.li", Egress: "direct", DurationMS: 80, Time: now, Message: "y",
			Fields: map[string]any{"status": "unknown", "err_kind": "rate_limited"}},
	})
	resp := f.do(t, c, "GET", "/api/diagnostics?hours=6", nil)
	var d struct {
		Diagnostics store.Diagnostics `json:"diagnostics"`
		Logs        struct {
			Count int64 `json:"count"`
		} `json:"logs"`
		Egresses []map[string]any `json:"egresses"`
	}
	decode(t, resp, &d)
	if resp.StatusCode != 200 || d.Diagnostics.Checks != 2 || len(d.Diagnostics.ByEgress) != 1 ||
		d.Diagnostics.ByEgress[0].RateLimited != 1 || d.Logs.Count != 2 || len(d.Egresses) != 1 {
		t.Fatalf("diagnostics = %d %+v", resp.StatusCode, d)
	}
	for _, bad := range []string{"hours=0", "hours=abc", "hours=100000"} {
		resp = f.do(t, c, "GET", "/api/diagnostics?"+bad, nil)
		var ok struct {
			Diagnostics store.Diagnostics `json:"diagnostics"`
		}
		decode(t, resp, &ok) // out-of-range values fall back to the default window rather than failing
		if resp.StatusCode != 200 {
			t.Errorf("%s = %d", bad, resp.StatusCode)
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

func TestLogsFiltersExportAndComponents(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	now := time.Now()
	f.st.InsertLogs(context.Background(), []store.LogEntry{
		{ID: 10, Level: "debug", Component: "dns", Event: "lookup", Message: "ns foo.com", Domain: "foo.com", Egress: "direct", DurationMS: 12, Time: now},
		{ID: 11, Level: "warn", Component: "rdap", Event: "throttle", Message: "429 from nic.ch", Egress: "proxy-2", Time: now,
			Fields: map[string]any{"wait_s": 5}},
		{ID: 12, Level: "error", Component: "notifier", Event: "send_failed", Message: "chat not found", Time: now},
	})

	resp := f.do(t, c, "GET", "/api/logs?component=rdap", nil)
	var one struct {
		Items []store.LogEntry `json:"items"`
	}
	decode(t, resp, &one)
	if len(one.Items) != 1 || one.Items[0].Event != "throttle" || one.Items[0].Fields["wait_s"] != float64(5) {
		t.Fatalf("component filter = %+v", one.Items)
	}
	resp = f.do(t, c, "GET", "/api/logs?q=chat+not+found&level=warn", nil)
	decode(t, resp, &one)
	if len(one.Items) != 1 || one.Items[0].Component != "notifier" {
		t.Fatalf("text filter = %+v", one.Items)
	}
	resp = f.do(t, c, "GET", "/api/logs?domain=foo.com&egress=direct", nil)
	decode(t, resp, &one)
	if len(one.Items) != 1 || one.Items[0].DurationMS != 12 {
		t.Fatalf("domain+egress filter = %+v", one.Items)
	}

	resp = f.do(t, c, "GET", "/api/logs/export?level=warn", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-ndjson") ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("export headers: %v", resp.Header)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("export lines = %d, want 2 (warn+error): %q", len(lines), body)
	}
	var first store.LogEntry
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil || first.Component != "rdap" {
		t.Fatalf("first export line not chronological JSON: %v %q", err, lines[0])
	}

	resp = f.do(t, c, "GET", "/api/logs/components", nil)
	var comps struct {
		Items []string `json:"items"`
	}
	decode(t, resp, &comps)
	if strings.Join(comps.Items, ",") != "dns,notifier,rdap" {
		t.Fatalf("components = %v", comps.Items)
	}
}

func TestStreamFiltersByComponent(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	req, _ := http.NewRequest("GET", f.srv.URL+"/api/logs/stream?component=egress", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 20)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	go func() {
		time.Sleep(150 * time.Millisecond)
		f.bus.Emit("info", "check", "done", 0, "other component", nil)
		f.bus.Emit("info", "egress", "switch", 0, "wanted line", nil)
	}()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("stream closed")
			}
			if strings.Contains(l, "other component") {
				t.Fatal("stream delivered a line from another component")
			}
			if strings.HasPrefix(l, "data:") && strings.Contains(l, "wanted line") {
				return
			}
		case <-deadline:
			t.Fatal("did not receive the filtered line")
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
