package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"domain_scanner/internal/egress"
	"domain_scanner/internal/proxy"
	"domain_scanner/internal/store"
)

type fakeProxy struct {
	mu      sync.Mutex
	reloads int
	tested  []int64
	cfgs    []string
	result  proxy.ProbeResult
}

func (f *fakeProxy) Reload(context.Context) error {
	f.mu.Lock()
	f.reloads++
	f.mu.Unlock()
	return nil
}
func (f *fakeProxy) TestOne(_ context.Context, id int64) (proxy.ProbeResult, error) {
	if id == 9999 {
		return proxy.ProbeResult{}, store.ErrNotFound
	}
	f.mu.Lock()
	f.tested = append(f.tested, id)
	f.mu.Unlock()
	return f.result, nil
}
func (f *fakeProxy) TestAll(context.Context) map[int64]proxy.ProbeResult {
	return map[int64]proxy.ProbeResult{1: f.result}
}
func (f *fakeProxy) TestConfig(_ context.Context, cfg []byte) proxy.ProbeResult {
	f.mu.Lock()
	f.cfgs = append(f.cfgs, string(cfg))
	f.mu.Unlock()
	return f.result
}
func (f *fakeProxy) Status() proxy.Status {
	return proxy.Status{XrayAvailable: true, Running: true, Egresses: []egress.Status{{ID: "direct", Name: "直连", Direct: true, Enabled: true, Healthy: true}}}
}
func (f *fakeProxy) reloadCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.reloads }

const vlessJSON = `{"protocol":"vless","settings":{"address":"a.example","port":443,"id":"secret-uuid-1234","encryption":"none"},"streamSettings":{"network":"ws","security":"tls"}}`

func newProxyFixture(t *testing.T) (*fixture, *http.Client, *fakeProxy) {
	f := newFixture(t)
	fp := &fakeProxy{result: proxy.ProbeResult{OK: true, DelayMS: 120, IP: "203.0.113.7", Country: "HK"}}
	f.proxy.set(fp)
	return f, f.login(t), fp
}

func TestOutboundEndpointsRequireLogin(t *testing.T) {
	f := newFixture(t)
	c := f.client(t)
	for _, p := range []string{"/api/outbounds", "/api/outbounds/1", "/api/egresses"} {
		resp := f.do(t, c, "GET", p, nil)
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("GET %s = %d, want 401", p, resp.StatusCode)
		}
	}
	for _, p := range []string{"/api/outbounds", "/api/outbounds/import", "/api/outbounds/test-all", "/api/outbounds/test-config", "/api/outbounds/1/test"} {
		resp := f.do(t, c, "POST", p, map[string]any{})
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("POST %s = %d, want 401", p, resp.StatusCode)
		}
	}
}

func TestCreateListGetUpdateDeleteOutbound(t *testing.T) {
	f, c, fp := newProxyFixture(t)

	resp := f.do(t, c, "POST", "/api/outbounds", map[string]any{"name": "hk node", "config": json.RawMessage(vlessJSON)})
	var created struct {
		ID       int64           `json:"id"`
		Name     string          `json:"name"`
		Protocol string          `json:"protocol"`
		Address  string          `json:"address"`
		Port     int             `json:"port"`
		Enabled  bool            `json:"enabled"`
		Config   json.RawMessage `json:"config"`
	}
	decode(t, resp, &created)
	if resp.StatusCode != 201 || created.ID == 0 || created.Name != "hk node" || created.Protocol != "vless" ||
		created.Address != "a.example" || created.Port != 443 || !created.Enabled {
		t.Fatalf("create = %d %+v", resp.StatusCode, created)
	}
	if fp.reloadCount() != 1 {
		t.Fatalf("xray was reloaded %d times, want 1", fp.reloadCount())
	}

	// the list must not leak credentials; the single GET returns the full config
	resp = f.do(t, c, "GET", "/api/outbounds", nil)
	raw := new(strings.Builder)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		raw.Write(buf[:n])
		if err != nil {
			break
		}
	}
	resp.Body.Close()
	if strings.Contains(raw.String(), "secret-uuid-1234") {
		t.Fatalf("list leaked credentials: %s", raw.String())
	}
	var list struct {
		Items []struct {
			ID        int64  `json:"id"`
			Transport string `json:"transport"`
			Security  string `json:"security"`
		} `json:"items"`
		Status proxy.Status `json:"status"`
	}
	if err := json.Unmarshal([]byte(raw.String()), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Transport != "ws" || list.Items[0].Security != "tls" || !list.Status.XrayAvailable {
		t.Fatalf("list = %s", raw.String())
	}

	resp = f.do(t, c, "GET", fmt.Sprintf("/api/outbounds/%d", created.ID), nil)
	var one struct {
		Config json.RawMessage `json:"config"`
	}
	decode(t, resp, &one)
	if !strings.Contains(string(one.Config), "secret-uuid-1234") {
		t.Fatalf("GET one must include the config: %s", one.Config)
	}

	resp = f.do(t, c, "PUT", fmt.Sprintf("/api/outbounds/%d", created.ID), map[string]any{"name": "renamed", "enabled": false, "config": json.RawMessage(vlessJSON)})
	var upd struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	decode(t, resp, &upd)
	if resp.StatusCode != 200 || upd.Name != "renamed" || upd.Enabled || fp.reloadCount() != 2 {
		t.Fatalf("update = %d %+v reloads=%d", resp.StatusCode, upd, fp.reloadCount())
	}

	resp = f.do(t, c, "DELETE", fmt.Sprintf("/api/outbounds/%d", created.ID), nil)
	resp.Body.Close()
	if resp.StatusCode != 204 || fp.reloadCount() != 3 {
		t.Fatalf("delete = %d reloads=%d", resp.StatusCode, fp.reloadCount())
	}
	resp = f.do(t, c, "GET", fmt.Sprintf("/api/outbounds/%d", created.ID), nil)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("after delete GET = %d, want 404", resp.StatusCode)
	}
}

