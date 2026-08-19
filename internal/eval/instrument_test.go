package eval

import (
	"testing"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// Solve rate on a saturated corpus reports the same score whether a task took
// four rounds or thirty, and the changes worth making to a prompt or a tool
// schema mostly move the second number. These two fields are what makes such
// a change measurable at all.
func TestToolErrorClassSeparatesAWrongCallFromAFailedOne(t *testing.T) {
	for _, tc := range []struct{ content, want string }{
		{`exec: pass exactly one of command or script`, "malformed"},
		{`exec: shell is retired, script takes the whole script: {"script": "ls | wc"}`, "malformed"},
		{"json: cannot unmarshal string into Go struct field", "malformed"},
		{"the request was not approved: runs a command", "denied"},
		{"exit status 1\nFAIL github.com/x/y", "ran-and-failed"},
		{"grep: no matches", "ran-and-failed"},
	} {
		if got := toolErrorClass(tc.content); got != tc.want {
			t.Errorf("toolErrorClass(%q) = %q, want %q", tc.content, got, tc.want)
		}
	}
}

// A collector counts a round per model call and a tool error per failed call,
// because those are the two signals a stopping change and a schema change move.
func TestCollectorCountsRoundsAndToolErrors(t *testing.T) {
	c := &usageCollector{byTarget: map[provider.RouteTargetID]provider.Usage{}}
	c.TurnUsage(session.Usage{})
	c.TurnUsage(session.Usage{})
	c.TurnUsage(session.Usage{})
	if c.rounds != 3 {
		t.Fatalf("rounds = %d, want one per model call", c.rounds)
	}

	c.ToolEnd(provider.ToolUse{Name: "exec"}, permission.Request{},
		tools.Result{Content: "exec: pass exactly one of command or script", IsError: true}, 0)
	c.ToolEnd(provider.ToolUse{Name: "exec"}, permission.Request{},
		tools.Result{Content: "exit status 2", IsError: true}, 0)
	c.ToolEnd(provider.ToolUse{Name: "exec"}, permission.Request{},
		tools.Result{Content: "fine", IsError: false}, 0)

	if got := c.toolErrors["exec/malformed"]; got != 1 {
		t.Errorf("exec/malformed = %d, want 1", got)
	}
	if got := c.toolErrors["exec/ran-and-failed"]; got != 1 {
		t.Errorf("exec/ran-and-failed = %d, want 1", got)
	}
	if len(c.toolErrors) != 2 {
		t.Errorf("a successful call was counted as an error: %v", c.toolErrors)
	}
}
