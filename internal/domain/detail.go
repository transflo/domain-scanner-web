package domain

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"

	"domain_scanner/internal/egress"
	"domain_scanner/internal/reserved"

	"github.com/likexian/whois"
)

// Status is the outcome of a registration check.
type Status string

const (
	StatusAvailable  Status = "available"
	StatusRegistered Status = "registered"
	StatusReserved   Status = "reserved"
	// StatusUnknown means no reliable evidence either way (timeouts, rate limits, unparseable
	// WHOIS). It must never be treated as available.
	StatusUnknown Status = "unknown"
)

// Verdict is the detailed result of Checker.Check.
type Verdict struct {
	Domain     string
	Status     Status
	Signatures []string
	Reason     string
	// ErrKind says why an unknown verdict is unknown (empty for known verdicts).
	ErrKind    ErrKind
	Steps      []Step
	DurationMS int64
}

// Checker decides whether a domain is registered. It reuses the upstream WHOIS indicator
// lists but reports "unknown" instead of guessing, and does a single WHOIS pass.
// All network access goes through injectable functions so it can be tested offline.
type Checker struct {
	LookupNS func(string) ([]*net.NS, error)
	LookupIP func(string) ([]net.IP, error)
	LookupMX func(string) ([]*net.MX, error)
	Whois    func(domain string, servers ...string) (string, error)
	HasTLS   func(domain string) bool
	// RDAP is consulted before WHOIS when set; nil disables it.
	RDAP func(ctx context.Context, domain string) (RDAPResult, error)

	Retries   int           // attempts per WHOIS server
	Backoff   time.Duration // base delay, doubled per retry
	Fallbacks []string      // tried in order after the default (IANA-referral) lookup fails

	// UseReserved enables the upstream "reserved name" heuristics. They treat every 1-2 letter
	// and 2-3 digit label as reserved, so they are off by default; WHOIS is authoritative.
	UseReserved bool
}

// NewChecker returns a Checker wired to the real network through the given egress (nil means
// direct). Every egress needs its own Checker: RDAP rate limits are per source address, so the
// throttle state must not be shared between paths.
//
// extraRDAP adds or replaces TLD -> RDAP base URL entries on top of the built-in verified list.
// notice (optional) receives human-readable operational messages, e.g. when an RDAP server asks
// us to slow down. DNS pre-checks stay on the host's resolver: they never reach a registry.
func NewChecker(extraRDAP map[string]string, notice func(format string, args ...any), eg *egress.Egress) *Checker {
	if eg == nil {
		eg = egress.Direct()
	}
	rdap := NewRDAPClient()
	rdap.HTTP = eg.HTTPClient(10 * time.Second)
	for tld, base := range extraRDAP {
		rdap.Overrides[tld] = base
	}
	if notice != nil {
		rdap.OnThrottle = func(base string, wait time.Duration) {
			notice("RDAP 服务器 %s 返回 429 限流(出口 %s):该出口上的所有任务暂停访问它 %s 后自动重试", base, eg.Name, wait.Round(time.Second))
		}
	}
	wc := whois.NewClient()
	wc.SetTimeout(10 * time.Second)
	wc.SetDialer(eg)
	resolver := net.DefaultResolver
	withTimeout := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 5*time.Second)
	}
	return &Checker{
		RDAP: rdap.Lookup,
		LookupNS: func(d string) ([]*net.NS, error) {
			ctx, cancel := withTimeout()
			defer cancel()
			return resolver.LookupNS(ctx, d)
		},
		LookupIP: func(d string) ([]net.IP, error) {
			ctx, cancel := withTimeout()
			defer cancel()
			addrs, err := resolver.LookupIPAddr(ctx, d)
			ips := make([]net.IP, 0, len(addrs))
			for _, a := range addrs {
				ips = append(ips, a.IP)
			}
			return ips, err
		},
		LookupMX: func(d string) ([]*net.MX, error) {
			ctx, cancel := withTimeout()
			defer cancel()
			return resolver.LookupMX(ctx, d)
		},
		Whois: wc.Whois,
		HasTLS: func(d string) bool {
			// Presence probe only: we just ask "does anything serve a certificate here?" and
			// send no data, so verification is deliberately skipped (self-signed still counts).
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			conn, err := eg.DialContext(ctx, "tcp", net.JoinHostPort(d, "443"))
			if err != nil {
				return false
			}
			defer conn.Close()
			tc := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: d})
			if err := tc.HandshakeContext(ctx); err != nil {
				return false
			}
			return len(tc.ConnectionState().PeerCertificates) > 0
		},
		Retries: 2,
		Backoff: time.Second,
		Fallbacks: []string{
			"whois.verisign-grs.com:43",
			"whois.nic.li:43",
			"whois.internic.net:43",
		},
	}
}

