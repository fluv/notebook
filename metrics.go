package main

import (
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
