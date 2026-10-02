package proxy

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Parse imports outbounds from pasted text: share links (vless/vmess/trojan/ss/socks/http, one
// per line), a base64 subscription blob of such links, or Xray JSON (a single outbound, an array
// of outbounds, or a whole config with an "outbounds" list). Bad lines are reported, not fatal.
func Parse(text string) ([]Imported, []ImportError) {
	text = strings.TrimSpace(strings.TrimPrefix(text, "\ufeff"))
	if text == "" {
		return nil, []ImportError{{Message: "内容为空"}}
	}
	if text[0] == '{' || text[0] == '[' {
		return parseJSON(text)
	}
	if !strings.Contains(text, "://") {
		if dec, ok := decodeBase64(text); ok && strings.Contains(dec, "://") {
			text = dec // a subscription body
		}
	}
	var items []Imported
	var errs []ImportError
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		it, err := parseLink(line)
		if err != nil {
			errs = append(errs, ImportError{Line: i + 1, Input: clip(line), Message: err.Error()})
			continue
		}
		items = append(items, it)
	}
	return items, errs
}

func clip(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

func parseJSON(text string) ([]Imported, []ImportError) {
	var root any
	if err := json.Unmarshal([]byte(text), &root); err != nil {
		return nil, []ImportError{{Message: "不是有效的 JSON:" + err.Error()}}
	}
	var objs []any
	full := false
	switch v := root.(type) {
	case map[string]any:
		if outs, ok := v["outbounds"].([]any); ok {
			objs, full = outs, true
		} else {
			objs = []any{v}
		}
	case []any:
		objs = v
	default:
		return nil, []ImportError{{Message: "JSON 应为 outbound 对象、数组或包含 outbounds 的完整配置"}}
	}
	var items []Imported
	var errs []ImportError
	for i, o := range objs {
		m := asMap(o)
		if m == nil {
			errs = append(errs, ImportError{Line: i + 1, Message: "不是对象"})
			continue
		}
		if p, _ := m["protocol"].(string); full && skipOnFullConfig[strings.ToLower(p)] {
			continue
		}
		it, err := fromObject(m)
		if err != nil {
			errs = append(errs, ImportError{Line: i + 1, Input: clip(fmt.Sprint(m["tag"])), Message: err.Error()})
			continue
		}
		items = append(items, it)
	}
	return items, errs
}

func decodeBase64(s string) (string, bool) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b), true
		}
	}
	return "", false
}

func parseLink(line string) (Imported, error) {
	scheme, _, ok := strings.Cut(line, "://")
	if !ok {
		return Imported{}, errors.New("不是分享链接(缺少 ://)")
	}
	switch strings.ToLower(scheme) {
	case "vless":
		return parseVLESS(line)
	case "vmess":
		return parseVMess(line)
	case "trojan":
		return parseTrojan(line)
	case "ss":
		return parseShadowsocks(line)
	case "socks", "socks5", "socks5h":
		return parseSocksHTTP(line, "socks")
	case "http", "https":
		return parseSocksHTTP(line, "http")
	}
	return Imported{}, fmt.Errorf("不支持的链接类型 %q://", scheme)
}

// ---- shared pieces ----

type hostPort struct {
	host string
	port int
}