// Check classifies a domain. It never returns StatusAvailable without an explicit RDAP or
// WHOIS signal. Every action is recorded as a Step on the returned Verdict (and reported live to
// a WithTrace callback).
func (c *Checker) Check(ctx context.Context, domain string) Verdict {
	if traceFrom(ctx) == nil {
		ctx = context.WithValue(ctx, traceKey, &tracer{})
	}
	start := time.Now()
	v := c.check(ctx, domain)
	v.Domain = domain
	step(ctx, "verdict", fmt.Sprintf("%s: %s", v.Status, v.Reason), start, v.Status != StatusUnknown)
	v.Steps = traceFrom(ctx).snapshot()
	v.DurationMS = time.Since(start).Milliseconds()
	return v
}

func (c *Checker) check(ctx context.Context, domain string) Verdict {
	var v Verdict
	if ctx.Err() != nil {
		v.Status, v.Reason, v.ErrKind = StatusUnknown, "cancelled", ErrCancelled
		return v
	}

	if c.UseReserved {
		t0 := time.Now()
		hit := reserved.IsReservedDomain(domain)
		step(ctx, "reserved", fmt.Sprintf("upstream reserved-name rules matched=%v", hit), t0, true)
		if hit {
			v.Status, v.Reason = StatusReserved, "matched reserved-name rules"
			return v
		}
	}

	t0 := time.Now()
	var found []string
	if ns, err := c.LookupNS(domain); err == nil && len(ns) > 0 {
		v.Signatures = append(v.Signatures, "DNS_NS")
		found = append(found, fmt.Sprintf("NS(%d)", len(ns)))
	}
	if ips, err := c.LookupIP(domain); err == nil && len(ips) > 0 {
		v.Signatures = append(v.Signatures, "DNS_A")
		found = append(found, fmt.Sprintf("A(%d)", len(ips)))
	}
	if mx, err := c.LookupMX(domain); err == nil && len(mx) > 0 {
		v.Signatures = append(v.Signatures, "DNS_MX")
		found = append(found, fmt.Sprintf("MX(%d)", len(mx)))
	}
	if len(v.Signatures) > 0 {
		step(ctx, "dns", "records: "+strings.Join(found, " "), t0, true)
		v.Status, v.Reason = StatusRegistered, "DNS records exist"
		return v
	}
	step(ctx, "dns", "no NS/A/MX records", t0, true)

	// RDAP is authoritative when the registry answers; WHOIS is only the fallback.
	rdapNote := ""
	if c.RDAP != nil {
		t1 := time.Now()
		res, err := c.RDAP(ctx, domain)
		detail := map[RDAPResult]string{
			RDAPFound: "registry has the domain", RDAPNotFound: "registry has no such domain",
			RDAPUnsupported: "no RDAP server for this TLD", RDAPError: "no usable answer", RDAPRateLimited: "rate limited",
		}[res]
		if err != nil {
			detail += ": " + err.Error()
		}
		step(ctx, "rdap", detail, t1, res == RDAPFound || res == RDAPNotFound)
		switch res {
		case RDAPFound:
			v.Status, v.Reason = StatusRegistered, "RDAP: registry has this domain"
			v.Signatures = append(v.Signatures, "RDAP")
			return v
		case RDAPNotFound:
			if c.probeTLS(ctx, domain) {
				v.Status, v.Reason = StatusRegistered, "RDAP says free but a TLS certificate is served"
				v.Signatures = append(v.Signatures, "SSL")
			} else {
				v.Status, v.Reason = StatusAvailable, "RDAP: registry has no such domain"
			}
			return v
		case RDAPRateLimited:
			v.Status, v.Reason, v.ErrKind = StatusUnknown, "RDAP rate limited (HTTP 429)", ErrRateLimited
			if err != nil {
				v.Reason = err.Error()
			}
			return v
		case RDAPError:
			if err != nil {
				rdapNote = err.Error()
			}
		}
	}

	text, reason, kind := c.queryWhois(ctx, domain)
	if text == "" {
		v.Status, v.Reason, v.ErrKind = StatusUnknown, reason, kind
		if rdapNote != "" {
			v.Reason = rdapNote + "; " + reason
		}
		return v
	}
	lower := strings.ToLower(text)

	switch {
	case isServiceError(lower):
		v.Status, v.Reason, v.ErrKind = StatusUnknown, "whois service error or rate limit", ErrRateLimited
	case containsAny(lower, registeredIndicators):
		v.Status, v.Reason = StatusRegistered, "whois shows registration data"
		v.Signatures = append(v.Signatures, "WHOIS")
	case containsAny(lower, reservedIndicators):
		v.Status, v.Reason = StatusReserved, "whois shows reserved status"
		v.Signatures = append(v.Signatures, "RESERVED")
	case isAvailableFromWHOIS(lower):
		if c.probeTLS(ctx, domain) {
			v.Status, v.Reason = StatusRegistered, "whois says free but a TLS certificate is served"
			v.Signatures = append(v.Signatures, "SSL")
		} else {
			v.Status, v.Reason = StatusAvailable, "whois reports no match"
		}
	case isUnavailableFromWHOIS(text) || isUnavailableFromWHOIS(lower):
		v.Status, v.Reason = StatusRegistered, "whois shows registration data"
		v.Signatures = append(v.Signatures, "WHOIS")
	default:
		v.Status, v.Reason, v.ErrKind = StatusUnknown, "whois response not recognised", ErrUnrecognised
	}
	return v
}

