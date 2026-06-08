package main

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/itchyny/gojq"
	"github.com/oklog/ulid/v2"
)

// Namespace name format: one or more of [A-Za-z0-9_-]. No dots (path traversal),
// no slashes, no leading dash. Capped at 64 characters.
var namespaceRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,63}$`)

// Entry is the canonical on-disk shape of one notebook record. Content is
// preserved as the caller submitted it.
//
// Content is `any`, not json.RawMessage. The SDK's schema inferer treats
// RawMessage as []byte and emits type: [null, array], which causes both
// input AND output validation to reject object payloads. `any` produces
// a permissive schema and round-trips structured values cleanly through
// json.Marshal / json.Unmarshal.
type Entry struct {
	ID      string `json:"id"`
	TS      string `json:"ts"`
	Content any    `json:"content"`
}

// Store is a single-replica, append-only JSONL store with per-namespace
// tombstone files. Concurrent access within the process is serialised by a
// per-namespace mutex; single-replica deployment guarantees no cross-process
// contention.
type Store struct {
	dir                     string
	muRoot                  sync.Mutex
	mus                     map[string]*sync.Mutex // per-namespace mutex map
	entropy                 ulid.MonotonicReader
	includeSensitiveDefault bool // set from NOTEBOOK_SENSITIVE_DEFAULT env var
}

// NewStore initialises the store. Set NOTEBOOK_SENSITIVE_DEFAULT=exclude to
// hide sensitive-marked entries from bare get/search calls by default; any
// other value (including unset) includes them.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return &Store{
		dir:                     dir,
		mus:                     make(map[string]*sync.Mutex),
		entropy:                 &ulid.LockedMonotonicReader{MonotonicReader: ulid.Monotonic(rand.Reader, 0)},
		includeSensitiveDefault: os.Getenv("NOTEBOOK_SENSITIVE_DEFAULT") != "exclude",
	}, nil
}

func (s *Store) lockFor(ns string) *sync.Mutex {
	s.muRoot.Lock()
	defer s.muRoot.Unlock()
	m, ok := s.mus[ns]
	if !ok {
		m = &sync.Mutex{}
		s.mus[ns] = m
	}
	return m
}

func validateNamespace(ns string) error {
	if !namespaceRE.MatchString(ns) {
		return fmt.Errorf("invalid namespace %q: must match %s", ns, namespaceRE.String())
	}
	return nil
}

func (s *Store) jsonlPath(ns string) string {
	return filepath.Join(s.dir, ns+".jsonl")
}

func (s *Store) tombstonePath(ns string) string {
	return filepath.Join(s.dir, ns+".tombstones")
}

// truncateTS parses an RFC3339Nano timestamp and returns it at second
// precision. Milliseconds are irrelevant to callers; removing them saves
// bytes in every tool response. The on-disk format remains RFC3339Nano.
func truncateTS(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts
	}
	return t.Format(time.RFC3339)
}

// isSensitiveEntry reports whether an entry should be treated as sensitive.
// An entry is sensitive when its content is a JSON object with "exportable"
// set to the boolean false. Absent field, null, true, and non-object content
// are all treated as non-sensitive (exportable by default).
func isSensitiveEntry(content any) bool {
	m, ok := content.(map[string]any)
	if !ok {
		return false
	}
	exp, ok := m["exportable"]
	if !ok {
		return false
	}
	b, ok := exp.(bool)
	return ok && !b
}

// isSensitiveRaw is like isSensitiveEntry but operates on the raw JSON of the
// content field, used in search paths where content hasn't been fully decoded.
func isSensitiveRaw(contentJSON json.RawMessage) bool {
	var m map[string]any
	if err := json.Unmarshal(contentJSON, &m); err != nil {
		return false
	}
	exp, ok := m["exportable"]
	if !ok {
		return false
	}
	b, ok := exp.(bool)
	return ok && !b
}

// tryDeserialiseString checks whether v is a string whose value is a valid
// JSON object or array. If so, the parsed structure is returned in place of
// the string. Otherwise v is returned unchanged.
//
// This undoes client-side pre-serialisation: some MCP clients (e.g. claude.ai)
// JSON-encode structured values before transmission even when the schema
// declares multi-type input, producing content stored as a JSON string rather
// than as the intended structure.
func tryDeserialiseString(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	var inner any
	if err := json.Unmarshal([]byte(s), &inner); err != nil {
		return v
	}
	switch inner.(type) {
	case map[string]any, []any:
		return inner
	}
	return v
}

// updateRec is the on-disk shape of an update operation. It is stored as a
// JSONL line alongside regular entries in the same namespace file. The "op"
// field distinguishes update records from regular entries during scan.
type updateRec struct {
	Op       string `json:"op"` // always "update"
	ID       string `json:"id"`
	UpdateTS string `json:"update_ts"`
	Field    string `json:"field"`
	Old      any    `json:"old"`
	New      any    `json:"new"`
}

// UpdateError is returned from Update when a validation rule prevents the
// operation. The Code field is one of the machine-readable codes in the spec.
type UpdateError struct {
	Code           string `json:"error"`
	Message        string `json:"message"`
	OriginalLength int    `json:"original_length,omitempty"`
	MaxAllowed     int    `json:"max_allowed,omitempty"`
	Provided       int    `json:"provided,omitempty"`
}

func (e *UpdateError) Error() string { return e.Code + ": " + e.Message }

// UpdateResult is returned from a successful Update call.
type UpdateResult struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace"`
	UpdateTS  string `json:"update_ts"`
	Field     string `json:"field"`
	Old       any    `json:"old"`
	New       any    `json:"new"`
}

