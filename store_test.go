package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func boolp(b bool) *bool { return &b }

func TestAppendAndGet_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	first, err := s.Append("scratch", mustJSON(t, map[string]any{"tag": "espresso", "shot": 18.0}))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	second, err := s.Append("scratch", mustJSON(t, "a free-text note"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if first.ID == "" || second.ID == "" {
		t.Fatalf("expected IDs, got %q %q", first.ID, second.ID)
	}
	if first.ID == second.ID {
		t.Fatalf("expected distinct IDs")
	}

	results, err := s.Get("scratch", "", 0, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(results))
	}
}

func TestAppend_RejectsInvalidNamespace(t *testing.T) {
	s := newTestStore(t)
	for _, bad := range []string{"", "with/slash", "with.dot", "-leading-dash", "with space", strings.Repeat("a", 65)} {
		if _, err := s.Append(bad, mustJSON(t, "x")); err == nil {
			t.Errorf("expected error for namespace %q", bad)
		}
	}
}

func TestAppend_RejectsInvalidJSON(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Append("ok", json.RawMessage("{not json")); err == nil {
		t.Error("expected error for invalid JSON content")
	}
}

func TestAppendMany_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	contents := []any{
		map[string]any{"tag": "espresso", "shot": 18.0},
		"a free-text note",
		42.0,
	}
	entries, err := s.AppendMany("scratch", contents)
	if err != nil {
		t.Fatalf("AppendMany: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	ids := map[string]bool{}
	for _, e := range entries {
		if e.ID == "" || e.TS == "" {
			t.Fatalf("entry missing ID or TS: %+v", e)
		}
		ids[e.ID] = true
	}
	if len(ids) != 3 {
		t.Error("expected all IDs to be distinct")
	}

	results, err := s.Get("scratch", "", 0, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 stored entries, got %d", len(results))
	}
}

func TestAppendMany_Empty(t *testing.T) {
	s := newTestStore(t)
	entries, err := s.AppendMany("scratch", nil)
	if err != nil {
		t.Fatalf("AppendMany(nil): %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
	entries, err = s.AppendMany("scratch", []any{})
	if err != nil {
		t.Fatalf("AppendMany([]): %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
}

func TestAppendMany_ULIDsAreMonotonic(t *testing.T) {
	s := newTestStore(t)
	contents := make([]any, 10)
	for i := range contents {
		contents[i] = i
	}
	entries, err := s.AppendMany("scratch", contents)
	if err != nil {
		t.Fatalf("AppendMany: %v", err)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].ID <= entries[i-1].ID {
			t.Errorf("ULIDs not monotonically increasing at index %d: %s <= %s",
				i, entries[i].ID, entries[i-1].ID)
		}
	}
}

func TestAppendMany_RejectsInvalidNamespace(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AppendMany("with/slash", []any{"x"}); err == nil {
		t.Error("expected error for invalid namespace")
	}
}

func TestDelete_TombstoneHidesEntry(t *testing.T) {
	s := newTestStore(t)
	entry, err := s.Append("scratch", mustJSON(t, "doomed"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Delete("scratch", entry.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	results, err := s.Get("scratch", "", 0, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected tombstoned entry hidden; got %d results", len(results))
	}
}

func TestDelete_IsIdempotent(t *testing.T) {
	s := newTestStore(t)
	entry, _ := s.Append("scratch", mustJSON(t, "x"))
	if err := s.Delete("scratch", entry.ID); err != nil {
		t.Fatalf("delete 1: %v", err)
	}
	if err := s.Delete("scratch", entry.ID); err != nil {
		t.Fatalf("delete 2: %v", err)
	}
}

func TestDelete_RejectsInvalidID(t *testing.T) {
	s := newTestStore(t)
	if err := s.Delete("scratch", "not-a-ulid"); err == nil {
		t.Error("expected error for invalid ID")
	}
}

func TestGet_JqFilterNestedFieldMatch(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"tag": "espresso", "shot": 18.0}))
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"tag": "filter", "shot": 22.0}))
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"tag": "espresso", "shot": 19.0}))

	results, err := s.Get("scratch", `select(.content.tag == "espresso")`, 0, nil)
	if err != nil {
		t.Fatalf("get with jq: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 espresso entries, got %d", len(results))
	}
}

