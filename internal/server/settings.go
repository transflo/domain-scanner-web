package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"domain_scanner/internal/appsettings"
	"domain_scanner/internal/notifier"
	"domain_scanner/internal/proxy"
	"domain_scanner/internal/store"
	"domain_scanner/internal/wordlists"
)

type wordlistsInfo = wordlists.Info

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// Exported keys used by main.
const (
	KeyLogLevel     = appsettings.KeyLogLevel
	KeyProxyTestURL = appsettings.KeyProxyTestURL
)

var (
	tokenRe   = regexp.MustCompile(`^\d{5,}:[A-Za-z0-9_-]{20,}$`)
	chatIDRe  = regexp.MustCompile(`^(-?\d{1,20}|@[A-Za-z0-9_]{3,64})$`)
	cfAcctRe  = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
	cfTokenRe = regexp.MustCompile(`^[A-Za-z0-9_\-]{20,200}$`)
)

// TelegramConfigFunc resolves the effective Telegram destination on every call: values saved in
// the settings page win over the environment fallback.
func TelegramConfigFunc(st *store.Store, env notifier.Config) notifier.ConfigFunc {
	return func() notifier.Config {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c := env
		if v, ok, _ := st.GetSetting(ctx, appsettings.KeyTelegramToken); ok && v != "" {
			c.Token = v
		}
		if v, ok, _ := st.GetSetting(ctx, appsettings.KeyTelegramChatID); ok && v != "" {
			c.ChatID = v
		}
		return c
	}
}

// maskToken keeps the (non-secret) bot id and the last four characters of a Telegram token.
func maskToken(t string) string {
	if t == "" {
		return ""
	}
	id, secret, ok := strings.Cut(t, ":")
	if !ok || len(secret) < 4 {
		return "****"
	}
	return id + ":****" + secret[len(secret)-4:]
}

// maskSecret keeps only the last four characters.
func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "****"
	}
	return "****" + s[len(s)-4:]
}

// refreshSecrets tells the log bus which values must never be printed.
func (a *api) refreshSecrets() {
	tg := TelegramConfigFunc(a.Store, a.TelegramEnv)()
	cf := appsettings.CloudflareFunc(a.Store, a.CloudflareEnv)()
	a.Bus.SetSecrets(tg.Token, cf.Token)
}

func (a *api) getSettings(w http.ResponseWriter, r *http.Request) {
	tg := TelegramConfigFunc(a.Store, a.TelegramEnv)()
	source := ""
	if tg.Token != "" && tg.ChatID != "" {
		source = "env"
		if v, ok, _ := a.Store.GetSetting(r.Context(), appsettings.KeyTelegramToken); ok && v != "" {
			source = "settings"
		}
	}
	cf := appsettings.CloudflareFunc(a.Store, a.CloudflareEnv)()
	pol := appsettings.RegisterPolicyFunc(a.Store)()
	writeJSON(w, http.StatusOK, map[string]any{
		"telegram_token":      maskToken(tg.Token),
		"telegram_chat_id":    tg.ChatID,
		"telegram_configured": tg.Token != "" && tg.ChatID != "",
		"telegram_source":     source,

		"cloudflare_account_id": cf.AccountID,
		"cloudflare_token":      maskSecret(cf.Token),
		"cloudflare_configured": cf.Configured(),

		"register_confirm":   pol.Confirm,
		"register_max_price": strconv.FormatFloat(pol.MaxPrice, 'f', -1, 64),
		"register_daily_cap": pol.DailyCap,
		"push_unconfirmed":   pol.PushUnconfirmed,

		"log_level":      appsettings.LogLevel(a.Store),
		"proxy_test_url": appsettings.ProxyTestURL(a.Store),
	})
}

