package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"domain_scanner/internal/appsettings"
)

const (
	acct  = "0123456789abcdef0123456789abcdef"
	token = "cfat_SECRETSECRETSECRETSECRETSECRETSECRETSECRET"
)

type cfServer struct {
	srv       *httptest.Server
	checkBody atomic.Value // last domain-check request body
	regBody   atomic.Value
	regPrefer atomic.Value
	hits      atomic.Int32
	handler   func(w http.ResponseWriter, r *http.Request)
}

func newCF(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*Client, *cfServer) {
	t.Helper()
	s := &cfServer{handler: h}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/domain-check"):
			s.checkBody.Store(string(body))
		case strings.HasSuffix(r.URL.Path, "/registrations") && r.Method == "POST":
			s.regBody.Store(string(body))
			s.regPrefer.Store(r.Header.Get("Prefer"))
		}
		s.handler(w, r)
	}))
	t.Cleanup(s.srv.Close)
	c := New(func() appsettings.Cloudflare { return appsettings.Cloudflare{AccountID: acct, Token: token} })
	c.BaseURL = s.srv.URL + "/client/v4"
	c.PollEvery = 5 * time.Millisecond
	c.MaxPollWait = 300 * time.Millisecond
	return c, s
}

func TestCheckParsesRegistrableAndRejectedDomains(t *testing.T) {
	c, s := newCF(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client/v4/accounts/"+acct+"/registrar/domain-check" || r.Method != "POST" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"result":{"domains":[
			{"name":"free.com","registrable":true,"tier":"standard","pricing":{"currency":"USD","registration_cost":"10.46","renewal_cost":"10.46"}},
			{"name":"google.com","registrable":false,"tier":"standard","reason":"domain_unavailable"},
			{"name":"x.li","registrable":false,"reason":"extension_not_supported"},
			{"name":"gem.com","registrable":false,"tier":"premium","reason":"domain_premium"}]},"success":true,"errors":[],"messages":[]}`)
	})
	got, err := c.Check(context.Background(), []string{"free.com", "google.com", "x.li", "gem.com"})
	if err != nil || len(got) != 4 {
		t.Fatalf("got %d results, err %v", len(got), err)
	}
	f := got[0]
	if !f.Registrable || f.Name != "free.com" || f.Currency != "USD" || f.RegistrationCost != "10.46" || f.RenewalCost != "10.46" || f.Tier != "standard" {
		t.Fatalf("free.com = %+v", f)
	}
	if got[1].Registrable || got[1].Reason != "domain_unavailable" {
		t.Fatalf("google.com = %+v", got[1])
	}
	if !got[2].Unsupported() || got[1].Unsupported() || got[3].Unsupported() {
		t.Fatalf("Unsupported(): li=%v google=%v premium=%v", got[2].Unsupported(), got[1].Unsupported(), got[3].Unsupported())
	}
	var sent struct {
		Domains []string `json:"domains"`
	}
	json.Unmarshal([]byte(s.checkBody.Load().(string)), &sent)
	if len(sent.Domains) != 4 || sent.Domains[0] != "free.com" {
		t.Fatalf("request body = %v", s.checkBody.Load())
	}
}

func TestCheckRejectsBadBatchSizesWithoutCallingTheAPI(t *testing.T) {
	c, s := newCF(t, func(w http.ResponseWriter, r *http.Request) {})
	if _, err := c.Check(context.Background(), nil); err == nil {
		t.Fatal("an empty batch must be rejected")
	}
	many := make([]string, 21)
	for i := range many {
		many[i] = fmt.Sprintf("d%d.com", i)
	}
	if _, err := c.Check(context.Background(), many); err == nil {
		t.Fatal("more than 20 domains must be rejected")
	}
	if s.hits.Load() != 0 {
		t.Fatal("no request may be made for an invalid batch")
	}
}

func TestNotConfiguredFailsFast(t *testing.T) {
	c := New(func() appsettings.Cloudflare { return appsettings.Cloudflare{} })
	if _, err := c.Check(context.Background(), []string{"a.com"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if _, err := c.Register(context.Background(), "a.com"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("register err = %v", err)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		hdr        map[string]string
		auth, rate bool
		temporary  bool
	}{
		{"unauthorized", 403, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`, nil, true, false, false},
		{"rate limited", 429, `{"success":false,"errors":[{"code":971,"message":"slow down"}]}`, map[string]string{"Retry-After": "7"}, false, true, true},
		{"server error", 502, `bad gateway`, nil, false, false, true},
		{"bad request", 400, `{"success":false,"errors":[{"code":10001,"message":"bad domain"}]}`, nil, false, false, false},
	}
	for _, tc := range cases {
		c, _ := newCF(t, func(w http.ResponseWriter, r *http.Request) {
			for k, v := range tc.hdr {
				w.Header().Set(k, v)
			}
			w.WriteHeader(tc.status)
			fmt.Fprint(w, tc.body)
		})
		_, err := c.Check(context.Background(), []string{"a.com"})
		var ae *APIError
		if !errors.As(err, &ae) {
			t.Errorf("%s: err = %v, want *APIError", tc.name, err)
			continue
		}
		if ae.Status != tc.status || ae.IsAuth() != tc.auth || ae.IsRateLimit() != tc.rate || ae.Temporary() != tc.temporary {
			t.Errorf("%s: %+v auth=%v rate=%v temp=%v", tc.name, ae, ae.IsAuth(), ae.IsRateLimit(), ae.Temporary())
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("%s: the token leaked into the error text: %v", tc.name, err)
		}
		if tc.rate && ae.RetryAfter != 7*time.Second {
			t.Errorf("RetryAfter = %v, want 7s", ae.RetryAfter)
		}
	}
}