func TestGet_LastTakesFinalN(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 5; i++ {
		_, _ = s.Append("scratch", mustJSON(t, i))
	}
	results, err := s.Get("scratch", "", 2, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Last two should be content=3 and content=4. json.Unmarshal of integers
	// into `any` produces float64 per the Go JSON convention.
	last := results[len(results)-1].(Entry)
	if got, _ := last.Content.(float64); got != 4 {
		t.Errorf("expected final entry content=4, got %v", last.Content)
	}
}

func TestGet_JqRunsBeforeLast(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 10; i++ {
		even := i%2 == 0
		_, _ = s.Append("scratch", mustJSON(t, map[string]any{"i": i, "even": even}))
	}
	results, err := s.Get("scratch", `select(.content.even)`, 2, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 even results, got %d", len(results))
	}
}

func TestGet_EmptyNamespaceReturnsEmpty(t *testing.T) {
	s := newTestStore(t)
	results, err := s.Get("never-touched", "", 0, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected empty, got %v", results)
	}
}

func TestGet_InvalidJqReturnsError(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, 1))
	if _, err := s.Get("scratch", "(((not jq", 0, nil); err == nil {
		t.Error("expected parse error")
	}
}

func TestListNamespaces_ReturnsSummaries(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("alpha", mustJSON(t, 1))
	_, _ = s.Append("alpha", mustJSON(t, 2))
	_, _ = s.Append("beta", mustJSON(t, 3))
	ns, err := s.ListNamespaces()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]int{}
	for _, n := range ns {
		got[n.Name] = n.EntryCount
		if n.LastTS == "" {
			t.Errorf("expected last_ts on %s", n.Name)
		}
	}
	if got["alpha"] != 2 {
		t.Errorf("alpha count: got %d, want 2", got["alpha"])
	}
	if got["beta"] != 1 {
		t.Errorf("beta count: got %d, want 1", got["beta"])
	}
}

func TestDescribe_BasicCounts(t *testing.T) {
	s := newTestStore(t)
	e1, _ := s.Append("scratch", mustJSON(t, map[string]any{"tag": "espresso"}))
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"tag": "espresso"}))
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"tag": "filter"}))
	_ = s.Delete("scratch", e1.ID)

	d, err := s.Describe("scratch", "")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if d.EntryCount != 2 {
		t.Errorf("entry_count: got %d, want 2", d.EntryCount)
	}
	if d.Tombstoned != 1 {
		t.Errorf("tombstoned: got %d, want 1", d.Tombstoned)
	}
	if d.FirstTS == "" || d.LastTS == "" {
		t.Errorf("expected ts range, got first=%q last=%q", d.FirstTS, d.LastTS)
	}
	if len(d.Distinct) != 0 {
		t.Errorf("expected no distinct without field, got %v", d.Distinct)
	}
}

func TestDescribe_DistinctValuesOnField(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"tag": "espresso"}))
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"tag": "espresso"}))
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"tag": "filter"}))
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"tag": "espresso"}))

	d, err := s.Describe("scratch", ".content.tag")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if len(d.Distinct) != 2 {
		t.Fatalf("expected 2 distinct tags, got %d (%v)", len(d.Distinct), d.Distinct)
	}
	// Sorted by count desc, so espresso (3) comes first.
	if d.Distinct[0].Value != "espresso" || d.Distinct[0].Count != 3 {
		t.Errorf("first distinct: got %+v, want {espresso 3}", d.Distinct[0])
	}
	if d.Distinct[1].Value != "filter" || d.Distinct[1].Count != 1 {
		t.Errorf("second distinct: got %+v, want {filter 1}", d.Distinct[1])
	}
}

func TestDescribe_EmptyNamespace(t *testing.T) {
	s := newTestStore(t)
	d, err := s.Describe("never-touched", "")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if d.EntryCount != 0 || d.Tombstoned != 0 || d.FirstTS != "" || d.LastTS != "" {
		t.Errorf("expected zero values, got %+v", d)
	}
}

func TestPersistence_AcrossStoreReopen(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore 1: %v", err)
	}
	entry, _ := s1.Append("scratch", mustJSON(t, "durable"))

	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore 2: %v", err)
	}
	results, err := s2.Get("scratch", "", 0, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result post-reopen, got %d", len(results))
	}
	got := results[0].(Entry)
	if got.ID != entry.ID {
		t.Errorf("expected id %s, got %s", entry.ID, got.ID)
	}
}

// ---- describe_namespace auto-shape tests ----

func TestDescribe_Shape_InfersSchema(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"tag": "espresso", "shot": 18.0}))
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"tag": "filter", "shot": 22.0}))

	d, err := s.Describe("scratch", "")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if len(d.Shape) == 0 {
		t.Fatal("expected shape, got none")
	}
	byPath := map[string]FieldShape{}
	for _, fs := range d.Shape {
		byPath[fs.Path] = fs
	}
	tag, ok := byPath[".content.tag"]
	if !ok {
		t.Fatalf("expected .content.tag in shape; got paths: %v", func() []string {
			ps := make([]string, 0, len(byPath))
			for p := range byPath {
				ps = append(ps, p)
			}
			return ps
		}())
	}
	if tag.Count != 2 {
		t.Errorf(".content.tag count: got %d, want 2", tag.Count)
	}
	if tag.DistinctCount != 2 {
		t.Errorf(".content.tag distinct_count: got %d, want 2", tag.DistinctCount)
	}
	if len(tag.Types) != 1 || tag.Types[0] != "string" {
		t.Errorf(".content.tag types: got %v, want [string]", tag.Types)
	}
	if len(tag.SampleValues) == 0 {
		t.Error(".content.tag: expected sample values")
	}
}

