// Notebook MCP server: namespaced append-only JSONL storage with ULID IDs,
// tombstone-based deletion, and optional jq filtering on read.
//
// Single-replica by design — the JSONL append model assumes a single writer.
// See the Deployment manifest for the matching Recreate strategy.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	serverName    = "notebook"
	serverVersion = "0.3.0"
	defaultPort   = 8080
	defaultDir    = "/data"
)

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	port := flag.Int("port", envInt("PORT", defaultPort), "HTTP port to listen on")
	dir := flag.String("data", envStr("DATA_DIR", defaultDir), "Directory holding namespace JSONL files")
	flag.Parse()

	store, err := NewStore(*dir)
	if err != nil {
		log.Fatalf("store init: %v", err)
	}

	// NewStreamableHTTPHandler accepts a factory so a fresh Server instance
	// could be returned per request; the notebook tools are stateless, so a
	// single Server is reused across requests. JSONResponse keeps the wire
	// format plain JSON instead of SSE framing — ingress-nginx buffering
	// makes SSE unreliable in this cluster (see vestibule-mcp/CLAUDE.md).
	server := mcp.NewServer(&mcp.Implementation{
		Name:    serverName,
		Version: serverVersion,
	}, nil)
	registerTools(server, store)

	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{JSONResponse: true},
	)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/mcp/", mcpHandler)
	mux.Handle("/metrics", metricsHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"notebook","version":"` + serverVersion + `"}` + "\n"))
	})

	addr := ":" + strconv.Itoa(*port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("notebook v%s listening on %s, data=%s", serverVersion, addr, *dir)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}
