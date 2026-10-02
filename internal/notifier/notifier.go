// Package notifier batches "available domain" hits, delivers them to Telegram (with price,
// confirmation labels and register buttons) and handles the button presses.
package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"domain_scanner/internal/logbus"
)

const (
	telegramAPI   = "https://api.telegram.org"
	maxAttempts   = 3
	maxMessageLen = 4000 // Telegram's hard limit is 4096; keep headroom for the header
	maxButtons    = 8    // register buttons per message
)

// Config is the Telegram destination. It is read on every send so UI changes apply at once.
type Config struct {
	Token  string
	ChatID string
}

type ConfigFunc func() Config

type Options struct {
	BaseURL     string        // default https://api.telegram.org
	FlushEvery  time.Duration // default 10s
	MaxBatch    int           // flush early once this many domains are queued; default 20
	Backoff     time.Duration // base retry delay; default 2s
	HTTPTimeout time.Duration // default 15s
	// PollTimeout is the getUpdates long-poll length for button presses (default 25s).
	PollTimeout time.Duration
	// LoadOffset / SaveOffset persist the Telegram update offset so presses handled before a
	// restart are never delivered (and acted on) again.
	LoadOffset func() int64
	SaveOffset func(int64)
}

type Notifier struct {
	cfg      ConfigFunc
	bus      *logbus.Bus
	lg       *logbus.Logger
	opts     Options
	http     *http.Client
	pollHTTP *http.Client

	mu      sync.Mutex
	pending map[string][]Item
	order   []string
	count   int

	trigger chan struct{}
	stop    chan struct{}
	stopped sync.Once
	wg      sync.WaitGroup
	cbWG    sync.WaitGroup

	flushMu    sync.Mutex
	warnedConf bool
}

func New(cfg ConfigFunc, log *logbus.Bus, opts Options) *Notifier {
	if opts.BaseURL == "" {
		opts.BaseURL = telegramAPI
	}
	if opts.FlushEvery <= 0 {
		opts.FlushEvery = 10 * time.Second
	}
	if opts.MaxBatch <= 0 {
		opts.MaxBatch = 20
	}
	if opts.Backoff <= 0 {
		opts.Backoff = 2 * time.Second
	}
	if opts.HTTPTimeout <= 0 {
		opts.HTTPTimeout = 15 * time.Second
	}
	if opts.PollTimeout <= 0 {
		opts.PollTimeout = 25 * time.Second
	}
	return &Notifier{
		cfg:      cfg,
		bus:      log,
		lg:       log.Logger("notifier"),
		opts:     opts,
		http:     &http.Client{Timeout: opts.HTTPTimeout},
		pollHTTP: &http.Client{Timeout: opts.PollTimeout + 15*time.Second},
		pending:  map[string][]Item{},
		trigger:  make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
}

// Start launches the background flush loop.
func (n *Notifier) Start(ctx context.Context) {
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		t := time.NewTicker(n.opts.FlushEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-n.stop:
				return
			case <-t.C:
				n.flush(ctx)
			case <-n.trigger:
				n.flush(ctx)
			}
		}
	}()
}

// Notify queues plain, unlabelled domains for a job (kept for callers without verification
// data). It never blocks.
func (n *Notifier) Notify(job string, domains []string) {
	items := make([]Item, 0, len(domains))
	for _, d := range domains {
		items = append(items, Item{Domain: d})
	}
	n.NotifyItems(job, items)
}

// NotifyItems queues items for a job. It never blocks.
func (n *Notifier) NotifyItems(job string, items []Item) {
	if len(items) == 0 {
		return
	}
	n.mu.Lock()
	if _, ok := n.pending[job]; !ok {
		n.order = append(n.order, job)
	}
	n.pending[job] = append(n.pending[job], items...)
	n.count += len(items)
	total, full := n.count, n.count >= n.opts.MaxBatch
	n.mu.Unlock()
	n.lg.Debug("queued", 0, fmt.Sprintf("已加入推送队列:任务「%s」+%d 个域名(队列共 %d)", job, len(items), total),
		logbus.Fields{"job": job, "added": len(items), "queued": total})
	if full {
		select {
		case n.trigger <- struct{}{}:
		default:
		}
	}
}