func TestDescribe_Shape_MixedTypes(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"val": "hello"}))
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"val": 42.0}))
	_, _ = s.Append("scratch", mustJSON(t, "just a string")) // non-object content

	d, err := s.Describe("scratch", "")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	byPath := map[string]FieldShape{}
	for _, fs := range d.Shape {
		byPath[fs.Path] = fs
	}

	val, ok := byPath[".content.val"]
	if !ok {
		t.Fatalf("expected .content.val in shape")
	}
	if len(val.Types) != 2 {
		t.Errorf(".content.val types: got %v, want [number string]", val.Types)
	}

	// The primitive "just a string" entry should contribute to .content directly.
	contentField, ok := byPath[".content"]
	if !ok {
		t.Fatalf("expected .content in shape for primitive entry")
	}
	if len(contentField.Types) != 1 || contentField.Types[0] != "string" {
		t.Errorf(".content types: got %v, want [string]", contentField.Types)
	}
}

func TestDescribe_Shape_NotSetWhenFieldGiven(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"tag": "x"}))

	d, err := s.Describe("scratch", ".content.tag")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if len(d.Shape) != 0 {
		t.Errorf("expected no shape when field is given, got %v", d.Shape)
	}
	if len(d.Distinct) == 0 {
		t.Error("expected distinct values when field is given")
	}
}

// ---- search tests ----

func TestSearch_SubstringHit(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"note": "best espresso"}))
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"note": "filter coffee"}))

	hits, err := s.Search("espresso", "scratch", false, 0, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	if !strings.Contains(hits[0].Snippet, "espresso") {
		t.Errorf("snippet missing match text: %q", hits[0].Snippet)
	}
	if hits[0].ID == "" || hits[0].TS == "" {
		t.Error("expected non-empty id and ts on hit")
	}
}

func TestSearch_RegexHit(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, "shot 18g"))
	_, _ = s.Append("scratch", mustJSON(t, "shot 22g"))
	_, _ = s.Append("scratch", mustJSON(t, "no match here"))

	hits, err := s.Search(`shot \d+g`, "scratch", true, 0, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 regex hits, got %d", len(hits))
	}
}

func TestSearch_SkipsTombstoned(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Append("scratch", mustJSON(t, "doomed espresso"))
	_, _ = s.Append("scratch", mustJSON(t, "survivor"))
	_ = s.Delete("scratch", e.ID)

	hits, err := s.Search("espresso", "scratch", false, 0, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits (tombstoned entry excluded), got %d", len(hits))
	}
}

func TestSearch_AllNamespaces(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("alpha", mustJSON(t, "espresso here"))
	_, _ = s.Append("beta", mustJSON(t, "espresso there"))
	_, _ = s.Append("gamma", mustJSON(t, "no match"))

	hits, err := s.Search("espresso", "", false, 0, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits across namespaces, got %d", len(hits))
	}
	ns := map[string]bool{}
	for _, h := range hits {
		ns[h.Namespace] = true
	}
	if !ns["alpha"] || !ns["beta"] {
		t.Errorf("expected hits in alpha and beta; got namespaces: %v", ns)
	}
}

func TestSearch_LimitCaps(t *testing.T) {
	s := newTestStore(t)
	for range 5 {
		_, _ = s.Append("scratch", mustJSON(t, "entry"))
	}
	hits, err := s.Search("entry", "scratch", false, 3, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("expected 3 hits (limit=3), got %d", len(hits))
	}
}

func TestSearch_EmptyQueryError(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Search("", "scratch", false, 0, nil); err == nil {
		t.Error("expected error for empty query")
	}
}

func TestSearch_InvalidRegexError(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, "x"))
	if _, err := s.Search(`[invalid`, "scratch", true, 0, nil); err == nil {
		t.Error("expected error for invalid regex")
	}
}

func TestSearch_EmptyNamespaceReturnsEmpty(t *testing.T) {
	s := newTestStore(t)
	hits, err := s.Search("anything", "never-touched", false, 0, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits for empty namespace, got %d", len(hits))
	}
}

// ---- fix: compactSample + depth-2 suppression ----

