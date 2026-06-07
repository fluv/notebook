package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Content accepts any JSON value (string, number, object, array, null) and is
// marshalled at handler time. `any` is used rather than json.RawMessage
// because the SDK's schema inferer treats RawMessage as []byte and emits
// type: [null, array], rejecting objects at the protocol layer.
//
// Entries is an optional bulk field: when present, content is ignored and all
// values are appended in a single file-open / fsync cycle.
type appendArgs struct {
	Namespace string `json:"namespace"`
	Content   any    `json:"content,omitempty"`
	Entries   []any  `json:"entries,omitempty"`
	Sensitive bool   `json:"sensitive,omitempty"`
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
			Description: "single entry value (any JSON type). Provide content OR entries, not both.",
		},
		"entries": {
			Type:        "array",
			Description: "bulk: array of content values appended in one call, one ULID per element. Provide entries OR content, not both.",
			Items: &jsonschema.Schema{
				Types: []string{"object", "array", "string", "number", "boolean", "null"},
			},
		},
		"sensitive": {
			Type:        "boolean",
			Description: "when true, mark this entry (or all bulk entries) as sensitive; hidden from get/search when NOTEBOOK_SENSITIVE_DEFAULT=exclude",
		},
	},
	Required: []string{"namespace"},
}

// appendResult echoes what was stored so callers can confirm without a
// follow-up read. Single-append sets entry; bulk-append sets entries.
type appendResult struct {
	Entry   *Entry  `json:"entry,omitempty" jsonschema:"set on single-append (content field)"`
	Entries []Entry `json:"entries,omitempty" jsonschema:"set on bulk-append (entries field), one element per input"`
}

type getArgs struct {
	Namespace        string `json:"namespace" jsonschema:"namespace to read from"`
	Jq               string `json:"jq,omitempty" jsonschema:"optional jq filter; applied to each entry; runs before last; an empty/no-output filter drops the entry"`
	Last             int    `json:"last,omitempty" jsonschema:"optional cap; if >0, return only the final N results after jq"`
	IncludeSensitive *bool  `json:"include_sensitive,omitempty" jsonschema:"override the server-level NOTEBOOK_SENSITIVE_DEFAULT: true includes sensitive entries, false excludes them; omit to use server default"`
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
	Field     string `json:"field,omitempty" jsonschema:"optional jq expression; if set, distinct emitted values are aggregated with counts (e.g. '.content.tag'); if omitted, an inferred schema digest is returned instead"`
}

type describeResult struct {
	NamespaceDescription
}

type searchArgs struct {
	Query            string `json:"query" jsonschema:"literal substring (verbatim, not keyword-decomposed) or Go regex; for multi-keyword OR use regex mode with alternation: shopping|recipe|meal"`
	Namespace        string `json:"namespace,omitempty" jsonschema:"namespace to search; omit to search all namespaces"`
	Regex            bool   `json:"regex,omitempty" jsonschema:"when true, treat query as a Go regular expression"`
	Limit            int    `json:"limit,omitempty" jsonschema:"maximum number of hits to return; defaults to 20"`
	IncludeSensitive *bool  `json:"include_sensitive,omitempty" jsonschema:"override the server-level NOTEBOOK_SENSITIVE_DEFAULT: true includes sensitive entries, false excludes them; omit to use server default"`
}

type searchResult struct {
	Hits []SearchHit `json:"hits"`
}

// boolPtr is a small helper for *bool fields in ToolAnnotations (the SDK
// mixes pointer and value-typed bools across fields).
func boolPtr(b bool) *bool { return &b }

// closedWorld marks every notebook tool as closed-world: notebook only
// interacts with its own PVC-backed storage, never with external
// entities. Set once and reused on every tool.
var closedWorld = boolPtr(false)

