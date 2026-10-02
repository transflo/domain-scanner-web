package domain

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- Checker integration with an injected RDAP function ----

func TestCheckerRDAPNotFoundIsAvailableWithoutWhois(t *testing.T) {
	f := &fakeNet{whois: func(string, ...string) (string, error) { return "", errors.New("whois refused") }}
	c := f.checker()
	c.RDAP = func(context.Context, string) (RDAPResult, error) { return RDAPNotFound, nil }
	v := c.Check(context.Background(), "26.li")
	if v.Status != StatusAvailable {
		t.Fatalf("status = %s (%s), want available", v.Status, v.Reason)
	}
	if f.whoisCall != 0 {
		t.Fatalf("whois used although RDAP answered: %d calls", f.whoisCall)
	}
}

func TestCheckerRDAPNotFoundButTLSMeansRegistered(t *testing.T) {
	f := &fakeNet{whois: whoisReturns("unused"), tls: true}
	c := f.checker()
	c.RDAP = func(context.Context, string) (RDAPResult, error) { return RDAPNotFound, nil }
	v := c.Check(context.Background(), "x.li")
	if v.Status != StatusRegistered || !contains(v.Signatures, "SSL") {
		t.Fatalf("got %s %v, want registered with SSL", v.Status, v.Signatures)
	}
}

func TestCheckerRDAPFoundIsRegistered(t *testing.T) {
	f := &fakeNet{whois: whoisReturns("unused")}
	c := f.checker()
	c.RDAP = func(context.Context, string) (RDAPResult, error) { return RDAPFound, nil }
	v := c.Check(context.Background(), "nic.li")
	if v.Status != StatusRegistered || !contains(v.Signatures, "RDAP") || f.whoisCall != 0 {
		t.Fatalf("got %s %v (whois calls %d)", v.Status, v.Signatures, f.whoisCall)
	}
}

func TestCheckerRDAPErrorFallsBackToWhois(t *testing.T) {
	f := &fakeNet{whois: whoisReturns(`No match for "X.LI"`)}
	c := f.checker()
	c.RDAP = func(context.Context, string) (RDAPResult, error) { return RDAPError, errors.New("429") }
	if v := c.Check(context.Background(), "x.li"); v.Status != StatusAvailable || f.whoisCall == 0 {
		t.Fatalf("got %s with %d whois calls, want fallback to whois", v.Status, f.whoisCall)
	}
}

func TestCheckerRDAPUnsupportedTLDFallsBackToWhois(t *testing.T) {
	f := &fakeNet{whois: whoisReturns("Registrar: Someone")}
	c := f.checker()
	c.RDAP = func(context.Context, string) (RDAPResult, error) { return RDAPUnsupported, nil }
	if v := c.Check(context.Background(), "x.zz"); v.Status != StatusRegistered {
		t.Fatalf("got %s, want registered via whois", v.Status)
	}
}

func TestCheckerRDAPErrorAndWhoisErrorIsUnknown(t *testing.T) {
	f := &fakeNet{whois: func(string, ...string) (string, error) { return "", errors.New("refused") }}
	c := f.checker()
	c.RDAP = func(context.Context, string) (RDAPResult, error) { return RDAPError, errors.New("rdap 503") }
	if v := c.Check(context.Background(), "x.li"); v.Status != StatusUnknown {
		t.Fatalf("got %s, want unknown (never available without evidence)", v.Status)
	}
}

func TestCheckerDNSHitSkipsRDAP(t *testing.T) {
	f := &fakeNet{ip: []net.IP{net.IPv4(1, 2, 3, 4)}, whois: whoisReturns("unused")}
	called := false
	c := f.checker()
	c.RDAP = func(context.Context, string) (RDAPResult, error) { called = true; return RDAPNotFound, nil }
	if v := c.Check(context.Background(), "x.li"); v.Status != StatusRegistered || called {
		t.Fatalf("got %s, rdap called=%v; DNS evidence must short-circuit", v.Status, called)
	}
}

// ---- RDAPClient against a fake IANA bootstrap + RDAP server ----

type rdapFixture struct {
	srv           *httptest.Server
	bootstrapHits atomic.Int32
	client        *RDAPClient
}

