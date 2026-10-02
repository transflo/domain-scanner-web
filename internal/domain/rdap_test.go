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
		switch name {
		case "taken.li":
			w.Header().Set("Content-Type", "application/rdap+json")
			fmt.Fprint(w, `{"objectClassName":"domain","ldhName":"taken.li"}`)
		case "free.li":
			w.WriteHeader(404)
			fmt.Fprint(w, `{"errorCode":404}`)
		case "limited.li":
			w.WriteHeader(429)
		case "broken.li":
			w.WriteHeader(503)
		case "weird.li":
			w.WriteHeader(400)
		default:
			w.WriteHeader(404)
		}
	})
	mux.HandleFunc("/rdap-com/domain/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	f.client = NewRDAPClient()
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
		{"limited.li", RDAPError, true},          // 429
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
