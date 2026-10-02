// Package cloudflare talks to the Cloudflare Registrar API: an authoritative availability check
// (domain-check) used as the last word before a domain is announced, and the registration call
// behind the Telegram button.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"domain_scanner/internal/appsettings"
)

const (
	defaultBase = "https://api.cloudflare.com/client/v4"
	maxBatch    = 20
	maxBody     = 1 << 20
)

// ErrNotConfigured means no account id / token is available.
var ErrNotConfigured = errors.New("Cloudflare 未配置(需要账户 ID 和 API Token)")

// APIError is a non-2xx answer. Its text never contains the token.
type APIError struct {
	Status     int
	Code       int
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("Cloudflare API HTTP %d(代码 %d):%s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("Cloudflare API HTTP %d:%s", e.Status, e.Message)
}

func (e *APIError) IsAuth() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}
func (e *APIError) IsRateLimit() bool { return e.Status == http.StatusTooManyRequests }

// Temporary reports whether retrying later may succeed.
func (e *APIError) Temporary() bool { return e.IsRateLimit() || e.Status >= 500 }

// Domain is the result of checking one name.
type Domain struct {
	Name             string
	Registrable      bool
	Tier             string // standard | premium
	Reason           string // set when not registrable
	Currency         string
	RegistrationCost string // first year, as a decimal string
	RenewalCost      string
}

// Unsupported reports that Cloudflare cannot register this extension at all, so it cannot
// confirm availability either (as opposed to the name being taken).
func (d Domain) Unsupported() bool {
	return d.Reason == "extension_not_supported" || d.Reason == "extension_not_supported_via_api"
}

// RegisterResult is the state of a registration workflow.
type RegisterResult struct {
	State              string // pending, in_progress, action_required, blocked, succeeded, failed
	Completed          bool
	Domain             string
	RegistrationStatus string // e.g. active
	ErrorCode          string
	ErrorMessage       string
}

func (r RegisterResult) Succeeded() bool { return r.State == "succeeded" }

// Client calls the Registrar API with credentials read on every request, so a token changed in
// the settings page applies at once.
type Client struct {
	Config      func() appsettings.Cloudflare
	BaseURL     string
	HTTP        *http.Client
	PollEvery   time.Duration // between registration status polls
	MaxPollWait time.Duration // how long Register waits for an async registration
}

func New(cfg func() appsettings.Cloudflare) *Client {
	return &Client{Config: cfg, BaseURL: defaultBase, HTTP: &http.Client{Timeout: 30 * time.Second},
		PollEvery: 4 * time.Second, MaxPollWait: 2 * time.Minute}
}

// Configured reports whether credentials are available.
func (c *Client) Configured() bool { return c.Config().Configured() }

type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiErrorEntry `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiErrorEntry struct {
	Code    json.Number `json:"code"`
	Message string      `json:"message"`
}

func (c *Client) do(ctx context.Context, method, path string, body any, header map[string]string) (int, json.RawMessage, error) {
	cfg := c.Config()
	if !cfg.Configured() {
		return 0, nil, ErrNotConfigured
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// net/http errors can embed the URL; the URL carries no secret, but be explicit anyway
		return 0, nil, errors.New(strings.ReplaceAll(err.Error(), cfg.Token, "***"))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))

	var env envelope
	_ = json.Unmarshal(raw, &env)
	if resp.StatusCode < 200 || resp.StatusCode > 299 || (len(raw) > 0 && json.Valid(raw) && !env.Success && len(env.Errors) > 0) {
		ae := &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
		if len(env.Errors) > 0 {
			ae.Message = env.Errors[0].Message
			n, _ := env.Errors[0].Code.Int64()
			ae.Code = int(n)
		}
		if len(ae.Message) > 300 {
			ae.Message = ae.Message[:300] + "…"
		}
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
			ae.RetryAfter = time.Duration(s) * time.Second
		}
		if ae.Status < 300 { // 2xx with success=false
			ae.Status = http.StatusBadRequest
		}
		ae.Message = strings.ReplaceAll(ae.Message, cfg.Token, "***")
		return resp.StatusCode, nil, ae
	}
	return resp.StatusCode, env.Result, nil
}

func (c *Client) acct() string { return c.Config().AccountID }

// Check asks Cloudflare whether each name can be registered right now (1-20 names per call).
func (c *Client) Check(ctx context.Context, domains []string) ([]Domain, error) {
	if len(domains) == 0 || len(domains) > maxBatch {
		return nil, fmt.Errorf("domain-check 一次需要 1-%d 个域名,收到 %d 个", maxBatch, len(domains))
	}
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	_, raw, err := c.do(ctx, http.MethodPost, "/accounts/"+c.acct()+"/registrar/domain-check",
		map[string]any{"domains": domains}, nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		Domains []struct {
			Name        string `json:"name"`
			Registrable bool   `json:"registrable"`
			Tier        string `json:"tier"`
			Reason      string `json:"reason"`
			Pricing     struct {
				Currency         string `json:"currency"`
				RegistrationCost string `json:"registration_cost"`
				RenewalCost      string `json:"renewal_cost"`
			} `json:"pricing"`
		} `json:"domains"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("无法解析 domain-check 响应:%w", err)
	}
	out := make([]Domain, 0, len(res.Domains))
	for _, d := range res.Domains {
		out = append(out, Domain{Name: d.Name, Registrable: d.Registrable, Tier: d.Tier, Reason: d.Reason,
			Currency: d.Pricing.Currency, RegistrationCost: d.Pricing.RegistrationCost, RenewalCost: d.Pricing.RenewalCost})
	}
	return out, nil
}

