package lsp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// The live test needs gopls and skips without it, so the suite stays
// runnable offline. What it pins was verified against gopls on this
// machine: the handshake completes with the minimal capabilities the
// client declares, and definition and references answer across files.
func liveGopls(t *testing.T) (*Server, *tools.Registry, string) {
	t.Helper()
	binary, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls is not installed on this machine")
	}

	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.test\n\ngo 1.21\n")
	write("thing.go", "package main\n\n// Answer is the thing the other file uses.\nfunc Answer() int { return 42 }\n")
	write("main.go", "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(Answer())\n}\n")

	registry, err := tools.NewRegistry(root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Argv: []string{binary}, Root: registry.Root()}
	t.Cleanup(server.Close)
	return server, registry, registry.Root()
}

func runTool(t *testing.T, tool tools.Tool, input string) tools.Result {
	t.Helper()
	plan, err := tool.Plan([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	res, err := plan.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestLiveGoplsAnswersAcrossFiles(t *testing.T) {
	server, registry, _ := liveGopls(t)

	def := runTool(t, NewDefinition(server, registry), `{"path":"main.go","line":6,"symbol":"Answer"}`)
	if def.IsError {
		t.Fatalf("definition failed: %s", def.Content)
	}
	if !strings.Contains(def.Content, "thing.go:4") {
		t.Errorf("definition = %q, want thing.go:4", def.Content)
	}

	refs := runTool(t, NewReferences(server, registry), `{"path":"thing.go","line":4,"symbol":"Answer"}`)
	if refs.IsError {
		t.Fatalf("references failed: %s", refs.Content)
	}
	for _, want := range []string{"thing.go:4", "main.go:6"} {
		if !strings.Contains(refs.Content, want) {
			t.Errorf("references = %q, missing %s", refs.Content, want)
		}
	}
}

func TestLiveToolRefusesWhatItCannotAnswerHonestly(t *testing.T) {
	server, registry, _ := liveGopls(t)
	tool := NewDefinition(server, registry)

	if _, err := tool.Plan([]byte(`{"path":"../outside.go","line":1,"symbol":"x"}`)); err == nil {
		t.Error("a path outside the workspace must fail at Plan time")
	}

	res := runTool(t, tool, `{"path":"main.go","line":6,"symbol":"NotOnThatLine"}`)
	if !res.IsError || !strings.Contains(res.Content, "does not appear on line") {
		t.Errorf("wrong symbol = %+v, want a refusal naming the mismatch", res)
	}
}

// liveServer builds a workspace from files and starts the named server
// over it, skipping when the machine lacks the binary. Each ecosystem
// listed in cmd/sb's candidate table earns its place by passing here.
func liveServer(t *testing.T, binary string, args []string, files map[string]string) (*Server, *tools.Registry) {
	t.Helper()
	path, err := exec.LookPath(binary)
	if err != nil {
		t.Skipf("%s is not installed on this machine", binary)
	}
	root := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := tools.NewRegistry(root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Argv: append([]string{path}, args...), Root: registry.Root()}
	t.Cleanup(server.Close)
	return server, registry
}

func TestLivePyrightAnswersAcrossFiles(t *testing.T) {
	server, registry := liveServer(t, "pyright-langserver", []string{"--stdio"}, map[string]string{
		"pyproject.toml": "[project]\nname = \"example\"\nversion = \"0.0.1\"\n",
		"lib.py":         "def answer() -> int:\n    return 42\n",
		"main.py":        "from lib import answer\n\nprint(answer())\n",
	})

	def := runTool(t, NewDefinition(server, registry), `{"path":"main.py","line":3,"symbol":"answer"}`)
	if def.IsError || !strings.Contains(def.Content, "lib.py:1") {
		t.Fatalf("definition = %+v, want lib.py:1", def)
	}
	refs := runTool(t, NewReferences(server, registry), `{"path":"lib.py","line":1,"symbol":"answer"}`)
	if refs.IsError || !strings.Contains(refs.Content, "main.py:3") {
		t.Fatalf("references = %+v, want the use in main.py", refs)
	}
}

// TypeScript 7's compiler carries its own language server; the wrapper the
// TS5 era used (typescript-language-server) refuses a TS7 installation and
// no TS5 exists on this machine to verify it against, so per the profile
// rule it is not in the candidate table until someone runs it for real.
func TestLiveTypeScriptNativeAnswersAcrossFiles(t *testing.T) {
	server, registry := liveServer(t, "tsc", []string{"--lsp", "-stdio"}, map[string]string{
		"tsconfig.json": `{"compilerOptions": {"strict": true}}`,
		"lib.ts":        "export function answer(): number {\n  return 42;\n}\n",
		"main.ts":       "import { answer } from \"./lib\";\n\nconsole.log(answer());\n",
	})

	def := runTool(t, NewDefinition(server, registry), `{"path":"main.ts","line":3,"symbol":"answer"}`)
	if def.IsError || !strings.Contains(def.Content, "lib.ts:1") {
		t.Fatalf("definition = %+v, want lib.ts:1", def)
	}
	refs := runTool(t, NewReferences(server, registry), `{"path":"lib.ts","line":1,"symbol":"answer"}`)
	if refs.IsError || !strings.Contains(refs.Content, "main.ts:3") {
		t.Fatalf("references = %+v, want the use in main.ts", refs)
	}
}

// rust-analyzer is deliberately not in the candidate table: on the
// verification machine the binary on PATH is a rustup shim that loops
// "unavailable for the active toolchain" and exits without ever speaking
// LSP, so the handshake was never demonstrated — the TS5-wrapper rule
// again. When a machine with a real rust-analyzer runs the verification,
// the entry can earn its place.
//
// clangd answers a compile_commands.json workspace, which is also its
// marker in the candidate table: without the database clangd guesses
// flags, and a table entry that worked by guessing would not be the
// verified claim the table promises. The retry loop covers background
// indexing, which the narrow client does not smooth over.
func TestLiveClangdAnswersAcrossFiles(t *testing.T) {
	path, err := exec.LookPath("clangd")
	if err != nil {
		t.Skip("clangd is not installed on this machine")
	}
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("lib.h", "#pragma once\nint answer(void);\n")
	write("lib.c", "#include \"lib.h\"\nint answer(void) { return 42; }\n")
	write("main.c", "#include \"lib.h\"\n#include <stdio.h>\n\nint main(void) {\n  printf(\"%d\", answer());\n  return 0;\n}\n")

	registry, err := tools.NewRegistry(root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	resolved := registry.Root()
	db := fmt.Sprintf(`[
  {"directory": %q, "file": "lib.c", "command": "cc -c lib.c"},
  {"directory": %q, "file": "main.c", "command": "cc -c main.c"}
]`, resolved, resolved)
	write("compile_commands.json", db)

	server := &Server{Argv: []string{path}, Root: resolved}
	t.Cleanup(server.Close)

	def := runTool(t, NewDefinition(server, registry), `{"path":"main.c","line":5,"symbol":"answer"}`)
	deadline := time.Now().Add(60 * time.Second)
	for (def.IsError || !strings.Contains(def.Content, "lib.")) && time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		def = runTool(t, NewDefinition(server, registry), `{"path":"main.c","line":5,"symbol":"answer"}`)
	}
	// clangd may answer the declaration (lib.h) or the definition (lib.c);
	// either is a real cross-file answer.
	if def.IsError || !strings.Contains(def.Content, "lib.") {
		t.Fatalf("definition = %+v, want a lib.h or lib.c location", def)
	}
	refs := runTool(t, NewReferences(server, registry), `{"path":"lib.c","line":2,"symbol":"answer"}`)
	if refs.IsError || !strings.Contains(refs.Content, "main.c:5") {
		t.Fatalf("references = %+v, want the use in main.c", refs)
	}
}
