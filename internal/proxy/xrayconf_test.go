package proxy

import (
	"encoding/json"
	"errors"
	"testing"
)

func entry(id int64, cfg string) Entry { return Entry{ID: id, Config: json.RawMessage(cfg)} }

const vlessCfg = `{"protocol":"vless","settings":{"address":"a.example","port":443,"id":"u","encryption":"none"}}`

func TestBuildConfigWiresOneSocksInboundPerOutbound(t *testing.T) {
	raw, ports, err := BuildConfig([]Entry{entry(3, vlessCfg), entry(7, vlessCfg)}, 21000)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Log      map[string]any   `json:"log"`
		Inbounds []map[string]any `json:"inbounds"`
		Outbound []map[string]any `json:"outbounds"`
		Routing  struct {
			Rules []map[string]any `json:"rules"`
		} `json:"routing"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Inbounds) != 2 || len(c.Outbound) != 2 || len(c.Routing.Rules) != 2 {
		t.Fatalf("counts: in=%d out=%d rules=%d", len(c.Inbounds), len(c.Outbound), len(c.Routing.Rules))
	}
	if ports[3] != 21003 || ports[7] != 21007 {
		t.Fatalf("ports = %v, want deterministic base+id", ports)
	}
	for _, in := range c.Inbounds {
		if in["listen"] != "127.0.0.1" || in["protocol"] != "socks" {
			t.Fatalf("inbound must be loopback-only SOCKS: %v", in)
		}
		if s, _ := in["settings"].(map[string]any); s["auth"] != "noauth" {
			t.Fatalf("inbound settings: %v", in["settings"])
		}
	}
	if c.Inbounds[0]["tag"] != "in-3" || c.Outbound[0]["tag"] != "out-3" {
		t.Fatalf("tags: %v / %v", c.Inbounds[0]["tag"], c.Outbound[0]["tag"])
	}
	r := c.Routing.Rules[1]
	if r["outboundTag"] != "out-7" || r["inboundTag"].([]any)[0] != "in-7" {
		t.Fatalf("rule must route in-7 to out-7: %v", r)
	}
	if c.Log["loglevel"] != "warning" {
		t.Fatalf("log = %v", c.Log)
	}
	// the stored config keeps its protocol/settings intact
	if c.Outbound[0]["protocol"] != "vless" {
		t.Fatalf("outbound = %v", c.Outbound[0])
	}
}

func TestBuildConfigRejectsEmptyAndBrokenInput(t *testing.T) {
	if _, _, err := BuildConfig(nil, 21000); !errors.Is(err, ErrNoOutbounds) {
		t.Fatalf("empty: err = %v, want ErrNoOutbounds", err)
	}
	if _, _, err := BuildConfig([]Entry{entry(1, `{not json`)}, 21000); err == nil {
		t.Fatal("broken outbound JSON must be rejected")
	}
	if _, _, err := BuildConfig([]Entry{entry(1, `[1,2]`)}, 21000); err == nil {
		t.Fatal("a non-object outbound must be rejected")
	}
}

func TestBuildConfigOverridesAStoredTag(t *testing.T) {
	raw, _, err := BuildConfig([]Entry{entry(5, `{"tag":"evil","protocol":"freedom"}`)}, 21000)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Outbound []map[string]any `json:"outbounds"`
	}
	json.Unmarshal(raw, &c)
	if c.Outbound[0]["tag"] != "out-5" {
		t.Fatalf("tag = %v; the runtime must own tags", c.Outbound[0]["tag"])
	}
}

func TestPortForStaysInRange(t *testing.T) {
	for _, id := range []int64{1, 19999, 20000, 20001, 123456789} {
		p := PortFor(21000, id)
		if p < 21000 || p >= 41000 {
			t.Errorf("PortFor(%d) = %d out of [21000, 41000)", id, p)
		}
	}
	if PortFor(21000, 1) == PortFor(21000, 2) {
		t.Fatal("distinct ids must map to distinct ports")
	}
}

func TestSingleConfigForTesting(t *testing.T) {
	raw, err := BuildSingle(json.RawMessage(vlessCfg), 24567)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Inbounds []map[string]any `json:"inbounds"`
	}
	json.Unmarshal(raw, &c)
	if len(c.Inbounds) != 1 || c.Inbounds[0]["port"] != float64(24567) {
		t.Fatalf("inbounds = %v", c.Inbounds)
	}
}
