package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"domain_scanner/internal/proxy"
	"domain_scanner/internal/store"
)

// ProxyService is what the HTTP layer needs from the outbound-proxy subsystem.
type ProxyService interface {
	Reload(ctx context.Context) error
	TestOne(ctx context.Context, id int64) (proxy.ProbeResult, error)
	TestAll(ctx context.Context) map[int64]proxy.ProbeResult
	TestConfig(ctx context.Context, cfg []byte) proxy.ProbeResult
	Status() proxy.Status
}

const maxImportBody = 4 << 20

// outboundView is an outbound as listed: everything except the credentials-bearing config.
type outboundView struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Protocol    string     `json:"protocol"`
	Address     string     `json:"address"`
	Port        int        `json:"port"`
	Transport   string     `json:"transport"`
	Security    string     `json:"security"`
	Enabled     bool       `json:"enabled"`
	LastTestAt  *time.Time `json:"last_test_at,omitempty"`
	LastOK      bool       `json:"last_ok"`
	LastDelayMS int64      `json:"last_delay_ms"`
	LastError   string     `json:"last_error"`
	LastIP      string     `json:"last_ip"`
	LastCountry string     `json:"last_country"`
	CreatedAt   time.Time  `json:"created_at"`
}

// outboundDetail adds the config (for the edit dialog) and the outcome of the xray reload.
type outboundDetail struct {
	outboundView
	Config      json.RawMessage `json:"config"`
	ReloadError string          `json:"reload_error,omitempty"`
}

func viewOf(o store.Outbound) outboundView {
	transport, security := "tcp", "none"
	var cfg struct {
		StreamSettings struct {
			Network  string `json:"network"`
			Security string `json:"security"`
		} `json:"streamSettings"`
	}
	if json.Unmarshal(o.Config, &cfg) == nil {
		if cfg.StreamSettings.Network != "" {
			transport = cfg.StreamSettings.Network
		}
		if cfg.StreamSettings.Security != "" {
			security = cfg.StreamSettings.Security
		}
	}
	return outboundView{ID: o.ID, Name: o.Name, Protocol: o.Protocol, Address: o.Address, Port: o.Port,
		Transport: transport, Security: security, Enabled: o.Enabled, LastTestAt: o.LastTestAt, LastOK: o.LastOK,
		LastDelayMS: o.LastDelayMS, LastError: o.LastError, LastIP: o.LastIP, LastCountry: o.LastCountry, CreatedAt: o.CreatedAt}
}

// configBytes accepts the config either as a JSON object or as a string holding JSON (the JSON
// tab sends text).
func configBytes(raw json.RawMessage) ([]byte, bool) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil, false
		}
		return []byte(s), true
	}
	return raw, true
}

func (a *api) reloadProxies(r *http.Request) string {
	if err := a.Proxy.Reload(r.Context()); err != nil {
		return err.Error()
	}
	return ""
}

func (a *api) listOutbounds(w http.ResponseWriter, r *http.Request) {
	list, err := a.Store.ListOutbounds(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	items := make([]outboundView, 0, len(list))
	for _, o := range list {
		items = append(items, viewOf(o))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "status": a.Proxy.Status()})
}

func (a *api) getOutbound(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	o, err := a.Store.GetOutbound(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, outboundDetail{outboundView: viewOf(*o), Config: o.Config})
}

type outboundBody struct {
	Name    string          `json:"name"`
	Config  json.RawMessage `json:"config"`
	Enabled *bool           `json:"enabled"`
}

