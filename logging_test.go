package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		" info ":  slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"":        slog.LevelInfo,
		"bogus":   slog.LevelInfo,
	}
	for in, want := range cases {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

// A handler that writes a body without calling WriteHeader must be recorded as
// 200, not the zero value — otherwise the access log reports a misleading
// status for every implicit-200 response.
func TestStatusRecorderImplicit200(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	if _, err := rec.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if rec.status != http.StatusOK {
		t.Errorf("implicit status = %d, want %d", rec.status, http.StatusOK)
	}
	if rec.bytes != 5 {
		t.Errorf("bytes = %d, want 5", rec.bytes)
	}
}

func TestStatusRecorderExplicitStatus(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	rec.WriteHeader(http.StatusNotFound)
	if rec.status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.status, http.StatusNotFound)
	}
}

// accessLog must capture a downstream handler's status code so non-2xx
// transport failures are visible in the log.
func TestAccessLogCapturesStatus(t *testing.T) {
	h := accessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rr.Code != http.StatusBadGateway {
		t.Errorf("downstream status = %d, want %d", rr.Code, http.StatusBadGateway)
	}
}
