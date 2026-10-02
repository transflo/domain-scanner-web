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
	// RetryDelay is the first pause after a 429. Further 429s double it up to MaxBackoff; a
	// successful answer resets it. The pause applies to every caller of that server, so parallel
	// workers back off together instead of each hammering a rate-limited registry.
	RetryDelay time.Duration
	MaxBackoff time.Duration
	// MaxWait bounds how long one Lookup keeps waiting out rate limits before giving up with
	// RDAPRateLimited (the domain is then reported as unknown).
	MaxWait time.Duration
	// OnThrottle, if set, is told whenever a server starts a new pause (for logging).
	OnThrottle func(base string, wait time.Duration)

	mu      sync.Mutex
	servers map[string]string
	fetched time.Time

	limMu sync.Mutex
	next  map[string]*serverState
}

// serverState is the shared pacing state for one RDAP server.
type serverState struct {
	next         time.Time     // earliest start of the next request
	blockedUntil time.Time     // pause requested by the server (429)
	penalty      time.Duration // current backoff, 0 when healthy
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
		RetryDelay:   5 * time.Second,
		MaxBackoff:   5 * time.Minute,
		MaxWait:      10 * time.Minute,
		next:         map[string]*serverState{},
	}
}

// state returns the pacing state for base; the caller must hold limMu.
func (c *RDAPClient) state(base string) *serverState {
	if c.next == nil {
		c.next = map[string]*serverState{}
	}
	st := c.next[base]
	if st == nil {
		st = &serverState{}
		c.next[base] = st
	}
	return st
}

// waitTurn blocks until this client may send the next request to base: after any server-imposed
// pause and at least MinInterval after the previous request.
func (c *RDAPClient) waitTurn(ctx context.Context, base string) error {
	c.limMu.Lock()
	st := c.state(base)
	at := time.Now()
	if st.next.After(at) {
		at = st.next
	}
	if st.blockedUntil.After(at) {
		at = st.blockedUntil
	}
	st.next = at.Add(c.MinInterval)
	c.limMu.Unlock()
	return sleepCtx(ctx, time.Until(at))
}

// throttle records a 429 from base and returns how long callers must now wait. Concurrent 429s
// that arrive during an existing pause share it rather than escalating the backoff again.
func (c *RDAPClient) throttle(base string, h http.Header) time.Duration {
	c.limMu.Lock()
	st := c.state(base)
	now := time.Now()
	if now.Before(st.blockedUntil) {
		wait := st.blockedUntil.Sub(now)
		c.limMu.Unlock()
		return wait
	}
	wait := st.penalty * 2
	if wait < c.RetryDelay {
		wait = c.RetryDelay
	}
	if s, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && s > 0 {
		if ra := time.Duration(s) * time.Second; ra > wait {
			wait = ra
		}
	}
	if c.MaxBackoff > 0 && wait > c.MaxBackoff {
		wait = c.MaxBackoff
	}
	st.penalty = wait
	st.blockedUntil = now.Add(wait)
	c.limMu.Unlock()
	if c.OnThrottle != nil {
		c.OnThrottle(base, wait)
	}
	return wait
}

// recovered clears the backoff after a normal answer.
func (c *RDAPClient) recovered(base string) {
	c.limMu.Lock()
	c.state(base).penalty = 0
	c.limMu.Unlock()
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

	maxWait := c.MaxWait
	if d, ok := ctx.Value(maxWaitKey).(time.Duration); ok {
		maxWait = d
	}
	start := time.Now()
	for attempt := 1; ; attempt++ {
		t0 := time.Now()
		if err := c.waitTurn(ctx, base); err != nil {
			return RDAPError, err
		}
		if waited := time.Since(t0); waited > 50*time.Millisecond {
			step(ctx, "rdap.wait", fmt.Sprintf("server=%s paced/paused for %s", base, waited.Round(time.Millisecond)), t0, true)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"domain/"+domain, nil)
		if err != nil {
			return RDAPError, err
		}
		req.Header.Set("Accept", "application/rdap+json")
		req.Header.Set("User-Agent", rdapUserAgent)
		t1 := time.Now()
		resp, err := c.HTTP.Do(req)
		if err != nil {
			step(ctx, "rdap.request", fmt.Sprintf("server=%s attempt=%d error=%v", base, attempt, err), t1, false)
			return RDAPError, fmt.Errorf("rdap request failed: %w", err)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		step(ctx, "rdap.request", fmt.Sprintf("server=%s attempt=%d status=%d", base, attempt, resp.StatusCode), t1,
			resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound)

		switch resp.StatusCode {
		case http.StatusOK:
			c.recovered(base)
			return RDAPFound, nil
		case http.StatusNotFound:
			c.recovered(base)
			return RDAPNotFound, nil
		case http.StatusTooManyRequests:
			wait := c.throttle(base, resp.Header)
			giveUp := maxWait > 0 && time.Since(start)+wait > maxWait
			step(ctx, "rdap.throttle", fmt.Sprintf("server=%s HTTP 429, server pause %s, giving up=%v", base,
				wait.Round(time.Millisecond), giveUp), time.Now(), false)
			if giveUp {
				return RDAPRateLimited, fmt.Errorf("rdap %s: HTTP 429 (rate limited, gave up after %s)",
					base, time.Since(start).Round(time.Second))
			}
			// loop: waitTurn sleeps out the pause, then the request is retried
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
