package tools

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