func TestDescribe_Shape_SamplesAreCompacted(t *testing.T) {
	s := newTestStore(t)
	longStr := strings.Repeat("x", 100)
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"note": longStr}))
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"nested": map[string]any{"a": 1, "b": 2}}))
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"tags": []any{"a", "b", "c"}}))

	d, err := s.Describe("scratch", "")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	byPath := map[string]FieldShape{}
	for _, fs := range d.Shape {
		byPath[fs.Path] = fs
	}

	note := byPath[".content.note"]
	if len(note.SampleValues) == 0 {
		t.Fatal("expected sample for .content.note")
	}
	sample, ok := note.SampleValues[0].(string)
	if !ok {
		t.Fatalf("expected string sample, got %T", note.SampleValues[0])
	}
	if len([]rune(sample)) > 84 {
		t.Errorf("string sample not truncated: len=%d", len([]rune(sample)))
	}
	if !strings.HasSuffix(sample, "...") {
		t.Errorf("expected truncation ellipsis, got %q", sample)
	}

	nested := byPath[".content.nested"]
	if len(nested.SampleValues) == 0 {
		t.Fatal("expected sample for .content.nested")
	}
	nestedSample, ok := nested.SampleValues[0].(string)
	if !ok {
		t.Fatalf("expected string token for object sample, got %T", nested.SampleValues[0])
	}
	if !strings.HasPrefix(nestedSample, "object (") {
		t.Errorf("expected object token, got %q", nestedSample)
	}

	tags := byPath[".content.tags"]
	if len(tags.SampleValues) == 0 {
		t.Fatal("expected sample for .content.tags")
	}
	tagsSample, ok := tags.SampleValues[0].(string)
	if !ok {
		t.Fatalf("expected string token for array sample, got %T", tags.SampleValues[0])
	}
	if !strings.HasPrefix(tagsSample, "array[") {
		t.Errorf("expected array token, got %q", tagsSample)
	}

	if _, ok := byPath[".content.nested.a"]; ok {
		t.Error(".content.nested.a should be filtered (count=1 at depth-2)")
	}
}

func TestDescribe_Shape_MapFieldsSuppressed(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{
		"individuals": map[string]any{"alice": map[string]any{"age": 30}},
	}))
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{
		"individuals": map[string]any{"bob": map[string]any{"age": 25}},
	}))

	d, err := s.Describe("scratch", "")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	byPath := map[string]FieldShape{}
	for _, fs := range d.Shape {
		byPath[fs.Path] = fs
	}

	if _, ok := byPath[".content.individuals"]; !ok {
		t.Error("expected .content.individuals in shape")
	}
	if _, ok := byPath[".content.individuals.alice"]; ok {
		t.Error(".content.individuals.alice should be suppressed (count=1)")
	}
	if _, ok := byPath[".content.individuals.bob"]; ok {
		t.Error(".content.individuals.bob should be suppressed (count=1)")
	}
}

func TestDescribe_FieldMode_SkipsErroringEntries(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"tag": "espresso"}))
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"tag": "filter"}))
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"other": "field"}))

	d, err := s.Describe("scratch", `.content | if has("tag") then .tag else error("no tag") end`)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if d.ErroredCount != 1 {
		t.Errorf("expected errored_count=1, got %d", d.ErroredCount)
	}
	if len(d.Distinct) != 2 {
		t.Fatalf("expected 2 distinct values, got %d", len(d.Distinct))
	}
}

func TestDescribe_MalformedLine_CountedNotErrored(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"tag": "good"}))

	// Inject a malformed line directly into the JSONL.
	f, err := os.OpenFile(filepath.Join(dir, "scratch.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open jsonl: %v", err)
	}
	_, _ = f.WriteString("\nnot valid json\n")
	f.Close()

	d, err := s.Describe("scratch", "")
	if err != nil {
		t.Fatalf("expected no error on malformed line, got %v", err)
	}
	if d.EntryCount != 1 {
		t.Errorf("expected entry_count=1, got %d", d.EntryCount)
	}
	if d.ErroredCount != 1 {
		t.Errorf("expected errored_count=1, got %d", d.ErroredCount)
	}
}

func TestSearch_SnippetCentresOnContent(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, "unique-term-here"))

	hits, err := s.Search("unique-term-here", "scratch", false, 0, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	if strings.HasPrefix(hits[0].Snippet, `{"id":`) {
		t.Errorf("snippet should not start with envelope; got %q", hits[0].Snippet)
	}
	if !strings.Contains(hits[0].Snippet, "unique-term-here") {
		t.Errorf("snippet should contain the match; got %q", hits[0].Snippet)
	}
}

// ---- timestamp truncation tests ----

