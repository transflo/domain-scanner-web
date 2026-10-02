// Package auth implements the mandatory single-password login: HMAC-signed session cookies,
// per-IP brute-force lockout and server-side revocation on logout.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	CookieName  = "ds_session"
	minPassword = 8
	maxFails    = 5
	lockFor     = 5 * time.Minute
	maxTracked  = 10000
)

var (
	ErrWeakPassword = errors.New("ADMIN_PASSWORD must be at least 8 characters")
	ErrBadPassword  = errors.New("incorrect password")
	ErrLocked       = errors.New("too many failed attempts, try again later")
)

type attempt struct {
	fails       int
	lockedUntil time.Time
}

type Auth struct {
	password []byte
	key      []byte
	ttl      time.Duration
	now      func() time.Time

	mu       sync.Mutex
	attempts map[string]*attempt
	revoked  map[string]time.Time // token -> expiry
}

// New builds an Auth. stored is a random secret persisted across restarts; the signing key is
// derived from it and the password, so changing the password invalidates every old session.
func New(password string, stored []byte, ttl time.Duration, now func() time.Time) (*Auth, error) {
	if len(password) < minPassword {
		return nil, ErrWeakPassword
	}
	if now == nil {
		now = time.Now
	}
	return &Auth{
		password: []byte(password),
		key:      DeriveSecret(stored, password),
		ttl:      ttl,
		now:      now,
		attempts: map[string]*attempt{},
		revoked:  map[string]time.Time{},
	}, nil
}

// DeriveSecret mixes the stored secret with the password into a signing key.
func DeriveSecret(stored []byte, password string) []byte {
	m := hmac.New(sha256.New, stored)
	m.Write([]byte("session-key:"))
	m.Write([]byte(password))
	return m.Sum(nil)
}

// Login checks the password for the client ip and returns a signed session value.
func (a *Auth) Login(ip, password string) (string, error) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()

	at := a.attempts[ip]
	if at != nil && now.Before(at.lockedUntil) {
		return "", ErrLocked
	}
	if subtle.ConstantTimeCompare([]byte(password), a.password) != 1 {
		if at == nil {
			if len(a.attempts) >= maxTracked {
				a.pruneAttempts(now)
			}
			at = &attempt{}
			a.attempts[ip] = at
		}
		at.fails++
		if at.fails >= maxFails {
			at.fails = 0
			at.lockedUntil = now.Add(lockFor)
		}
		return "", ErrBadPassword
	}
	delete(a.attempts, ip)
	return a.sign(now.Add(a.ttl)), nil
}

func (a *Auth) pruneAttempts(now time.Time) {
	for ip, at := range a.attempts {
		if now.After(at.lockedUntil) {
			delete(a.attempts, ip)
		}
	}
}

func (a *Auth) sign(expiry time.Time) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(expiry.Unix()))
	payload := base64.RawURLEncoding.EncodeToString(b[:])
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Verify reports whether value is an unexpired, untampered, unrevoked session.
func (a *Auth) Verify(value string) bool {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok || payload == "" || sig == "" {
		return false
	}
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(payload))
	want := base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || len(raw) != 8 {
		return false
	}
	expiry := time.Unix(int64(binary.BigEndian.Uint64(raw)), 0)
	now := a.now()
	if !now.Before(expiry) {
		return false
	}
	a.mu.Lock()
	_, revoked := a.revoked[value]
	a.mu.Unlock()
	return !revoked
}

// Revoke invalidates a session server-side (logout).
func (a *Auth) Revoke(value string) {
	payload, _, ok := strings.Cut(value, ".")
	if !ok {
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || len(raw) != 8 {
		return
	}
	expiry := time.Unix(int64(binary.BigEndian.Uint64(raw)), 0)
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for tok, exp := range a.revoked { // forget revocations that expired anyway
		if !now.Before(exp) {
			delete(a.revoked, tok)
		}
	}
	a.revoked[value] = expiry
}

// SetCookie writes the session cookie. Secure is set when the request arrived over https
// (directly or via a proxy announcing it).
func (a *Auth) SetCookie(w http.ResponseWriter, r *http.Request, value string) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		MaxAge: int(a.ttl.Seconds()),
	})
}

// ClearCookie expires the session cookie.
func (a *Auth) ClearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		MaxAge: -1,
	})
}

// Authenticated reports whether the request carries a valid session.
func (a *Auth) Authenticated(r *http.Request) bool {
	c, err := r.Cookie(CookieName)
	return err == nil && a.Verify(c.Value)
}

// Middleware rejects requests without a valid session with 401, except exact public paths.
func (a *Auth) Middleware(next http.Handler, public ...string) http.Handler {
	pub := map[string]bool{}
	for _, p := range public {
		pub[p] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pub[r.URL.Path] || a.Authenticated(r) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	})
}
