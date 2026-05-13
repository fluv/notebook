package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Content accepts any JSON value (string, number, object, array, null) and is
// marshalled at handler time. `any` is used rather than json.RawMessage
// because the SDK's schema inferer treats RawMessage as []byte and emits
// type: [null, array], rejecting objects at the protocol layer.
type appendArgs struct {
	Namespace string `json:"namespace"`
	Content   any    `json:"content"`
}

// appendInputSchema overrides the SDK's auto-generated schema. The inferer
// emits no `type` for an `any` field — just a description. Claude.ai (and
// likely other clients) interpret a typeless property as "send a string"
// and JSON-encode the value before transmission. The result on disk is a
// stringified JSON blob inside .content rather than a structured object,
// which forces jq filters into `.content | fromjson | .field`.
//
// Declaring an explicit multi-type schema tells the client every JSON
// kind is acceptable, so structured payloads stay structured on the wire
// and at rest.
var appendInputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"namespace": {
			Type:        "string",
			Description: "namespace name; created on first append; alphanumeric, dash, underscore; up to 64 chars",
		},
		"content": {
			Types:       []string{"object", "array", "string", "number", "boolean", "null"},
			Description: "any JSON value — object/array/string/number/boolean/null. Send the value structurally; do not pre-serialize.",
		},
	},
	Required: []string{"namespace", "content"},
}

// appendResult echoes the stored entry in full. Callers can confirm what
// landed on disk without a follow-up read.
type appendResult struct {
	Entry Entry `json:"entry" jsonschema:"the entry that was just stored, including id, ts, and content"`
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
	Namespaces []NamespaceSummary `json:"namespaces" jsonschema:"summary per namespace: name, entry_count, last_ts"`
}

type describeArgs struct {
	Namespace string `json:"namespace" jsonschema:"namespace to inspect"`
	Field     string `json:"field,omitempty" jsonschema:"optional jq expression; if set, distinct emitted values are aggregated with counts (e.g. '.content.tag')"`
}

type describeResult struct {
	NamespaceDescription
}

// registerTools wires the notebook tools onto the MCP server. Each handler
// is a thin shim that validates input and delegates to the store.
func registerTools(server *mcp.Server, store *Store) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "list_namespaces",
		Description: "List notebook namespaces with summary info (entry count, last-updated timestamp) " +
			"for each. Tombstoned entries are excluded from the count.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ listNamespacesArgs) (*mcp.CallToolResult, listNamespacesResult, error) {
		ns, err := store.ListNamespaces()
		if err != nil {
			return nil, listNamespacesResult{}, err
		}
		return nil, listNamespacesResult{Namespaces: ns}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "describe_namespace",
		Description: "Return entry count, tombstoned count, and timestamp range for a namespace. " +
			"If `field` is set, also returns distinct values emitted by that jq expression with " +
			"their occurrence counts (e.g. field=\".content.tag\" lists each tag and how often " +
			"it appears). Answers \"what's in here\" without reading every entry.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args describeArgs) (*mcp.CallToolResult, describeResult, error) {
		d, err := store.Describe(args.Namespace, args.Field)
		if err != nil {
			return nil, describeResult{}, err
		}
		return nil, describeResult{NamespaceDescription: d}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "append",
		Description: "Append a content value to a namespace. Content may be any JSON value (string, " +
			"number, object, array, null). Returns the stored entry in full so the caller can " +
			"confirm what landed without a follow-up read. The namespace is created on first append.",
		InputSchema: appendInputSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args appendArgs) (*mcp.CallToolResult, appendResult, error) {
		raw, err := json.Marshal(args.Content)
		if err != nil {
			return nil, appendResult{}, fmt.Errorf("marshal content: %w", err)
		}
		entry, err := store.Append(args.Namespace, raw)
		if err != nil {
			return nil, appendResult{}, err
		}
		return nil, appendResult{Entry: entry}, nil
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