func TestAppend_TSTruncatedToSeconds(t *testing.T) {
	s := newTestStore(t)
	e, err := s.Append("scratch", mustJSON(t, "x"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	// RFC3339 (second precision) parses cleanly and has no sub-second component.
	parsed, err := time.Parse(time.RFC3339, e.TS)
	if err != nil {
		t.Fatalf("ts not RFC3339: %q, err: %v", e.TS, err)
	}
	if parsed.Nanosecond() != 0 {
		t.Errorf("expected second precision, got nanosecond=%d in %q", parsed.Nanosecond(), e.TS)
	}
	// Verify on-disk remains full precision.
	results, _ := s.Get("scratch", "", 0, nil)
	got := results[0].(Entry)
	if got.TS != e.TS {
		// both should be RFC3339; make sure the returned value is also truncated
		t.Errorf("Get returned ts %q, Append returned %q; expected same", got.TS, e.TS)
	}
}

func TestGet_TSTruncatedToSeconds(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, "x"))

	results, err := s.Get("scratch", "", 0, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	e := results[0].(Entry)
	if _, err := time.Parse(time.RFC3339, e.TS); err != nil {
		t.Errorf("ts not RFC3339: %q, err: %v", e.TS, err)
	}
	if strings.Contains(e.TS, ".") {
		t.Errorf("ts has sub-second component: %q", e.TS)
	}
}

func TestSearch_TSTruncatedToSeconds(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, "findme"))

	hits, err := s.Search("findme", "scratch", false, 0, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	if _, err := time.Parse(time.RFC3339, hits[0].TS); err != nil {
		t.Errorf("hit ts not RFC3339: %q", hits[0].TS)
	}
	if strings.Contains(hits[0].TS, ".") {
		t.Errorf("hit ts has sub-second component: %q", hits[0].TS)
	}
}

// ---- sensitive entry tests ----
//
// Sensitivity is declared in content: entries whose content is a JSON object
// with "exportable": false are treated as sensitive. exportable:true, null,
// absent, and non-object content are all non-sensitive.

func sensitiveContent(msg string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"exportable": false, "msg": msg})
	return b
}

func TestSensitive_HiddenWhenDefaultExclude(t *testing.T) {
	t.Setenv("NOTEBOOK_SENSITIVE_DEFAULT", "exclude")
	s := newTestStore(t)
	hidden, _ := s.Append("scratch", sensitiveContent("secret"))
	visible, _ := s.Append("scratch", mustJSON(t, "public"))

	results, err := s.Get("scratch", "", 0, nil) // nil = use default = exclude
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 visible entry, got %d", len(results))
	}
	got := results[0].(Entry)
	if got.ID == hidden.ID {
		t.Error("sensitive entry should be hidden by default when NOTEBOOK_SENSITIVE_DEFAULT=exclude")
	}
	if got.ID != visible.ID {
		t.Error("public entry should be visible")
	}
}

func TestSensitive_IncludeOverride(t *testing.T) {
	t.Setenv("NOTEBOOK_SENSITIVE_DEFAULT", "exclude")
	s := newTestStore(t)
	_, _ = s.Append("scratch", sensitiveContent("secret"))
	_, _ = s.Append("scratch", mustJSON(t, "public"))

	results, err := s.Get("scratch", "", 0, boolp(true))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 entries with include_sensitive=true, got %d", len(results))
	}
}

func TestSensitive_ExcludeOverride(t *testing.T) {
	// Default is include; explicit false should exclude.
	s := newTestStore(t)
	_, _ = s.Append("scratch", sensitiveContent("secret"))
	_, _ = s.Append("scratch", mustJSON(t, "public"))

	results, err := s.Get("scratch", "", 0, boolp(false))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result with include_sensitive=false, got %d", len(results))
	}
	got := results[0].(Entry)
	if got.Content.(string) != "public" {
		t.Errorf("expected public entry, got %v", got.Content)
	}
}

func TestSensitive_IncludedByDefaultWithoutEnvVar(t *testing.T) {
	// No env var → default is include, so sensitive entries are visible.
	s := newTestStore(t)
	_, _ = s.Append("scratch", sensitiveContent("secret"))

	results, err := s.Get("scratch", "", 0, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 entry (sensitive included by default), got %d", len(results))
	}
}

func TestSensitive_ExportableNullNotSensitive(t *testing.T) {
	// exportable:null is not treated as sensitive — only false is.
	t.Setenv("NOTEBOOK_SENSITIVE_DEFAULT", "exclude")
	s := newTestStore(t)
	nullContent, _ := json.Marshal(map[string]any{"exportable": nil, "msg": "ambiguous"})
	_, _ = s.Append("scratch", nullContent)

	results, err := s.Get("scratch", "", 0, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result (exportable:null not sensitive), got %d", len(results))
	}
}

func TestSensitive_NonObjectContentNotSensitive(t *testing.T) {
	t.Setenv("NOTEBOOK_SENSITIVE_DEFAULT", "exclude")
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, "just a string"))
	_, _ = s.Append("scratch", mustJSON(t, 42.0))

	results, err := s.Get("scratch", "", 0, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results (non-object content never sensitive), got %d", len(results))
	}
}

func TestSensitive_SearchExcludesWhenDefaultExclude(t *testing.T) {
	t.Setenv("NOTEBOOK_SENSITIVE_DEFAULT", "exclude")
	s := newTestStore(t)
	_, _ = s.Append("scratch", sensitiveContent("espresso secret note"))
	_, _ = s.Append("scratch", mustJSON(t, "public espresso note"))

	hits, err := s.Search("espresso", "scratch", false, 0, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit (sensitive excluded by default), got %d", len(hits))
	}
	if !strings.Contains(hits[0].Snippet, "public") {
		t.Errorf("expected public hit, got %q", hits[0].Snippet)
	}
}

