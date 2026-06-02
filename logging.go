package main

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// setupLogging installs a JSON slog handler on the default logger and returns
// it. Output goes to stdout so the kubelet captures it and Loki ingests it;
// JSON keeps fields queryable rather than forcing regex over free text.
//
// The level is read from LOG_LEVEL (debug|info|warn|error, case-insensitive),
// defaulting to info. Set LOG_LEVEL=debug to surface health/scrape access logs
// and other high-volume detail when chasing a connectivity problem.
func setupLogging() *slog.Logger {
	level := parseLevel(os.Getenv("LOG_LEVEL"))
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	return logger
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// statusRecorder wraps http.ResponseWriter to capture the status code and the
// number of bytes written, neither of which the standard interface exposes
// after the fact. Capturing the status is the whole point of the access log:
// the 4xx/5xx responses are what signal a genuine transport problem (a
// malformed MCP body, a wrong path or method) as opposed to a tool-level
// error, which the SDK returns as a 200 with an error body.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

// WriteHeader records the status before delegating. If a handler writes a body
// without calling WriteHeader, net/http implicitly sends 200; Write below
// backfills that so the log never reports a misleading zero status.
func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// accessLog wraps an http.Handler and logs one structured line per request:
// method, path, remote address, status, response size, and duration. It is the
// connectivity/transport view — it sees requests that never reach a tool
// handler (404s, malformed bodies, probe traffic), which tool-level logging
// cannot.
//
// High-frequency infrastructure paths (/healthz liveness/readiness probes every
// 10–30s, /metrics scrapes every ~30s) are logged at debug so they don't drown
// the steady-state log; raise LOG_LEVEL=debug to see them.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		switch {
		case r.URL.Path == "/healthz" || r.URL.Path == "/metrics":
			level = slog.LevelDebug
		case rec.status >= 500:
			level = slog.LevelError
		case rec.status >= 400:
			level = slog.LevelWarn
		}

		slog.LogAttrs(r.Context(), level, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("remote", r.RemoteAddr),
			slog.Int("status", rec.status),
			slog.Int("bytes", rec.bytes),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		)

		// Log session lifecycle events so idle intervals are recoverable.
		// The SDK sets Mcp-Session-Id in the response on a successful
		// initialize; client-initiated teardown sends it in the request.
		switch r.Method {
		case http.MethodPost:
			if rec.status == http.StatusOK {
				if sid := rec.Header().Get("Mcp-Session-Id"); sid != "" {
					slog.Info("session created",
						slog.String("session_id", sid),
						slog.String("remote", r.RemoteAddr),
					)
				}
			}
		case http.MethodDelete:
			if rec.status == http.StatusOK {
				if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
					slog.Info("session terminated",
						slog.String("session_id", sid),
						slog.String("remote", r.RemoteAddr),
						slog.String("cause", "client"),
					)
				}
			}
		}
	})
}