func endpointFromURL(u *url.URL) (hostPort, error) {
	h := u.Hostname()
	if h == "" {
		return hostPort{}, errors.New("缺少服务器地址")
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil || p < 1 || p > 65535 {
		return hostPort{}, fmt.Errorf("端口无效:%q", u.Port())
	}
	return hostPort{h, p}, nil
}

type streamParams struct {
	network, security, sni, fp, pbk, sid, spx string
	alpn                                      []string
	insecure                                  bool
	host, path, serviceName, mode, headerType string
	seed, authority                           string
}

func paramsFromQuery(q url.Values, defaultSecurity string) streamParams {
	p := streamParams{
		network: q.Get("type"), security: q.Get("security"), sni: first(q.Get("sni"), q.Get("peer")),
		fp: q.Get("fp"), pbk: q.Get("pbk"), sid: q.Get("sid"), spx: q.Get("spx"),
		host: q.Get("host"), path: q.Get("path"), serviceName: q.Get("serviceName"), mode: q.Get("mode"),
		headerType: q.Get("headerType"), seed: q.Get("seed"), authority: q.Get("authority"),
		insecure: q.Get("allowInsecure") == "1" || strings.EqualFold(q.Get("allowInsecure"), "true") || q.Get("insecure") == "1",
	}
	if a := q.Get("alpn"); a != "" {
		for _, x := range strings.Split(a, ",") {
			if x = strings.TrimSpace(x); x != "" {
				p.alpn = append(p.alpn, x)
			}
		}
	}
	if p.security == "" {
		p.security = defaultSecurity
	}
	return p
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// stream builds Xray streamSettings. It returns nil for plain TCP without TLS (Xray's default).
func (p streamParams) stream() (map[string]any, error) {
	network := strings.ToLower(p.network)
	switch network {
	case "", "raw":
		network = "tcp"
	case "splithttp":
		network = "xhttp"
	case "mkcp":
		network = "kcp"
	case "h2", "http", "quic":
		return nil, fmt.Errorf("Xray 新版已不再支持 %s 传输,请让服务端改用 xhttp、grpc 或 ws", network)
	case "tcp", "ws", "grpc", "httpupgrade", "xhttp", "kcp":
	default:
		return nil, fmt.Errorf("不支持的传输方式 %q", p.network)
	}
	ss := map[string]any{"network": network}

	switch network {
	case "ws":
		ws := map[string]any{"path": first(p.path, "/")}
		if p.host != "" {
			ws["headers"] = map[string]any{"Host": p.host}
		}
		ss["wsSettings"] = ws
	case "grpc":
		g := map[string]any{"serviceName": p.serviceName, "multiMode": p.mode == "multi"}
		if p.authority != "" {
			g["authority"] = p.authority
		}
		ss["grpcSettings"] = g
	case "httpupgrade":
		h := map[string]any{"path": first(p.path, "/")}
		if p.host != "" {
			h["host"] = p.host
		}
		ss["httpupgradeSettings"] = h
	case "xhttp":
		x := map[string]any{"path": first(p.path, "/")}
		if p.host != "" {
			x["host"] = p.host
		}
		if p.mode != "" {
			x["mode"] = p.mode
		}
		ss["xhttpSettings"] = x
	case "kcp":
		k := map[string]any{"header": map[string]any{"type": first(p.headerType, "none")}}
		if p.seed != "" {
			k["seed"] = p.seed
		}
		ss["kcpSettings"] = k
	case "tcp":
		if p.headerType == "http" {
			req := map[string]any{"path": []string{first(p.path, "/")}}
			if p.host != "" {
				req["headers"] = map[string]any{"Host": []string{p.host}}
			}
			ss["tcpSettings"] = map[string]any{"header": map[string]any{"type": "http", "request": req}}
		}
	}

	switch strings.ToLower(p.security) {
	case "", "none":
		ss["security"] = "none"
	case "tls":
		ss["security"] = "tls"
		tls := map[string]any{}
		if s := first(p.sni, p.host); s != "" {
			tls["serverName"] = s
		}
		if p.fp != "" {
			tls["fingerprint"] = p.fp
		}
		if len(p.alpn) > 0 {
			tls["alpn"] = p.alpn
		}
		if p.insecure {
			tls["allowInsecure"] = true
		}
		ss["tlsSettings"] = tls
	case "reality":
		ss["security"] = "reality"
		ss["realitySettings"] = map[string]any{
			"serverName": p.sni, "fingerprint": first(p.fp, "chrome"), "publicKey": p.pbk, "shortId": p.sid, "spiderX": p.spx,
		}
	default:
		return nil, fmt.Errorf("不支持的 security %q", p.security)
	}
	if network == "tcp" && ss["security"] == "none" && ss["tcpSettings"] == nil {
		return nil, nil
	}
	return ss, nil
}

func assemble(proto string, settings, stream map[string]any, name string, hp hostPort) (Imported, error) {
	cfg := map[string]any{"protocol": proto, "settings": settings}
	if stream != nil {
		cfg["streamSettings"] = stream
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return Imported{}, err
	}
	if name = strings.TrimSpace(name); name == "" {
		name = defaultName(proto, hp.host, hp.port)
	}
	return Imported{Name: name, Protocol: proto, Address: hp.host, Port: hp.port, Config: raw}, nil
}

// ---- protocols ----

func parseVLESS(line string) (Imported, error) {
	u, err := url.Parse(line)
	if err != nil {
		return Imported{}, fmt.Errorf("链接格式错误:%v", err)
	}
	id := u.User.Username()
	if id == "" {
		return Imported{}, errors.New("缺少 UUID")
	}
	hp, err := endpointFromURL(u)
	if err != nil {
		return Imported{}, err
	}
	q := u.Query()
	settings := map[string]any{"address": hp.host, "port": hp.port, "id": id, "encryption": first(q.Get("encryption"), "none")}
	if f := q.Get("flow"); f != "" {
		settings["flow"] = f
	}
	stream, err := paramsFromQuery(q, "none").stream()
	if err != nil {
		return Imported{}, err
	}
	return assemble("vless", settings, stream, u.Fragment, hp)
}

func parseTrojan(line string) (Imported, error) {
	u, err := url.Parse(line)
	if err != nil {
		return Imported{}, fmt.Errorf("链接格式错误:%v", err)
	}
	pw := u.User.Username()
	if p, ok := u.User.Password(); ok {
		pw += ":" + p
	}
	if pw == "" {
		return Imported{}, errors.New("缺少密码")
	}
	hp, err := endpointFromURL(u)
	if err != nil {
		return Imported{}, err
	}
	settings := map[string]any{"servers": []any{map[string]any{"address": hp.host, "port": hp.port, "password": pw}}}
	stream, err := paramsFromQuery(u.Query(), "tls").stream()
	if err != nil {
		return Imported{}, err
	}
	return assemble("trojan", settings, stream, u.Fragment, hp)
}

func parseVMess(line string) (Imported, error) {
	rest := strings.TrimPrefix(line, line[:strings.Index(line, "://")+3])
	rest, name, _ := strings.Cut(rest, "#")
	dec, ok := decodeBase64(rest)
	if !ok {
		return Imported{}, errors.New("vmess:// 后不是有效的 base64")
	}
	var j map[string]any
	if err := json.Unmarshal([]byte(dec), &j); err != nil {
		return Imported{}, fmt.Errorf("vmess 内容不是有效的 JSON:%v", err)
	}
	str := func(k string) string {
		switch v := j[k].(type) {
		case string:
			return v
		case float64:
			return strconv.Itoa(int(v))
		}
		return ""
	}
	hp := hostPort{host: strings.TrimSpace(str("add")), port: numOf(j["port"])}
	if hp.host == "" {
		return Imported{}, errors.New("缺少服务器地址 add")
	}
	if hp.port < 1 || hp.port > 65535 {
		return Imported{}, fmt.Errorf("端口无效:%q", str("port"))
	}
	if str("id") == "" {
		return Imported{}, errors.New("缺少 UUID id")
	}
	settings := map[string]any{"address": hp.host, "port": hp.port, "id": str("id"), "security": first(str("scy"), "auto")}
	p := streamParams{network: str("net"), sni: str("sni"), fp: str("fp"), host: str("host"), path: str("path"), headerType: str("type")}
	if a := str("alpn"); a != "" {
		p.alpn = strings.Split(a, ",")
	}
	if str("tls") == "tls" {
		p.security = "tls"
	}
	if p.network == "grpc" {
		p.serviceName, p.path = p.path, ""
		if p.headerType == "multi" {
			p.mode = "multi"
		}
	}
	stream, err := p.stream()
	if err != nil {
		return Imported{}, err
	}
	return assemble("vmess", settings, stream, first(name, str("ps")), hp)
}

func parseShadowsocks(line string) (Imported, error) {
	rest := strings.TrimPrefix(line, "ss://")
	rest, frag, _ := strings.Cut(rest, "#")
	name, _ := url.PathUnescape(frag)
	main, query, _ := strings.Cut(rest, "?")
	if strings.Contains(query, "plugin=") {
		return Imported{}, errors.New("暂不支持带 plugin 的 Shadowsocks 链接(plugin 需要额外的插件进程)")
	}
	var method, password, host, portStr string
	if strings.Contains(main, "@") { // SIP002: userinfo@host:port
		u, err := url.Parse("ss://" + strings.TrimSuffix(main, "/"))
		if err != nil {
			return Imported{}, fmt.Errorf("链接格式错误:%v", err)
		}
		host, portStr = u.Hostname(), u.Port()
		if pw, ok := u.User.Password(); ok {
			method, password = u.User.Username(), pw
		} else if dec, ok := decodeBase64(u.User.Username()); ok {
			method, password, _ = strings.Cut(dec, ":")
		}
	} else { // legacy: base64(method:password@host:port)
		dec, ok := decodeBase64(strings.TrimSuffix(main, "/"))
		if !ok {
			return Imported{}, errors.New("无法解码 ss:// 链接")
		}
		at := strings.LastIndex(dec, "@")
		if at < 0 {
			return Imported{}, errors.New("ss:// 内容缺少 @")
		}
		method, password, _ = strings.Cut(dec[:at], ":")
		u, err := url.Parse("ss://" + dec[at+1:])
		if err != nil {
			return Imported{}, fmt.Errorf("链接格式错误:%v", err)
		}
		host, portStr = u.Hostname(), u.Port()
	}
	if method == "" || password == "" {
		return Imported{}, errors.New("缺少加密方式或密码")
	}
	port, err := strconv.Atoi(portStr)
	if host == "" || err != nil || port < 1 || port > 65535 {
		return Imported{}, errors.New("服务器地址或端口无效")
	}
	hp := hostPort{host, port}
	settings := map[string]any{"servers": []any{map[string]any{"address": host, "port": port, "method": method, "password": password}}}
	return assemble("shadowsocks", settings, nil, name, hp)
}

func parseSocksHTTP(line, proto string) (Imported, error) {
	u, err := url.Parse(line)
	if err != nil {
		return Imported{}, fmt.Errorf("链接格式错误:%v", err)
	}
	hp, err := endpointFromURL(u)
	if err != nil {
		return Imported{}, err
	}
	server := map[string]any{"address": hp.host, "port": hp.port}
	if u.User != nil {
		user, pass := u.User.Username(), ""
		if p, ok := u.User.Password(); ok {
			pass = p
		} else if dec, ok := decodeBase64(user); ok && strings.Contains(dec, ":") { // socks://base64(user:pass)@...
			user, pass, _ = strings.Cut(dec, ":")
		}
		if user != "" {
			server["users"] = []any{map[string]any{"user": user, "pass": pass}}
		}
	}
	var stream map[string]any
	if strings.EqualFold(u.Scheme, "https") {
		stream = map[string]any{"network": "tcp", "security": "tls", "tlsSettings": map[string]any{"serverName": hp.host}}
	}
	return assemble(proto, map[string]any{"servers": []any{server}}, stream, u.Fragment, hp)
}