func TestSensitive_BulkAppendMany(t *testing.T) {
	t.Setenv("NOTEBOOK_SENSITIVE_DEFAULT", "exclude")
	s := newTestStore(t)
	_, _ = s.AppendMany("scratch", []any{
		map[string]any{"exportable": false, "msg": "sec1"},
		map[string]any{"exportable": false, "msg": "sec2"},
	})
	_, _ = s.Append("scratch", mustJSON(t, "public"))

	results, err := s.Get("scratch", "", 0, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 visible entry (2 sensitive hidden), got %d", len(results))
	}
}

func TestSensitive_PersistsAcrossReopen(t *testing.T) {
	// Sensitivity is in content, so it persists trivially — no separate file needed.
	t.Setenv("NOTEBOOK_SENSITIVE_DEFAULT", "exclude")
	dir := t.TempDir()
	s1, _ := NewStore(dir)
	_, _ = s1.Append("scratch", sensitiveContent("secret"))
	_, _ = s1.Append("scratch", mustJSON(t, "public"))

	s2, _ := NewStore(dir)
	results, err := s2.Get("scratch", "", 0, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 visible entry after reopen, got %d", len(results))
	}
}

// ---- string-JSON deserialise tests ----

func TestGet_DeserialiseStringObject(t *testing.T) {
	s := newTestStore(t)
	// Simulate a client that pre-serialised a structured value as a JSON string.
	raw := json.RawMessage(`"{\"tag\":\"espresso\",\"shot\":18}"`)
	_, err := s.Append("scratch", raw)
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	results, err := s.Get("scratch", "", 0, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	e := results[0].(Entry)
	m, ok := e.Content.(map[string]any)
	if !ok {
		t.Fatalf("expected content deserialised to map, got %T: %v", e.Content, e.Content)
	}
	if m["tag"] != "espresso" {
		t.Errorf("expected tag=espresso, got %v", m["tag"])
	}
}

func TestGet_DeserialiseStringArray(t *testing.T) {
	s := newTestStore(t)
	raw := json.RawMessage(`"[1,2,3]"`)
	_, _ = s.Append("scratch", raw)

	results, _ := s.Get("scratch", "", 0, nil)
	e := results[0].(Entry)
	if _, ok := e.Content.([]any); !ok {
		t.Fatalf("expected content deserialised to array, got %T", e.Content)
	}
}

func TestGet_StringNotDeserialised_WhenNotJSON(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, "just a plain string"))

	results, _ := s.Get("scratch", "", 0, nil)
	e := results[0].(Entry)
	if _, ok := e.Content.(string); !ok {
		t.Errorf("plain string should remain a string, got %T", e.Content)
	}
}

func TestGet_StringNotDeserialised_WhenJSONPrimitive(t *testing.T) {
	// A string whose value is a JSON primitive (e.g. "true", "42") stays a string.
	s := newTestStore(t)
	raw := json.RawMessage(`"true"`)
	_, _ = s.Append("scratch", raw)

	results, _ := s.Get("scratch", "", 0, nil)
	e := results[0].(Entry)
	if _, ok := e.Content.(string); !ok {
		t.Errorf("JSON-primitive string should remain a string, got %T", e.Content)
	}
}

func TestGet_JqSeesDeserialised(t *testing.T) {
	s := newTestStore(t)
	// Store a pre-serialised object.
	raw := json.RawMessage(`"{\"tag\":\"espresso\"}"`)
	_, _ = s.Append("scratch", raw)
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"tag": "filter"}))

	// jq should see the deserialised content, not the raw string.
	results, err := s.Get("scratch", `select(.content.tag == "espresso")`, 0, nil)
	if err != nil {
		t.Fatalf("get with jq: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d (jq may not see deserialised content)", len(results))
	}
}

// ---- update tests ----

func mustUpdate(t *testing.T, s *Store, ns, id, field string, value any) UpdateResult {
	t.Helper()
	r, err := s.Update(ns, id, field, value, false)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	return r
}

func updateCode(t *testing.T, s *Store, ns, id, field string, value any, sensitive bool) string {
	t.Helper()
	_, err := s.Update(ns, id, field, value, sensitive)
	if err == nil {
		t.Fatal("expected Update error, got nil")
	}
	var ue *UpdateError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *UpdateError, got %T: %v", err, err)
	}
	return ue.Code
}

