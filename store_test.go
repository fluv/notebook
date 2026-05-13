package main

import (
	"encoding/json"
	"reflect"
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
	// Last two should be content=3 and content=4
	last := results[len(results)-1].(Entry)
	if string(last.Content) != "4" {
		t.Errorf("expected final entry content=4, got %s", last.Content)
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

func TestListNamespaces(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Append("alpha", mustJSON(t, 1))
	_, _ = s.Append("beta", mustJSON(t, 2))
	_, _ = s.Append("gamma", mustJSON(t, 3))
	ns, err := s.ListNamespaces()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := map[string]bool{"alpha": true, "beta": true, "gamma": true}
	got := map[string]bool{}
	for _, n := range ns {
		got[n] = true
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("expected %v, got %v", want, got)
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