func newRDAPFixture(t *testing.T) *rdapFixture {
	f := &rdapFixture{}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("/bootstrap.json", func(w http.ResponseWriter, r *http.Request) {
		f.bootstrapHits.Add(1)
		fmt.Fprintf(w, `{"services":[[["li","ch"],["%s/rdap-ch/"]],[["com"],["%s/rdap-com"]]]}`, f.srv.URL, f.srv.URL)
	})
	mux.HandleFunc("/rdap-ch/domain/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/rdap-ch/domain/")
		if r.Header.Get("Accept") != "application/rdap+json" {
			http.Error(w, "bad accept", 406)
			return
		}
		switch {
		case strings.HasPrefix(name, "taken."): // any TLD routed here counts as registered
			w.Header().Set("Content-Type", "application/rdap+json")
			fmt.Fprintf(w, `{"objectClassName":"domain","ldhName":%q}`, name)
		case name == "free.li":
			w.WriteHeader(404)
			fmt.Fprint(w, `{"errorCode":404}`)
		case name == "limited.li":
			w.WriteHeader(429)
		case name == "broken.li":
			w.WriteHeader(503)
		case name == "weird.li":
			w.WriteHeader(400)
		default:
			w.WriteHeader(404)
		}
	})
	mux.HandleFunc("/rdap-com/domain/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	f.client = NewRDAPClient()
	f.client.Overrides = map[string]string{} // tests must not touch the real servers
	f.client.RetryDelay = time.Millisecond
	f.client.BootstrapURL = f.srv.URL + "/bootstrap.json"
	return f
}

func TestRDAPClientClassifiesResponses(t *testing.T) {
	f := newRDAPFixture(t)
	ctx := context.Background()
	cases := []struct {
		domain  string
		want    RDAPResult
		wantErr bool
	}{
		{"taken.li", RDAPFound, false},
		{"free.li", RDAPNotFound, false},
		{"FREE.LI", RDAPNotFound, false},         // case-insensitive
		{"limited.li", RDAPRateLimited, true},    // 429
		{"broken.li", RDAPError, true},           // 5xx
		{"weird.li", RDAPError, true},            // 400
		{"anything.com", RDAPNotFound, false},    // second bootstrap entry, URL without trailing slash
		{"x.unknowntld", RDAPUnsupported, false}, // TLD absent from bootstrap
	}
	for _, c := range cases {
		got, err := f.client.Lookup(ctx, c.domain)
		if got != c.want || (err != nil) != c.wantErr {
			t.Errorf("Lookup(%q) = %v, %v; want %v (err=%v)", c.domain, got, err, c.want, c.wantErr)
		}
	}
}

func TestRDAPDefaultsCoverTLDsMissingFromIANABootstrap(t *testing.T) {
	c := NewRDAPClient()
	for _, tld := range []string{"li", "ch", "de", "nl", "fr", "cz", "io", "ai", "pl"} {
		if base := c.Overrides[tld]; !strings.HasPrefix(base, "https://") || !strings.HasSuffix(base, "/") {
			t.Errorf("default RDAP server for .%s = %q, want an https URL ending in /", tld, base)
		}
	}
}

func TestRDAPOverridesWinAndSkipBootstrap(t *testing.T) {
	f := newRDAPFixture(t)
	// .de is not in the fake bootstrap; the override must be used without fetching it at all.
	f.client.Overrides = map[string]string{"de": f.srv.URL + "/rdap-ch/"}
	got, err := f.client.Lookup(context.Background(), "taken.de")
	if got != RDAPFound || err != nil {
		t.Fatalf("override lookup = %v, %v; want Found", got, err)
	}
	if n := f.bootstrapHits.Load(); n != 0 {
		t.Fatalf("bootstrap fetched %d times although an override covered the TLD", n)
	}
	// an override also beats a bootstrap entry for the same TLD
	// The bootstrap routes .com to /rdap-com/ (always 404); the override routes it to /rdap-ch/
	// (taken.* is 200), so Found proves the override beat the bootstrap entry.
	f.client.Overrides["com"] = f.srv.URL + "/rdap-ch/"
	if got, _ := f.client.Lookup(context.Background(), "taken.com"); got != RDAPFound {
		t.Fatalf("override should take precedence over bootstrap, got %v", got)
	}
}

