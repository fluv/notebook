package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type appendArgs struct {
	Namespace string          `json:"namespace" jsonschema:"namespace name; created on first append; alphanumeric, dash, underscore; up to 64 chars"`
	Content   json.RawMessage `json:"content" jsonschema:"any JSON value (string, number, object, array, null)"`
}

type appendResult struct {
	ID string `json:"id" jsonschema:"ULID of the appended entry"`
	TS string `json:"ts" jsonschema:"RFC3339Nano UTC timestamp"`
}

type getArgs struct {
	Namespace string `json:"namespace" jsonschema:"namespace to read from"`
	Jq        string `json:"jq,omitempty" jsonschema:"optional jq filter; applied to each entry; runs before last; an empty/no-output filter drops the entry"`
	Last      int    `json:"last,omitempty" jsonschema:"optional cap; if >0, return only the final N results after jq"`
}

type getResult struct {
	Entries []any `json:"entries" jsonschema:"list of entries (or arbitrary values if jq reshapes them)"`
}

type deleteArgs struct {
	Namespace string `json:"namespace"`
	ID        string `json:"id" jsonschema:"ULID of the entry to tombstone"`
}

type deleteResult struct {
	OK bool `json:"ok"`
}

type listNamespacesArgs struct{}

type listNamespacesResult struct {
	Namespaces []string `json:"namespaces"`
}

// registerTools wires the four notebook tools onto the MCP server. Each
// handler is a thin shim that validates input and delegates to the store.
func registerTools(server *mcp.Server, store *Store) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_namespaces",
		Description: "List notebook namespaces that currently exist (i.e. have at least one entry on disk).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ listNamespacesArgs) (*mcp.CallToolResult, listNamespacesResult, error) {
		ns, err := store.ListNamespaces()
		if err != nil {
			return nil, listNamespacesResult{}, err
		}
		return nil, listNamespacesResult{Namespaces: ns}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "append",
		Description: "Append a content value to a namespace. Content may be any JSON value (string, " +
			"number, object, array, null). Returns the assigned ULID and UTC timestamp. The " +
			"namespace is created on first append.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args appendArgs) (*mcp.CallToolResult, appendResult, error) {
		if len(args.Content) == 0 {
			return nil, appendResult{}, fmt.Errorf("content is required")
		}
		entry, err := store.Append(args.Namespace, args.Content)
		if err != nil {
			return nil, appendResult{}, err
		}
		return nil, appendResult{ID: entry.ID, TS: entry.TS}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "get",
		Description: "Read entries from a namespace. Tombstoned entries are excluded. " +
			"If `jq` is set, each entry is piped through the jq filter and the filter's " +
			"outputs are collected (e.g. `select(.content.tag == \"espresso\")`). If `last` " +
			"is set, only the final N results (post-jq) are returned.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getArgs) (*mcp.CallToolResult, getResult, error) {
		entries, err := store.Get(args.Namespace, args.Jq, args.Last)
		if err != nil {
			return nil, getResult{}, err
		}
		return nil, getResult{Entries: entries}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "delete",
		Description: "Tombstone an entry by ID. The underlying JSONL is never rewritten; the ID is " +
			"recorded in a per-namespace tombstone file and filtered out of subsequent reads. " +
			"Idempotent.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args deleteArgs) (*mcp.CallToolResult, deleteResult, error) {
		if err := store.Delete(args.Namespace, args.ID); err != nil {
			return nil, deleteResult{}, err
		}
		return nil, deleteResult{OK: true}, nil
	})
}
