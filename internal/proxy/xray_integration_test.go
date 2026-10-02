package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run the real xray binary (`xray run -test`) against configs produced by the parser,
// so a change in what xray accepts is caught here instead of in production. They are skipped
// unless XRAY_BIN points at an xray executable.
func realXray(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("XRAY_BIN")
	if bin == "" {
		t.Skip("XRAY_BIN not set; skipping real-xray tests")
	}
	return bin
}

func xrayAccepts(t *testing.T, bin string, cfg json.RawMessage) (string, error) {
	t.Helper()
	full, err := BuildSingle(cfg, 21999)
	if err != nil {
		t.Fatalf("BuildSingle: %v", err)
	}
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, full, 0o600); err != nil {
		t.Fatal(err)
	}
	return execTest(context.Background(), bin, p)
}

func vmessLink() string {
	j := `{"v":"2","ps":"vm","add":"vm.example.com","port":"443","id":"b831381d-6324-4d53-ad4f-8cda48b30811","aid":"0","scy":"auto","net":"ws","host":"h.example.com","path":"/p","tls":"tls","sni":"vm.example.com"}`
	return "vmess://" + base64.StdEncoding.EncodeToString([]byte(j))
}

func TestRealXrayAcceptsEveryLinkShapeTheParserProduces(t *testing.T) {
	bin := realXray(t)
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	const pbk = "yR6l2b1pCpV7kqkH9GmWl3aJmJz2m6Zk0r6Qp4o0Y3o"
	links := map[string]string{
		"vless-ws-tls":      "vless://" + uuid + "@example.com:443?encryption=none&security=tls&sni=cdn.example.com&fp=chrome&alpn=h2,http/1.1&type=ws&host=ws.example.com&path=%2Fvl#a",
		"vless-tcp-reality": "vless://" + uuid + "@example.com:443?encryption=none&security=reality&sni=www.microsoft.com&fp=chrome&pbk=" + pbk + "&sid=6ba85179e30d4fc2&flow=xtls-rprx-vision&type=tcp#b",
		"vless-grpc-tls":    "vless://" + uuid + "@example.com:443?encryption=none&security=tls&sni=g.example.com&type=grpc&serviceName=svc&mode=gun#c",
		"vless-xhttp-tls":   "vless://" + uuid + "@example.com:443?encryption=none&security=tls&sni=x.example.com&type=xhttp&path=%2Fx&mode=auto#d",
		"vless-plain-tcp":   "vless://" + uuid + "@example.com:8080?encryption=none&type=tcp#e",
		"trojan-tls":        "trojan://secret@example.com:443?security=tls&sni=t.example.com&type=tcp#f",
		"trojan-ws":         "trojan://secret@example.com:443?security=tls&sni=t.example.com&type=ws&path=%2Ft#g",
		"vmess-ws-tls":      vmessLink(),
		"ss":                "ss://" + base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:pass")) + "@example.com:8388#h",
		"ss-2022":           "ss://" + base64.StdEncoding.EncodeToString([]byte("2022-blake3-aes-128-gcm:"+base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")))) + "@example.com:8388#i",
		"socks":             "socks5://user:pw@example.com:1080#j",
		"socks-noauth":      "socks5://example.com:1080#k",
		"http":              "http://user:pw@example.com:3128#l",
	}
	for name, link := range links {
		t.Run(name, func(t *testing.T) {
			items, errs := Parse(link)
			if len(errs) != 0 || len(items) != 1 {
				t.Fatalf("parse: items=%d errs=%v", len(items), errs)
			}
			if out, err := xrayAccepts(t, bin, items[0].Config); err != nil {
				t.Fatalf("xray rejected the generated config: %s\nsummary: %s\nconfig: %s", out, summarize(out), items[0].Config)
			}
		})
	}
}

func TestRealXrayRejectionsAreSummarisedToTheirCause(t *testing.T) {
	bin := realXray(t)
	cases := []struct{ name, cfg, want string }{
		{"bogus-network", `{"protocol":"vless","settings":{"address":"a.example","port":443,"id":"b831381d-6324-4d53-ad4f-8cda48b30811","encryption":"none"},"streamSettings":{"network":"bogus"}}`, "bogus"},
		{"bad-fingerprint", `{"protocol":"vless","settings":{"address":"a.example","port":443,"id":"b831381d-6324-4d53-ad4f-8cda48b30811","encryption":"none"},"streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"fingerprint":"nonsense-fp"}}}`, "fingerprint"},
		{"reality-without-key", `{"protocol":"vless","settings":{"address":"a.example","port":443,"id":"b831381d-6324-4d53-ad4f-8cda48b30811","encryption":"none"},"streamSettings":{"network":"tcp","security":"reality","realitySettings":{"serverName":"x.example"}}}`, "REALITY"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := xrayAccepts(t, bin, json.RawMessage(c.cfg))
			if err == nil {
				t.Fatalf("xray accepted a bad config: %s", out)
			}
			s := summarize(out)
			if !strings.Contains(s, c.want) {
				t.Fatalf("summary %q lacks %q\nraw: %s", s, c.want, out)
			}
			for _, noise := range []string{"failed to load config files", "Reading config", "main:"} {
				if strings.Contains(s, noise) {
					t.Fatalf("summary %q still has noise %q", s, noise)
				}
			}
		})
	}
}