func (a *api) putSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TGToken      *string `json:"telegram_token"`
		TGChatID     *string `json:"telegram_chat_id"`
		CFAccountID  *string `json:"cloudflare_account_id"`
		CFToken      *string `json:"cloudflare_token"`
		Confirm      *bool   `json:"register_confirm"`
		MaxPrice     *string `json:"register_max_price"`
		DailyCap     *int    `json:"register_daily_cap"`
		PushUncfm    *bool   `json:"push_unconfirmed"`
		LogLevel     *string `json:"log_level"`
		ProxyTestURL *string `json:"proxy_test_url"`
	}
	if !decodeJSON(w, r, 16<<10, &body) {
		return
	}
	set := map[string]string{} // validated writes, applied together at the end
	bad := func(msg string) { writeErr(w, http.StatusBadRequest, msg) }
	trim := func(p *string) string { return strings.TrimSpace(*p) }
	masked := func(s string) bool { return strings.Contains(s, "****") } // echoed placeholder = keep current

	if body.TGToken != nil {
		t := trim(body.TGToken)
		if !masked(t) {
			if t != "" && !tokenRe.MatchString(t) {
				bad("Bot Token 格式不正确(应形如 123456789:AAH…)")
				return
			}
			set[appsettings.KeyTelegramToken] = t
		}
	}
	if body.TGChatID != nil {
		c := trim(body.TGChatID)
		if c != "" && !chatIDRe.MatchString(c) {
			bad("Chat ID 格式不正确(数字或 @频道名)")
			return
		}
		set[appsettings.KeyTelegramChatID] = c
	}
	if body.CFAccountID != nil {
		v := trim(body.CFAccountID)
		if v != "" && !cfAcctRe.MatchString(v) {
			bad("Cloudflare 账户 ID 应为 32 位十六进制")
			return
		}
		set[appsettings.KeyCFAccountID] = strings.ToLower(v)
	}
	if body.CFToken != nil {
		v := trim(body.CFToken)
		if !masked(v) {
			if v != "" && !cfTokenRe.MatchString(v) {
				bad("Cloudflare API Token 格式不正确")
				return
			}
			set[appsettings.KeyCFToken] = v
		}
	}
	if body.Confirm != nil {
		set[appsettings.KeyRegisterConfirm] = strconv.FormatBool(*body.Confirm)
	}
	if body.MaxPrice != nil {
		v := trim(body.MaxPrice)
		if f, err := strconv.ParseFloat(v, 64); err != nil || f < 0 || f > 100000 {
			bad("单价上限应为 0-100000 的数字(0 表示不限制)")
			return
		}
		set[appsettings.KeyRegisterMaxPrice] = v
	}
	if body.DailyCap != nil {
		if *body.DailyCap < 0 || *body.DailyCap > 1000 {
			bad("每日注册上限应为 0-1000(0 表示不限制)")
			return
		}
		set[appsettings.KeyRegisterDailyCap] = strconv.Itoa(*body.DailyCap)
	}
	if body.PushUncfm != nil {
		set[appsettings.KeyPushUnconfirmed] = strconv.FormatBool(*body.PushUncfm)
	}
	if body.LogLevel != nil {
		switch v := trim(body.LogLevel); v {
		case "debug", "info", "warn", "error":
			set[appsettings.KeyLogLevel] = v
		default:
			bad("日志级别应为 debug、info、warn 或 error")
			return
		}
	}
	if body.ProxyTestURL != nil {
		v := trim(body.ProxyTestURL)
		if v == "" {
			v = proxy.DefaultTestURL
		}
		if u, err := url.Parse(v); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad("测活地址应为 http(s) URL")
			return
		}
		set[appsettings.KeyProxyTestURL] = v
	}

	changed := make([]string, 0, len(set))
	for k, v := range set {
		if err := a.Store.SetSetting(r.Context(), k, v); err != nil {
			a.fail(w, err)
			return
		}
		changed = append(changed, k)
	}
	if lv, ok := set[appsettings.KeyLogLevel]; ok {
		a.Bus.SetMinLevel(lv)
	}
	a.refreshSecrets()
	// names only: never the values, the secrets among them
	a.Bus.Logger("settings").Info("updated", 0, "设置已更新", map[string]any{"keys": changed})
	a.getSettings(w, r)
}

func (a *api) testTelegram(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	if err := a.Telegram.SendTest(ctx); err != nil {
		a.Bus.Logger("notifier").Warn("test_failed", 0, "Telegram 测试消息失败:"+err.Error(), nil)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	a.Bus.Logger("notifier").Info("test_ok", 0, "Telegram 测试消息已发送", nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