// Stop ends the loops and flushes whatever is still queued. It is safe to call repeatedly.
func (n *Notifier) Stop() {
	n.stopped.Do(func() {
		close(n.stop)
		n.wg.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		n.flush(ctx)
		done := make(chan struct{})
		go func() { n.cbWG.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
}

// SendTest sends one message and reports the failure reason (token redacted) if any.
func (n *Notifier) SendTest(ctx context.Context) error {
	c := n.cfg()
	if c.Token == "" || c.ChatID == "" {
		return errors.New("Telegram 未配置:请先填写 Bot Token 和 Chat ID")
	}
	if _, err := n.sendMessage(ctx, c, "✅ <b>Domain Scanner</b> 测试消息:通知通道工作正常。", nil); err != nil {
		return errors.New(logbus.RedactSecrets(err.Error(), c.Token))
	}
	return nil
}

func (n *Notifier) flush(ctx context.Context) {
	n.flushMu.Lock()
	defer n.flushMu.Unlock()

	n.mu.Lock()
	pending, order := n.pending, n.order
	n.pending, n.order, n.count = map[string][]Item{}, nil, 0
	n.mu.Unlock()
	if len(order) == 0 {
		return
	}

	c := n.cfg()
	if c.Token == "" || c.ChatID == "" {
		if !n.warnedConf {
			n.warnedConf = true
			n.lg.Warn("unconfigured", 0, fmt.Sprintf("Telegram 未配置,已丢弃 %d 个任务的通知(请在设置页填写 Bot Token 和 Chat ID)", len(order)), nil)
		}
		return
	}
	n.warnedConf = false

	for _, job := range order {
		items := pending[job]
		delivered := true
		msgs := buildMessages(job, items, buttonsAllowed(c.ChatID))
		for i, m := range msgs {
			t0 := time.Now()
			if err := n.sendWithRetry(ctx, c, m.Text, m.markup()); err != nil {
				n.lg.Error("send_failed", 0, fmt.Sprintf("Telegram 通知发送失败(任务 %q):%s", job, logbus.RedactSecrets(err.Error(), c.Token)),
					logbus.Fields{"job": job, "part": i + 1, "parts": len(msgs), "domains": len(items)})
				delivered = false
				break
			}
			n.lg.Debug("part_sent", 0, fmt.Sprintf("任务 %q 的第 %d/%d 条消息已发送", job, i+1, len(msgs)),
				logbus.Fields{"job": job, "part": i + 1, "parts": len(msgs), "duration_ms": time.Since(t0), "buttons": len(m.Buttons)})
		}
		if delivered {
			n.lg.Info("pushed", 0, fmt.Sprintf("Telegram 已推送任务 %q 的 %d 个可注册域名(%d 条消息)", job, len(items), len(msgs)),
				logbus.Fields{"job": job, "domains": len(items), "messages": len(msgs)})
		}
	}
}

// buttonsAllowed: register buttons only go to a private chat (a positive numeric id), where the
// only person who can press them is the owner. In groups and channels anyone present could
// spend the account's money.
func buttonsAllowed(chatID string) bool {
	if chatID == "" || strings.HasPrefix(chatID, "-") || strings.HasPrefix(chatID, "@") {
		return false
	}
	for _, r := range chatID {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

type button struct{ Text, Data string }

type message struct {
	Text    string
	Buttons []button
}

func (m message) markup() any {
	if len(m.Buttons) == 0 {
		return nil
	}
	rows := make([][]map[string]string, 0, len(m.Buttons))
	for _, b := range m.Buttons {
		rows = append(rows, []map[string]string{{"text": b.Text, "callback_data": b.Data}})
	}
	return map[string]any{"inline_keyboard": rows}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func itemLine(it Item) string {
	d := "<code>" + html.EscapeString(it.Domain) + "</code>"
	switch it.Status {
	case StatusConfirmed:
		price := ""
		if it.Price != "" {
			price = fmt.Sprintf(" · %s %s", html.EscapeString(it.Price), html.EscapeString(it.Currency))
		}
		return "✅ " + d + price + " · Cloudflare 已确认"
	case StatusUnconfirmed:
		note := ""
		if it.Note != "" {
			note = "(" + html.EscapeString(it.Note) + ")"
		}
		return "⚠️ " + d + " · 未经 Cloudflare 确认" + note
	}
	return "• " + d
}

// buildMessages renders items into messages within Telegram's length limit and at most
// maxButtons register buttons each. Only confirmed items that have a result id get a button.
func buildMessages(job string, items []Item, withButtons bool) []message {
	confirmed := 0
	for _, it := range items {
		if it.Status == StatusConfirmed {
			confirmed++
		}
	}
	header := func(part int) string {
		h := fmt.Sprintf("🔔 <b>%s</b> 发现 %d 个可注册域名", html.EscapeString(job), len(items))
		if confirmed > 0 {
			h += fmt.Sprintf("(%d 个已由 Cloudflare 确认)", confirmed)
		}
		if part > 1 {
			h += fmt.Sprintf("(续 %d)", part)
		}
		return h
	}
	var out []message
	part := 1
	cur := message{Text: header(part)}
	size := len([]rune(cur.Text))
	flush := func() {
		out = append(out, cur)
		part++
		cur = message{Text: header(part)}
		size = len([]rune(cur.Text))
	}
	for _, it := range items {
		line := "\n" + itemLine(it)
		l := len([]rune(line))
		wantsButton := withButtons && it.Status == StatusConfirmed && it.ResultID != 0
		if size+l > maxMessageLen || (wantsButton && len(cur.Buttons) >= maxButtons) {
			flush()
		}
		cur.Text += line
		size += l
		if wantsButton {
			label := "注册 " + it.Domain
			if it.Price != "" {
				label += fmt.Sprintf(" · %s %s", it.Price, it.Currency)
			}
			cur.Buttons = append(cur.Buttons, button{Text: clip(label, 60), Data: fmt.Sprintf("reg:%d", it.ResultID)})
		}
	}
	return append(out, cur)
}

type sendError struct {
	msg   string
	retry bool
}

func (e *sendError) Error() string { return e.msg }

func (n *Notifier) sendMessage(ctx context.Context, c Config, text string, markup any) (int64, error) {
	payload := map[string]any{"chat_id": c.ChatID, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	raw, err := n.call(ctx, n.http, c, "sendMessage", payload)
	if err != nil {
		return 0, err
	}
	var r struct {
		MessageID int64 `json:"message_id"`
	}
	_ = json.Unmarshal(raw, &r)
	return r.MessageID, nil
}

func (n *Notifier) sendWithRetry(ctx context.Context, c Config, text string, markup any) error {
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		t0 := time.Now()
		var id int64
		if id, err = n.sendMessage(ctx, c, text, markup); err == nil {
			n.lg.Debug("send_ok", 0, fmt.Sprintf("sendMessage 成功(message_id=%d,第 %d 次尝试)", id, attempt),
				logbus.Fields{"message_id": id, "attempt": attempt, "duration_ms": time.Since(t0), "bytes": len(text)})
			return nil
		}
		n.lg.Debug("send_attempt_failed", 0, fmt.Sprintf("sendMessage 第 %d 次尝试失败:%s", attempt, logbus.RedactSecrets(err.Error(), c.Token)),
			logbus.Fields{"attempt": attempt, "duration_ms": time.Since(t0)})
		var se *sendError
		if errors.As(err, &se) && !se.retry {
			return err
		}
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(n.opts.Backoff * time.Duration(attempt)):
			}
		}
	}
	return err
}

// call makes one Telegram Bot API request. Errors never contain the token.
func (n *Notifier) call(ctx context.Context, client *http.Client, c Config, method string, payload any) (json.RawMessage, error) {
	body, _ := json.Marshal(payload)
	url := strings.TrimRight(n.opts.BaseURL, "/") + "/bot" + c.Token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, &sendError{msg: logbus.RedactSecrets(err.Error(), c.Token)}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// net/http errors embed the request URL, which contains the token.
		return nil, &sendError{msg: logbus.RedactSecrets(err.Error(), c.Token), retry: true}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	_ = json.Unmarshal(data, &r)
	if resp.StatusCode == http.StatusOK && r.OK {
		return r.Result, nil
	}
	desc := r.Description
	if desc == "" {
		desc = strings.TrimSpace(string(data))
		if len(desc) > 200 {
			desc = desc[:200]
		}
	}
	if strings.Contains(strings.ToLower(desc), "chat not found") {
		desc += "(提示:请先在 Telegram 里打开这个机器人并点击 Start / 发送 /start,机器人才能给你发消息;也请核对 Chat ID)"
	}
	return nil, &sendError{
		msg:   logbus.RedactSecrets(fmt.Sprintf("Telegram API 错误 (HTTP %d): %s", resp.StatusCode, desc), c.Token),
		retry: resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests,
	}
}
