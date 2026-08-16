package tools

import (
	"strings"
	"testing"
)

func TestTodoReplacesListAndSnapshots(t *testing.T) {
	r, _ := newRegistry(t)

	res := run(t, r, "todo", map[string]any{"items": []map[string]any{
		{"text": "read the failing test", "status": "done"},
		{"text": "fix the off-by-one", "status": "active"},
		{"text": "run the suite", "status": "pending"},
	}})
	if res.IsError {
		t.Fatalf("todo failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "[x] read the failing test") ||
		!strings.Contains(res.Content, "[>] fix the off-by-one") ||
		!strings.Contains(res.Content, "[ ] run the suite") {
		t.Errorf("result must render the list with markers: %q", res.Content)
	}
	if !strings.Contains(res.Content, "1 of 3 done") {
		t.Errorf("result must summarize progress: %q", res.Content)
	}

	items := r.Todos()
	if len(items) != 3 || items[1].Status != TodoActive {
		t.Fatalf("snapshot = %+v, want the three items as sent", items)
	}

	// The next call replaces, never appends.
	run(t, r, "todo", map[string]any{"items": []map[string]any{
		{"text": "only one left", "status": "active"},
	}})
	if items := r.Todos(); len(items) != 1 {
		t.Errorf("second call must replace the list, got %+v", items)
	}
}

func TestTodoClearsOnEmptyList(t *testing.T) {
	r, _ := newRegistry(t)
	run(t, r, "todo", map[string]any{"items": []map[string]any{
		{"text": "something", "status": "pending"},
	}})

	res := run(t, r, "todo", map[string]any{"items": []map[string]any{}})
	if !strings.Contains(res.Content, "cleared") {
		t.Errorf("clearing must say so: %q", res.Content)
	}
	if items := r.Todos(); len(items) != 0 {
		t.Errorf("list not cleared: %+v", items)
	}
}

func TestTodoRejectsMalformedLists(t *testing.T) {
	r, _ := newRegistry(t)

	cases := []struct {
		name  string
		items []map[string]any
	}{
		{"two active items", []map[string]any{
			{"text": "a", "status": "active"},
			{"text": "b", "status": "active"},
		}},
		{"unknown status", []map[string]any{
			{"text": "a", "status": "blocked"},
		}},
		{"empty text", []map[string]any{
			{"text": "  ", "status": "pending"},
		}},
	}
	for _, c := range cases {
		if _, err := tryRun(r, "todo", map[string]any{"items": c.items}); err == nil {
			t.Errorf("%s must fail at Plan time", c.name)
		}
	}

	// A rejected call must not disturb the stored list.
	if items := r.Todos(); len(items) != 0 {
		t.Errorf("a rejected call changed state: %+v", items)
	}
}
