package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNoOutbounds is returned when there is nothing to put into a runtime configuration.
var ErrNoOutbounds = errors.New("no outbounds")

const (
	// BasePort is where the per-outbound local SOCKS listeners start. The port of outbound id
	// is BasePort + id%PortSpan, so it is stable across restarts and config reloads.
	BasePort = 21000
	PortSpan = 20000
)

// Entry is one stored outbound to run.
type Entry struct {
	ID     int64
	Config json.RawMessage // Xray outbound object, no tag
}

// PortFor returns the local SOCKS port assigned to outbound id.
func PortFor(base int, id int64) int { return base + int(id%PortSpan) }

func tagOut(id int64) string { return fmt.Sprintf("out-%d", id) }
func tagIn(id int64) string  { return fmt.Sprintf("in-%d", id) }

func socksInbound(tag string, port int) map[string]any {
	return map[string]any{
		"tag": tag, "listen": "127.0.0.1", "port": port, "protocol": "socks",
		"settings": map[string]any{"auth": "noauth", "udp": false},
	}
}

// BuildConfig builds the Xray configuration that exposes every entry as its own loopback SOCKS5
// listener: inbound in-<id> is routed only to outbound out-<id>. It returns the id -> port map.
func BuildConfig(entries []Entry, basePort int) ([]byte, map[int64]int, error) {
	if len(entries) == 0 {
		return nil, nil, ErrNoOutbounds
	}
	var inbounds, outbounds, rules []any
	ports := map[int64]int{}
	for _, e := range entries {
		var out map[string]any
		if err := json.Unmarshal(e.Config, &out); err != nil || out == nil {
			return nil, nil, fmt.Errorf("outbound %d: 配置不是有效的 JSON 对象", e.ID)
		}
		out["tag"] = tagOut(e.ID) // the runtime owns tags
		port := PortFor(basePort, e.ID)
		ports[e.ID] = port
		inbounds = append(inbounds, socksInbound(tagIn(e.ID), port))
		outbounds = append(outbounds, out)
		rules = append(rules, map[string]any{"type": "field", "inboundTag": []string{tagIn(e.ID)}, "outboundTag": tagOut(e.ID)})
	}
	cfg := map[string]any{
		"log":       map[string]any{"loglevel": "warning"},
		"inbounds":  inbounds,
		"outbounds": outbounds,
		"routing":   map[string]any{"domainStrategy": "AsIs", "rules": rules},
	}
	raw, err := json.Marshal(cfg)
	return raw, ports, err
}

// BuildSingle builds a throw-away configuration for testing one (possibly unsaved) outbound on
// the given local port.
func BuildSingle(cfg json.RawMessage, port int) ([]byte, error) {
	const id = 0
	var out map[string]any
	if err := json.Unmarshal(cfg, &out); err != nil || out == nil {
		return nil, errors.New("配置不是有效的 JSON 对象")
	}
	out["tag"] = tagOut(id)
	c := map[string]any{
		"log":       map[string]any{"loglevel": "warning"},
		"inbounds":  []any{socksInbound(tagIn(id), port)},
		"outbounds": []any{out},
		"routing": map[string]any{"domainStrategy": "AsIs", "rules": []any{
			map[string]any{"type": "field", "inboundTag": []string{tagIn(id)}, "outboundTag": tagOut(id)}}},
	}
	return json.Marshal(c)
}