// scanAllRecords reads the namespace JSONL and separates regular entries from
// update records. It returns entries in insertion order and update records
// keyed by entry ID (in file order, which equals chronological order).
//
// The caller must hold the namespace mutex. Missing file is not an error.
func (s *Store) scanAllRecords(ns string) (entries []Entry, updatesByID map[string][]updateRec, err error) {
	updatesByID = make(map[string][]updateRec)
	f, err := os.Open(s.jsonlPath(ns))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, updatesByID, nil
		}
		return nil, nil, fmt.Errorf("open jsonl: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		b := scanner.Bytes()
		if len(b) == 0 {
			continue
		}
		var peek struct {
			Op       string `json:"op,omitempty"`
			UpdateTS string `json:"update_ts,omitempty"`
		}
		if err := json.Unmarshal(b, &peek); err != nil {
			return nil, nil, fmt.Errorf("parse line %d: %w", lineNo, err)
		}
		if peek.Op == "update" && peek.UpdateTS != "" {
			var rec updateRec
			if err := json.Unmarshal(b, &rec); err != nil {
				return nil, nil, fmt.Errorf("parse update record line %d: %w", lineNo, err)
			}
			updatesByID[rec.ID] = append(updatesByID[rec.ID], rec)
		} else {
			var entry Entry
			if err := json.Unmarshal(b, &entry); err != nil {
				return nil, nil, fmt.Errorf("parse entry line %d: %w", lineNo, err)
			}
			entries = append(entries, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan jsonl: %w", err)
	}
	return entries, updatesByID, nil
}

// applyUpdateRecs returns an Entry with all update records applied in order.
// The last update to a given field wins. Entries with non-object content can
// only be updated via field "." (whole-content replacement).
func applyUpdateRecs(entry Entry, recs []updateRec) Entry {
	for _, rec := range recs {
		if rec.Field == "." {
			entry.Content = rec.New
		} else {
			m, ok := entry.Content.(map[string]any)
			if !ok {
				continue
			}
			// shallow-copy so we don't mutate the original map
			updated := make(map[string]any, len(m))
			for k, v := range m {
				updated[k] = v
			}
			updated[rec.Field] = rec.New
			entry.Content = updated
		}
	}
	return entry
}

// newID generates a monotonic ULID with millisecond precision. Monotonic
// ordering is preserved across same-millisecond calls within a process.
func (s *Store) newID(t time.Time) (string, error) {
	id, err := ulid.New(ulid.Timestamp(t), s.entropy)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// Append serialises a new entry to the namespace's JSONL file. Content
// may be any JSON value (string, number, object, array, null). The
// caller passes a native Go value; Append marshals it.
//
// Sensitivity is declared in the content itself: entries whose content is a
// JSON object with "exportable": false are treated as sensitive at read time
// and filtered out by Get/Search when NOTEBOOK_SENSITIVE_DEFAULT=exclude.
//
// The returned entry carries a second-precision timestamp (not nano); the
// on-disk record retains full RFC3339Nano precision.
func (s *Store) Append(ns string, content any) (Entry, error) {
	if err := validateNamespace(ns); err != nil {
		return Entry{}, err
	}

	now := time.Now().UTC()
	id, err := s.newID(now)
	if err != nil {
		return Entry{}, fmt.Errorf("generate id: %w", err)
	}
	entry := Entry{
		ID:      id,
		TS:      now.Format(time.RFC3339Nano), // full precision on disk
		Content: content,
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return Entry{}, fmt.Errorf("marshal entry: %w", err)
	}
	line = append(line, '\n')

	mu := s.lockFor(ns)
	mu.Lock()
	defer mu.Unlock()

	f, err := os.OpenFile(s.jsonlPath(ns), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return Entry{}, fmt.Errorf("open jsonl: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return Entry{}, fmt.Errorf("write entry: %w", err)
	}
	if err := f.Sync(); err != nil {
		return Entry{}, fmt.Errorf("fsync: %w", err)
	}
	entry.TS = truncateTS(entry.TS) // truncate for response
	return entry, nil
}

// AppendMany writes multiple entries to the namespace's JSONL file in one
// lock-acquire / file-open / fsync cycle. All entries land in a single write
// so they are contiguous on disk and cheaper than N separate Append calls.
//
// Contents are marshalled before acquiring the lock so the critical section is
// as short as possible. Returned entries carry second-precision timestamps.
// Sensitivity is per-entry in content (see Append).
func (s *Store) AppendMany(ns string, contents []any) ([]Entry, error) {
	if err := validateNamespace(ns); err != nil {
		return nil, err
	}
	if len(contents) == 0 {
		return []Entry{}, nil
	}

	// Pre-marshal all entries before acquiring the lock.
	entries := make([]Entry, 0, len(contents))
	var buf []byte
	for i, content := range contents {
		now := time.Now().UTC()
		id, err := s.newID(now)
		if err != nil {
			return nil, fmt.Errorf("generate id for entry %d: %w", i, err)
		}
		entry := Entry{ID: id, TS: now.Format(time.RFC3339Nano), Content: content}
		line, err := json.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("marshal entry %d: %w", i, err)
		}
		buf = append(buf, line...)
		buf = append(buf, '\n')
		entries = append(entries, entry)
	}

	mu := s.lockFor(ns)
	mu.Lock()
	defer mu.Unlock()

	f, err := os.OpenFile(s.jsonlPath(ns), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open jsonl: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(buf); err != nil {
		return nil, fmt.Errorf("write entries: %w", err)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("fsync: %w", err)
	}
	for i := range entries {
		entries[i].TS = truncateTS(entries[i].TS)
	}
	return entries, nil
}

// Delete tombstones an ID in the given namespace. Idempotent: tombstoning an
// already-deleted or non-existent ID is not an error.
//
// Tombstones are append-only; the underlying JSONL is never rewritten. A
// future operator could remove a line directly without drift (entries are
// addressed by ULID, not by line position), but tombstoning is the supported
// path for normal use.
func (s *Store) Delete(ns string, id string) error {
	if err := validateNamespace(ns); err != nil {
		return err
	}
	if _, err := ulid.Parse(id); err != nil {
		return fmt.Errorf("invalid id %q: %w", id, err)
	}

	mu := s.lockFor(ns)
	mu.Lock()
	defer mu.Unlock()

	f, err := os.OpenFile(s.tombstonePath(ns), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open tombstones: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(id + "\n"); err != nil {
		return fmt.Errorf("write tombstone: %w", err)
	}
	return f.Sync()
}

// loadTombstones returns the set of tombstoned IDs for the namespace. Missing
// file is not an error.
func (s *Store) loadTombstones(ns string) (map[string]struct{}, error) {
	set := make(map[string]struct{})
	f, err := os.Open(s.tombstonePath(ns))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return set, nil
		}
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		set[line] = struct{}{}
	}
	return set, scanner.Err()
}

// Get reads entries from the namespace, filtering out tombstoned IDs. Update
// records are applied before entries are returned, so the result reflects the
// current (corrected) state of each entry.
//
// If jqFilter is non-empty, each surviving entry is passed through the filter
// and the filter's output values are collected. If last > 0, only the final
// last results are returned.
//
// includeSensitive controls whether sensitive-marked entries are returned. If
// nil, the store's includeSensitiveDefault (from NOTEBOOK_SENSITIVE_DEFAULT)
// is used. Pass a non-nil pointer to override per-call.
//
// Timestamps in results are truncated to second precision (RFC3339). The
// on-disk format is unchanged (RFC3339Nano). Content that is a JSON-encoded
// string containing an object or array is transparently deserialised.
func (s *Store) Get(ns string, jqFilter string, last int, includeSensitive *bool) ([]any, error) {
	if err := validateNamespace(ns); err != nil {
		return nil, err
	}

	mu := s.lockFor(ns)
	mu.Lock()
	defer mu.Unlock()

	tombstones, err := s.loadTombstones(ns)
	if err != nil {
		return nil, fmt.Errorf("load tombstones: %w", err)
	}

	includeSens := s.includeSensitiveDefault
	if includeSensitive != nil {
		includeSens = *includeSensitive
	}

	var query *gojq.Query
	if jqFilter != "" {
		q, err := gojq.Parse(jqFilter)
		if err != nil {
			return nil, fmt.Errorf("parse jq filter: %w", err)
		}
		query = q
	}

	entries, updatesByID, err := s.scanAllRecords(ns)
	if err != nil {
		return nil, err
	}

	results := []any{}
	for _, entry := range entries {
		if _, dead := tombstones[entry.ID]; dead {
			continue
		}
		if recs, ok := updatesByID[entry.ID]; ok {
			entry = applyUpdateRecs(entry, recs)
		}
		if !includeSens && isSensitiveEntry(entry.Content) {
			continue
		}
		entry.TS = truncateTS(entry.TS)
		entry.Content = tryDeserialiseString(entry.Content)
		if query == nil {
			results = append(results, entry)
			continue
		}
		// Marshal the (possibly updated) entry back to JSON so gojq can walk it.
		b, err := json.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("marshal entry %s for jq: %w", entry.ID, err)
		}
		var generic any
		if err := json.Unmarshal(b, &generic); err != nil {
			return nil, fmt.Errorf("decode entry %s for jq: %w", entry.ID, err)
		}
		iter := query.Run(generic)
		for {
			v, ok := iter.Next()
			if !ok {
				break
			}
			if err, isErr := v.(error); isErr {
				return nil, fmt.Errorf("jq error on entry %s: %w", entry.ID, err)
			}
			results = append(results, v)
		}
	}

	if last > 0 && len(results) > last {
		results = results[len(results)-last:]
	}
	return results, nil
}

// ListNamespaceNames returns the names of namespaces that have at least one
// JSONL file on disk. Tombstone-only namespaces (deleted before any entry
// existed) are not listed.
func (s *Store) ListNamespaceNames() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		names = append(names, strings.TrimSuffix(name, ".jsonl"))
	}
	return names, nil
}

