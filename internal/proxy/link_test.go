package proxy

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func mustOne(t *testing.T, link string) Imported {
	t.Helper()
	items, errs := Parse(link)
	if len(errs) != 0 || len(items) != 1 {
		t.Fatalf("Parse(%q): items=%d errs=%v", link, len(items), errs)
	}
	return items[0]
}

func cfgMap(t *testing.T, it Imported) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(it.Config, &m); err != nil {
		t.Fatalf("config is not JSON: %v\n%s", err, it.Config)
	}
	return m
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func TestVLESSWebsocketTLS(t *testing.T) {
	it := mustOne(t, "vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?encryption=none&security=tls&sni=cdn.example.com&fp=chrome&alpn=h2,http/1.1&type=ws&host=ws.example.com&path=%2Fvl#My%20Node")
	if it.Name != "My Node" || it.Protocol != "vless" || it.Address != "example.com" || it.Port != 443 {
		t.Fatalf("meta = %+v", it)
	}
	m := cfgMap(t, it)
	if dig(m, "protocol") != "vless" || dig(m, "settings", "id") != "b831381d-6324-4d53-ad4f-8cda48b30811" ||
		dig(m, "settings", "address") != "example.com" || dig(m, "settings", "encryption") != "none" {
		t.Fatalf("settings: %s", it.Config)
	}
	if dig(m, "streamSettings", "network") != "ws" || dig(m, "streamSettings", "security") != "tls" ||
		dig(m, "streamSettings", "wsSettings", "path") != "/vl" ||
		dig(m, "streamSettings", "tlsSettings", "serverName") != "cdn.example.com" ||
		dig(m, "streamSettings", "tlsSettings", "fingerprint") != "chrome" {
		t.Fatalf("stream: %s", it.Config)
	}
	alpn, _ := dig(m, "streamSettings", "tlsSettings", "alpn").([]any)
	if len(alpn) != 2 || alpn[0] != "h2" {
		t.Fatalf("alpn = %v", alpn)
	}
	if h := dig(m, "streamSettings", "wsSettings", "headers", "Host"); h != "ws.example.com" {
		t.Fatalf("ws host header = %v", h)
	}
}

func TestVLESSRealityVision(t *testing.T) {
	it := mustOne(t, "vless://11111111-2222-3333-4444-555555555555@1.2.3.4:8443?type=tcp&security=reality&sni=www.microsoft.com&fp=chrome&pbk=PUBKEY123&sid=ab12&spx=%2F&flow=xtls-rprx-vision&encryption=none#reality")
	m := cfgMap(t, it)
	if dig(m, "settings", "flow") != "xtls-rprx-vision" {
		t.Fatalf("flow: %s", it.Config)
	}
	if dig(m, "streamSettings", "security") != "reality" ||
		dig(m, "streamSettings", "realitySettings", "publicKey") != "PUBKEY123" ||
		dig(m, "streamSettings", "realitySettings", "shortId") != "ab12" ||
		dig(m, "streamSettings", "realitySettings", "serverName") != "www.microsoft.com" ||
		dig(m, "streamSettings", "realitySettings", "spiderX") != "/" {
		t.Fatalf("reality: %s", it.Config)
	}
}

func TestVLESSGrpcAndXhttpAndHttpupgrade(t *testing.T) {
	g := cfgMap(t, mustOne(t, "vless://u@h.example:443?type=grpc&serviceName=svc&mode=multi&security=tls&sni=h.example#g"))
	if dig(g, "streamSettings", "network") != "grpc" || dig(g, "streamSettings", "grpcSettings", "serviceName") != "svc" ||
		dig(g, "streamSettings", "grpcSettings", "multiMode") != true {
		t.Fatalf("grpc: %v", g)
	}
	x := cfgMap(t, mustOne(t, "vless://u@h.example:443?type=xhttp&path=%2Fx&host=cdn.example&mode=auto&security=tls#x"))
	if dig(x, "streamSettings", "network") != "xhttp" || dig(x, "streamSettings", "xhttpSettings", "path") != "/x" ||
		dig(x, "streamSettings", "xhttpSettings", "host") != "cdn.example" || dig(x, "streamSettings", "xhttpSettings", "mode") != "auto" {
		t.Fatalf("xhttp: %v", x)
	}
	h := cfgMap(t, mustOne(t, "vless://u@h.example:80?type=httpupgrade&path=%2Fu&host=up.example#h"))
	if dig(h, "streamSettings", "network") != "httpupgrade" || dig(h, "streamSettings", "httpupgradeSettings", "path") != "/u" {
		t.Fatalf("httpupgrade: %v", h)
	}
}

func TestVLESSTcpHttpHeader(t *testing.T) {
	m := cfgMap(t, mustOne(t, "vless://u@h.example:80?type=tcp&headerType=http&host=a.example&path=%2Fp#t"))
	if dig(m, "streamSettings", "tcpSettings", "header", "type") != "http" {
		t.Fatalf("tcp http header: %v", m)
	}
}