func TestUpdate_BasicFieldUpdate(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Append("scratch", mustJSON(t, map[string]any{"date": "2026-06-06", "body": "Pact is roast-dated"}))

	r := mustUpdate(t, s, "scratch", e.ID, "body", "Pact at Waitrose is roast-dated")
	if r.Old != "Pact is roast-dated" {
		t.Errorf("old: got %v, want 'Pact is roast-dated'", r.Old)
	}
	if r.New != "Pact at Waitrose is roast-dated" {
		t.Errorf("new: got %v, want 'Pact at Waitrose is roast-dated'", r.New)
	}
	if r.Field != "body" {
		t.Errorf("field: got %q, want 'body'", r.Field)
	}
	if r.UpdateTS == "" {
		t.Error("expected non-empty update_ts")
	}

	// Get should return updated content.
	results, _ := s.Get("scratch", "", 0, nil)
	entry := results[0].(Entry)
	m, ok := entry.Content.(map[string]any)
	if !ok {
		t.Fatalf("expected map content, got %T", entry.Content)
	}
	if m["body"] != "Pact at Waitrose is roast-dated" {
		t.Errorf("get returned stale body: %v", m["body"])
	}
	if m["date"] != "2026-06-06" {
		t.Errorf("sibling field mutated: %v", m["date"])
	}
}

func TestUpdate_PlainStringEntry(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Append("scratch", mustJSON(t, "3 weeks off roast"))

	r := mustUpdate(t, s, "scratch", e.ID, ".", "6 weeks off roast")
	if r.Old != "3 weeks off roast" {
		t.Errorf("old: got %v", r.Old)
	}
	if r.New != "6 weeks off roast" {
		t.Errorf("new: got %v", r.New)
	}

	results, _ := s.Get("scratch", "", 0, nil)
	entry := results[0].(Entry)
	if entry.Content != "6 weeks off roast" {
		t.Errorf("expected updated string, got %v", entry.Content)
	}
}

func TestUpdate_MultipleUpdates_LastWins(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Append("scratch", mustJSON(t, map[string]any{"note": "v1"}))

	mustUpdate(t, s, "scratch", e.ID, "note", "v2")
	mustUpdate(t, s, "scratch", e.ID, "note", "v3")

	results, _ := s.Get("scratch", "", 0, nil)
	entry := results[0].(Entry)
	if m, ok := entry.Content.(map[string]any); ok {
		if m["note"] != "v3" {
			t.Errorf("expected last update to win: got %v", m["note"])
		}
	} else {
		t.Fatalf("expected map, got %T", entry.Content)
	}
}

func TestUpdate_EntryNotFound(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, "x"))
	// Valid ULID format (26 Crockford base32 chars) that is not in the namespace.
	fakeID := "00000000000000000000000000"
	if code := updateCode(t, s, "scratch", fakeID, ".", "y", false); code != "ENTRY_NOT_FOUND" {
		t.Errorf("expected ENTRY_NOT_FOUND, got %s", code)
	}
}

func TestUpdate_TombstonedEntry(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Append("scratch", mustJSON(t, map[string]any{"x": "y"}))
	_ = s.Delete("scratch", e.ID)

	if code := updateCode(t, s, "scratch", e.ID, "x", "z", false); code != "ENTRY_TOMBSTONED" {
		t.Errorf("expected ENTRY_TOMBSTONED, got %s", code)
	}
}

func TestUpdate_ImmutableField_ID(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Append("scratch", mustJSON(t, map[string]any{"x": "y"}))
	if code := updateCode(t, s, "scratch", e.ID, "id", "newid", false); code != "IMMUTABLE_FIELD" {
		t.Errorf("expected IMMUTABLE_FIELD, got %s", code)
	}
}

func TestUpdate_ImmutableField_TS(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Append("scratch", mustJSON(t, map[string]any{"x": "y"}))
	if code := updateCode(t, s, "scratch", e.ID, "ts", "2020-01-01T00:00:00Z", false); code != "IMMUTABLE_FIELD" {
		t.Errorf("expected IMMUTABLE_FIELD, got %s", code)
	}
}

func TestUpdate_KeyDrift_AddKey(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Append("scratch", mustJSON(t, map[string]any{"body": "original"}))
	if code := updateCode(t, s, "scratch", e.ID, "note", "extra", false); code != "KEY_DRIFT" {
		t.Errorf("expected KEY_DRIFT on new key, got %s", code)
	}
}

func TestUpdate_KeyDrift_NonObjectContent(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Append("scratch", mustJSON(t, "plain string"))
	if code := updateCode(t, s, "scratch", e.ID, "body", "replacement", false); code != "KEY_DRIFT" {
		t.Errorf("expected KEY_DRIFT when targeting named field on non-object, got %s", code)
	}
}

func TestUpdate_TypeMismatch(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Append("scratch", mustJSON(t, map[string]any{"count": 5.0}))
	if code := updateCode(t, s, "scratch", e.ID, "count", "five", false); code != "TYPE_MISMATCH" {
		t.Errorf("expected TYPE_MISMATCH, got %s", code)
	}
}

