// Package proxy manages outbound proxies: parsing share links and Xray JSON, validating and
// turning them into a running Xray instance (one local SOCKS5 listener per outbound), and
// testing that each one actually works.
package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Imported is a parsed outbound ready to be stored. Config is an Xray outbound object without
// a tag (tags are assigned when the runtime configuration is built).
type Imported struct {
	Name     string          `json:"name"`
	Protocol string          `json:"protocol"`
	Address  string          `json:"address"`
	Port     int             `json:"port"`
	Config   json.RawMessage `json:"config"`
}

// ImportError describes one input line (or JSON element) that could not be imported.
type ImportError struct {
	Line    int    `json:"line"`
	Input   string `json:"input"`
	Message string `json:"message"`
}

// usable protocols; freedom is allowed so a loopback "direct through xray" outbound can be used
// to test the machinery without a remote proxy.
var allowedProtocols = map[string]bool{
	"vless": true, "vmess": true, "trojan": true, "shadowsocks": true, "socks": true, "http": true,
	"wireguard": true, "hysteria": true, "freedom": true,
}

// protocols that never make a useful scan egress; skipped when importing a full Xray config.
var skipOnFullConfig = map[string]bool{"freedom": true, "blackhole": true, "dns": true, "loopback": true}

// Validate checks an Xray outbound object and extracts its endpoint. The returned Imported has
// the tag removed and the JSON normalised.
func Validate(cfg []byte) (Imported, error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(cfg))
	if err := dec.Decode(&m); err != nil {
		return Imported{}, fmt.Errorf("不是有效的 JSON:%v", err)
	}
	return fromObject(m)
}

func fromObject(m map[string]any) (Imported, error) {
	proto, _ := m["protocol"].(string)
	proto = strings.ToLower(strings.TrimSpace(proto))
	if proto == "" {
		return Imported{}, errors.New("缺少 protocol 字段")
	}
	if !allowedProtocols[proto] {
		return Imported{}, fmt.Errorf("不支持的协议 %q(支持 vless、vmess、trojan、shadowsocks、socks、http、wireguard、hysteria、freedom)", proto)
	}
	m["protocol"] = proto
	name, _ := m["tag"].(string)
	delete(m, "tag")
	addr, port, err := endpointOf(proto, m)
	if err != nil {
		return Imported{}, err
	}
	if name = strings.TrimSpace(name); name == "" {
		name = defaultName(proto, addr, port)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return Imported{}, err
	}
	return Imported{Name: name, Protocol: proto, Address: addr, Port: port, Config: raw}, nil
}

func defaultName(proto, addr string, port int) string {
	if addr == "" {
		return proto
	}
	return fmt.Sprintf("%s %s", proto, net.JoinHostPort(addr, strconv.Itoa(port)))
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func firstOf(v any) map[string]any {
	if arr, ok := v.([]any); ok && len(arr) > 0 {
		return asMap(arr[0])
	}
	return nil
}

func numOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(n))
		return i
	}
	return 0
}

// endpointOf finds the server address and port in any of the layouts Xray accepts
// (flat settings, vnext, servers, wireguard peers).
func endpointOf(proto string, m map[string]any) (string, int, error) {
	settings := asMap(m["settings"])
	if proto == "freedom" {
		return "", 0, nil
	}
	if settings == nil {
		return "", 0, errors.New("缺少 settings")
	}
	var addr string
	var port int
	switch {
	case settings["address"] != nil:
		addr, _ = settings["address"].(string)
		port = numOf(settings["port"])
	case firstOf(settings["vnext"]) != nil:
		v := firstOf(settings["vnext"])
		addr, _ = v["address"].(string)
		port = numOf(v["port"])
	case firstOf(settings["servers"]) != nil:
		v := firstOf(settings["servers"])
		addr, _ = v["address"].(string)
		port = numOf(v["port"])
	case firstOf(settings["peers"]) != nil:
		ep, _ := firstOf(settings["peers"])["endpoint"].(string)
		h, p, err := net.SplitHostPort(ep)
		if err != nil {
			return "", 0, fmt.Errorf("wireguard endpoint 格式应为 host:port:%v", err)
		}
		addr, port = h, numOf(p)
	}
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", 0, errors.New("缺少服务器地址")
	}
	if port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("端口 %d 无效(应为 1-65535)", port)
	}
	return addr, port, nil
}
