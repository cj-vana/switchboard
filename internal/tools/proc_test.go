//go:build unix

package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func runTool(t *testing.T, r *Registry, name, input string) Result {
	t.Helper()
	plan, err := r.tools[name].Plan([]byte(input))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	res, err := plan.Run(t.Context())
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

// The shape the synchronous tool could not express at all.
func TestExecStartsSomethingAndProcReadsIt(t *testing.T) {
	r, _ := newRegistry(t)
	t.Cleanup(r.StopBackgroundCommands)

	started := runTool(t, r, "exec", `{"script":"echo listening; sleep 30","background":true}`)
	if started.IsError {
		t.Fatalf("exec refused to start a background command: %s", started.Content)
	}
	if !strings.Contains(started.Content, "bg1") {
		t.Fatalf("exec did not report a handle: %s", started.Content)
	}

	deadline := time.Now().Add(5 * time.Second)
	var read Result
	for time.Now().Before(deadline) {
		read = runTool(t, r, "proc", `{"action":"read","id":"bg1"}`)
		if strings.Contains(read.Content, "listening") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(read.Content, "listening") {
		t.Fatalf("proc read did not return the command's output: %s", read.Content)
	}
	if !strings.Contains(read.Content, "running") {
		t.Errorf("proc read did not say the command is still running: %s", read.Content)
	}

	listed := runTool(t, r, "proc", `{"action":"list"}`)
	if !strings.Contains(listed.Content, "bg1") {
		t.Errorf("proc list omitted the running command: %s", listed.Content)
	}

	stopped := runTool(t, r, "proc", `{"action":"stop","id":"bg1"}`)
	if !strings.Contains(stopped.Content, "stopped") {
		t.Errorf("proc stop did not confirm the end: %s", stopped.Content)
	}
	after := runTool(t, r, "proc", `{"action":"list"}`)
	if strings.Contains(after.Content, "running for") {
		t.Errorf("the command is still listed as running after a stop: %s", after.Content)
	}
}

// A stop reaches into a running process with this account's reach, so it is
// priced as an execution rather than as a read.
func TestProcCarriesTheExecuteEffect(t *testing.T) {
	r, _ := newRegistry(t)
	plan, err := r.tools["proc"].Plan([]byte(`{"action":"list"}`))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Request.Effect != "execute" {
		t.Errorf("effect = %s, want execute", plan.Request.Effect)
	}
}

// Starting what you cannot stop is a leak with extra steps.
func TestBackgroundIsRefusedWithoutTheProcTool(t *testing.T) {
	r, _ := newRegistry(t)
	t.Cleanup(r.StopBackgroundCommands)
	if err := r.Restrict([]string{"exec"}); err != nil {
		t.Fatal(err)
	}

	res := runTool(t, r, "exec", `{"script":"sleep 30","background":true}`)
	if !res.IsError {
		t.Fatal("a restricted agent started a process it has no verb to stop")
	}
	if !strings.Contains(res.Content, "proc") {
		t.Errorf("the refusal does not say what is missing: %s", res.Content)
	}
}

// A malformed call is a tool error the model can correct, not a crash.
func TestProcRejectsAnUnknownActionAndAMissingID(t *testing.T) {
	r, _ := newRegistry(t)
	if _, err := r.tools["proc"].Plan([]byte(`{"action":"restart","id":"bg1"}`)); err == nil {
		t.Error("an unknown action planned")
	}
	if _, err := r.tools["proc"].Plan([]byte(`{"action":"stop"}`)); err == nil {
		t.Error("a stop with no id planned")
	}
}

// The schema is in the frozen zone, so it has to be valid and closed.
func TestProcSchemaIsValidAndClosed(t *testing.T) {
	r, _ := newRegistry(t)
	var schema map[string]any
	if err := json.Unmarshal(r.tools["proc"].Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	if schema["additionalProperties"] != false {
		t.Error("the schema accepts properties it does not define")
	}
}
