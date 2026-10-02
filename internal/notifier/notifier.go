// Package notifier batches "available domain" hits and delivers them to Telegram.
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
}

type Notifier struct {
	cfg  ConfigFunc
	log  *logbus.Bus
	opts Options
	http *http.Client

	mu      sync.Mutex
	pending map[string][]string
	order   []string
	count   int

	trigger chan struct{}
	stop    chan struct{}
	stopped sync.Once
	wg      sync.WaitGroup

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
	return &Notifier{
		cfg:     cfg,
		log:     log,
		opts:    opts,
		http:    &http.Client{Timeout: opts.HTTPTimeout},
		pending: map[string][]string{},
		trigger: make(chan struct{}, 1),
		stop:    make(chan struct{}),
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

// Notify queues domains for a job. It never blocks.
func (n *Notifier) Notify(job string, domains []string) {
	if len(domains) == 0 {
		return
	}
	n.mu.Lock()
	if _, ok := n.pending[job]; !ok {
		n.order = append(n.order, job)
	}
	n.pending[job] = append(n.pending[job], domains...)
	n.count += len(domains)
	full := n.count >= n.opts.MaxBatch
	n.mu.Unlock()
	if full {
		select {
		case n.trigger <- struct{}{}:
		default:
		}
	}
}

// Stop ends the loop and flushes whatever is still queued. It is safe to call repeatedly.
func (n *Notifier) Stop() {
	n.stopped.Do(func() {
		close(n.stop)
		n.wg.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		n.flush(ctx)
	})
}

// SendTest sends one message and reports the failure reason (token redacted) if any.
func (n *Notifier) SendTest(ctx context.Context) error {
	c := n.cfg()
	if c.Token == "" || c.ChatID == "" {
		return errors.New("Telegram 未配置:请先填写 Bot Token 和 Chat ID")
	}
	err := n.send(ctx, c, "✅ <b>Domain Scanner</b> 测试消息:通知通道工作正常。")
	if err != nil {
		return errors.New(logbus.RedactSecrets(err.Error(), c.Token))
	}
	return nil
}

func (n *Notifier) flush(ctx context.Context) {
	n.flushMu.Lock()
	defer n.flushMu.Unlock()

	n.mu.Lock()
	pending, order := n.pending, n.order
	n.pending, n.order, n.count = map[string][]string{}, nil, 0
	n.mu.Unlock()
	if len(order) == 0 {
		return
	}

	c := n.cfg()
	if c.Token == "" || c.ChatID == "" {
		if !n.warnedConf {
			n.warnedConf = true
			n.log.Log("warn", 0, "Telegram 未配置,已丢弃 %d 个任务的通知(请在设置页填写 Bot Token 和 Chat ID)", len(order))
		}
		return
	}
	n.warnedConf = false

	for _, job := range order {
		delivered := true
		for _, text := range buildMessages(job, pending[job]) {
			if err := n.sendWithRetry(ctx, c, text); err != nil {
				n.log.Log("error", 0, "Telegram 通知发送失败(任务 %q):%s", job,
					logbus.RedactSecrets(err.Error(), c.Token))
				delivered = false
				break
			}
		}
		if delivered {
			n.log.Log("info", 0, "Telegram 已推送任务 %q 的 %d 个可注册域名", job, len(pending[job]))
		}
	}
}

// buildMessages renders HTML messages, each within Telegram's length limit.
func buildMessages(job string, domains []string) []string {
	header := func(part int) string {
		h := fmt.Sprintf("🔔 <b>%s</b> 发现 %d 个可注册域名", html.EscapeString(job), len(domains))
		if part > 1 {
			h += fmt.Sprintf("(续 %d)", part)
		}
		return h
	}
	var out []string
	part := 1
	var sb strings.Builder
	sb.WriteString(header(part))
	size := len([]rune(sb.String()))
	for _, d := range domains {
		line := "\n<code>" + html.EscapeString(d) + "</code>"
		l := len([]rune(line))
		if size+l > maxMessageLen {
			out = append(out, sb.String())
			part++
			sb.Reset()
			sb.WriteString(header(part))
			size = len([]rune(sb.String()))
		}
		sb.WriteString(line)
		size += l
	}
	return append(out, sb.String())
}

type sendError struct {
	msg   string
	retry bool
}

func (e *sendError) Error() string { return e.msg }

func (n *Notifier) sendWithRetry(ctx context.Context, c Config, text string) error {
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err = n.send(ctx, c, text); err == nil {
			return nil
		}
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

// send makes a single attempt.
func (n *Notifier) send(ctx context.Context, c Config, text string) error {
	body, _ := json.Marshal(map[string]any{
		"chat_id":                  c.ChatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	})
	url := strings.TrimRight(n.opts.BaseURL, "/") + "/bot" + c.Token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return &sendError{msg: logbus.RedactSecrets(err.Error(), c.Token)}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.http.Do(req)
	if err != nil {
		// net/http errors embed the request URL, which contains the token.
		return &sendError{msg: logbus.RedactSecrets(err.Error(), c.Token), retry: true}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(data, &r)
	if resp.StatusCode == http.StatusOK && r.OK {
		return nil
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
	return &sendError{
		msg:   logbus.RedactSecrets(fmt.Sprintf("Telegram API 错误 (HTTP %d): %s", resp.StatusCode, desc), c.Token),
		retry: resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests,
	}
}
