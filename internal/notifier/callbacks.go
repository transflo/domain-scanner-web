package notifier

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"domain_scanner/internal/logbus"
	"domain_scanner/internal/register"
)

// confirmTTL is how long a "confirm registration" prompt stays valid. A forgotten prompt must
// not be able to spend money hours later.
const confirmTTL = 15 * time.Minute

// Registrar is what the button handlers need from the registration service.
type Registrar interface {
	Preview(ctx context.Context, id int64) (register.Preview, error)
	Register(ctx context.Context, id int64) (register.Outcome, error)
}

type callbackQuery struct {
	ID   string `json:"id"`
	Data string `json:"data"`
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Message struct {
		MessageID int64 `json:"message_id"`
		Date      int64 `json:"date"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
}

type update struct {
	UpdateID      int64          `json:"update_id"`
	CallbackQuery *callbackQuery `json:"callback_query"`
}

// StartCallbacks long-polls Telegram for button presses until ctx ends or Stop is called. It
// idles while Telegram is not configured.
func (n *Notifier) StartCallbacks(ctx context.Context, reg Registrar) {
	n.cbWG.Add(1)
	go func() {
		defer n.cbWG.Done()
		var offset int64
		if n.opts.LoadOffset != nil {
			offset = n.opts.LoadOffset()
		}
		failures := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-n.stop:
				return
			default:
			}
			c := n.cfg()
			if c.Token == "" || c.ChatID == "" {
				if !n.sleep(ctx, 2*time.Second) {
					return
				}
				continue
			}
			payload := map[string]any{"timeout": int(n.opts.PollTimeout.Seconds()), "allowed_updates": []string{"callback_query"}}
			if offset > 0 {
				payload["offset"] = offset
			}
			raw, err := n.call(ctx, n.pollHTTP, c, "getUpdates", payload)
			if err != nil {
				failures++
				if failures == 1 || failures%20 == 0 {
					n.lg.Warn("poll_failed", 0, "获取 Telegram 按钮回调失败:"+logbus.RedactSecrets(err.Error(), c.Token), logbus.Fields{"consecutive": failures})
				}
				if !n.sleep(ctx, minDur(time.Duration(failures)*time.Second, 30*time.Second)) {
					return
				}
				continue
			}
			failures = 0
			var ups []update
			_ = json.Unmarshal(raw, &ups)
			for _, u := range ups {
				if u.UpdateID >= offset {
					offset = u.UpdateID + 1
				}
				if u.CallbackQuery != nil {
					n.handleCallback(ctx, c, reg, *u.CallbackQuery)
				}
				if n.opts.SaveOffset != nil {
					n.opts.SaveOffset(offset)
				}
			}
			if len(ups) == 0 && n.opts.PollTimeout < time.Second {
				n.sleep(ctx, 50*time.Millisecond) // a degenerate poll length must not spin
			}
		}
	}()
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (n *Notifier) sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-n.stop:
		return false
	case <-time.After(d):
		return true
	}
}

func (n *Notifier) answer(ctx context.Context, c Config, id, text string, alert bool) {
	text = clip(logbus.RedactSecrets(text, c.Token), 190)
	if _, err := n.call(ctx, n.http, c, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text, "show_alert": alert}); err != nil {
		n.lg.Debug("answer_failed", 0, "answerCallbackQuery 失败:"+logbus.RedactSecrets(err.Error(), c.Token), nil)
	}
}

func (n *Notifier) edit(ctx context.Context, c Config, chatID, msgID int64, text string, markup any) {
	payload := map[string]any{"chat_id": chatID, "message_id": msgID, "text": logbus.RedactSecrets(text, c.Token),
		"parse_mode": "HTML", "disable_web_page_preview": true}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	if _, err := n.call(ctx, n.http, c, "editMessageText", payload); err != nil {
		n.lg.Warn("edit_failed", 0, "editMessageText 失败:"+logbus.RedactSecrets(err.Error(), c.Token), nil)
	}
}

func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// handleCallback processes one button press. Only the configured private chat may press.
func (n *Notifier) handleCallback(ctx context.Context, c Config, reg Registrar, cb callbackQuery) {
	chatID := strconv.FormatInt(cb.Message.Chat.ID, 10)
	if chatID != c.ChatID || cb.From.ID != cb.Message.Chat.ID {
		n.lg.Warn("unauthorised", 0, fmt.Sprintf("忽略来自未授权会话的按钮点击(chat=%d from=%d)", cb.Message.Chat.ID, cb.From.ID),
			logbus.Fields{"chat": cb.Message.Chat.ID, "from": cb.From.ID, "data": cb.Data})
		n.answer(ctx, c, cb.ID, "无权操作", true)
		return
	}
	action, idStr, _ := strings.Cut(cb.Data, ":")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		n.answer(ctx, c, cb.ID, "按钮数据无效", true)
		return
	}
	n.lg.Info("callback", 0, fmt.Sprintf("收到按钮点击:%s #%d", action, id), logbus.Fields{"action": action, "result_id": id})

	switch action {
	case "reg":
		p, err := reg.Preview(ctx, id)
		if err != nil {
			n.refuse(ctx, c, cb, err)
			return
		}
		price := p.Price + " " + p.Currency
		if !p.Confirm {
			n.answer(ctx, c, cb.ID, "正在注册…", false)
			n.cbWG.Add(1)
			go func() {
				defer n.cbWG.Done()
				n.runRegistration(ctx, c, reg, id, 0, 0)
			}()
			return
		}
		text := fmt.Sprintf("⚠️ 确认注册 <code>%s</code>?\n价格:<b>%s</b>(首年)\n将立即从 Cloudflare 账户扣费,<b>注册后不可退款</b>。",
			esc(p.Domain), esc(strings.TrimSpace(price)))
		markup := map[string]any{"inline_keyboard": [][]map[string]string{{
			{"text": "✅ 确认注册", "callback_data": fmt.Sprintf("ok:%d", id)},
			{"text": "❌ 取消", "callback_data": fmt.Sprintf("no:%d", id)},
		}}}
		if _, err := n.sendMessage(ctx, c, text, markup); err != nil {
			n.answer(ctx, c, cb.ID, "无法发送确认消息:"+err.Error(), true)
			return
		}
		n.answer(ctx, c, cb.ID, "请在新消息中确认", false)

	case "ok":
		if age := time.Since(time.Unix(cb.Message.Date, 0)); age > confirmTTL {
			n.lg.Warn("stale_confirmation", 0, fmt.Sprintf("忽略过期的注册确认(消息已存在 %s)", age.Round(time.Second)), logbus.Fields{"result_id": id})
			n.answer(ctx, c, cb.ID, "确认已过期,请重新点击「注册」", true)
			return
		}
		n.answer(ctx, c, cb.ID, "已提交,正在注册…", false)
		n.cbWG.Add(1)
		go func() {
			defer n.cbWG.Done()
			n.runRegistration(ctx, c, reg, id, cb.Message.Chat.ID, cb.Message.MessageID)
		}()

	case "no":
		n.answer(ctx, c, cb.ID, "已取消", false)
		n.edit(ctx, c, cb.Message.Chat.ID, cb.Message.MessageID, "已取消注册,未产生任何扣费。", nil)

	default:
		n.answer(ctx, c, cb.ID, "未知操作", true)
	}
}

// refuse tells the user why a registration cannot go ahead.
func (n *Notifier) refuse(ctx context.Context, c Config, cb callbackQuery, err error) {
	msg := logbus.RedactSecrets(err.Error(), c.Token)
	n.lg.Info("refused", 0, "注册请求被拒绝:"+msg, nil)
	n.answer(ctx, c, cb.ID, msg, true)
}

// runRegistration registers and reports the result. With msgID == 0 (no confirmation message to
// edit) the result is sent as a new message. The registration itself is deliberately not tied to
// the polling context: a shutdown must not abandon a charge half way.
func (n *Notifier) runRegistration(_ context.Context, c Config, reg Registrar, id, chatID, msgID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	show := func(text string) {
		if msgID != 0 {
			n.edit(ctx, c, chatID, msgID, text, nil)
		} else if _, err := n.sendMessage(ctx, c, text, nil); err != nil {
			n.lg.Warn("result_send_failed", 0, "无法发送注册结果:"+logbus.RedactSecrets(err.Error(), c.Token), nil)
		}
	}
	if msgID != 0 {
		show("⏳ 正在注册,请稍候…(最长约 2 分钟)")
	}
	out, err := reg.Register(ctx, id)
	if err != nil {
		show("❌ " + esc(logbus.RedactSecrets(err.Error(), c.Token)))
		return
	}
	switch out.Status {
	case "succeeded":
		show(fmt.Sprintf("🎉 <b>注册成功</b>:<code>%s</code>\n已从 Cloudflare 账户扣费。可在 Cloudflare 面板管理该域名。", esc(out.Domain)))
	case "pending":
		show(fmt.Sprintf("⏳ <code>%s</code> %s", esc(out.Domain), esc(out.Message)))
	default:
		show(fmt.Sprintf("❌ <code>%s</code> 注册失败:%s", esc(out.Domain), esc(out.Message)))
	}
}
