package main

import (
	"context"
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

// A POST /mcp that returns Mcp-Session-Id in the response must emit a
// "session created" log. A POST without the header must not.
func TestSessionCreatedLog(t *testing.T) {
	const sid = "TESTSESSIONID"

	withSession := accessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Mcp-Session-Id", sid)
		w.WriteHeader(http.StatusOK)
	}))
	withoutSession := accessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	var logged []slog.Record
	logger := slog.New(recordHandler{&logged})
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	rr := httptest.NewRecorder()
	withSession.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if !containsMsg(logged, "session created") {
		t.Error("expected 'session created' log, got none")
	}

	logged = logged[:0]
	rr = httptest.NewRecorder()
	withoutSession.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if containsMsg(logged, "session created") {
		t.Error("unexpected 'session created' log on response without session header")
	}
}

// A DELETE /mcp that carries Mcp-Session-Id in the request and returns 200
// must emit a "session terminated" log.
func TestSessionTerminatedLog(t *testing.T) {
	const sid = "TESTSESSIONID"

	h := accessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	var logged []slog.Record
	logger := slog.New(recordHandler{&logged})
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set("Mcp-Session-Id", sid)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !containsMsg(logged, "session terminated") {
		t.Error("expected 'session terminated' log, got none")
	}
}

// recordHandler is a minimal slog.Handler that collects records for test assertions.
type recordHandler struct{ records *[]slog.Record }

func (h recordHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h recordHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r)
	return nil
}
func (h recordHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h recordHandler) WithGroup(_ string) slog.Handler      { return h }

func containsMsg(records []slog.Record, msg string) bool {
	for _, r := range records {
		if r.Message == msg {
			return true
		}
	}
	return false
}