func TestUpdate_TooLong(t *testing.T) {
	s := newTestStore(t)
	original := strings.Repeat("a", 50)
	e, _ := s.Append("scratch", mustJSON(t, map[string]any{"body": original}))
	// max = max(100, 150) = 150; 200 chars exceeds that
	tooLong := strings.Repeat("b", 200)
	_, err := s.Update("scratch", e.ID, "body", tooLong, false)
	if err == nil {
		t.Fatal("expected TOO_LONG error")
	}
	var ue *UpdateError
	if !errors.As(err, &ue) || ue.Code != "TOO_LONG" {
		t.Fatalf("expected TOO_LONG UpdateError, got %v", err)
	}
	if ue.OriginalLength != 50 {
		t.Errorf("original_length: got %d, want 50", ue.OriginalLength)
	}
	if ue.MaxAllowed != 150 {
		t.Errorf("max_allowed: got %d, want 150", ue.MaxAllowed)
	}
	if ue.Provided != 200 {
		t.Errorf("provided: got %d, want 200", ue.Provided)
	}
}

func TestUpdate_TooLong_ShortStringsUse100Floor(t *testing.T) {
	s := newTestStore(t)
	// original = 10 chars; max = max(20, 110) = 110; 111 chars should fail
	e, _ := s.Append("scratch", mustJSON(t, map[string]any{"body": "ten chars!"}))
	tooLong := strings.Repeat("x", 111)
	if code := updateCode(t, s, "scratch", e.ID, "body", tooLong, false); code != "TOO_LONG" {
		t.Errorf("expected TOO_LONG, got %s", code)
	}
	// 110 chars should pass
	exactlyMax := strings.Repeat("x", 110)
	mustUpdate(t, s, "scratch", e.ID, "body", exactlyMax)
}

func TestUpdate_SensitiveRequiresOptIn(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Append("scratch", sensitiveContent("original"))
	if code := updateCode(t, s, "scratch", e.ID, "msg", "corrected", false); code != "SENSITIVE_REQUIRES_OPT_IN" {
		t.Errorf("expected SENSITIVE_REQUIRES_OPT_IN, got %s", code)
	}
}

func TestUpdate_SensitiveWithOptIn(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Append("scratch", sensitiveContent("original"))
	r, err := s.Update("scratch", e.ID, "msg", "corrected", true)
	if err != nil {
		t.Fatalf("Update with include_sensitive=true: %v", err)
	}
	if r.Old != "original" {
		t.Errorf("old: got %v", r.Old)
	}
}

func TestUpdate_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s1, _ := NewStore(dir)
	e, _ := s1.Append("scratch", mustJSON(t, map[string]any{"body": "before"}))
	_, _ = s1.Update("scratch", e.ID, "body", "after", false)

	s2, _ := NewStore(dir)
	results, _ := s2.Get("scratch", "", 0, nil)
	if len(results) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(results))
	}
	entry := results[0].(Entry)
	if m, ok := entry.Content.(map[string]any); ok {
		if m["body"] != "after" {
			t.Errorf("update not persisted: got %v", m["body"])
		}
	} else {
		t.Fatalf("expected map, got %T", entry.Content)
	}
}

func TestUpdate_TombstonedAfterUpdateHidesEntry(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Append("scratch", mustJSON(t, map[string]any{"body": "original"}))
	mustUpdate(t, s, "scratch", e.ID, "body", "corrected")
	_ = s.Delete("scratch", e.ID)

	results, _ := s.Get("scratch", "", 0, nil)
	if len(results) != 0 {
		t.Errorf("tombstoned entry should be hidden even after update; got %d results", len(results))
	}
}

func TestUpdate_DoesNotInflateDescribeCount(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Append("scratch", mustJSON(t, map[string]any{"body": "original"}))
	mustUpdate(t, s, "scratch", e.ID, "body", "corrected")

	d, err := s.Describe("scratch", "")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if d.EntryCount != 1 {
		t.Errorf("describe should count 1 entry, got %d (update records should not inflate count)", d.EntryCount)
	}
}

func TestUpdate_SearchSkipsUpdateRecords(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, map[string]any{"note": "espresso"}))
	e2, _ := s.Append("scratch", mustJSON(t, map[string]any{"note": "filter"}))
	mustUpdate(t, s, "scratch", e2.ID, "note", "espresso update")

	// search operates on raw JSONL; update records are skipped as hits
	hits, err := s.Search("espresso", "scratch", false, 0, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, h := range hits {
		if h.ID == e2.ID {
			// the update record for e2 should not appear as a hit
			// (the original entry for e2 says "filter" and won't match)
			t.Errorf("unexpected hit for e2 (the update record should be skipped): %+v", h)
		}
	}
	if len(hits) != 1 {
		t.Errorf("expected 1 hit (original espresso entry), got %d", len(hits))
	}
}
