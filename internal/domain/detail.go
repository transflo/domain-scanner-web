package domain

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"time"

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

// NewChecker returns a Checker wired to the real network.
func NewChecker() *Checker {
	wc := whois.NewClient()
	wc.SetTimeout(10 * time.Second)
	resolver := net.DefaultResolver
	withTimeout := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 5*time.Second)
	}
	return &Checker{
		RDAP: NewRDAPClient().Lookup,
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
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", d+":443",
				&tls.Config{InsecureSkipVerify: true})
			if err != nil {
				return false
			}
			defer conn.Close()
			return len(conn.ConnectionState().PeerCertificates) > 0
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

// Check classifies a domain. It never returns StatusAvailable without an explicit WHOIS signal.
func (c *Checker) Check(ctx context.Context, domain string) Verdict {
	v := Verdict{Domain: domain}
	if ctx.Err() != nil {
		v.Status, v.Reason = StatusUnknown, "cancelled"
		return v
	}

	if c.UseReserved && reserved.IsReservedDomain(domain) {
		v.Status, v.Reason = StatusReserved, "matched reserved-name rules"
		return v
	}

	if ns, err := c.LookupNS(domain); err == nil && len(ns) > 0 {
		v.Signatures = append(v.Signatures, "DNS_NS")
	}
	if ips, err := c.LookupIP(domain); err == nil && len(ips) > 0 {
		v.Signatures = append(v.Signatures, "DNS_A")
	}
	if mx, err := c.LookupMX(domain); err == nil && len(mx) > 0 {
		v.Signatures = append(v.Signatures, "DNS_MX")
	}
	if len(v.Signatures) > 0 {
		v.Status, v.Reason = StatusRegistered, "DNS records exist"
		return v
	}

	// RDAP is authoritative when the registry answers; WHOIS is only the fallback.
	rdapNote := ""
	if c.RDAP != nil {
		res, err := c.RDAP(ctx, domain)
		switch res {
		case RDAPFound:
			v.Status, v.Reason = StatusRegistered, "RDAP: registry has this domain"
			v.Signatures = append(v.Signatures, "RDAP")
			return v
		case RDAPNotFound:
			if c.HasTLS != nil && c.HasTLS(domain) {
				v.Status, v.Reason = StatusRegistered, "RDAP says free but a TLS certificate is served"
				v.Signatures = append(v.Signatures, "SSL")
			} else {
				v.Status, v.Reason = StatusAvailable, "RDAP: registry has no such domain"
			}
			return v
		case RDAPError:
			if err != nil {
				rdapNote = err.Error()
			}
		}
	}

	text, reason := c.queryWhois(ctx, domain)
	if text == "" {
		v.Status, v.Reason = StatusUnknown, reason
		if rdapNote != "" {
			v.Reason = rdapNote + "; " + reason
		}
		return v
	}
	lower := strings.ToLower(text)

	switch {
	case isServiceError(lower):
		v.Status, v.Reason = StatusUnknown, "whois service error or rate limit"
	case containsAny(lower, registeredIndicators):
		v.Status, v.Reason = StatusRegistered, "whois shows registration data"
		v.Signatures = append(v.Signatures, "WHOIS")
	case containsAny(lower, reservedIndicators):
		v.Status, v.Reason = StatusReserved, "whois shows reserved status"
		v.Signatures = append(v.Signatures, "RESERVED")
	case isAvailableFromWHOIS(lower):
		if c.HasTLS != nil && c.HasTLS(domain) {
			v.Status, v.Reason = StatusRegistered, "whois says free but a TLS certificate is served"
			v.Signatures = append(v.Signatures, "SSL")
		} else {
			v.Status, v.Reason = StatusAvailable, "whois reports no match"
		}
	case isUnavailableFromWHOIS(text) || isUnavailableFromWHOIS(lower):
		v.Status, v.Reason = StatusRegistered, "whois shows registration data"
		v.Signatures = append(v.Signatures, "WHOIS")
	default:
		v.Status, v.Reason = StatusUnknown, "whois response not recognised"
	}
	return v
}

// queryWhois tries the default (IANA referral) lookup, then each fallback server, each with
// retries and exponential backoff. It returns the first non-empty response.
func (c *Checker) queryWhois(ctx context.Context, domain string) (text, reason string) {
	servers := append([]string{""}, c.Fallbacks...)
	retries := c.Retries
	if retries < 1 {
		retries = 1
	}
	var lastErr error
	for _, server := range servers {
		for i := 0; i < retries; i++ {
			if ctx.Err() != nil {
				return "", "cancelled"
			}
			var res string
			var err error
			if server == "" {
				res, err = c.Whois(domain)
			} else {
				res, err = c.Whois(domain, server)
			}
			if err == nil && strings.TrimSpace(res) != "" {
				return res, ""
			}
			if err != nil {
				lastErr = err
			}
			if i < retries-1 && c.Backoff > 0 {
				select {
				case <-ctx.Done():
					return "", "cancelled"
				case <-time.After(c.Backoff * time.Duration(1<<i)):
				}
			}
		}
	}
	if lastErr != nil {
		return "", "whois failed: " + lastErr.Error()
	}
	return "", "whois returned empty response"
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
