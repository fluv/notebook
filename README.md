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
  If `field` is omitted, a schema digest is inferred from live entries: for
  each field path up to depth 2, the types seen, occurrence count, distinct
  value count, and up to 3 sample values are returned in `shape`.
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
  a ~120-character context window around the first match. The search runs
  against raw JSONL, so it also matches on the `id` and `ts` fields.

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

| Flag      | Env       | Default | Meaning                          |
|-----------|-----------|---------|----------------------------------|
| `-port`   | `PORT`    | `8080`  | HTTP listen port                 |
| `-data`   | `DATA_DIR`| `/data` | Directory holding namespace files|

Endpoints
---------

- `POST /mcp` — MCP Streamable HTTP transport (stateless, JSON responses).
- `GET /healthz` — liveness probe.

Deployment lives in [fluv/kube](https://github.com/fluv/kube) under
`claude-notebook/`.

Building
--------

```
go build ./...
go test ./...
```

The Dockerfile produces a static binary on `gcr.io/distroless/static:nonroot`.