func TestVMessBase64JSON(t *testing.T) {
	j := `{"v":"2","ps":"vm node","add":"vm.example.com","port":"10086","id":"b831381d-6324-4d53-ad4f-8cda48b30811","aid":"0","scy":"auto","net":"ws","type":"none","host":"vm.example.com","path":"/ray","tls":"tls","sni":"vm.example.com","fp":"firefox"}`
	it := mustOne(t, "vmess://"+base64.StdEncoding.EncodeToString([]byte(j)))
	if it.Name != "vm node" || it.Protocol != "vmess" || it.Address != "vm.example.com" || it.Port != 10086 {
		t.Fatalf("meta = %+v", it)
	}
	m := cfgMap(t, it)
	if dig(m, "settings", "id") != "b831381d-6324-4d53-ad4f-8cda48b30811" || dig(m, "settings", "security") != "auto" ||
		dig(m, "streamSettings", "network") != "ws" || dig(m, "streamSettings", "security") != "tls" ||
		dig(m, "streamSettings", "wsSettings", "path") != "/ray" {
		t.Fatalf("vmess config: %s", it.Config)
	}
}

func TestVMessPortMayBeANumberAndBase64MayBeUnpadded(t *testing.T) {
	j := `{"v":2,"ps":"n","add":"a.example","port":8080,"id":"b831381d-6324-4d53-ad4f-8cda48b30811","net":"tcp"}`
	enc := strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(j)), "=")
	it := mustOne(t, "vmess://"+enc)
	if it.Port != 8080 || it.Address != "a.example" {
		t.Fatalf("meta = %+v", it)
	}
}

func TestTrojan(t *testing.T) {
	it := mustOne(t, "trojan://p%40ss@tj.example.com:443?security=tls&sni=tj.example.com&type=tcp&fp=chrome#trojan-1")
	m := cfgMap(t, it)
	if it.Protocol != "trojan" || it.Name != "trojan-1" {
		t.Fatalf("meta = %+v", it)
	}
	servers, _ := dig(m, "settings", "servers").([]any)
	if len(servers) != 1 {
		t.Fatalf("trojan servers: %s", it.Config)
	}
	s := servers[0].(map[string]any)
	if s["password"] != "p@ss" || s["address"] != "tj.example.com" || s["port"] != float64(443) {
		t.Fatalf("trojan server: %v", s)
	}
	if dig(m, "streamSettings", "security") != "tls" {
		t.Fatalf("trojan stream: %s", it.Config)
	}
}

func TestTrojanDefaultsToTLS(t *testing.T) {
	m := cfgMap(t, mustOne(t, "trojan://pw@tj.example.com:443#t"))
	if dig(m, "streamSettings", "security") != "tls" {
		t.Fatalf("trojan without security param must default to tls: %v", m)
	}
}

func TestShadowsocksSIP002AndLegacy(t *testing.T) {
	userinfo := base64.RawURLEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:secret"))
	it := mustOne(t, "ss://"+userinfo+"@ss.example.com:8388#ss-node")
	m := cfgMap(t, it)
	s := dig(m, "settings", "servers").([]any)[0].(map[string]any)
	if it.Protocol != "shadowsocks" || s["method"] != "chacha20-ietf-poly1305" || s["password"] != "secret" ||
		s["address"] != "ss.example.com" || s["port"] != float64(8388) || it.Name != "ss-node" {
		t.Fatalf("sip002: meta=%+v server=%v", it, s)
	}
	legacy := base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:pw123@10.0.0.5:9000"))
	it2 := mustOne(t, "ss://"+legacy+"#old")
	s2 := dig(cfgMap(t, it2), "settings", "servers").([]any)[0].(map[string]any)
	if s2["method"] != "aes-256-gcm" || s2["password"] != "pw123" || s2["address"] != "10.0.0.5" || s2["port"] != float64(9000) {
		t.Fatalf("legacy ss: %v", s2)
	}
}

func TestShadowsocksPluginIsRejectedClearly(t *testing.T) {
	userinfo := base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:pw"))
	_, errs := Parse("ss://" + userinfo + "@h.example:1/?plugin=v2ray-plugin%3Btls#p")
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "plugin") {
		t.Fatalf("errs = %+v, want a message naming the unsupported plugin", errs)
	}
}

func TestSocksAndHTTPProxies(t *testing.T) {
	s := mustOne(t, "socks5://user:pw@10.1.1.1:1080#sk")
	m := cfgMap(t, s)
	srv := dig(m, "settings", "servers").([]any)[0].(map[string]any)
	if s.Protocol != "socks" || srv["address"] != "10.1.1.1" || srv["port"] != float64(1080) {
		t.Fatalf("socks: %+v %v", s, srv)
	}
	users := srv["users"].([]any)[0].(map[string]any)
	if users["user"] != "user" || users["pass"] != "pw" {
		t.Fatalf("socks users: %v", users)
	}
	h := mustOne(t, "http://proxy.example:3128#h")
	if h.Protocol != "http" || h.Port != 3128 {
		t.Fatalf("http: %+v", h)
	}
	anon := dig(cfgMap(t, h), "settings", "servers").([]any)[0].(map[string]any)
	if _, has := anon["users"]; has {
		t.Fatalf("anonymous proxy must not carry a users list: %v", anon)
	}
}