// probeTLS reports whether a certificate is served on :443, recording a "tls" step.
func (c *Checker) probeTLS(ctx context.Context, domain string) bool {
	if c.HasTLS == nil {
		return false
	}
	t0 := time.Now()
	has := c.HasTLS(domain)
	detail := "no certificate served on :443"
	if has {
		detail = "certificate served on :443"
	}
	step(ctx, "tls", detail, t0, true)
	return has
}

func errKindOf(err error) ErrKind {
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "timeout") || strings.Contains(s, "deadline exceeded") {
		return ErrTimeout
	}
	return ErrNetwork
}

// queryWhois tries the default (IANA referral) lookup, then each fallback server, each with
// retries and exponential backoff. It returns the first non-empty response; otherwise a reason
// and the kind of failure.
func (c *Checker) queryWhois(ctx context.Context, domain string) (text, reason string, kind ErrKind) {
	servers := append([]string{""}, c.Fallbacks...)
	retries := c.Retries
	if retries < 1 {
		retries = 1
	}
	var lastErr error
	for _, server := range servers {
		label := server
		if label == "" {
			label = "iana-referral"
		}
		for i := 0; i < retries; i++ {
			if ctx.Err() != nil {
				return "", "cancelled", ErrCancelled
			}
			t0 := time.Now()
			var res string
			var err error
			if server == "" {
				res, err = c.Whois(domain)
			} else {
				res, err = c.Whois(domain, server)
			}
			if err == nil && strings.TrimSpace(res) != "" {
				step(ctx, "whois", fmt.Sprintf("server=%s attempt=%d bytes=%d", label, i+1, len(res)), t0, true)
				return res, "", ErrNone
			}
			if err != nil {
				lastErr = err
				step(ctx, "whois", fmt.Sprintf("server=%s attempt=%d error=%v", label, i+1, err), t0, false)
			} else {
				step(ctx, "whois", fmt.Sprintf("server=%s attempt=%d empty response", label, i+1), t0, false)
			}
			if i < retries-1 && c.Backoff > 0 {
				select {
				case <-ctx.Done():
					return "", "cancelled", ErrCancelled
				case <-time.After(c.Backoff * time.Duration(1<<i)):
				}
			}
		}
	}
	if lastErr != nil {
		return "", "whois failed: " + lastErr.Error(), errKindOf(lastErr)
	}
	return "", "whois returned empty response", ErrNetwork
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