type workflow struct {
	State     string `json:"state"`
	Completed bool   `json:"completed"`
	Context   struct {
		DomainName   string `json:"domain_name"`
		Registration struct {
			Status string `json:"status"`
		} `json:"registration"`
	} `json:"context"`
	Links struct {
		Self string `json:"self"`
	} `json:"links"`
	Error *struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	} `json:"error"`
}

func (w workflow) result(domain string) RegisterResult {
	r := RegisterResult{State: w.State, Completed: w.Completed, Domain: w.Context.DomainName,
		RegistrationStatus: w.Context.Registration.Status}
	if r.Domain == "" {
		r.Domain = domain
	}
	if w.Error != nil {
		r.ErrorMessage = w.Error.Message
		r.ErrorCode = strings.Trim(string(w.Error.Code), `"`)
	}
	return r
}

// Register starts a registration and waits (up to MaxPollWait) for it to finish. It authorises
// a real, non-refundable charge — callers must have confirmed with the user. auto_renew is never
// sent, so it stays off.
func (c *Client) Register(ctx context.Context, domain string) (RegisterResult, error) {
	if !c.Configured() {
		return RegisterResult{}, ErrNotConfigured
	}
	_, raw, err := c.do(ctx, http.MethodPost, "/accounts/"+c.acct()+"/registrar/registrations",
		map[string]any{"domain_name": domain}, map[string]string{"Prefer": "respond-async"})
	if err != nil {
		return RegisterResult{}, err
	}
	var w workflow
	if err := json.Unmarshal(raw, &w); err != nil {
		return RegisterResult{}, fmt.Errorf("无法解析注册响应:%w", err)
	}
	deadline := time.Now().Add(c.MaxPollWait)
	for !w.Completed {
		// Only follow a relative API path: the token must never be sent to another host.
		if !strings.HasPrefix(w.Links.Self, "/accounts/") || time.Now().After(deadline) {
			return w.result(domain), nil
		}
		select {
		case <-ctx.Done():
			return w.result(domain), ctx.Err()
		case <-time.After(c.PollEvery):
		}
		self := w.Links.Self
		_, raw, err := c.do(ctx, http.MethodGet, self, nil, nil)
		if err != nil {
			var ae *APIError
			if errors.As(err, &ae) && ae.Temporary() {
				continue
			}
			return w.result(domain), err
		}
		next := workflow{}
		if err := json.Unmarshal(raw, &next); err != nil {
			return w.result(domain), fmt.Errorf("无法解析注册状态:%w", err)
		}
		if next.Links.Self == "" {
			next.Links.Self = self
		}
		w = next
	}
	return w.result(domain), nil
}

// Verify checks that the token is active and that the Registrar API accepts it.
func (c *Client) Verify(ctx context.Context) error {
	if !c.Configured() {
		return ErrNotConfigured
	}
	_, raw, err := c.do(ctx, http.MethodGet, "/accounts/"+c.acct()+"/tokens/verify", nil, nil)
	if err != nil {
		return fmt.Errorf("校验 Token 失败:%w", err)
	}
	var tv struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(raw, &tv)
	if tv.Status != "" && tv.Status != "active" {
		return fmt.Errorf("Token 状态为 %s(需要 active)", tv.Status)
	}
	if _, err := c.Check(ctx, []string{"example.com"}); err != nil {
		return fmt.Errorf("Token 有效,但 Registrar 接口不可用(请确认 Token 含 Registrar 权限、账户已启用 Registrar):%w", err)
	}
	return nil
}
