package main

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type Config struct {
	ListenAddr      string
	DataDir         string
	WordlistDir     string
	AdminPassword   string
	TelegramToken   string
	TelegramChatID  string
	MaxParallelJobs int
	TrustProxy      bool
	RDAPServers     map[string]string // extra TLD -> RDAP base URL (RDAP_SERVERS)
	XrayBin         string            // path of the xray-core binary (XRAY_BIN)
	LogStdoutLevel  string            // lowest level printed to stdout (LOG_STDOUT_LEVEL); the DB keeps more
	CFAccountID     string            // CLOUDFLARE_ACCOUNT_ID (the settings page overrides it)
	CFToken         string            // CLOUDFLARE_API_TOKEN
}

const minPasswordLen = 8

// LoadConfig reads configuration from the environment (via getenv). ADMIN_PASSWORD is mandatory:
// the service refuses to start without it, so the UI can never be exposed unprotected.
func LoadConfig(getenv func(string) string) (*Config, error) {
	or := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	c := &Config{
		ListenAddr:     or("LISTEN_ADDR", ":8080"),
		DataDir:        or("DATA_DIR", "/data"),
		WordlistDir:    or("WORDLIST_DIR", "/app/wordlists"),
		XrayBin:        or("XRAY_BIN", "/usr/local/bin/xray"),
		LogStdoutLevel: strings.ToLower(or("LOG_STDOUT_LEVEL", "info")),
		CFAccountID:    strings.TrimSpace(getenv("CLOUDFLARE_ACCOUNT_ID")),
		CFToken:        strings.TrimSpace(getenv("CLOUDFLARE_API_TOKEN")),
		AdminPassword:  getenv("ADMIN_PASSWORD"),
		TelegramToken:  strings.TrimSpace(getenv("TELEGRAM_BOT_TOKEN")),
		TelegramChatID: strings.TrimSpace(getenv("TELEGRAM_CHAT_ID")),
	}
	if len(c.AdminPassword) < minPasswordLen {
		return nil, errors.New("ADMIN_PASSWORD 未设置或少于 8 位:口令保护是强制的,请在 .env 中设置至少 8 位的 ADMIN_PASSWORD")
	}

	// 0 = unlimited. The cap only applies to jobs routed through proxies; direct jobs never wait.
	switch c.LogStdoutLevel {
	case "debug", "info", "warn", "error":
	default:
		return nil, fmt.Errorf("LOG_STDOUT_LEVEL 必须是 debug、info、warn 或 error,当前为 %q", c.LogStdoutLevel)
	}
	c.MaxParallelJobs = 0
	if v := strings.TrimSpace(getenv("MAX_PARALLEL_JOBS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100 {
			return nil, fmt.Errorf("MAX_PARALLEL_JOBS 必须是 0-100 的整数(0 = 不限制),当前为 %q", v)
		}
		c.MaxParallelJobs = n
	}
	if v := strings.TrimSpace(getenv("TRUST_PROXY")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("TRUST_PROXY 必须是 true 或 false,当前为 %q", v)
		}
		c.TrustProxy = b
	}
	servers, err := parseRDAPServers(getenv("RDAP_SERVERS"))
	if err != nil {
		return nil, err
	}
	c.RDAPServers = servers
	return c, nil
}

// parseRDAPServers parses "tld=https://rdap.example/,tld2=https://..." into a map with
// lower-cased TLD keys (leading dot allowed) and base URLs normalised to end in "/".
func parseRDAPServers(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		tld, base, ok := strings.Cut(item, "=")
		tld = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(tld), "."))
		base = strings.TrimSpace(base)
		u, err := url.Parse(base)
		if !ok || tld == "" || base == "" || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, fmt.Errorf("RDAP_SERVERS 格式错误:%q(应为 tld=https://rdap.example.com/)", item)
		}
		if !strings.HasSuffix(base, "/") {
			base += "/"
		}
		out[tld] = base
	}
	return out, nil
}