func TestWrongTokenIsAnAuthError(t *testing.T) {
	c, _ := newCF(t, func(w http.ResponseWriter, r *http.Request) {})
	c.Config = func() appsettings.Cloudflare {
		return appsettings.Cloudflare{AccountID: acct, Token: "cfat_wrongwrongwrongwrongwrong"}
	}
	_, err := c.Check(context.Background(), []string{"a.com"})
	var ae *APIError
	if !errors.As(err, &ae) || !ae.IsAuth() {
		t.Fatalf("err = %v, want an auth APIError", err)
	}
}

func TestRegisterSynchronousSuccess(t *testing.T) {
	c, s := newCF(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		fmt.Fprint(w, `{"success":true,"result":{"state":"succeeded","completed":true,"context":{"domain_name":"free.com","registration":{"domain_name":"free.com","status":"active","auto_renew":false}},"links":{"self":"/accounts/`+acct+`/registrar/registrations/free.com/registration-status"}}}`)
	})
	res, err := c.Register(context.Background(), "free.com")
	if err != nil || !res.Succeeded() || res.Domain != "free.com" || res.RegistrationStatus != "active" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	var sent map[string]any
	json.Unmarshal([]byte(s.regBody.Load().(string)), &sent)
	if sent["domain_name"] != "free.com" {
		t.Fatalf("body = %v", sent)
	}
	if sent["auto_renew"] == true {
		t.Fatal("auto_renew must never be switched on implicitly: it authorises recurring charges")
	}
}

func TestRegisterAsyncPollsUntilDone(t *testing.T) {
	var polls atomic.Int32
	c, s := newCF(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(202)
			fmt.Fprint(w, `{"success":true,"result":{"state":"in_progress","completed":false,"context":{"domain_name":"free.com"},"links":{"self":"/accounts/`+acct+`/registrar/registrations/free.com/registration-status"}}}`)
			return
		}
		if polls.Add(1) < 3 {
			fmt.Fprint(w, `{"success":true,"result":{"state":"in_progress","completed":false,"context":{"domain_name":"free.com"}}}`)
			return
		}
		fmt.Fprint(w, `{"success":true,"result":{"state":"succeeded","completed":true,"context":{"domain_name":"free.com","registration":{"status":"active"}}}}`)
	})
	res, err := c.Register(context.Background(), "free.com")
	if err != nil || !res.Succeeded() {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if polls.Load() != 3 {
		t.Fatalf("polled %d times, want 3", polls.Load())
	}
	if s.regPrefer.Load() != "respond-async" {
		t.Fatalf("Prefer header = %v, want respond-async", s.regPrefer.Load())
	}
}

func TestRegisterFailureCarriesTheReason(t *testing.T) {
	c, _ := newCF(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(202)
			fmt.Fprint(w, `{"success":true,"result":{"state":"in_progress","completed":false,"links":{"self":"/accounts/`+acct+`/registrar/registrations/x.com/registration-status"}}}`)
			return
		}
		fmt.Fprint(w, `{"success":true,"result":{"state":"failed","completed":true,"error":{"code":"payment_required","message":"no default payment method"}}}`)
	})
	res, err := c.Register(context.Background(), "x.com")
	if err != nil || res.Succeeded() || !res.Completed || !strings.Contains(res.ErrorMessage, "payment method") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestRegisterStillPendingAfterMaxWaitIsReportedNotFailed(t *testing.T) {
	c, _ := newCF(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(202)
		}
		fmt.Fprint(w, `{"success":true,"result":{"state":"in_progress","completed":false,"links":{"self":"/accounts/`+acct+`/registrar/registrations/x.com/registration-status"}}}`)
	})
	res, err := c.Register(context.Background(), "x.com")
	if err != nil || res.Completed || res.Succeeded() || res.State != "in_progress" {
		t.Fatalf("res=%+v err=%v; a slow registration is pending, not an error", res, err)
	}
}

func TestPollNeverFollowsALinkToAnotherHost(t *testing.T) {
	var evil atomic.Int32
	evilSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evil.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("the API token was sent to a foreign host")
		}
	}))
	defer evilSrv.Close()
	c, _ := newCF(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		fmt.Fprint(w, `{"success":true,"result":{"state":"in_progress","completed":false,"links":{"self":"`+evilSrv.URL+`/steal"}}}`)
	})
	res, err := c.Register(context.Background(), "x.com")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if evil.Load() != 0 || res.Succeeded() {
		t.Fatalf("foreign link was followed (%d hits) or result wrong: %+v", evil.Load(), res)
	}
}

func TestVerify(t *testing.T) {
	c, _ := newCF(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tokens/verify"):
			fmt.Fprint(w, `{"result":{"id":"abc","status":"active"},"success":true,"errors":[],"messages":[]}`)
		case strings.HasSuffix(r.URL.Path, "/domain-check"):
			fmt.Fprint(w, `{"result":{"domains":[{"name":"example.com","registrable":false,"reason":"domain_unavailable"}]},"success":true,"errors":[],"messages":[]}`)
		}
	})
	if err := c.Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	bad, _ := newCF(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tokens/verify") {
			fmt.Fprint(w, `{"result":{"id":"abc","status":"disabled"},"success":true}`)
			return
		}
		w.WriteHeader(403)
		fmt.Fprint(w, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`)
	})
	err := bad.Verify(context.Background())
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("a disabled token must be reported: %v", err)
	}
}
