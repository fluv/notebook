package main

import (
	"encoding/json"
	"strings"
	"testing"
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

	results, err := s.Get("scratch", "", 0)
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

func TestDelete_TombstoneHidesEntry(t *testing.T) {
	s := newTestStore(t)
	entry, err := s.Append("scratch", mustJSON(t, "doomed"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Delete("scratch", entry.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	results, err := s.Get("scratch", "", 0)
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

	results, err := s.Get("scratch", `select(.content.tag == "espresso")`, 0)
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
	results, err := s.Get("scratch", "", 2)
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
	results, err := s.Get("scratch", `select(.content.even)`, 2)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 even results, got %d", len(results))
	}
}

func TestGet_EmptyNamespaceReturnsEmpty(t *testing.T) {
	s := newTestStore(t)
	results, err := s.Get("never-touched", "", 0)
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
	if _, err := s.Get("scratch", "(((not jq", 0); err == nil {
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
	results, err := s2.Get("scratch", "", 0)
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

	hits, err := s.Search("espresso", "scratch", false, 0)
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

	hits, err := s.Search(`shot \d+g`, "scratch", true, 0)
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

	hits, err := s.Search("espresso", "scratch", false, 0)
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

	hits, err := s.Search("espresso", "", false, 0)
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
	hits, err := s.Search("entry", "scratch", false, 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("expected 3 hits (limit=3), got %d", len(hits))
	}
}

func TestSearch_EmptyQueryError(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Search("", "scratch", false, 0); err == nil {
		t.Error("expected error for empty query")
	}
}

func TestSearch_InvalidRegexError(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("scratch", mustJSON(t, "x"))
	if _, err := s.Search(`[invalid`, "scratch", true, 0); err == nil {
		t.Error("expected error for invalid regex")
	}
}

func TestSearch_EmptyNamespaceReturnsEmpty(t *testing.T) {
	s := newTestStore(t)
	hits, err := s.Search("anything", "never-touched", false, 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits for empty namespace, got %d", len(hits))
	}
}
