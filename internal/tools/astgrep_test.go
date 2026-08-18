package tools

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
)

// The live tests need the binary and skip without it, so the suite stays
// runnable offline; the shape they pin was captured against ast-grep 0.45.1.
func astGrepRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	binary, err := exec.LookPath("ast-grep")
	if err != nil {
		t.Skip("ast-grep is not installed on this machine")
	}
	r, root := newRegistry(t)
	if err := r.AddExternal(NewAstGrep(r, binary)); err != nil {
		t.Fatal(err)
	}
	return r, root
}

func TestAstGrepFindsShapesNotText(t *testing.T) {
	r, root := astGrepRegistry(t)
	writeFile(t, filepath.Join(root, "main.go"), `package main

import "fmt"

// fmt.Errorf("a comment that mentions the call", nothing)
func main() {
	_ = fmt.Errorf("real: %s", "value")
}
`)

	res := run(t, r, "astgrep", map[string]any{"pattern": "fmt.Errorf($MSG, $$$ARGS)", "lang": "go"})
	if res.IsError {
		t.Fatalf("astgrep failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "main.go:7") {
		t.Errorf("result = %q, want the call site at its 1-based line", res.Content)
	}
	if strings.Count(res.Content, "main.go") != 1 {
		t.Errorf("result = %q, want exactly one match: the comment mentioning the call must not count", res.Content)
	}
}

// A Go one-argument call is grammar-ambiguous — the bare pattern parses as
// a type conversion — and the selector escape hatch is what resolves it.
// This is the quirk the tool description teaches; the test keeps the
// teaching true against the binary actually installed.
func TestAstGrepSelectorDisambiguates(t *testing.T) {
	r, root := astGrepRegistry(t)
	writeFile(t, filepath.Join(root, "main.go"), `package main

import "fmt"

func main() {
	fmt.Println("hello")
}
`)

	bare := run(t, r, "astgrep", map[string]any{"pattern": "fmt.Println($A)", "lang": "go"})
	if !strings.Contains(bare.Content, "no structural matches") {
		t.Fatalf("bare single-arg pattern = %q; if this now matches, the description's quirk note is stale", bare.Content)
	}

	res := run(t, r, "astgrep", map[string]any{
		"pattern": "x := fmt.Println($A)", "selector": "call_expression", "lang": "go",
	})
	if res.IsError || !strings.Contains(res.Content, "main.go:6") {
		t.Fatalf("selector form = %+v, want the call found at its line", res)
	}
}

func TestAstGrepSaysSoOnNoMatches(t *testing.T) {
	r, root := astGrepRegistry(t)
	writeFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() {}\n")

	res := run(t, r, "astgrep", map[string]any{"pattern": "os.Exit($A)", "lang": "go"})
	if res.IsError || !strings.Contains(res.Content, "no structural matches") {
		t.Fatalf("result = %+v, want a plain no-matches answer", res)
	}
}

func TestAstGrepStaysInsideTheWorkspace(t *testing.T) {
	r, _ := astGrepRegistry(t)
	tool, _ := r.Get("astgrep")
	if _, err := tool.Plan([]byte(`{"pattern":"x","path":"../outside"}`)); err == nil {
		t.Fatal("a path outside the workspace must fail at Plan time")
	}
	if _, err := tool.Plan([]byte(`{"pattern":"  "}`)); err == nil {
		t.Fatal("an empty pattern must fail at Plan time")
	}
}

func TestAstGrepEffectFollowsConfinement(t *testing.T) {
	r, _ := astGrepRegistry(t)
	tool, _ := r.Get("astgrep")
	plan, err := tool.Plan([]byte(`{"pattern":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	// The test registry carries no confinement, so the call must ask like
	// any other subprocess rather than riding the read effect unwrapped.
	if plan.Request.Effect != "execute" {
		t.Errorf("effect without confinement = %q, want execute", plan.Request.Effect)
	}
}

func TestAstGrepExecuteRequestCarriesExactReviewedArgv(t *testing.T) {
	root := t.TempDir()
	r, err := NewRegistry(root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	const binary = "/opt/tools/ast-grep"
	tool := NewAstGrep(r, binary)
	plan, err := tool.Plan(json.RawMessage(`{"pattern":"fmt.Errorf($MSG, $$$ARGS)","lang":"go","selector":"call_expression","path":"."}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{binary, "run", "--pattern", "fmt.Errorf($MSG, $$$ARGS)", "--json=compact", "--lang", "go", "--selector", "call_expression", r.Root()}
	if plan.Request.Effect != permission.EffectExecute || !reflect.DeepEqual(plan.Request.Argv, want) {
		t.Fatalf("review request = effect %q argv %#v, want execute %#v", plan.Request.Effect, plan.Request.Argv, want)
	}
	if plan.Request.Path != "." {
		t.Fatalf("review path = %q, want workspace-relative dot", plan.Request.Path)
	}

	// The permission packet is inspectable, not an alias of the closure's
	// executable argv.
	plan.Request.Argv[0] = "/tmp/tampered"
	if plan.Request.Execution == nil {
		t.Fatal("astgrep request omitted its execution posture")
	}
}

func TestAstGrepConfinementPolicyUsesRegistryRootNotProcessCWD(t *testing.T) {
	r, err := NewRegistry(t.TempDir(), execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	tool := NewAstGrep(r, "/opt/tools/ast-grep").(*astGrepTool)
	policy := execution.CommandPolicy{Network: execution.NetworkLoopback}
	cmd := tool.command([]string{"/opt/tools/ast-grep", "run"}, policy)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Dir != r.Root() || cmd.Policy.Workspace != r.Root() {
		t.Fatalf("astgrep command root drifted: dir=%q policy=%q want=%q", cmd.Dir, cmd.Policy.Workspace, r.Root())
	}
	if cmd.Policy.Workspace == cwd {
		t.Fatalf("test setup did not separate process cwd %q from registry root", cwd)
	}
}

func TestAstGrepIsAlwaysExecuteBecauseStandardSandboxAllowsWrites(t *testing.T) {
	r, err := NewRegistry(t.TempDir(), execution.TestingVerifiedCapability())
	if err != nil {
		t.Fatal(err)
	}
	tool := NewAstGrep(r, "/tmp/path-shadowed-ast-grep")
	plan, err := tool.Plan(json.RawMessage(`{"pattern":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Request.Effect != permission.EffectExecute {
		t.Fatalf("confined external binary effect = %q, want execute", plan.Request.Effect)
	}
	engine := permission.NewEngine(permission.ModePlan, execution.TestingVerifiedCapability())
	if out := engine.Check(plan.Request); out.Decision != permission.Deny {
		t.Fatalf("plan mode allowed PATH binary that can write workspace: %+v", out)
	}
	if tool.ParallelSafe() {
		t.Fatal("external astgrep binary was marked parallel-safe despite writable sandbox paths")
	}
}
