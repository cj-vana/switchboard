package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/cj-vana/switchboard/internal/permission"
)

// TodoStatus is the lifecycle of one task. Three states, no more: a richer
// vocabulary reads as project management, and the model maintaining the list
// is mid-task with a fixed round budget.
type TodoStatus string

const (
	TodoPending TodoStatus = "pending"
	TodoActive  TodoStatus = "active"
	TodoDone    TodoStatus = "done"
)

type TodoItem struct {
	Text   string     `json:"text"`
	Status TodoStatus `json:"status"`
}

// todoState is session-scoped and deliberately not persisted. On resume the
// context that produced the plan is gone, so a replayed list would assert
// intent the new context never formed; the model rebuilds it the same way it
// re-reads files (the fileVersions rule, for plans).
type todoState struct {
	mu    sync.Mutex
	items []TodoItem
}

func (s *todoState) set(items []TodoItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = items
}

// Todos returns a snapshot of the current task list. It is safe to call from
// outside the loop's goroutine, which is how a UI reads it: the ToolEnd event
// says the list changed, this says what it now is.
func (r *Registry) Todos() []TodoItem {
	r.todos.mu.Lock()
	defer r.todos.mu.Unlock()
	return append([]TodoItem(nil), r.todos.items...)
}

const maxTodoItems = 50

type todoTool struct{ r *Registry }

func (t *todoTool) Name() string { return "todo" }

func (t *todoTool) Description() string {
	return "Maintain the task list for the current job. Send the whole list each time; it " +
		"replaces the previous one. Statuses are pending, active, and done, with at most " +
		"one item active. Use it for work with three or more distinct steps: write the " +
		"list before starting, mark each step active when you begin it and done when it " +
		"is finished. Skip it for single-step tasks."
}

// ParallelSafe is false because each call replaces the whole list, and two
// concurrent replacements would leave whichever finished last.
func (t *todoTool) ParallelSafe() bool { return false }

func (t *todoTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "items": {
      "type": "array",
      "description": "The complete task list, replacing the previous one. An empty list clears it.",
      "items": {
        "type": "object",
        "properties": {
          "text": {"type": "string", "description": "The task, imperative and short."},
          "status": {"type": "string", "enum": ["pending", "active", "done"]}
        },
        "required": ["text", "status"]
      }
    }
  },
  "required": ["items"]
}`)
}

type todoInput struct {
	Items []TodoItem `json:"items"`
}

func (t *todoTool) Plan(input json.RawMessage) (Plan, error) {
	var in todoInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("todo: %w", err)
	}
	if len(in.Items) > maxTodoItems {
		return Plan{}, fmt.Errorf("todo: %d items; a list this long is a plan document, not a task list. Keep it under %d", len(in.Items), maxTodoItems)
	}
	active := 0
	for i, item := range in.Items {
		if strings.TrimSpace(item.Text) == "" {
			return Plan{}, fmt.Errorf("todo: item %d has no text", i+1)
		}
		switch item.Status {
		case TodoPending, TodoDone:
		case TodoActive:
			active++
		default:
			return Plan{}, fmt.Errorf("todo: item %d has status %q; use pending, active, or done", i+1, item.Status)
		}
	}
	if active > 1 {
		return Plan{}, fmt.Errorf("todo: %d items are active; work on one thing at a time", active)
	}

	// The list is session state, not an effect on the world, so it carries the
	// read effect: allowed in every mode, plan mode included, because planning
	// is exactly when a task list earns its place.
	return Plan{
		Request: permission.Request{Tool: t.Name(), Effect: permission.EffectRead},
		Run: func(context.Context) (Result, error) {
			t.r.todos.set(in.Items)
			return Result{Content: renderTodos(in.Items)}, nil
		},
	}, nil
}

// renderTodos is the model-facing rendering. Plain markers, one line per
// item: the model pastes from its own previous output when it updates the
// list, so the format has to survive a round trip through its context.
func renderTodos(items []TodoItem) string {
	if len(items) == 0 {
		return "task list cleared"
	}
	done := 0
	var b strings.Builder
	for _, item := range items {
		mark := "[ ]"
		switch item.Status {
		case TodoActive:
			mark = "[>]"
		case TodoDone:
			mark = "[x]"
			done++
		}
		fmt.Fprintf(&b, "%s %s\n", mark, item.Text)
	}
	fmt.Fprintf(&b, "%d of %d done", done, len(items))
	return b.String()
}
