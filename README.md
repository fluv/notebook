notebook
========

MCP server for namespaced append-only storage. Use case: cross-conversation
context that is too large or too structured for claude.ai's memory system
— espresso dial-ins, accumulated free-text notes, audit logs, anything
where the natural shape is "keep appending, occasionally read back".

Tools
-----

- `list_namespaces()` — returns the namespaces with at least one entry on disk,
  with live entry count and last-updated timestamp per namespace.
- `describe_namespace(namespace, field=null)` — returns entry count, tombstoned
  count, and timestamp range. If `field` is set (a jq expression such as
  `".content.tag"`), distinct emitted values and their counts are returned.
  Entries that error under the jq filter are skipped; their count appears in
  `errored_count` (omitted when zero). If `field` is omitted, a schema digest
  is inferred from live entries: for each field path up to depth 2, the types
  seen, occurrence count, distinct value count, and up to 3 sample values are
  returned in `shape`. String samples are capped at 80 runes (truncated with
  `"..."`). Object and array values are replaced with type tokens such as
  `"object (3 keys)"` or `"array[5]"`. Depth-2 paths (e.g. `.content.obj.key`)
  are omitted unless they appear in ≥2 entries — single-occurrence depth-2
  paths are typically map keys rather than schema fields.
- `append(namespace, content)` — appends a content value to a namespace
  (created on first append). Content is any JSON value. Returns
  `{id, ts}` where `id` is a ULID and `ts` is a UTC RFC3339Nano timestamp.
- `get(namespace, jq=null, last=null)` — reads entries from a namespace.
  Tombstoned entries are excluded. If `jq` is set, each surviving entry
  is piped through the filter and the filter's outputs are collected.
  If `last` is set, only the final N results (post-jq) are returned.
- `delete(namespace, id)` — tombstones an entry. Idempotent.
- `search(query, namespace=null, regex=false, limit=20)` — scans one or all
  namespaces for entries whose raw JSON contains `query` as a substring (or
  regex when `regex` is true). Tombstoned entries are excluded. Returns up to
  `limit` hits, each with `{namespace, id, ts, snippet}` where `snippet` is
  a ~120-character context window around the first match, centred on the
  content portion of the line where possible (so the `{"id":...,"ts":...}`
  envelope is excluded from the window unless the match is on those fields).
  The search runs against raw JSONL, so it also matches on the `id` and `ts`
  fields.

Storage
-------

One JSONL file per namespace at `${DATA_DIR}/<namespace>.jsonl`, one
entry per line. Tombstones are recorded in a parallel
`${DATA_DIR}/<namespace>.tombstones` file (one ULID per line). The
JSONL is never rewritten — a future operator could remove a line
directly without drifting anything (entries are addressed by ULID, not
by line position), but tombstoning is the supported path.

Append-only and single-writer by design. The matching k8s Deployment
uses the `Recreate` strategy and a single replica so the JSONL append
model is preserved.

Configuration
-------------

| Flag      | Env        | Default | Meaning                           |
|-----------|------------|---------|-----------------------------------|
| `-port`   | `PORT`     | `8080`  | HTTP listen port                  |
| `-data`   | `DATA_DIR` | `/data` | Directory holding namespace files |
|           | `LOG_LEVEL`| `info`  | Log verbosity: `debug`/`info`/`warn`/`error` |

Logging
-------

Logs are structured JSON on stdout (picked up by the kubelet → Loki). Two
layers:

- **HTTP access log** — one line per request (method, path, remote, status,
  bytes, duration). This is the transport/connectivity view: it captures
  requests that never reach a tool handler (404s, malformed bodies). Probe
  and scrape traffic (`/healthz`, `/metrics`) is logged at `debug` to avoid
  drowning the steady state — set `LOG_LEVEL=debug` to see it.
- **Tool-call log** (`msg: "tool call"`) — one line per MCP tool invocation
  with the tool name, duration, outcome, and tool-specific context
  (namespace, id, counts). Successes log at `info`; failures log at `error`
  with the underlying message. Note that tool errors are returned to the
  client as HTTP 200 with an error body, so the access log shows `200` — the
  tool-call log is where failures actually surface.

Aggregate usage and latency are also exported as Prometheus metrics
(`notebook_tool_calls_total`, `notebook_tool_duration_seconds`).

Endpoints
---------

- `POST /mcp` — MCP Streamable HTTP transport (stateless, JSON responses).
- `GET /healthz` — liveness probe.
- `GET /metrics` — Prometheus metrics.

Deployment lives in [fluv/kube](https://github.com/fluv/kube) under
`claude-notebook/`.

Building
--------

```
go build ./...
go test ./...
```

The Dockerfile produces a static binary on `gcr.io/distroless/static:nonroot`.
