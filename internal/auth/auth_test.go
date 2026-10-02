package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newAuth(t *testing.T, pw string, c *clock) *Auth {
	t.Helper()
	a, err := New(pw, []byte("stored-secret-0123456789abcdef"), 7*24*time.Hour, c.now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestWeakPasswordRejected(t *testing.T) {
	c := &clock{time.Unix(1_000_000, 0)}
	for _, pw := range []string{"", "short", "1234567"} {
		if _, err := New(pw, []byte("s"), time.Hour, c.now); !errors.Is(err, ErrWeakPassword) {
			t.Errorf("password %q: err = %v, want ErrWeakPassword", pw, err)
		}
	}
	if _, err := New("12345678", []byte("s"), time.Hour, c.now); err != nil {
		t.Fatalf("8 chars must be accepted: %v", err)
	}
}

func TestLoginAndVerify(t *testing.T) {
	c := &clock{time.Unix(1_000_000, 0)}
	a := newAuth(t, "correct-horse", c)
	cookie, err := a.Login("1.2.3.4", "correct-horse")
	if err != nil || !a.Verify(cookie) {
		t.Fatalf("Login/Verify failed: %v", err)
	}
	if _, err := a.Login("1.2.3.4", "wrong"); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("err = %v, want ErrBadPassword", err)
	}
}

func TestLockoutAfterFiveFailuresThenRecovers(t *testing.T) {
	c := &clock{time.Unix(1_000_000, 0)}
	a := newAuth(t, "correct-horse", c)
	for i := 0; i < 5; i++ {
		if _, err := a.Login("9.9.9.9", "nope"); !errors.Is(err, ErrBadPassword) {
			t.Fatalf("attempt %d: err = %v", i+1, err)
		}
	}
	if _, err := a.Login("9.9.9.9", "correct-horse"); !errors.Is(err, ErrLocked) {
		t.Fatalf("correct password during lockout: err = %v, want ErrLocked", err)
	}
	if _, err := a.Login("8.8.8.8", "correct-horse"); err != nil {
		t.Fatalf("other IP must not be locked: %v", err)
	}
	c.t = c.t.Add(5*time.Minute + time.Second)
	if _, err := a.Login("9.9.9.9", "correct-horse"); err != nil {
		t.Fatalf("lock should expire after 5 minutes: %v", err)
	}
}

func TestSuccessResetsFailureCounter(t *testing.T) {
	c := &clock{time.Unix(1_000_000, 0)}
	a := newAuth(t, "correct-horse", c)
	for i := 0; i < 4; i++ {
		a.Login("1.1.1.1", "bad")
	}
	a.Login("1.1.1.1", "correct-horse")
	for i := 0; i < 4; i++ {
		if _, err := a.Login("1.1.1.1", "bad"); errors.Is(err, ErrLocked) {
			t.Fatalf("counter was not reset by a successful login")
		}
	}
}

func TestTamperedAndExpiredCookiesFail(t *testing.T) {
	c := &clock{time.Unix(1_000_000, 0)}
	a := newAuth(t, "correct-horse", c)
	cookie, _ := a.Login("1.1.1.1", "correct-horse")
	for _, bad := range []string{"", "x", cookie + "a", "a" + cookie, "abc.def", "."} {
		if a.Verify(bad) {
			t.Errorf("Verify(%q) = true", bad)
		}
	}
	c.t = c.t.Add(7*24*time.Hour + time.Second)
	if a.Verify(cookie) {
		t.Fatal("expired cookie accepted")
	}
}

func TestPasswordChangeInvalidatesOldSessions(t *testing.T) {
	c := &clock{time.Unix(1_000_000, 0)}
	a := newAuth(t, "old-password", c)
	cookie, _ := a.Login("1.1.1.1", "old-password")
	b := newAuth(t, "new-password", c) // same stored secret, new password
	if b.Verify(cookie) {
		t.Fatal("session survived a password change")
	}
}

func TestRevokeInvalidatesSession(t *testing.T) {
	c := &clock{time.Unix(1_000_000, 0)}
	a := newAuth(t, "correct-horse", c)
	cookie, _ := a.Login("1.1.1.1", "correct-horse")
	a.Revoke(cookie)
	if a.Verify(cookie) {
		t.Fatal("revoked session still valid")
	}
}

func TestMiddleware(t *testing.T) {
	c := &clock{time.Unix(1_000_000, 0)}
	a := newAuth(t, "correct-horse", c)
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), "/api/health")

	do := func(path, cookie string) int {
		req := httptest.NewRequest("GET", path, nil)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	cookie, _ := a.Login("1.1.1.1", "correct-horse")
	if got := do("/api/health", ""); got != 204 {
		t.Errorf("public path: %d, want 204", got)
	}
	if got := do("/api/jobs", ""); got != 401 {
		t.Errorf("no cookie: %d, want 401", got)
	}
	if got := do("/api/jobs", "garbage"); got != 401 {
		t.Errorf("bad cookie: %d, want 401", got)
	}
	if got := do("/api/jobs", cookie); got != 204 {
		t.Errorf("good cookie: %d, want 204", got)
	}
	if got := do("/api/health/../jobs", ""); got == 204 {
		t.Errorf("path trick must not bypass auth")
	}
}

func TestCookieAttributes(t *testing.T) {
	c := &clock{time.Unix(1_000_000, 0)}
	a := newAuth(t, "correct-horse", c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/auth/login", nil)
	a.SetCookie(rec, req, "v")
	ck := rec.Result().Cookies()[0]
	if !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/" || ck.Secure {
		t.Fatalf("cookie = %+v", ck)
	}
	req.Header.Set("X-Forwarded-Proto", "https")
	rec = httptest.NewRecorder()
	a.SetCookie(rec, req, "v")
	if !rec.Result().Cookies()[0].Secure {
		t.Fatal("cookie must be Secure behind an https proxy")
	}
}
