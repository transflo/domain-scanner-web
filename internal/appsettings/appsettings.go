// Package appsettings reads the settings that can be changed at runtime from the UI, falling
// back to environment values or defaults. Everything is read on use, so a change takes effect
// without a restart.
package appsettings

import (
	"context"
	"strconv"
	"time"

	"domain_scanner/internal/store"
)

// Setting keys in the settings table.
const (
	KeyTelegramToken  = "telegram_token"
	KeyTelegramChatID = "telegram_chat_id"

	KeyCFAccountID = "cloudflare_account_id"
	KeyCFToken     = "cloudflare_token"

	KeyRegisterConfirm  = "register_confirm"
	KeyRegisterMaxPrice = "register_max_price"
	KeyRegisterDailyCap = "register_daily_cap"
	KeyPushUnconfirmed  = "push_unconfirmed"

	KeyLogLevel     = "log_level"
	KeyProxyTestURL = "proxy_test_url"
)

const (
	DefaultLogLevel     = "debug"
	DefaultProxyTestURL = "https://cp.cloudflare.com/generate_204"
	defaultMaxPrice     = 30.0
	defaultDailyCap     = 5
)

func get(st *store.Store, key string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	v, ok, err := st.GetSetting(ctx, key)
	if err != nil {
		return "", false
	}
	return v, ok
}

func getBool(st *store.Store, key string, def bool) bool {
	if v, ok := get(st, key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// Cloudflare holds the credentials used for the registrar API.
type Cloudflare struct {
	AccountID string
	Token     string
}

func (c Cloudflare) Configured() bool { return c.AccountID != "" && c.Token != "" }

// CloudflareFunc resolves the effective credentials: saved settings win over the environment.
func CloudflareFunc(st *store.Store, env Cloudflare) func() Cloudflare {
	return func() Cloudflare {
		c := env
		if v, ok := get(st, KeyCFAccountID); ok && v != "" {
			c.AccountID = v
		}
		if v, ok := get(st, KeyCFToken); ok && v != "" {
			c.Token = v
		}
		return c
	}
}

// RegisterPolicy is what protects the account from unwanted purchases and decides what is pushed.
type RegisterPolicy struct {
	Confirm         bool    // ask again before spending money
	MaxPrice        float64 // refuse to register above this first-year price (0 = no limit)
	DailyCap        int     // registrations per rolling day (0 = no limit)
	PushUnconfirmed bool    // also notify domains Cloudflare could not confirm (unsupported TLD, check failed)
}

func RegisterPolicyFunc(st *store.Store) func() RegisterPolicy {
	return func() RegisterPolicy {
		p := RegisterPolicy{Confirm: getBool(st, KeyRegisterConfirm, true), MaxPrice: defaultMaxPrice,
			DailyCap: defaultDailyCap, PushUnconfirmed: getBool(st, KeyPushUnconfirmed, true)}
		if v, ok := get(st, KeyRegisterMaxPrice); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
				p.MaxPrice = f
			}
		}
		if v, ok := get(st, KeyRegisterDailyCap); ok {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				p.DailyCap = n
			}
		}
		return p
	}
}

// LogLevel is the lowest level that is recorded.
func LogLevel(st *store.Store) string {
	if v, ok := get(st, KeyLogLevel); ok && v != "" {
		return v
	}
	return DefaultLogLevel
}

// ProxyTestURL is the address probed through each outbound proxy.
func ProxyTestURL(st *store.Store) string {
	if v, ok := get(st, KeyProxyTestURL); ok && v != "" {
		return v
	}
	return DefaultProxyTestURL
}