func (a *api) createOutbound(w http.ResponseWriter, r *http.Request) {
	var b outboundBody
	if !decodeJSON(w, r, maxJSONBody, &b) {
		return
	}
	cfg, ok := configBytes(b.Config)
	if !ok {
		writeErr(w, http.StatusBadRequest, "缺少 config")
		return
	}
	imp, err := proxy.Validate(cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(b.Name)
	if name == "" {
		name = imp.Name
	}
	enabled := b.Enabled == nil || *b.Enabled
	id, err := a.Store.CreateOutbound(r.Context(), &store.Outbound{Name: name, Protocol: imp.Protocol, Address: imp.Address,
		Port: imp.Port, Config: imp.Config, Enabled: enabled})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.Bus.Logger("proxy").Info("created", 0, "新增出站代理「"+name+"」("+imp.Protocol+" "+imp.Address+")", map[string]any{"id": id, "protocol": imp.Protocol, "enabled": enabled})
	reloadErr := a.reloadProxies(r)
	o, _ := a.Store.GetOutbound(r.Context(), id)
	writeJSON(w, http.StatusCreated, outboundDetail{outboundView: viewOf(*o), Config: o.Config, ReloadError: reloadErr})
}

func (a *api) updateOutbound(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	cur, err := a.Store.GetOutbound(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	var b outboundBody
	if !decodeJSON(w, r, maxJSONBody, &b) {
		return
	}
	if cfg, has := configBytes(b.Config); has {
		imp, err := proxy.Validate(cfg)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		cur.Protocol, cur.Address, cur.Port, cur.Config = imp.Protocol, imp.Address, imp.Port, imp.Config
	}
	if n := strings.TrimSpace(b.Name); n != "" {
		cur.Name = n
	}
	if b.Enabled != nil {
		cur.Enabled = *b.Enabled
	}
	if err := a.Store.UpdateOutbound(r.Context(), cur); err != nil {
		a.fail(w, err)
		return
	}
	a.Bus.Logger("proxy").Info("updated", 0, "更新出站代理「"+cur.Name+"」", map[string]any{"id": id, "enabled": cur.Enabled})
	reloadErr := a.reloadProxies(r)
	o, _ := a.Store.GetOutbound(r.Context(), id)
	writeJSON(w, http.StatusOK, outboundDetail{outboundView: viewOf(*o), Config: o.Config, ReloadError: reloadErr})
}

func (a *api) deleteOutbound(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.Store.DeleteOutbound(r.Context(), id); err != nil {
		a.fail(w, err)
		return
	}
	a.Bus.Logger("proxy").Info("deleted", 0, "删除出站代理", map[string]any{"id": id})
	a.reloadProxies(r)
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) importOutbounds(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Text string `json:"text"`
		Save bool   `json:"save"`
	}
	if !decodeJSON(w, r, maxImportBody, &b) {
		return
	}
	items, errs := proxy.Parse(b.Text)
	if items == nil {
		items = []proxy.Imported{}
	}
	if errs == nil {
		errs = []proxy.ImportError{}
	}
	out := map[string]any{"items": items, "errors": errs, "saved": []int64{}, "skipped": 0}
	if b.Save && len(items) > 0 {
		existing, err := a.Store.ListOutbounds(r.Context())
		if err != nil {
			a.fail(w, err)
			return
		}
		have := map[string]bool{}
		for _, o := range existing {
			have[string(o.Config)] = true
		}
		saved, skipped := []int64{}, 0
		for _, it := range items {
			if have[string(it.Config)] {
				skipped++
				continue
			}
			id, err := a.Store.CreateOutbound(r.Context(), &store.Outbound{Name: it.Name, Protocol: it.Protocol,
				Address: it.Address, Port: it.Port, Config: it.Config, Enabled: true})
			if err != nil {
				a.fail(w, err)
				return
			}
			have[string(it.Config)] = true
			saved = append(saved, id)
		}
		out["saved"], out["skipped"] = saved, skipped
		a.Bus.Logger("proxy").Info("imported", 0, "导入出站代理", map[string]any{"saved": len(saved), "skipped": skipped, "errors": len(errs)})
		if len(saved) > 0 {
			if msg := a.reloadProxies(r); msg != "" {
				out["reload_error"] = msg
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) testOutbound(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	res, err := a.Proxy.TestOne(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *api) testAllOutbounds(w http.ResponseWriter, r *http.Request) {
	res := a.Proxy.TestAll(r.Context())
	out := make(map[string]proxy.ProbeResult, len(res))
	for id, v := range res {
		out[strconv.FormatInt(id, 10)] = v
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

func (a *api) testOutboundConfig(w http.ResponseWriter, r *http.Request) {
	var b outboundBody
	if !decodeJSON(w, r, maxJSONBody, &b) {
		return
	}
	cfg, ok := configBytes(b.Config)
	if !ok {
		writeErr(w, http.StatusBadRequest, "缺少 config")
		return
	}
	imp, err := proxy.Validate(cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.Proxy.TestConfig(r.Context(), imp.Config))
}

func (a *api) reloadOutbounds(w http.ResponseWriter, r *http.Request) {
	if msg := a.reloadProxies(r); msg != "" {
		writeErr(w, http.StatusBadGateway, msg)
		return
	}
	writeJSON(w, http.StatusOK, a.Proxy.Status())
}

func (a *api) listEgresses(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": a.Proxy.Status().Egresses})
}
