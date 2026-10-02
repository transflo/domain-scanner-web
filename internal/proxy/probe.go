package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"domain_scanner/internal/egress"
)

const (
	// DefaultTestURL answers 204 from anywhere in the world and needs no sign-up.
	DefaultTestURL  = "https://cp.cloudflare.com/generate_204"
	DefaultTraceURL = "https://cloudflare.com/cdn-cgi/trace"
	drainLimit      = 256 << 10
)

// ProbeOptions tunes a liveness test.
type ProbeOptions struct {
	TestURL  string        // requested through the proxy; any 2xx/3xx answer counts as alive
	TraceURL string        // optional: reveals the proxy's exit IP and country
	Timeout  time.Duration // per request (default 10s)
}

// ProbeResult is the outcome of one liveness test.
type ProbeResult struct {
	OK      bool   `json:"ok"`
	DelayMS int64  `json:"delay_ms"` // warm request (keep-alive connection): the tunnel's real round trip
	ColdMS  int64  `json:"cold_ms"`  // first request including the proxy and TLS handshakes
	Status  int    `json:"status"`
	IP      string `json:"ip"`
	Country string `json:"country"`
	Error   string `json:"error"`
}

// Probe tests a SOCKS5 listener by making two requests through it: a cold one (full handshake)
// and a warm one on the kept-alive connection, whose time is reported as the delay, the way
// 3x-ui does. It then asks the trace endpoint where the traffic exits.
func Probe(ctx context.Context, socksAddr string, o ProbeOptions) ProbeResult {
	if o.TestURL == "" {
		o.TestURL = DefaultTestURL
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	eg := egress.Socks("probe", "probe", socksAddr)
	client := eg.HTTPClient(o.Timeout)
	defer client.CloseIdleConnections()

	var r ProbeResult
	do := func(url string) (int, int64, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, 0, err
		}
		req.Header.Set("User-Agent", "domain-scanner-web/1.0 probe")
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return 0, time.Since(start).Milliseconds(), err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
		return resp.StatusCode, time.Since(start).Milliseconds(), nil
	}

	status, cold, err := do(o.TestURL)
	r.ColdMS, r.Status = cold, status
	if err != nil {
		r.Error = shortErr(err)
		return r
	}
	if status < 200 || status >= 400 {
		r.Error = fmt.Sprintf("测试地址返回 HTTP %d", status)
		return r
	}
	r.OK = true
	if _, warm, err := do(o.TestURL); err == nil {
		r.DelayMS = warm
	} else {
		r.DelayMS = cold
	}

	if o.TraceURL != "" {
		tctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		if req, err := http.NewRequestWithContext(tctx, http.MethodGet, o.TraceURL, nil); err == nil {
			if resp, err := client.Do(req); err == nil {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
				resp.Body.Close()
				for _, line := range strings.Split(string(body), "\n") {
					k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
					switch k {
					case "ip":
						r.IP = v
					case "loc":
						r.Country = v
					}
				}
			}
		}
	}
	return r
}

// shortErr trims Go's verbose network errors down to the part a person can act on.
func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 && len(s) > 120 {
		s = s[i+2:]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