func TestCreateOutboundValidation(t *testing.T) {
	f, c, _ := newProxyFixture(t)
	bad := map[string]any{
		"unknown protocol": map[string]any{"config": map[string]any{"protocol": "teleport", "settings": map[string]any{"address": "a", "port": 1}}},
		"no endpoint":      map[string]any{"config": map[string]any{"protocol": "vless", "settings": map[string]any{}}},
		"no config":        map[string]any{"name": "x"},
		"config not json":  map[string]any{"config": "{broken"},
	}
	for name, body := range bad {
		resp := f.do(t, c, "POST", "/api/outbounds", body)
		var e map[string]string
		decode(t, resp, &e)
		if resp.StatusCode != 400 || e["error"] == "" {
			t.Errorf("%s: %d %v, want 400 with a message", name, resp.StatusCode, e)
		}
	}
	// a config given as a JSON string is accepted too (the JSON tab sends text)
	resp := f.do(t, c, "POST", "/api/outbounds", map[string]any{"config": vlessJSON})
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("config as string = %d, want 201", resp.StatusCode)
	}
}

func TestImportPreviewThenSaveAndDeduplicate(t *testing.T) {
	f, c, fp := newProxyFixture(t)
	links := "vless://11111111-2222-3333-4444-555555555555@a.example:443?security=tls&type=ws&path=%2Fx#one\n" +
		"trojan://pw@b.example:443#two\nthis is not a link"

	resp := f.do(t, c, "POST", "/api/outbounds/import", map[string]any{"text": links})
	var prev struct {
		Items  []proxy.Imported    `json:"items"`
		Errors []proxy.ImportError `json:"errors"`
		Saved  []int64             `json:"saved"`
	}
	decode(t, resp, &prev)
	if resp.StatusCode != 200 || len(prev.Items) != 2 || len(prev.Errors) != 1 || len(prev.Saved) != 0 {
		t.Fatalf("preview = %d %+v", resp.StatusCode, prev)
	}
	if list, _ := f.st.ListOutbounds(context.Background()); len(list) != 0 {
		t.Fatal("a preview must not store anything")
	}

	resp = f.do(t, c, "POST", "/api/outbounds/import", map[string]any{"text": links, "save": true})
	var saved struct {
		Saved   []int64 `json:"saved"`
		Skipped int     `json:"skipped"`
	}
	decode(t, resp, &saved)
	if len(saved.Saved) != 2 || saved.Skipped != 0 || fp.reloadCount() != 1 {
		t.Fatalf("save = %+v reloads=%d", saved, fp.reloadCount())
	}
	resp = f.do(t, c, "POST", "/api/outbounds/import", map[string]any{"text": links, "save": true})
	decode(t, resp, &saved)
	if len(saved.Saved) != 0 || saved.Skipped != 2 {
		t.Fatalf("re-import should skip duplicates: %+v", saved)
	}
}

func TestOutboundTestEndpoints(t *testing.T) {
	f, c, fp := newProxyFixture(t)
	resp := f.do(t, c, "POST", "/api/outbounds", map[string]any{"config": vlessJSON})
	var o struct{ ID int64 }
	decode(t, resp, &o)

	resp = f.do(t, c, "POST", fmt.Sprintf("/api/outbounds/%d/test", o.ID), nil)
	var r proxy.ProbeResult
	decode(t, resp, &r)
	if resp.StatusCode != 200 || !r.OK || r.IP != "203.0.113.7" || len(fp.tested) != 1 {
		t.Fatalf("test one = %d %+v tested=%v", resp.StatusCode, r, fp.tested)
	}
	resp = f.do(t, c, "POST", "/api/outbounds/9999/test", nil)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("test of a missing outbound = %d, want 404", resp.StatusCode)
	}

	resp = f.do(t, c, "POST", "/api/outbounds/test-config", map[string]any{"config": json.RawMessage(vlessJSON)})
	decode(t, resp, &r)
	if resp.StatusCode != 200 || !r.OK || len(fp.cfgs) != 1 {
		t.Fatalf("test-config = %d %+v", resp.StatusCode, r)
	}
	resp = f.do(t, c, "POST", "/api/outbounds/test-config", map[string]any{"config": map[string]any{"protocol": "nope"}})
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("test-config with an invalid outbound = %d, want 400", resp.StatusCode)
	}

	resp = f.do(t, c, "POST", "/api/outbounds/test-all", nil)
	var all struct {
		Results map[string]proxy.ProbeResult `json:"results"`
	}
	decode(t, resp, &all)
	if resp.StatusCode != 200 || len(all.Results) != 1 {
		t.Fatalf("test-all = %d %+v", resp.StatusCode, all)
	}
}

func TestEgressListForTheJobForm(t *testing.T) {
	f, c, _ := newProxyFixture(t)
	resp := f.do(t, c, "GET", "/api/egresses", nil)
	var out struct {
		Items []egress.Status `json:"items"`
	}
	decode(t, resp, &out)
	if resp.StatusCode != 200 || len(out.Items) != 1 || out.Items[0].ID != "direct" {
		t.Fatalf("egresses = %d %+v", resp.StatusCode, out)
	}
}
