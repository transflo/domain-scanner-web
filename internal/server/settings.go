package server

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"domain_scanner/internal/notifier"
	"domain_scanner/internal/store"
	"domain_scanner/internal/wordlists"
)

type wordlistsInfo = wordlists.Info

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

const (
	keyTelegramToken  = "telegram_token"
	keyTelegramChatID = "telegram_chat_id"
)

var (
	tokenRe  = regexp.MustCompile(`^\d{5,}:[A-Za-z0-9_-]{20,}$`)
	chatIDRe = regexp.MustCompile(`^(-?\d{1,20}|@[A-Za-z0-9_]{3,64})$`)
)

// TelegramConfigFunc resolves the effective Telegram destination on every call: values saved in
// the settings page win over the environment fallback.
func TelegramConfigFunc(st *store.Store, env notifier.Config) notifier.ConfigFunc {
	return func() notifier.Config {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c := env
		if v, ok, _ := st.GetSetting(ctx, keyTelegramToken); ok && v != "" {
			c.Token = v
		}
		if v, ok, _ := st.GetSetting(ctx, keyTelegramChatID); ok && v != "" {
			c.ChatID = v
		}
		return c
	}
}

// maskToken keeps the (non-secret) bot id and the last four characters.
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

func (a *api) getSettings(w http.ResponseWriter, r *http.Request) {
	cfg := TelegramConfigFunc(a.Store, a.TelegramEnv)()
	source := ""
	if cfg.Token != "" && cfg.ChatID != "" {
		source = "env"
		if v, ok, _ := a.Store.GetSetting(r.Context(), keyTelegramToken); ok && v != "" {
			source = "settings"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"telegram_token":      maskToken(cfg.Token),
		"telegram_chat_id":    cfg.ChatID,
		"telegram_configured": cfg.Token != "" && cfg.ChatID != "",
		"telegram_source":     source,
	})
}

func (a *api) putSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token  *string `json:"telegram_token"`
		ChatID *string `json:"telegram_chat_id"`
	}
	if !decodeJSON(w, r, 16<<10, &body) {
		return
	}
	if body.Token != nil {
		t := strings.TrimSpace(*body.Token)
		if t != "" && !strings.Contains(t, "****") && !tokenRe.MatchString(t) {
			writeErr(w, http.StatusBadRequest, "Bot Token 格式不正确(应形如 123456789:AAH…)")
			return
		}
		body.Token = &t
	}
	if body.ChatID != nil {
		c := strings.TrimSpace(*body.ChatID)
		if c != "" && !chatIDRe.MatchString(c) {
			writeErr(w, http.StatusBadRequest, "Chat ID 格式不正确(数字或 @频道名)")
			return
		}
		body.ChatID = &c
	}
	ctx := r.Context()
	if body.Token != nil && !strings.Contains(*body.Token, "****") { // masked echo = keep current
		if err := a.Store.SetSetting(ctx, keyTelegramToken, *body.Token); err != nil {
			a.fail(w, err)
			return
		}
	}
	if body.ChatID != nil {
		if err := a.Store.SetSetting(ctx, keyTelegramChatID, *body.ChatID); err != nil {
			a.fail(w, err)
			return
		}
	}
	a.Bus.Log("info", 0, "Telegram 设置已更新")
	a.getSettings(w, r)
}

func (a *api) testTelegram(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	if err := a.Telegram.SendTest(ctx); err != nil {
		a.Bus.Log("warn", 0, "Telegram 测试消息失败:%v", err)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	a.Bus.Log("info", 0, "Telegram 测试消息已发送")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
