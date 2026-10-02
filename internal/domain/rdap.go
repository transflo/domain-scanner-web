package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RDAPResult is the outcome of an RDAP domain lookup.
type RDAPResult int

const (
	// RDAPUnsupported: the TLD has no RDAP server in the IANA bootstrap file.
	RDAPUnsupported RDAPResult = iota
	// RDAPFound: the registry knows the domain (HTTP 200) — it is registered.
	RDAPFound
	// RDAPNotFound: the registry answered 404 — the domain is not registered.
	RDAPNotFound
	// RDAPError: no usable answer (5xx, network); callers may fall back to WHOIS.
	RDAPError
	// RDAPRateLimited: the server kept answering 429. Another protocol will not help, so callers
	// should report "unknown" instead of falling back.
	RDAPRateLimited
)

const (
	maxRateLimitRetries = 2
	maxRetryAfter       = 10 * time.Second
)

const (
	defaultBootstrapURL = "https://data.iana.org/rdap/dns.json"
	rdapUserAgent       = "domain-scanner-web/1.0 (+https://github.com/xuemian168/domain-scanner)"
)

// RDAPClient queries registry RDAP servers, locating them through the IANA bootstrap file.
// RDAP is WHOIS's standardised HTTP replacement: it is machine readable and, unlike many
// registries' port-43 WHOIS, generally open to automated clients.
type RDAPClient struct {
	HTTP         *http.Client
	BootstrapURL string
	BootstrapTTL time.Duration
	// Overrides maps a TLD to an RDAP base URL (ending in "/") and wins over the IANA bootstrap.
	// Many ccTLDs run RDAP but are not in the bootstrap file, so a verified list is built in.
	Overrides map[string]string
	// MinInterval is the minimum spacing between requests to the same RDAP server, shared by all
	// workers, so a burst of parallel scans does not trip the registry's rate limit.
	MinInterval time.Duration
	// RetryDelay is used after a 429 that carries no usable Retry-After header.
	RetryDelay time.Duration

	mu      sync.Mutex
	servers map[string]string
	fetched time.Time

	limMu sync.Mutex
	next  map[string]time.Time
}

// defaultRDAPServers are ccTLD/gTLD RDAP endpoints that are absent from the IANA bootstrap
// (or unreliable there) and were verified to answer 200 for a registered name and 404 for a
// random unregistered one.
var defaultRDAPServers = map[string]string{
	"li": "https://rdap.nic.ch/",
	"ch": "https://rdap.nic.ch/",
	"de": "https://rdap.denic.de/",
	"nl": "https://rdap.sidn.nl/",
	"fr": "https://rdap.nic.fr/",
	"cz": "https://rdap.nic.cz/",
	"pl": "https://rdap.dns.pl/",
	"io": "https://rdap.identitydigital.services/rdap/",
	"ai": "https://rdap.identitydigital.services/rdap/",
	"sh": "https://rdap.identitydigital.services/rdap/",
	"ac": "https://rdap.identitydigital.services/rdap/",
	"cc": "https://tld-rdap.verisign.com/cc/v1/",
}

func NewRDAPClient() *RDAPClient {
	overrides := make(map[string]string, len(defaultRDAPServers))
	for k, v := range defaultRDAPServers {
		overrides[k] = v
	}
	return &RDAPClient{
		HTTP:         &http.Client{Timeout: 10 * time.Second},
		BootstrapURL: defaultBootstrapURL,
		BootstrapTTL: 24 * time.Hour,
		Overrides:    overrides,
		MinInterval:  250 * time.Millisecond,
		RetryDelay:   2 * time.Second,
		next:         map[string]time.Time{},
	}
}

// waitTurn blocks until this client may send the next request to base.
func (c *RDAPClient) waitTurn(ctx context.Context, base string) error {
	if c.MinInterval <= 0 {
		return nil
	}
	c.limMu.Lock()
	if c.next == nil {
		c.next = map[string]time.Time{}
	}
	at := c.next[base]
	if now := time.Now(); at.Before(now) {
		at = now
	}
	c.next[base] = at.Add(c.MinInterval)
	c.limMu.Unlock()
	return sleepCtx(ctx, time.Until(at))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryDelay honours a numeric Retry-After header (capped), else the configured default.
func (c *RDAPClient) retryDelay(h http.Header) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && s >= 0 {
		d := time.Duration(s) * time.Second
		if d > maxRetryAfter {
			d = maxRetryAfter
		}
		return d
	}
	return c.RetryDelay
}

// Lookup asks the TLD's RDAP server about domain.
func (c *RDAPClient) Lookup(ctx context.Context, domain string) (RDAPResult, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	i := strings.LastIndex(domain, ".")
	if i < 0 || i == len(domain)-1 {
		return RDAPError, fmt.Errorf("rdap: %q has no TLD", domain)
	}
	tld := domain[i+1:]
	base, ok := c.Overrides[tld]
	if !ok {
		servers, err := c.bootstrap(ctx)
		if err != nil {
			return RDAPError, err
		}
		if base, ok = servers[tld]; !ok {
			return RDAPUnsupported, nil
		}
	}

	for attempt := 0; ; attempt++ {
		if err := c.waitTurn(ctx, base); err != nil {
			return RDAPError, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"domain/"+domain, nil)
		if err != nil {
			return RDAPError, err
		}
		req.Header.Set("Accept", "application/rdap+json")
		req.Header.Set("User-Agent", rdapUserAgent)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return RDAPError, fmt.Errorf("rdap request failed: %w", err)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusOK:
			return RDAPFound, nil
		case http.StatusNotFound:
			return RDAPNotFound, nil
		case http.StatusTooManyRequests:
			if attempt >= maxRateLimitRetries {
				return RDAPRateLimited, fmt.Errorf("rdap %s: HTTP 429 (rate limited)", base)
			}
			if err := sleepCtx(ctx, c.retryDelay(resp.Header)); err != nil {
				return RDAPError, err
			}
		default:
			return RDAPError, fmt.Errorf("rdap %s: HTTP %d", base, resp.StatusCode)
		}
	}
}

// bootstrap returns tld -> RDAP base URL (always ending in "/"), cached for BootstrapTTL.
// A failed refresh falls back to the stale copy when there is one.
func (c *RDAPClient) bootstrap(ctx context.Context) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.servers != nil && time.Since(c.fetched) < c.BootstrapTTL {
		return c.servers, nil
	}
	servers, err := c.fetchBootstrap(ctx)
	if err != nil {
		if c.servers != nil {
			return c.servers, nil
		}
		return nil, err
	}
	c.servers, c.fetched = servers, time.Now()
	return servers, nil
}

func (c *RDAPClient) fetchBootstrap(ctx context.Context) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BootstrapURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", rdapUserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rdap bootstrap: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rdap bootstrap: HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Services [][][]string `json:"services"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("rdap bootstrap: %w", err)
	}
	servers := map[string]string{}
	for _, svc := range doc.Services {
		if len(svc) != 2 || len(svc[1]) == 0 {
			continue
		}
		base := svc[1][0]
		for _, u := range svc[1] { // prefer https
			if strings.HasPrefix(u, "https://") {
				base = u
				break
			}
		}
		if !strings.HasSuffix(base, "/") {
			base += "/"
		}
		for _, tld := range svc[0] {
			servers[strings.ToLower(tld)] = base
		}
	}
	return servers, nil
}