func TestMultipleLinesAndPerLineErrors(t *testing.T) {
	good1 := "vless://u@a.example:443?security=tls#one"
	good2 := "trojan://p@b.example:443#two"
	items, errs := Parse(good1 + "\n\n# a comment line\n" + "not a link\n" + good2 + "\r\nvless://@:0#broken")
	if len(items) != 2 {
		t.Fatalf("items = %d, want the 2 valid links", len(items))
	}
	if len(errs) != 2 {
		t.Fatalf("errs = %+v, want one per bad line", errs)
	}
	if errs[0].Line != 4 || errs[1].Line != 6 {
		t.Fatalf("error line numbers = %d, %d; want 4 and 6", errs[0].Line, errs[1].Line)
	}
}

func TestSubscriptionBlobIsDecoded(t *testing.T) {
	blob := "vless://u@a.example:443?security=tls#one\ntrojan://p@b.example:443#two\n"
	enc := base64.StdEncoding.EncodeToString([]byte(blob))
	items, errs := Parse(enc)
	if len(errs) != 0 || len(items) != 2 {
		t.Fatalf("subscription: items=%d errs=%v", len(items), errs)
	}
}

func TestJSONImportShapes(t *testing.T) {
	one := `{"tag":"hk","protocol":"vless","settings":{"address":"h.example","port":443,"id":"u","encryption":"none"}}`
	items, errs := Parse(one)
	if len(errs) != 0 || len(items) != 1 || items[0].Name != "hk" || items[0].Address != "h.example" || items[0].Port != 443 {
		t.Fatalf("single outbound: %+v %v", items, errs)
	}
	arr := `[` + one + `,{"protocol":"trojan","settings":{"servers":[{"address":"t.example","port":8443,"password":"p"}]}}]`
	items, errs = Parse(arr)
	if len(errs) != 0 || len(items) != 2 || items[1].Address != "t.example" || items[1].Port != 8443 {
		t.Fatalf("array: %+v %v", items, errs)
	}
	full := `{"log":{},"inbounds":[],"outbounds":[` + one + `,{"protocol":"freedom","tag":"direct"},{"protocol":"blackhole","tag":"block"}]}`
	items, errs = Parse(full)
	if len(errs) != 0 || len(items) != 1 {
		t.Fatalf("full config must import only the usable outbounds: %+v %v", items, errs)
	}
	legacy := `{"protocol":"vmess","settings":{"vnext":[{"address":"v.example","port":10000,"users":[{"id":"u","security":"auto"}]}]}}`
	items, errs = Parse(legacy)
	if len(errs) != 0 || len(items) != 1 || items[0].Address != "v.example" || items[0].Port != 10000 {
		t.Fatalf("vnext form: %+v %v", items, errs)
	}
}

func TestRejectsBadInput(t *testing.T) {
	bad := []string{
		"vless://uuid@:443",        // no host
		"vless://uuid@host:0",      // port 0
		"vless://uuid@host:99999",  // port out of range
		"vless://@host:443",        // no uuid
		"vmess://%%%not-base64%%%", // garbage
		"trojan://@host:443",       // no password
		"ss://@host:443",           // no method/password
		"unknown://x@y:1",          // unsupported scheme
		`{"protocol":"vless"}`,     // JSON without endpoint
		`{"protocol":"teleport","settings":{"address":"a","port":1}}`,
		`{not json`,
	}
	for _, in := range bad {
		items, errs := Parse(in)
		if len(items) != 0 || len(errs) == 0 {
			t.Errorf("Parse(%q) = %d items, %d errs; want an error and no items", in, len(items), len(errs))
		}
	}
}

func TestDefaultNameFallsBackToEndpoint(t *testing.T) {
	it := mustOne(t, "trojan://p@b.example:443")
	if it.Name != "trojan b.example:443" {
		t.Fatalf("name = %q", it.Name)
	}
}

func TestImportedConfigNeverContainsATag(t *testing.T) {
	// tags are assigned when the runtime config is built; a stored tag would clash
	it := mustOne(t, "vless://u@a.example:443#x")
	if _, has := cfgMap(t, it)["tag"]; has {
		t.Fatalf("stored config must not carry a tag: %s", it.Config)
	}
	items, _ := Parse(`{"tag":"mine","protocol":"vless","settings":{"address":"a","port":1,"id":"u"}}`)
	if _, has := cfgMap(t, items[0])["tag"]; has {
		t.Fatalf("JSON import must strip the tag: %s", items[0].Config)
	}
}
