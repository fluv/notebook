CLAUDE.md — notebook
====================

Invariants for future work on this repo. Anything listed here requires
human sign-off to change, not just a PR that looks sensible.

Append-only, never rewrite
--------------------------

The JSONL file for a namespace is appended to and never rewritten in
place. Deletion is via the parallel `.tombstones` file. Rewriting the
JSONL (e.g. to compact tombstoned entries) is out of scope without
sign-off — atomic-rename + reload is plausible but has subtle failure
modes (mid-rename crash, concurrent reads) that need design work, not a
casual PR.

Single writer, single replica
-----------------------------

The append model assumes one writer. The matching k8s Deployment uses
the `Recreate` strategy with `replicas: 1`. Do not raise replicas, and
do not switch the rollout strategy to `RollingUpdate` — overlapping
pods would corrupt the JSONL during cutover.

Storage shape stability
-----------------------

The on-disk shape is `{"id": "<ULID>", "ts": "<RFC3339Nano>", "content": <any JSON>}`,
one per line, UTF-8. The `id` field is a stable identifier the caller may
rely on. Changing the entry shape (renaming fields, switching ID
formats) is a breaking change for any consumer that's persisted IDs
externally — it needs a versioned migration, not an in-place edit.

Style
-----

- Flat Go. Everything in `package main` at the repo root. No `internal/`
  subpackages.
- Standard library first. Acceptable third-party deps are the three
  in `go.mod`: `gojq`, `oklog/ulid/v2`, `modelcontextprotocol/go-sdk`.
  Adding a new dependency needs a specific reason tied to the change.
- `main.go` is the reader's entry point. Don't bury core logic behind
  layers of indirection.

Namespace validation
--------------------

Namespace names must match `^[A-Za-z0-9_][A-Za-z0-9_-]{0,63}$`. No dots
(path traversal), no slashes, no leading dash. The regex is the
contract — relaxing it needs design work, not a permissive `-x` flag.
