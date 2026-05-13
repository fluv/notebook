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
	dir     string
	muRoot  sync.Mutex
	mus     map[string]*sync.Mutex // per-namespace mutex map
	entropy *monotonicEntropy
}

type monotonicEntropy struct {
	mu sync.Mutex
	e  *ulid.MonotonicEntropy
}

func (m *monotonicEntropy) Read(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.e.Read(p)
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return &Store{
		dir:     dir,
		mus:     make(map[string]*sync.Mutex),
		entropy: &monotonicEntropy{e: ulid.Monotonic(rand.Reader, 0)},
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
		TS:      now.Format(time.RFC3339Nano),
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
	return entry, nil
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

// Get reads entries from the namespace, filtering out tombstoned IDs. If
// jqFilter is non-empty, each surviving entry is passed through the filter
// and the filter's output values are collected. If last > 0, only the final
// last results are returned.
//
// Without a jq filter the result is the entry stream as-is (a list of
// {id, ts, content} objects). With a jq filter, the result is whatever the
// filter produces — typically a filtered or projected entry list, but the
// caller is free to write a filter that reshapes entries arbitrarily.
func (s *Store) Get(ns string, jqFilter string, last int) ([]any, error) {
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

	var query *gojq.Query
	if jqFilter != "" {
		q, err := gojq.Parse(jqFilter)
		if err != nil {
			return nil, fmt.Errorf("parse jq filter: %w", err)
		}
		query = q
	}

	f, err := os.Open(s.jsonlPath(ns))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []any{}, nil
		}
		return nil, fmt.Errorf("open jsonl: %w", err)
	}
	defer f.Close()

	results := []any{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // tolerate up to 8MB lines
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		var entry Entry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("parse line %d: %w", lineNo, err)
		}
		if _, dead := tombstones[entry.ID]; dead {
			continue
		}
		if query == nil {
			results = append(results, entry)
			continue
		}
		// Decode the entry once more into a generic map so gojq can walk it.
		var generic any
		if err := json.Unmarshal(raw, &generic); err != nil {
			return nil, fmt.Errorf("decode line %d for jq: %w", lineNo, err)
		}
		iter := query.Run(generic)
		for {
			v, ok := iter.Next()
			if !ok {
				break
			}
			if err, isErr := v.(error); isErr {
				return nil, fmt.Errorf("jq error on line %d: %w", lineNo, err)
			}
			results = append(results, v)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan jsonl: %w", err)
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

// NamespaceDescription is the deeper shape returned by Describe: counts
// (live and tombstoned), the timestamp range, and — if a jq field
// expression was supplied — distinct values emitted by the field and
// their occurrence counts.
type NamespaceDescription struct {
	Namespace  string          `json:"namespace"`
	EntryCount int             `json:"entry_count"`
	Tombstoned int             `json:"tombstoned"`
	FirstTS    string          `json:"first_ts,omitempty"`
	LastTS     string          `json:"last_ts,omitempty"`
	Field      string          `json:"field,omitempty"`
	Distinct   []DistinctValue `json:"distinct,omitempty"`
}

// inspect walks a single namespace once, applying an optional jq
// expression and aggregating distinct outputs. Returns zero values for
// a non-existent namespace (treating it as empty rather than an error).
func (s *Store) inspect(ns string, jqField string) (NamespaceDescription, error) {
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
	for scanner.Scan() {
		lineNo++
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		var entry Entry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return NamespaceDescription{}, fmt.Errorf("parse line %d: %w", lineNo, err)
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
			continue
		}
		var generic any
		if err := json.Unmarshal(raw, &generic); err != nil {
			return NamespaceDescription{}, fmt.Errorf("decode line %d for jq: %w", lineNo, err)
		}
		iter := query.Run(generic)
		for {
			v, ok := iter.Next()
			if !ok {
				break
			}
			if err, isErr := v.(error); isErr {
				return NamespaceDescription{}, fmt.Errorf("jq error on line %d: %w", lineNo, err)
			}
			key, kerr := json.Marshal(v)
			if kerr != nil {
				return NamespaceDescription{}, fmt.Errorf("marshal jq output line %d: %w", lineNo, kerr)
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
	return desc, nil
}

// Describe returns a NamespaceDescription for one namespace. If field is
// non-empty it is parsed as a jq expression and applied to each live
// entry; emitted values are aggregated into Distinct.
func (s *Store) Describe(ns string, field string) (NamespaceDescription, error) {
	return s.inspect(ns, field)
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
		d, err := s.inspect(name, "")
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
