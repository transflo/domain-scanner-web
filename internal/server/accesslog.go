package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach Flush (needed for the SSE log stream).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// accessLog records every API request: method, path (never the body or secrets), status,
// duration and client address. Polling GETs are debug noise, changes are info, client and server
// errors are warnings/errors. The health check and the long-lived log stream are not logged.
func (a *api) accessLog(next http.Handler) http.Handler {
	lg := a.Bus.Logger("http")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" || r.URL.Path == "/api/logs/stream" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		level := "debug"
		switch {
		case rec.status >= 500:
			level = "error"
		case rec.status >= 400:
			level = "warn"
		case r.Method != http.MethodGet && r.Method != http.MethodHead:
			level = "info"
		}
		ip := a.clientIP(r)
		lg.Emit(level, "request", 0, fmt.Sprintf("%s %s → %d(%dms)", r.Method, r.URL.Path, rec.status, time.Since(start).Milliseconds()),
			map[string]any{"method": r.Method, "path": r.URL.Path, "status": rec.status, "duration_ms": time.Since(start),
				"ip": ip, "bytes": rec.bytes, "ua": clip(strings.TrimSpace(r.UserAgent()), 80)})
	})
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
