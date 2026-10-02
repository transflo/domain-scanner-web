package domain

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeNet builds a Checker whose network layer is fully stubbed.
type fakeNet struct {
	ns        []*net.NS
	ip        []net.IP
	mx        []*net.MX
	whois     func(domain string, servers ...string) (string, error)
	tls       bool
	whoisCall int
}

func (f *fakeNet) checker() *Checker {
	return &Checker{
		LookupNS: func(string) ([]*net.NS, error) { return f.ns, nil },
		LookupIP: func(string) ([]net.IP, error) { return f.ip, nil },
		LookupMX: func(string) ([]*net.MX, error) { return f.mx, nil },
		Whois: func(d string, s ...string) (string, error) {
			f.whoisCall++
			return f.whois(d, s...)
		},
		HasTLS:    func(string) bool { return f.tls },
		Retries:   2,
		Backoff:   0,
		Fallbacks: []string{"a.example:43", "b.example:43"},
	}
}

func whoisReturns(text string) func(string, ...string) (string, error) {
	return func(string, ...string) (string, error) { return text, nil }
}

func TestCheckerDNSHitMeansRegisteredWithoutWhois(t *testing.T) {
	f := &fakeNet{ns: []*net.NS{{Host: "ns1.example."}}, whois: whoisReturns("should not be used")}
	v := f.checker().Check(context.Background(), "taken.li")
	if v.Status != StatusRegistered {
		t.Fatalf("status = %s, want registered", v.Status)
	}
	if !contains(v.Signatures, "DNS_NS") {
		t.Fatalf("signatures = %v, want DNS_NS", v.Signatures)
	}
	if f.whoisCall != 0 {
		t.Fatalf("whois called %d times, want 0", f.whoisCall)
	}
}

func TestCheckerWhoisNoMatchIsAvailable(t *testing.T) {
	f := &fakeNet{whois: whoisReturns(`No match for "FREE123.LI"`)}
	v := f.checker().Check(context.Background(), "free123.li")
	if v.Status != StatusAvailable {
		t.Fatalf("status = %s (%s), want available", v.Status, v.Reason)
	}
}

func TestCheckerWhoisRegistrarIsRegistered(t *testing.T) {
	f := &fakeNet{whois: whoisReturns("Domain: x.li\nRegistrar: Example Inc\n")}
	v := f.checker().Check(context.Background(), "x.li")
	if v.Status != StatusRegistered || !contains(v.Signatures, "WHOIS") {
		t.Fatalf("got %s %v, want registered with WHOIS", v.Status, v.Signatures)
	}
}

func TestCheckerRateLimitIsUnknownNotAvailable(t *testing.T) {
	for _, text := range []string{
		"Error: too many requests, slow down",
		"The domain name search is temporarily unavailable. Please try again later.",
		"Query limit exceeded: no match for yet",
	} {
		f := &fakeNet{whois: whoisReturns(text)}
		v := f.checker().Check(context.Background(), "free.li")
		if v.Status != StatusUnknown {
			t.Errorf("text %q => %s, want unknown", text, v.Status)
		}
	}
}

func TestCheckerAllWhoisErrorsIsUnknown(t *testing.T) {
	f := &fakeNet{whois: func(string, ...string) (string, error) { return "", errors.New("dial timeout") }}
	v := f.checker().Check(context.Background(), "free.li")
	if v.Status != StatusUnknown {
		t.Fatalf("status = %s, want unknown", v.Status)
	}
	// default server + 2 fallbacks, 2 retries each
	if f.whoisCall != 6 {
		t.Fatalf("whois calls = %d, want 6", f.whoisCall)
	}
}

func TestCheckerFallbackServerRecovers(t *testing.T) {
	calls := 0
	f := &fakeNet{whois: func(_ string, s ...string) (string, error) {
		calls++
		if len(s) == 0 {
			return "", errors.New("iana lookup failed")
		}
		return "No match for \"x.li\"", nil
	}}
	v := f.checker().Check(context.Background(), "x.li")
	if v.Status != StatusAvailable {
		t.Fatalf("status = %s, want available via fallback", v.Status)
	}
}

func TestCheckerTLSOverridesWhoisAvailable(t *testing.T) {
	f := &fakeNet{whois: whoisReturns("No match for x.li"), tls: true}
	v := f.checker().Check(context.Background(), "x.li")
	if v.Status != StatusRegistered || !contains(v.Signatures, "SSL") {
		t.Fatalf("got %s %v, want registered with SSL", v.Status, v.Signatures)
	}
}

func TestCheckerReservedRulesOptIn(t *testing.T) {
	f := &fakeNet{whois: whoisReturns("No match for www.li")}
	c := f.checker()
	c.UseReserved = true
	v := c.Check(context.Background(), "www.li")
	if v.Status != StatusReserved {
		t.Fatalf("status = %s, want reserved", v.Status)
	}
	if f.whoisCall != 0 {
		t.Fatalf("network used for reserved name: %d whois calls", f.whoisCall)
	}
}

func TestCheckerShortNamesNotSkippedByDefault(t *testing.T) {
	// upstream heuristics treat 2-3 digit labels as reserved; the default must leave them to WHOIS
	f := &fakeNet{whois: whoisReturns("No match for 123.li")}
	v := f.checker().Check(context.Background(), "123.li")
	if v.Status != StatusAvailable {
		t.Fatalf("status = %s, want available", v.Status)
	}
}

func TestCheckerWhoisReservedStatus(t *testing.T) {
	f := &fakeNet{whois: whoisReturns("Domain: x.li\nStatus: reserved\n")}
	v := f.checker().Check(context.Background(), "x.li")
	if v.Status != StatusReserved {
		t.Fatalf("status = %s, want reserved", v.Status)
	}
}

func TestCheckerCancelledContextIsUnknown(t *testing.T) {
	f := &fakeNet{whois: whoisReturns("No match")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v := f.checker().Check(ctx, "x.li")
	if v.Status != StatusUnknown || !strings.Contains(v.Reason, "cancelled") {
		t.Fatalf("got %s %q, want unknown/cancelled", v.Status, v.Reason)
	}
}

func TestCheckerUnrecognisedWhoisIsUnknown(t *testing.T) {
	f := &fakeNet{whois: whoisReturns("lorem ipsum dolor")}
	v := f.checker().Check(context.Background(), "x.li")
	if v.Status != StatusUnknown {
		t.Fatalf("status = %s, want unknown", v.Status)
	}
}

func TestCheckerBackoffIsApplied(t *testing.T) {
	f := &fakeNet{whois: func(string, ...string) (string, error) { return "", errors.New("x") }}
	c := f.checker()
	c.Backoff = 5 * time.Millisecond
	c.Fallbacks = nil
	start := time.Now()
	c.Check(context.Background(), "x.li")
	if time.Since(start) < 5*time.Millisecond {
		t.Fatalf("expected at least one backoff sleep")
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