// NamespaceSummary is the minimal at-a-glance shape: name, live entry
// count, and the timestamp of the most recent live entry. Tombstoned
// entries are excluded from both counts and timestamp.
type NamespaceSummary struct {
	Name       string `json:"name"`
	EntryCount int    `json:"entry_count"`
	LastTS     string `json:"last_ts,omitempty"`
}

// DistinctValue is one row in a distinct-values report.
type DistinctValue struct {
	Value any `json:"value"`
	Count int `json:"count"`
}

// FieldShape describes one field path inferred from live entries when
// describe_namespace is called without a field argument.
type FieldShape struct {
	Path          string   `json:"path"`
	Types         []string `json:"types"`
	Count         int      `json:"count"`
	DistinctCount int      `json:"distinct_count"`
	SampleValues  []any    `json:"sample_values,omitempty"`
}

// SearchHit is one result entry returned by Search.
type SearchHit struct {
	Namespace string `json:"namespace"`
	ID        string `json:"id"`
	TS        string `json:"ts"`
	Snippet   string `json:"snippet"`
}

// NamespaceDescription is the deeper shape returned by Describe: counts
// (live and tombstoned), the timestamp range, and either distinct values
// (when field is set) or an inferred schema digest (when field is omitted).
type NamespaceDescription struct {
	Namespace    string          `json:"namespace"`
	EntryCount   int             `json:"entry_count"`
	Tombstoned   int             `json:"tombstoned"`
	ErroredCount int             `json:"errored_count,omitempty"`
	FirstTS      string          `json:"first_ts,omitempty"`
	LastTS       string          `json:"last_ts,omitempty"`
	Field        string          `json:"field,omitempty"`
	Distinct     []DistinctValue `json:"distinct,omitempty"`
	Shape        []FieldShape    `json:"shape,omitempty"`
}

