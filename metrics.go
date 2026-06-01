package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	toolCallsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "notebook",
		Name:      "tool_calls_total",
		Help:      "Total number of MCP tool calls, partitioned by tool name and outcome.",
	}, []string{"tool", "status"})

	toolDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "notebook",
		Name:      "tool_duration_seconds",
		Help:      "Latency of MCP tool calls in seconds.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"tool"})
)

// metricsHandler returns the default prometheus registry handler.
func metricsHandler() http.Handler {
	return promhttp.Handler()
}

// recordCall increments the tool call counter and records the call duration.
// Call at the end of each tool handler, passing the tool name, start time,
// and the error returned (nil → status="ok", non-nil → status="error").
func recordCall(tool string, start time.Time, err error) {
	status := "ok"
	if err != nil {
		status = "error"
	}
	toolCallsTotal.WithLabelValues(tool, status).Inc()
	toolDurationSeconds.WithLabelValues(tool).Observe(time.Since(start).Seconds())
}

// observe records metrics for a tool call (via recordCall) and emits one
// structured log line carrying the outcome and any tool-specific context the
// handler supplies as attrs (namespace, id, result counts, …). Successful
// calls log at info so usage is traceable per-call; failures log at error with
// the message attached, which is what turns "3 appends errored" in the metrics
// into "which namespace, what error" in the log.
//
// It is the tool/usage view, paired with the HTTP access log's transport view.
func observe(tool string, start time.Time, err error, attrs ...slog.Attr) {
	recordCall(tool, start, err)

	attrs = append(attrs,
		slog.String("tool", tool),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
	)
	if err != nil {
		attrs = append(attrs, slog.String("status", "error"), slog.String("error", err.Error()))
		slog.LogAttrs(context.Background(), slog.LevelError, "tool call", attrs...)
		return
	}
	attrs = append(attrs, slog.String("status", "ok"))
	slog.LogAttrs(context.Background(), slog.LevelInfo, "tool call", attrs...)
}
