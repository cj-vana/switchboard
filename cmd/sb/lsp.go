package main

// LSP assembly. Three things have to line up before definition and
// references join the suite: a Go module at the workspace root, a gopls on
// the machine, and a trust grant to this checkout — the same grant a
// repository's declared processes need, because a language server builds
// the module's dependency graph, and modern Go modules can direct that
// build (toolchain directives, generated code paths). Opening a repository
// is not permission to run what its module implies; /trust grant is.

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/cj-vana/switchboard/internal/lsp"
	"github.com/cj-vana/switchboard/internal/tools"
	"github.com/cj-vana/switchboard/internal/trust"
)

// setupLSP registers the tools when everything lines up, and returns the
// server for shutdown plus a note explaining whichever line did not.
func setupLSP(workspace string, trustStore *trust.Store, registry *tools.Registry) (*lsp.Server, string) {
	if _, err := os.Stat(filepath.Join(workspace, "go.mod")); err != nil {
		return nil, "" // not a Go module; nothing to offer yet
	}
	binary, err := exec.LookPath("gopls")
	if err != nil {
		return nil, "" // no server on the machine; absent, not broken
	}
	if trustStore == nil || !trustStore.Trusted(workspace) {
		return nil, "gopls is installed and this is a Go module; /trust grant lets it serve definition and references"
	}

	server := &lsp.Server{Argv: []string{binary}, Root: workspace}
	for _, tool := range []tools.Tool{lsp.NewDefinition(server, registry), lsp.NewReferences(server, registry)} {
		if err := registry.AddExternal(tool); err != nil {
			return server, "language server tools unavailable: " + err.Error()
		}
	}
	return server, "gopls serves definition and references for this workspace"
}