// pathAcc accumulates per-path type and value information across entries.
type pathAcc struct {
	types    map[string]struct{}
	distinct map[string]struct{}
	count    int
	samples  []any
}

// jsonType returns the JSON type name for a Go value produced by json.Unmarshal.
func jsonType(v any) string {
	if v == nil {
		return "null"
	}
	switch v.(type) {
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

// compactSample replaces values that would bloat the shape digest with compact
// tokens. Strings longer than 80 runes are truncated. Objects and arrays are
// replaced with type tokens so the digest stays smaller than reading entries.
func compactSample(v any) any {
	switch val := v.(type) {
	case string:
		runes := []rune(val)
		if len(runes) > 80 {
			return string(runes[:80]) + "..."
		}
		return val
	case map[string]any:
		return fmt.Sprintf("object (%d keys)", len(val))
	case []any:
		return fmt.Sprintf("array[%d]", len(val))
	default:
		return v
	}
}

// recordPath records one observation of path=v into acc.
func recordPath(acc map[string]*pathAcc, path string, v any) {
	a, ok := acc[path]
	if !ok {
		a = &pathAcc{
			types:    make(map[string]struct{}),
			distinct: make(map[string]struct{}),
		}
		acc[path] = a
	}
	a.count++
	a.types[jsonType(v)] = struct{}{}
	// v always originates from json.Unmarshal and is therefore always JSON-safe;
	// this branch cannot fire in practice, but skip distinct tracking defensively
	// to avoid silent bucket corruption on unexpected input.
	key, kerr := json.Marshal(v)
	if kerr != nil {
		return
	}
	ks := string(key)
	if _, seen := a.distinct[ks]; !seen {
		a.distinct[ks] = struct{}{}
		if len(a.samples) < 3 {
			a.samples = append(a.samples, compactSample(v))
		}
	}
}

// accumulatePaths walks a content value up to depth 2 from the root,
// recording each path into acc. Object roots are descended into rather
// than recorded directly; paths therefore begin at .content.field.
// Non-object roots are recorded at ".content" itself.
func accumulatePaths(acc map[string]*pathAcc, path string, v any, depth int) {
	if depth == 0 {
		if m, ok := v.(map[string]any); ok {
			for k, child := range m {
				accumulatePaths(acc, path+"."+k, child, 1)
			}
			return
		}
		recordPath(acc, path, v)
		return
	}
	recordPath(acc, path, v)
	if depth < 2 {
		if m, ok := v.(map[string]any); ok {
			for k, child := range m {
				accumulatePaths(acc, path+"."+k, child, depth+1)
			}
		}
	}
}

// pathAccToShape converts accumulated path data to a sorted FieldShape slice.
// Fields are ordered by descending count, then ascending path for ties.
func pathAccToShape(acc map[string]*pathAcc) []FieldShape {
	shapes := make([]FieldShape, 0, len(acc))
	for path, a := range acc {
		// Depth-2 paths (3 dots) with count=1 are likely map keys (data), not schema fields.
		if strings.Count(path, ".") >= 3 && a.count < 2 {
			continue
		}
		types := make([]string, 0, len(a.types))
		for t := range a.types {
			types = append(types, t)
		}
		sort.Strings(types)
		shapes = append(shapes, FieldShape{
			Path:          path,
			Types:         types,
			Count:         a.count,
			DistinctCount: len(a.distinct),
			SampleValues:  a.samples,
		})
	}
	sort.Slice(shapes, func(i, j int) bool {
		if shapes[i].Count != shapes[j].Count {
			return shapes[i].Count > shapes[j].Count
		}
		return shapes[i].Path < shapes[j].Path
	})
	return shapes
}

// makeSnippet returns a ~width-rune context window centred on matchBytePos
// in text, with "..." ellipsis on truncated ends.
func makeSnippet(text string, matchBytePos, width int) string {
	runes := []rune(text)
	runeMatch := utf8.RuneCountInString(text[:matchBytePos])
	half := width / 2
	start := runeMatch - half
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(runes) {
		end = len(runes)
		start = end - width
		if start < 0 {
			start = 0
		}
	}
	snippet := string(runes[start:end])
	if start > 0 {
		snippet = "..." + snippet
	}
	if end < len(runes) {
		snippet += "..."
	}
	return snippet
}

// inspect walks a single namespace once, applying an optional jq expression
// and aggregating distinct outputs. When shapeMode is true (and jqField is
// empty), it infers a schema digest from .content instead. Returns zero
// values for a non-existent namespace (treating it as empty rather than an
// error).
func (s *Store) inspect(ns string, jqField string, shapeMode bool) (NamespaceDescription, error) {
	if err := validateNamespace(ns); err != nil {
		return NamespaceDescription{}, err
	}

	mu := s.lockFor(ns)
	mu.Lock()
	defer mu.Unlock()

	tombstones, err := s.loadTombstones(ns)
	if err != nil {
		return NamespaceDescription{}, fmt.Errorf("load tombstones: %w", err)
	}

	var query *gojq.Query
	if jqField != "" {
		q, err := gojq.Parse(jqField)
		if err != nil {
			return NamespaceDescription{}, fmt.Errorf("parse jq field: %w", err)
		}
		query = q
	}

	desc := NamespaceDescription{
		Namespace: ns,
		Field:     jqField,
	}

	var shapeAcc map[string]*pathAcc
	if shapeMode {
		shapeAcc = make(map[string]*pathAcc)
	}

	f, err := os.Open(s.jsonlPath(ns))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return desc, nil
		}
		return NamespaceDescription{}, fmt.Errorf("open jsonl: %w", err)
	}
	defer f.Close()

	// Distinct aggregation: marshal each emitted value to canonical JSON
	// and use that as the map key. Track first-seen actual value for
	// readable output. Stable: identical values land in the same bucket
	// regardless of underlying Go type.
	type bucket struct {
		value any
		count int
	}
	buckets := map[string]*bucket{}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	lineNo := 0
lineLoop:
	for scanner.Scan() {
		lineNo++
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		// Skip update records — they are not entries and must not inflate counts.
		var peek struct {
			Op       string `json:"op,omitempty"`
			UpdateTS string `json:"update_ts,omitempty"`
		}
		if err := json.Unmarshal(raw, &peek); err == nil && peek.Op == "update" && peek.UpdateTS != "" {
			continue
		}
		var entry Entry
		if err := json.Unmarshal(raw, &entry); err != nil {
			desc.ErroredCount++
			continue
		}
		if _, dead := tombstones[entry.ID]; dead {
			desc.Tombstoned++
			continue
		}
		desc.EntryCount++
		if desc.FirstTS == "" || entry.TS < desc.FirstTS {
			desc.FirstTS = entry.TS
		}
		if entry.TS > desc.LastTS {
			desc.LastTS = entry.TS
		}
		if query == nil {
			if shapeMode {
				accumulatePaths(shapeAcc, ".content", entry.Content, 0)
			}
			continue
		}
		var generic any
		if err := json.Unmarshal(raw, &generic); err != nil {
			desc.ErroredCount++
			continue
		}
		iter := query.Run(generic)
		for {
			v, ok := iter.Next()
			if !ok {
				break
			}
			if _, isErr := v.(error); isErr {
				desc.ErroredCount++
				continue lineLoop
			}
			key, kerr := json.Marshal(v)
			if kerr != nil {
				desc.ErroredCount++
				continue lineLoop
			}
			if b, ok := buckets[string(key)]; ok {
				b.count++
			} else {
				buckets[string(key)] = &bucket{value: v, count: 1}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return NamespaceDescription{}, fmt.Errorf("scan jsonl: %w", err)
	}

	if query != nil {
		desc.Distinct = make([]DistinctValue, 0, len(buckets))
		for _, b := range buckets {
			desc.Distinct = append(desc.Distinct, DistinctValue{Value: b.value, Count: b.count})
		}
		sort.Slice(desc.Distinct, func(i, j int) bool {
			if desc.Distinct[i].Count != desc.Distinct[j].Count {
				return desc.Distinct[i].Count > desc.Distinct[j].Count
			}
			ki, _ := json.Marshal(desc.Distinct[i].Value)
			kj, _ := json.Marshal(desc.Distinct[j].Value)
			return string(ki) < string(kj)
		})
	}
	if shapeMode {
		desc.Shape = pathAccToShape(shapeAcc)
	}
	desc.FirstTS = truncateTS(desc.FirstTS)
	desc.LastTS = truncateTS(desc.LastTS)
	return desc, nil
}

// Describe returns a NamespaceDescription for one namespace. If field is
// non-empty it is parsed as a jq expression and applied to each live entry;
// emitted values are aggregated into Distinct. If field is empty, Shape is
// populated with an inferred schema digest (types, counts, samples per path).
func (s *Store) Describe(ns string, field string) (NamespaceDescription, error) {
	return s.inspect(ns, field, field == "")
}

// ListNamespaces returns a NamespaceSummary for every namespace with a
// JSONL file on disk. Walks each file once.
func (s *Store) ListNamespaces() ([]NamespaceSummary, error) {
	names, err := s.ListNamespaceNames()
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	out := make([]NamespaceSummary, 0, len(names))
	for _, name := range names {
		d, err := s.inspect(name, "", false)
		if err != nil {
			return nil, fmt.Errorf("summarise %s: %w", name, err)
		}
		out = append(out, NamespaceSummary{
			Name:       name,
			EntryCount: d.EntryCount,
			LastTS:     d.LastTS,
		})
	}
	return out, nil
}

// searchOne scans one namespace JSONL for entries matching the given matcher.
// It stops after collecting remaining hits. The caller holds no lock; this
// method acquires the namespace lock internally. Sensitive-marked entries are
// excluded when includeSens is false.
func (s *Store) searchOne(ns string, matcher func(string) (int, bool), remaining int, includeSens bool) ([]SearchHit, error) {
	mu := s.lockFor(ns)
	mu.Lock()
	defer mu.Unlock()

	tombstones, err := s.loadTombstones(ns)
	if err != nil {
		return nil, fmt.Errorf("load tombstones for %s: %w", ns, err)
	}

	f, err := os.Open(s.jsonlPath(ns))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open jsonl %s: %w", ns, err)
	}
	defer f.Close()

	var hits []SearchHit
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		// Skip update records — they're not entries and would produce misleading hits.
		var opPeek struct {
			Op       string `json:"op,omitempty"`
			UpdateTS string `json:"update_ts,omitempty"`
		}
		if err := json.Unmarshal(raw, &opPeek); err == nil && opPeek.Op == "update" && opPeek.UpdateTS != "" {
			continue
		}
		var hdr struct {
			ID      string          `json:"id"`
			TS      string          `json:"ts"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &hdr); err != nil {
			return nil, fmt.Errorf("parse line %d in %s: %w", lineNo, ns, err)
		}
		if _, dead := tombstones[hdr.ID]; dead {
			continue
		}
		if !includeSens && isSensitiveRaw(hdr.Content) {
			continue
		}
		text := string(raw)
		pos, ok := matcher(text)
		if !ok {
			continue
		}
		snippetText := text
		snippetPos := pos
		const contentField = `"content":`
		if ci := strings.Index(text, contentField); ci >= 0 {
			contentStart := ci + len(contentField)
			if pos >= contentStart {
				snippetText = text[contentStart:]
				snippetPos = pos - contentStart
			}
		}
		hits = append(hits, SearchHit{
			Namespace: ns,
			ID:        hdr.ID,
			TS:        truncateTS(hdr.TS),
			Snippet:   makeSnippet(snippetText, snippetPos, 120),
		})
		if len(hits) >= remaining {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan jsonl %s: %w", ns, err)
	}
	return hits, nil
}

// Search scans one or all namespaces for entries whose raw JSONL line
// contains query as a substring (or regex when useRegex is true). Tombstoned
// entries are excluded. Returns up to limit hits (default 20). The snippet
// field in each hit is a ~120-rune context window around the first match.
//
// includeSensitive follows the same semantics as Get: nil uses the store
// default (NOTEBOOK_SENSITIVE_DEFAULT); non-nil overrides per-call.
func (s *Store) Search(query, namespace string, useRegex bool, limit int, includeSensitive *bool) ([]SearchHit, error) {
	if query == "" {
		return nil, fmt.Errorf("query must not be empty")
	}
	if limit <= 0 {
		limit = 20
	}

	includeSens := s.includeSensitiveDefault
	if includeSensitive != nil {
		includeSens = *includeSensitive
	}

	var matcher func(string) (int, bool)
	if useRegex {
		re, err := regexp.Compile(query)
		if err != nil {
			return nil, fmt.Errorf("invalid regex: %w", err)
		}
		matcher = func(s string) (int, bool) {
			loc := re.FindStringIndex(s)
			if loc == nil {
				return -1, false
			}
			return loc[0], true
		}
	} else {
		matcher = func(s string) (int, bool) {
			i := strings.Index(s, query)
			return i, i >= 0
		}
	}

	var namespaces []string
	if namespace != "" {
		if err := validateNamespace(namespace); err != nil {
			return nil, err
		}
		namespaces = []string{namespace}
	} else {
		var err error
		namespaces, err = s.ListNamespaceNames()
		if err != nil {
			return nil, fmt.Errorf("list namespaces: %w", err)
		}
		sort.Strings(namespaces)
	}

	hits := []SearchHit{}
	for _, ns := range namespaces {
		if len(hits) >= limit {
			break
		}
		nsHits, err := s.searchOne(ns, matcher, limit-len(hits), includeSens)
		if err != nil {
			return nil, err
		}
		hits = append(hits, nsHits...)
	}
	return hits, nil
}

// Update patches a single field of an existing entry in place. It is intended
// for minor factual corrections — stale dates, small detail changes, short
// qualifiers — where tombstone + reappend would destroy correct sibling claims.
//
// Rules enforced:
//   - The entry must exist and not be tombstoned.
//   - "id" and "ts" are immutable.
//   - Top-level keys of an object entry cannot be added or removed (KEY_DRIFT).
//   - The field's JSON type cannot change (TYPE_MISMATCH).
//   - String fields are capped at max(2×original_length, original_length+100) runes (TOO_LONG).
//   - Entries with exportable:false require includeSensitive=true (SENSITIVE_REQUIRES_OPT_IN).
//
// Use field "." to replace the entire content of a plain-string or other
// non-object entry.
//
// Updates are stored as append records in the same JSONL file. The read path
// (Get) folds them in chronological order; the last update to a given field
// wins.
func (s *Store) Update(ns, id, field string, value any, includeSensitive bool) (res UpdateResult, err error) {
	if err := validateNamespace(ns); err != nil {
		return UpdateResult{}, err
	}
	if _, err := ulid.Parse(id); err != nil {
		return UpdateResult{}, fmt.Errorf("invalid id %q: %w", id, err)
	}
	if field == "id" || field == "ts" {
		return UpdateResult{}, &UpdateError{
			Code:    "IMMUTABLE_FIELD",
			Message: "id and ts cannot be updated",
		}
	}

	mu := s.lockFor(ns)
	mu.Lock()
	defer mu.Unlock()

	tombstones, err := s.loadTombstones(ns)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("load tombstones: %w", err)
	}

	entries, updatesByID, err := s.scanAllRecords(ns)
	if err != nil {
		return UpdateResult{}, err
	}

	var found *Entry
	for i := range entries {
		if entries[i].ID == id {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		return UpdateResult{}, &UpdateError{
			Code:    "ENTRY_NOT_FOUND",
			Message: fmt.Sprintf("entry %s not found in namespace %s", id, ns),
		}
	}
	if _, dead := tombstones[id]; dead {
		return UpdateResult{}, &UpdateError{
			Code:    "ENTRY_TOMBSTONED",
			Message: "entry has been tombstoned; use entries to write a replacement",
		}
	}

	// Apply existing update records to get the current (post-correction) state.
	current := *found
	if recs, ok := updatesByID[id]; ok {
		current = applyUpdateRecs(current, recs)
	}

	if !includeSensitive && isSensitiveEntry(current.Content) {
		return UpdateResult{}, &UpdateError{
			Code:    "SENSITIVE_REQUIRES_OPT_IN",
			Message: "entry has exportable:false; re-send with include_sensitive:true",
		}
	}

	// Determine old value; validate field, key drift, and types.
	var oldValue any
	if field == "." {
		oldValue = current.Content
	} else {
		m, ok := current.Content.(map[string]any)
		if !ok {
			return UpdateResult{}, &UpdateError{
				Code:    "KEY_DRIFT",
				Message: "content is not a JSON object; use field '.' for non-object entries. Use delete + entries instead.",
			}
		}
		existing, ok := m[field]
		if !ok {
			return UpdateResult{}, &UpdateError{
				Code:    "KEY_DRIFT",
				Message: fmt.Sprintf("key %q does not exist; top-level keys may not change via update. Use delete + entries instead.", field),
			}
		}
		oldValue = existing
	}

	oldType := jsonType(oldValue)
	newType := jsonType(value)
	if oldType != newType {
		return UpdateResult{}, &UpdateError{
			Code:    "TYPE_MISMATCH",
			Message: fmt.Sprintf("field type must not change: was %s, got %s. Use delete + entries instead.", oldType, newType),
		}
	}

	if oldType == "string" {
		oldStr, _ := oldValue.(string)
		newStr, _ := value.(string)
		origLen := utf8.RuneCountInString(oldStr)
		newLen := utf8.RuneCountInString(newStr)
		maxAllowed := 2 * origLen
		if origLen+100 > maxAllowed {
			maxAllowed = origLen + 100
		}
		if newLen > maxAllowed {
			return UpdateResult{}, &UpdateError{
				Code:           "TOO_LONG",
				Message:        "string length exceeds the correction cap; use delete + entries instead",
				OriginalLength: origLen,
				MaxAllowed:     maxAllowed,
				Provided:       newLen,
			}
		}
	}

	now := time.Now().UTC()
	updateTS := now.Format(time.RFC3339)
	rec := updateRec{
		Op:       "update",
		ID:       id,
		UpdateTS: updateTS,
		Field:    field,
		Old:      oldValue,
		New:      value,
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("marshal update record: %w", err)
	}
	line = append(line, '\n')

	f, err := os.OpenFile(s.jsonlPath(ns), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("open jsonl: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close jsonl: %w", cerr)
		}
	}()
	if _, err = f.Write(line); err != nil {
		return res, fmt.Errorf("write update record: %w", err)
	}
	if err = f.Sync(); err != nil {
		return res, fmt.Errorf("fsync: %w", err)
	}

	res = UpdateResult{
		ID:        id,
		Namespace: ns,
		UpdateTS:  updateTS,
		Field:     field,
		Old:       oldValue,
		New:       value,
	}
	return
}
