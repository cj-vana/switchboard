package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/tools"
	"github.com/switchboard-code/switchboard/internal/trust"
)

func TestSetupLSPFreezesAllSemanticToolsBeforeLazyStart(t *testing.T) {
	workspace := t.TempDir()
	marker := "fixture.project"
	if err := os.WriteFile(filepath.Join(workspace, marker), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldCandidates := lspCandidates
	lspCandidates = []struct {
		marker string
		detect func() ([]string, bool)
	}{{marker: marker, detect: func() ([]string, bool) {
		return []string{filepath.Join(workspace, "fixture-ls"), "--stdio"}, true
	}}}
	t.Cleanup(func() { lspCandidates = oldCandidates })

	store, err := trust.OpenFile(filepath.Join(t.TempDir(), "trust.toml"))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(workspace, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	workspace = registry.Root()
	if err := store.Grant(workspace); err != nil {
		t.Fatal(err)
	}
	server, note := setupLSP(workspace, store, registry)
	if server == nil || !strings.Contains(note, "diagnostics") {
		t.Fatalf("setupLSP = (%v, %q)", server, note)
	}
	t.Cleanup(server.Close)
	if status := server.Status(); status.State != "configured" {
		t.Fatalf("setup started the lazy runtime: %+v", status)
	}

	want := map[string]bool{"definition": true, "references": true, "outline": true, "symbols": true}
	for _, definition := range registry.Definitions() {
		if !want[definition.Name] {
			continue
		}
		if !json.Valid(definition.Schema) || definition.Description == "" {
			t.Errorf("frozen %s definition is invalid: %+v", definition.Name, definition)
		}
		delete(want, definition.Name)
	}
	if len(want) != 0 {
		t.Fatalf("semantic tools missing from frozen definitions: %v", want)
	}
}

func TestSetupLSPGatesOnModuleBinaryAndTrust(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls is not installed on this machine")
	}
	newParts := func(t *testing.T, withMod bool) (string, *trust.Store, *tools.Registry) {
		t.Helper()
		workspace := t.TempDir()
		if withMod {
			if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module x\n\ngo 1.21\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		store, err := trust.OpenFile(filepath.Join(t.TempDir(), "trust.toml"))
		if err != nil {
			t.Fatal(err)
		}
		registry, err := tools.NewRegistry(workspace, execution.Capability{})
		if err != nil {
			t.Fatal(err)
		}
		return registry.Root(), store, registry
	}

	// No module: silently absent, whatever else is installed.
	workspace, store, registry := newParts(t, false)
	if server, note := setupLSP(workspace, store, registry); server != nil || note != "" {
		t.Fatalf("no go.mod, but setupLSP returned (%v, %q)", server, note)
	}

	// A module without trust: the note says what would unlock it, and no
	// server process is on offer.
	workspace, store, registry = newParts(t, true)
	server, note := setupLSP(workspace, store, registry)
	if server != nil || !strings.Contains(note, "/trust grant") {
		t.Fatalf("untrusted module: (%v, %q), want the trust pointer and no server", server, note)
	}
	for _, name := range []string{"definition", "references", "outline", "symbols"} {
		if _, ok := registry.Get(name); ok {
			t.Fatalf("%s registered without a trust grant", name)
		}
	}

	// Trusted: both tools join the suite and the server is handed back for
	// shutdown. Lazy start means no process has run yet.
	if err := store.Grant(workspace); err != nil {
		t.Fatal(err)
	}
	server, note = setupLSP(workspace, store, registry)
	if server == nil || !strings.Contains(note, "outlines") {
		t.Fatalf("trusted module: (%v, %q), want the tools live", server, note)
	}
	defer server.Close()
	for _, name := range []string{"definition", "references", "outline", "symbols"} {
		if _, ok := registry.Get(name); !ok {
			t.Errorf("%s missing from the suite after the grant", name)
		}
	}
}

func TestTypeScriptNativeIsVersionGated(t *testing.T) {
	path, err := exec.LookPath("tsc")
	if err != nil {
		t.Skip("no tsc on this machine")
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		t.Skip("tsc did not answer --version")
	}
	argv, ok := typescriptNative()
	wantNative := strings.Contains(string(out), "Version 7.") ||
		strings.Contains(string(out), "Version 8.")
	if ok != wantNative {
		t.Fatalf("typescriptNative() = %v for %q; the gate and the binary disagree", ok, strings.TrimSpace(string(out)))
	}
	if ok && (len(argv) != 3 || argv[1] != "--lsp") {
		t.Fatalf("argv = %v, want the verified --lsp -stdio form", argv)
	}
}
