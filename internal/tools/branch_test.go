package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// The frozen-zone half of Branch's contract: whatever a branch may or may
// not run, its schemas render byte-identical to the source's, because they
// sit ahead of a prefix the provider may still hold warm.
func TestBranchDefinitionsRenderByteIdentical(t *testing.T) {
	r, _ := newRegistry(t)
	branch := r.Branch(map[string]string{"exec": "refused for the test"})

	before, err := json.Marshal(r.Definitions())
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(branch.Definitions())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("branching changed the schemas:\nsource %s\nbranch %s", before, after)
	}
}

// The read-state half: the branch starts from the source's record — at
// branch time their contexts hold the same bytes — and the records diverge
// from there, so a read that arms the §6.7 skip in one context never
// answers with a marker in the other.
func TestBranchReadStateIsCopiedNotShared(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, root+"/before.go", "read before the branch\n")
	writeFile(t, root+"/after.go", "read after the branch\n")

	// A full read before branching arms the skip for both: the branch's
	// context is the same bytes, so the marker is as true there.
	run(t, r, "read", map[string]any{"path": "before.go"})
	branch := r.Branch(nil)
	if res := run(t, branch, "read", map[string]any{"path": "before.go"}); !strings.Contains(res.Content, "unchanged") {
		t.Errorf("a file the shared prefix holds re-read as bytes in the branch:\n%s", res.Content)
	}

	// A full read after branching arms only the context that did it. The
	// other context never received the bytes, so a marker there would tell
	// the model to rely on content it does not have.
	run(t, branch, "read", map[string]any{"path": "after.go"})
	if res := run(t, r, "read", map[string]any{"path": "after.go"}); strings.Contains(res.Content, "unchanged") {
		t.Errorf("a branch read armed the source's skip:\n%s", res.Content)
	}
	if res := run(t, branch, "read", map[string]any{"path": "after.go"}); !strings.Contains(res.Content, "unchanged") {
		t.Errorf("the branch's own full read did not arm its skip:\n%s", res.Content)
	}
}

// A refused tool keeps its schema and refuses at Plan, so the reason
// reaches the model as a tool error and no permission answer is recorded
// for a call that was going nowhere.
func TestBranchRefusalAnswersWithTheReason(t *testing.T) {
	r, _ := newRegistry(t)
	branch := r.Branch(map[string]string{"exec": "not in a race branch"})

	tool, ok := branch.Get("exec")
	if !ok {
		t.Fatal("the refused tool left the suite; its schema must stay")
	}
	if _, err := tool.Plan(json.RawMessage(`{"command":"true"}`)); err == nil || !strings.Contains(err.Error(), "not in a race branch") {
		t.Errorf("Plan on a refused tool: got %v, want the refusal reason", err)
	}
}

// The branch carries no checkpointer: nothing it runs may mutate, and an
// undo scope for turns that mutate nothing would file checkpoints under a
// session /undo never sees. Refusing mutation is the permission engine's
// job, not the registry's, so this pins only that no capture happens.
func TestBranchDropsTheCheckpointer(t *testing.T) {
	r, root := newRegistry(t)
	rec := &captureRecorder{}
	r.SetCheckpoints(rec)
	branch := r.Branch(nil)

	writeFile(t, root+"/target.go", "original\n")
	run(t, branch, "read", map[string]any{"path": "target.go"})
	run(t, branch, "write", map[string]any{"path": "target.go", "content": "changed\n"})
	if len(rec.paths) != 0 {
		t.Errorf("a branch write reached the source's checkpointer: %v", rec.paths)
	}
}

type captureRecorder struct{ paths []string }

func (c *captureRecorder) Record(abs string) { c.paths = append(c.paths, abs) }

// A branch's todo list is its own; two arms sharing one list would render
// each other's plans.
func TestBranchStartsWithItsOwnTodos(t *testing.T) {
	r, _ := newRegistry(t)
	run(t, r, "todo", map[string]any{"items": []map[string]any{{"text": "source task", "status": "pending"}}})
	branch := r.Branch(nil)
	if todos := branch.Todos(); len(todos) != 0 {
		t.Errorf("the branch inherited the source's todo list: %v", todos)
	}
}
