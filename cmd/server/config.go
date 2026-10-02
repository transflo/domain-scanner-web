package main

import (
	"errors"
	"fmt"
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
		AdminPassword:  getenv("ADMIN_PASSWORD"),
		TelegramToken:  strings.TrimSpace(getenv("TELEGRAM_BOT_TOKEN")),
		TelegramChatID: strings.TrimSpace(getenv("TELEGRAM_CHAT_ID")),
	}
	if len(c.AdminPassword) < minPasswordLen {
		return nil, errors.New("ADMIN_PASSWORD 未设置或少于 8 位:口令保护是强制的,请在 .env 中设置至少 8 位的 ADMIN_PASSWORD")
	}

	c.MaxParallelJobs = 2
	if v := strings.TrimSpace(getenv("MAX_PARALLEL_JOBS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return nil, fmt.Errorf("MAX_PARALLEL_JOBS 必须是 1-100 的整数,当前为 %q", v)
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
	return c, nil
}
