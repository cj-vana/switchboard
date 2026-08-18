package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/execution"
)

// An array field named "command" invites whitespace splitting, because that is
// what argv is. Three prose copies of the shell rule lost to that prior in one
// recorded session. A string field named "script" matches the prior instead,
// so the wrong shape is no longer expressible rather than merely refused.
func TestExecTakesExactlyOneOfCommandOrScript(t *testing.T) {
	registry, err := NewRegistry(t.TempDir(), execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := registry.Get("exec")
	if !ok {
		t.Fatal("no exec tool")
	}

	if _, err := tool.Plan(json.RawMessage(`{"command":["echo","hi"]}`)); err != nil {
		t.Fatalf("a direct call was refused: %v", err)
	}
	if _, err := tool.Plan(json.RawMessage(`{"script":"echo hi | tr a-z A-Z"}`)); err != nil {
		t.Fatalf("a script was refused: %v", err)
	}

	for _, bad := range []struct{ input, want string }{
		{`{}`, "exactly one"},
		{`{"command":[]}`, "exactly one"},
		{`{"command":["echo","hi"],"script":"echo hi"}`, "exactly one"},
	} {
		_, err := tool.Plan(json.RawMessage(bad.input))
		if err == nil {
			t.Errorf("%s was accepted", bad.input)
			continue
		}
		if !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s refused with %q, want it to say %q", bad.input, err, bad.want)
		}
	}

	// The retired field is still decoded on purpose: a resumed session replays
	// its own earlier tool_use blocks and a model mimics its history, so
	// ignoring it would run a pipeline as argv[0]. It is refused with the
	// shape that works, which is the thing the model reads at the moment of
	// the mistake.
	_, err = tool.Plan(json.RawMessage(`{"command":["grep -r foo . | head -20"],"shell":true}`))
	if err == nil {
		t.Fatal("the retired shell field was silently accepted")
	}
	if !strings.Contains(err.Error(), `"script"`) || !strings.Contains(err.Error(), "grep -r foo") {
		t.Errorf("the refusal should hand back the call that works, got %q", err)
	}
}
