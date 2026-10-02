// Package server exposes the REST/SSE API consumed by the web UI.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"domain_scanner/internal/appsettings"
	"domain_scanner/internal/auth"
	"domain_scanner/internal/logbus"
	"domain_scanner/internal/notifier"
	"domain_scanner/internal/scheduler"
	"domain_scanner/internal/store"
	"domain_scanner/internal/wordlists"
)

// Narrow interfaces so the server can be tested without the real scheduler/notifier.
type Sched interface {
	Create(ctx context.Context, p scheduler.Params) (*store.Job, error)
	Pause(id int64) error
	Resume(id int64) error
	Cancel(id int64) error
	Delete(ctx context.Context, id int64) error
}

type WordlistMgr interface {
	List() ([]wordlists.Info, error)
	Save(name string, r io.Reader, maxBytes int64) (wordlists.Info, error)
}

type TelegramTester interface {
	SendTest(ctx context.Context) error
}

type Deps struct {
	Store            *store.Store
	Bus              *logbus.Bus
	Sched            Sched
	Words            WordlistMgr
	Telegram         TelegramTester
	Proxy            ProxyService
	Cloudflare       CloudflareTester
	Auth             *auth.Auth
	TelegramEnv      notifier.Config // fallback when nothing is saved in settings
	CloudflareEnv    appsettings.Cloudflare
	MaxWordlistBytes int64
	TrustProxy       bool // read the client IP from the last X-Forwarded-For entry
}

type api struct{ Deps }

const maxJSONBody = 1 << 20

// New builds the HTTP handler. Everything except the public paths needs a valid session.
func New(d Deps) http.Handler {
	a := &api{d}
	a.refreshSecrets()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("POST /api/auth/login", a.login)
	mux.HandleFunc("POST /api/auth/logout", a.logout)
	mux.HandleFunc("GET /api/auth/me", a.me)

	mux.HandleFunc("GET /api/stats", a.stats)
	mux.HandleFunc("GET /api/jobs", a.listJobs)
	mux.HandleFunc("POST /api/jobs", a.createJob)
	mux.HandleFunc("GET /api/jobs/{id}", a.getJob)
	mux.HandleFunc("DELETE /api/jobs/{id}", a.deleteJob)
	mux.HandleFunc("POST /api/jobs/{id}/{action}", a.jobAction)
	mux.HandleFunc("GET /api/results", a.listResults)
	mux.HandleFunc("GET /api/results/export", a.exportResults)
	mux.HandleFunc("GET /api/logs", a.listLogs)
	mux.HandleFunc("GET /api/logs/stream", a.streamLogs)
	mux.HandleFunc("GET /api/logs/export", a.exportLogs)
	mux.HandleFunc("GET /api/logs/components", a.logComponents)
	mux.HandleFunc("GET /api/diagnostics", a.diagnostics)
	mux.HandleFunc("GET /api/outbounds", a.listOutbounds)
	mux.HandleFunc("POST /api/outbounds", a.createOutbound)
	mux.HandleFunc("POST /api/outbounds/import", a.importOutbounds)
	mux.HandleFunc("POST /api/outbounds/test-all", a.testAllOutbounds)
	mux.HandleFunc("POST /api/outbounds/test-config", a.testOutboundConfig)
	mux.HandleFunc("POST /api/outbounds/reload", a.reloadOutbounds)
	mux.HandleFunc("GET /api/outbounds/{id}", a.getOutbound)
	mux.HandleFunc("PUT /api/outbounds/{id}", a.updateOutbound)
	mux.HandleFunc("DELETE /api/outbounds/{id}", a.deleteOutbound)
	mux.HandleFunc("POST /api/outbounds/{id}/test", a.testOutbound)
	mux.HandleFunc("GET /api/egresses", a.listEgresses)
	mux.HandleFunc("GET /api/wordlists", a.listWordlists)
	mux.HandleFunc("POST /api/wordlists", a.uploadWordlist)
	mux.HandleFunc("GET /api/settings", a.getSettings)
	mux.HandleFunc("PUT /api/settings", a.putSettings)
	mux.HandleFunc("POST /api/settings/telegram/test", a.testTelegram)
	mux.HandleFunc("POST /api/settings/cloudflare/test", a.testCloudflare)

	protected := d.Auth.Middleware(mux, "/api/health", "/api/auth/login", "/api/auth/logout", "/api/auth/me")
	return jsonErrors(a.accessLog(protected))
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// fail maps domain errors to HTTP statuses.
func (a *api) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, scheduler.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, scheduler.ErrState):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	default:
		a.Bus.Logger("http").Error("internal_error", 0, fmt.Sprintf("API 内部错误:%v", err), map[string]any{"error": err.Error()})
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeErr(w, http.StatusBadRequest, "invalid JSON body")
		}
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

func intQuery(r *http.Request, key string) int {
	v, _ := strconv.Atoi(r.URL.Query().Get(key))
	return v
}

func int64Query(r *http.Request, key string) int64 {
	v, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	return v
}

// clientIP is the address used for login rate limiting. Behind the bundled web proxy the real
// client is the last X-Forwarded-For entry (appended by that trusted hop).
func (a *api) clientIP(r *http.Request) string {
	if a.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// jsonErrors turns the mux's plain-text 404/405 responses into JSON like every other error.
func jsonErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&errWriter{ResponseWriter: w}, r)
	})
}

type errWriter struct {
	http.ResponseWriter
	swallow bool
}

func (e *errWriter) WriteHeader(code int) {
	if (code == http.StatusNotFound || code == http.StatusMethodNotAllowed) &&
		strings.HasPrefix(e.Header().Get("Content-Type"), "text/plain") {
		e.swallow = true
		e.Header().Set("Content-Type", "application/json")
		e.Header().Del("Content-Length")
		e.ResponseWriter.WriteHeader(code)
		msg := "not found"
		if code == http.StatusMethodNotAllowed {
			msg = "method not allowed"
		}
		_, _ = e.ResponseWriter.Write([]byte(`{"error":"` + msg + `"}` + "\n"))
		return
	}
	e.ResponseWriter.WriteHeader(code)
}

func (e *errWriter) Write(b []byte) (int, error) {
	if e.swallow {
		return len(b), nil
	}
	return e.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush (needed for SSE).
func (e *errWriter) Unwrap() http.ResponseWriter { return e.ResponseWriter }
