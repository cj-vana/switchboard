package main

// LSP assembly. Three things have to line up before definition and
// references join the suite: a workspace whose marker names an ecosystem,
// that ecosystem's server on the machine, and a trust grant to this
// checkout — the same grant a repository's declared processes need,
// because a language server runs what the workspace directs (a Go module's
// toolchain directives, a TypeScript project's plugins), unconfined.
// Opening a repository is not permission to run what its build graph
// implies; /trust grant is.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cj-vana/switchboard/internal/lsp"
	"github.com/cj-vana/switchboard/internal/tools"
	"github.com/cj-vana/switchboard/internal/trust"
)

// lspCandidates maps workspace markers to the server that speaks for them.
// Order is precedence: the first marker present whose server detects wins,
// one server per session. Every entry was verified live against the real
// server before it was listed — argv, handshake, and a cross-file
// definition and references answer (internal/lsp/live_test.go); a server
// nobody has run against for real does not belong in the table, which is
// the §5.2 profile rule applied to language servers.
var lspCandidates = []struct {
	marker string
	detect func() ([]string, bool)
}{
	{"go.mod", plainServer("gopls")},
	{"tsconfig.json", typescriptNative},
	{"package.json", typescriptNative},
	{"pyproject.toml", plainServer("pyright-langserver", "--stdio")},
	{"setup.py", plainServer("pyright-langserver", "--stdio")},
}

func plainServer(binary string, args ...string) func() ([]string, bool) {
	return func() ([]string, bool) {
		path, err := exec.LookPath(binary)
		if err != nil {
			return nil, false
		}
		return append([]string{path}, args...), true
	}
}

// typescriptNative offers tsc's own language server, which the compiler
// carries from TypeScript 7 on — the version probe reads nothing from the
// workspace, only which tsc this machine has. The TS5-era wrapper
// (typescript-language-server) refuses a 7 installation, and no 5 exists
// on the verification machine to run it against, so it is deliberately
// not offered.
func typescriptNative() ([]string, bool) {
	path, err := exec.LookPath("tsc")
	if err != nil {
		return nil, false
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return nil, false
	}
	version, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "Version ")
	if !ok {
		return nil, false
	}
	major, _, _ := strings.Cut(version, ".")
	if n, err := strconv.Atoi(major); err != nil || n < 7 {
		return nil, false
	}
	return []string{path, "--lsp", "-stdio"}, true
}

// setupLSP registers the tools when everything lines up, and returns the
// server for shutdown plus a note explaining whichever line did not.
func setupLSP(workspace string, trustStore *trust.Store, registry *tools.Registry) (*lsp.Server, string) {
	for _, c := range lspCandidates {
		if _, err := os.Stat(filepath.Join(workspace, c.marker)); err != nil {
			continue
		}
		argv, ok := c.detect()
		if !ok {
			continue // the marker's server is not on this machine; try the next marker
		}
		name := filepath.Base(argv[0])
		if trustStore == nil || !trustStore.Trusted(workspace) {
			return nil, name + " can serve this workspace's " + c.marker +
				"; /trust grant lets it answer definition and references"
		}
		server := &lsp.Server{Argv: argv, Root: workspace}
		for _, tool := range []tools.Tool{lsp.NewDefinition(server, registry), lsp.NewReferences(server, registry)} {
			if err := registry.AddExternal(tool); err != nil {
				return server, "language server tools unavailable: " + err.Error()
			}
		}
		return server, name + " serves definition and references for this workspace"
	}
	return nil, "" // no ecosystem marker with a server on the machine; absent, not broken
}