// registerTools wires the notebook tools onto the MCP server. Each handler
// is a thin shim that validates input and delegates to the store.
//
// Tool annotations describe behaviour to the client so it can decide
// whether to auto-execute or prompt: read-only tools (list_namespaces,
// describe_namespace, get) are safe to run without confirmation; append
// and delete are destructive (in the change-state sense). delete is
// idempotent (tombstoning twice is a no-op); append is not (each call
// adds a new entry).
func registerTools(server *mcp.Server, store *Store) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "list_namespaces",
		Description: "List notebook namespaces with summary info (entry count, last-updated timestamp) " +
			"for each. Tombstoned entries are excluded from the count.",
		Annotations: &mcp.ToolAnnotations{
			Title:         "List namespaces",
			ReadOnlyHint:  true,
			OpenWorldHint: closedWorld,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ listNamespacesArgs) (*mcp.CallToolResult, listNamespacesResult, error) {
		start := time.Now()
		ns, err := store.ListNamespaces()
		observe("list_namespaces", start, err, slog.Int("namespaces", len(ns)))
		if err != nil {
			return nil, listNamespacesResult{}, err
		}
		return nil, listNamespacesResult{Namespaces: ns}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "describe_namespace",
		Description: "Return entry count, tombstoned count, and timestamp range for a namespace. " +
			"If `field` is set, distinct values emitted by that jq expression are returned with " +
			"their occurrence counts (e.g. field=\".content.tag\" lists each tag and how often it " +
			"appears). Entries that error under the jq filter are skipped; their count is in " +
			"`errored_count`. If `field` is omitted, a schema digest is inferred from live entries: for " +
			"each field path up to depth 2, the types seen, occurrence count, distinct value count, " +
			"and up to 3 sample values are returned. String samples are capped at 80 runes; object " +
			"and array samples are replaced with type tokens (e.g. \"object (3 keys)\", \"array[5]\"). " +
			"Depth-2 paths (e.g. .content.obj.key) are omitted unless they appear in ≥2 entries — " +
			"single-occurrence depth-2 paths are likely map keys, not schema fields. " +
			"Answers \"what's in here\" in a single pass without returning the entries themselves.",
		Annotations: &mcp.ToolAnnotations{
			Title:         "Describe namespace",
			ReadOnlyHint:  true,
			OpenWorldHint: closedWorld,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args describeArgs) (*mcp.CallToolResult, describeResult, error) {
		start := time.Now()
		d, err := store.Describe(args.Namespace, args.Field)
		observe("describe_namespace", start, err,
			slog.String("namespace", args.Namespace),
			slog.String("field", args.Field),
		)
		if err != nil {
			return nil, describeResult{}, err
		}
		return nil, describeResult{NamespaceDescription: d}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "append",
		Description: "Append one or more content values to a namespace. Each value may be any JSON " +
			"type (string, number, object, array, null). " +
			"Single-entry: pass `content`; returns `{entry: {id, ts, content}}`. " +
			"Bulk: pass `entries` (array of values); all are written in one file-open/fsync cycle " +
			"and `{entries: [{id, ts, content}, ...]}` is returned. Prefer bulk over repeated " +
			"single calls when appending N items. The namespace is created on first append.",
		InputSchema: appendInputSchema,
		Annotations: &mcp.ToolAnnotations{
			Title: "Append entry",
			// ReadOnlyHint defaults to false. Destructive=false because
			// append only adds; it never overwrites or removes. Not
			// idempotent: each call adds a new entry with a fresh ULID.
			DestructiveHint: boolPtr(false),
			OpenWorldHint:   closedWorld,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args appendArgs) (*mcp.CallToolResult, appendResult, error) {
		start := time.Now()
		hasBulk := args.Entries != nil
		hasSingle := args.Content != nil
		if hasBulk && hasSingle {
			return nil, appendResult{}, errors.New("provide content OR entries, not both")
		}
		if !hasBulk && !hasSingle {
			return nil, appendResult{}, errors.New("provide either content (single value) or entries (array of values)")
		}
		if hasBulk {
			entries, err := store.AppendMany(args.Namespace, args.Entries, args.Sensitive)
			observe("append", start, err,
				slog.String("namespace", args.Namespace),
				slog.String("mode", "bulk"),
				slog.Int("count", len(args.Entries)),
				slog.Bool("sensitive", args.Sensitive),
			)
			if err != nil {
				return nil, appendResult{}, err
			}
			return nil, appendResult{Entries: entries}, nil
		}
		entry, err := store.Append(args.Namespace, args.Content, args.Sensitive)
		observe("append", start, err,
			slog.String("namespace", args.Namespace),
			slog.String("mode", "single"),
			slog.Int("count", 1),
			slog.Bool("sensitive", args.Sensitive),
		)
		if err != nil {
			return nil, appendResult{}, err
		}
		return nil, appendResult{Entry: &entry}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "get",
		Description: "Read entries from a namespace. Tombstoned entries are excluded. " +
			"If `jq` is set, each entry is piped through the jq filter and the filter's " +
			"outputs are collected (e.g. `select(.content.tag == \"espresso\")`). If `last` " +
			"is set, only the final N results (post-jq) are returned. " +
			"Sensitive-marked entries are shown or hidden according to `include_sensitive` " +
			"(omit to use the server default set by NOTEBOOK_SENSITIVE_DEFAULT). " +
			"Timestamps are second-precision; content that was stored as a JSON-encoded string " +
			"is transparently deserialised to its original structure.",
		Annotations: &mcp.ToolAnnotations{
			Title:         "Get entries",
			ReadOnlyHint:  true,
			OpenWorldHint: closedWorld,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getArgs) (*mcp.CallToolResult, getResult, error) {
		start := time.Now()
		entries, err := store.Get(args.Namespace, args.Jq, args.Last, args.IncludeSensitive)
		observe("get", start, err,
			slog.String("namespace", args.Namespace),
			slog.Bool("has_jq", args.Jq != ""),
			slog.Int("last", args.Last),
			slog.Int("returned", len(entries)),
		)
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
		Annotations: &mcp.ToolAnnotations{
			Title:           "Delete entry",
			DestructiveHint: boolPtr(true),
			IdempotentHint:  true,
			OpenWorldHint:   closedWorld,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args deleteArgs) (*mcp.CallToolResult, deleteResult, error) {
		start := time.Now()
		err := store.Delete(args.Namespace, args.ID)
		observe("delete", start, err,
			slog.String("namespace", args.Namespace),
			slog.String("id", args.ID),
		)
		if err != nil {
			return nil, deleteResult{}, err
		}
		return nil, deleteResult{OK: true}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "search",
		Description: "Scan one or all namespaces for entries whose raw JSON contains a match. " +
			"Matching is substring by default — the query is matched verbatim, not decomposed into keywords. " +
			"For multi-keyword OR searches, set `regex: true` and use alternation: `shopping|recipe|meal`. " +
			"Tombstoned entries are skipped. Returns up to `limit` hits (default 20), each " +
			"with namespace, id, ts, and a ~120-character snippet centred on the first match. " +
			"The search runs against raw JSONL so it also matches on id and ts fields.",
		Annotations: &mcp.ToolAnnotations{
			Title:         "Search entries",
			ReadOnlyHint:  true,
			OpenWorldHint: closedWorld,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, searchResult, error) {
		start := time.Now()
		hits, err := store.Search(args.Query, args.Namespace, args.Regex, args.Limit, args.IncludeSensitive)
		searchScope := args.Namespace
		if searchScope == "" {
			searchScope = "*"
		}
		observe("search", start, err,
			slog.String("namespace", searchScope),
			slog.Bool("regex", args.Regex),
			slog.Int("hits", len(hits)),
		)
		if err != nil {
			return nil, searchResult{}, err
		}
		return nil, searchResult{Hits: hits}, nil
	})
}