// rateLimitFixture serves RDAP for one TLD and answers 429 for the first `limited` requests.
func rateLimitFixture(t *testing.T, limited int32, retryAfter string) (*RDAPClient, *atomic.Int32) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= limited {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, `{"objectClassName":"domain"}`)
	}))
	t.Cleanup(srv.Close)
	c := NewRDAPClient()
	c.Overrides = map[string]string{"li": srv.URL + "/"}
	c.RetryDelay = 5 * time.Millisecond
	return c, &hits
}

func TestRDAPClientRetriesAfter429(t *testing.T) {
	c, hits := rateLimitFixture(t, 1, "0")
	got, err := c.Lookup(context.Background(), "taken.li")
	if got != RDAPFound || err != nil {
		t.Fatalf("got %v, %v; want Found after one retry", got, err)
	}
	if hits.Load() != 2 {
		t.Fatalf("requests = %d, want 2", hits.Load())
	}
}

func TestRDAPClientGivesUpWithRateLimitedResult(t *testing.T) {
	c, hits := rateLimitFixture(t, 100, "")
	got, err := c.Lookup(context.Background(), "taken.li")
	if got != RDAPRateLimited || err == nil {
		t.Fatalf("got %v, %v; want RDAPRateLimited with an error", got, err)
	}
	if hits.Load() != 3 {
		t.Fatalf("requests = %d, want 3 (1 try + 2 retries)", hits.Load())
	}
}

func TestRDAPClientSpacesRequestsPerServer(t *testing.T) {
	c, _ := rateLimitFixture(t, 0, "")
	c.MinInterval = 40 * time.Millisecond
	start := time.Now()
	done := make(chan struct{}, 5)
	for i := 0; i < 5; i++ {
		go func() { c.Lookup(context.Background(), "taken.li"); done <- struct{}{} }()
	}
	for i := 0; i < 5; i++ {
		<-done
	}
	if el := time.Since(start); el < 4*40*time.Millisecond {
		t.Fatalf("5 concurrent lookups finished in %v, want >= 160ms of spacing", el)
	}
}

func TestCheckerRDAPRateLimitedIsUnknownWithoutWhois(t *testing.T) {
	f := &fakeNet{whois: whoisReturns(`No match for "X.LI"`)}
	c := f.checker()
	c.RDAP = func(context.Context, string) (RDAPResult, error) {
		return RDAPRateLimited, errors.New("rdap: HTTP 429")
	}
	v := c.Check(context.Background(), "x.li")
	if v.Status != StatusUnknown || !strings.Contains(v.Reason, "429") {
		t.Fatalf("got %s %q, want unknown mentioning the rate limit", v.Status, v.Reason)
	}
	if f.whoisCall != 0 {
		t.Fatalf("whois consulted after an RDAP rate limit (%d calls); it cannot give a better answer", f.whoisCall)
	}
}

func TestRDAPClientCachesBootstrap(t *testing.T) {
	f := newRDAPFixture(t)
	for i := 0; i < 5; i++ {
		f.client.Lookup(context.Background(), "taken.li")
	}
	if n := f.bootstrapHits.Load(); n != 1 {
		t.Fatalf("bootstrap fetched %d times, want 1", n)
	}
}

func TestRDAPClientBootstrapFailureIsErrorNotUnsupported(t *testing.T) {
	c := NewRDAPClient()
	c.Overrides = nil
	c.BootstrapURL = "http://127.0.0.1:1/never"
	c.HTTP.Timeout = 500 * time.Millisecond
	got, err := c.Lookup(context.Background(), "x.li")
	if got != RDAPError || err == nil {
		t.Fatalf("got %v, %v; a failed bootstrap must be an error so WHOIS fallback and unknown logic apply", got, err)
	}
}

func TestRDAPClientHonoursContext(t *testing.T) {
	f := newRDAPFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := f.client.Lookup(ctx, "taken.li"); got != RDAPError || err == nil {
		t.Fatalf("cancelled ctx: got %v, %v", got, err)
	}
}
