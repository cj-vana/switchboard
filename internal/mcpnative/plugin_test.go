package mcpnative

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/switchboard-code/switchboard/internal/trust"
)

func TestParsePluginMCPFileAndInlineUseNormalExecutionGates(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	mustMkdir(t, bin)
	mustWrite(t, filepath.Join(bin, "server"), "fixture")
	component := filepath.Join(root, ".mcp.json")
	mustWrite(t, component, `{"mcpServers":{"wrapped":{"command":"${CLAUDE_PLUGIN_ROOT}/bin/server","args":["--root=${CLAUDE_PLUGIN_ROOT}"]}}}`)

	result := ParsePluginMCP(PluginMCPOptions{
		Dialect: DialectClaude, PluginID: "review-tools", PluginRoot: root,
		Path: ".mcp.json", Shape: PluginMCPAuto,
	})
	server := serverNamed(t, result, "claude:plugin:review-tools:wrapped")
	if !server.Supported || server.Provenance.Scope != ScopePlugin ||
		server.Provenance.PluginID != "review-tools" || !server.ExecutionTrustRequired {
		t.Fatalf("plugin provenance/gates = %+v", server)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if expose(server.Command) != filepath.Join(realRoot, "bin", "server") || server.Args[0].raw() != "--root="+realRoot {
		t.Fatalf("plugin-root path handling = %+v", server)
	}

	activation := approve(t, result, server.ID)
	if _, err := result.Materialize(server.ID, nil, fixedPolicy{allowed: true}, activation); err == nil {
		t.Fatal("plugin MCP materialized without executable trust")
	}
	store, err := trust.OpenFile(filepath.Join(t.TempDir(), "trust.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Grant(realRoot); err != nil {
		t.Fatal(err)
	}
	materialized, err := result.Materialize(server.ID, store, fixedPolicy{allowed: true}, activation)
	if err != nil {
		t.Fatal(err)
	}
	if materialized.Command.Expose() != filepath.Join(realRoot, "bin", "server") {
		t.Fatalf("materialized plugin command = %s", materialized.Command.Expose())
	}

	manifest := filepath.Join(root, ".claude-plugin", "plugin.json")
	mustMkdir(t, filepath.Dir(manifest))
	mustWrite(t, manifest, `{}`)
	direct := ParsePluginMCP(PluginMCPOptions{
		Dialect: DialectClaude, PluginID: "review-tools", PluginRoot: root, Path: manifest,
		InlineJSON: []byte(`{"inline":{"command":"node"}}`), Shape: PluginMCPDirect,
	})
	if got := serverNamed(t, direct, "claude:plugin:review-tools:inline"); !got.Supported || got.Provenance.RealPath == "" {
		t.Fatalf("inline plugin MCP = %+v", got)
	}
}

func TestParsePluginMCPFailsClosedOnShapeContainmentAndDefinitionChanges(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "plugin.json")
	mustWrite(t, manifest, `{}`)

	ambiguous := ParsePluginMCP(PluginMCPOptions{
		Dialect: DialectCodex, PluginID: "tools", PluginRoot: root, Path: manifest,
		InlineJSON: []byte(`{"mcpServers":{},"extra":{}}`), Shape: PluginMCPAuto,
	})
	if len(ambiguous.Servers) != 0 || !hasDiagnostic(ambiguous, "invalid-plugin-mcp-shape") {
		t.Fatalf("ambiguous plugin MCP shape did not fail closed: %+v", ambiguous)
	}

	escape := filepath.Join(t.TempDir(), "outside.json")
	mustWrite(t, escape, `{}`)
	outside := ParsePluginMCP(PluginMCPOptions{
		Dialect: DialectClaude, PluginID: "tools", PluginRoot: root, Path: escape,
	})
	if !hasDiagnostic(outside, "plugin-source-outside-root") {
		t.Fatalf("outside plugin source accepted: %+v", outside)
	}

	badCommand := ParsePluginMCP(PluginMCPOptions{
		Dialect: DialectCodex, PluginID: "tools", PluginRoot: root, Path: manifest,
		InlineJSON: []byte(`{"bad":{"command":"runner","cwd":"../outside"}}`), Shape: PluginMCPDirect,
	})
	bad := serverNamed(t, badCommand, "codex:plugin:tools:bad")
	if bad.Supported {
		t.Fatalf("escaping relative command remained supported: %+v", bad)
	}
	if _, err := badCommand.ActivationRequest(bad.ID); !errors.Is(err, ErrInvalidServer) {
		t.Fatalf("invalid plugin server accepted activation: %v", err)
	}

	first := ParsePluginMCP(PluginMCPOptions{
		Dialect: DialectCodex, PluginID: "tools", PluginRoot: root, Path: manifest,
		InlineJSON: []byte(`{"same":{"command":"one"}}`), Shape: PluginMCPDirect,
	})
	second := ParsePluginMCP(PluginMCPOptions{
		Dialect: DialectCodex, PluginID: "tools", PluginRoot: root, Path: manifest,
		InlineJSON: []byte(`{"same":{"command":"two"}}`), Shape: PluginMCPDirect,
	})
	key := []byte("0123456789abcdef0123456789abcdef")
	one, _ := first.ActivationRequest("codex:plugin:tools:same")
	two, _ := second.ActivationRequest("codex:plugin:tools:same")
	oneID, _ := one.Identity(key)
	twoID, _ := two.Identity(key)
	if oneID.Digest == twoID.Digest {
		t.Fatal("plugin definition change did not invalidate activation")
	}
}

func TestParsePluginMCPInlineEmptyIsDistinctFromFileMode(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "plugin.json")
	if err := os.WriteFile(manifest, []byte(`{"not":"mcp"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result := ParsePluginMCP(PluginMCPOptions{
		Dialect: DialectClaude, PluginID: "tools", PluginRoot: root, Path: manifest,
		InlineJSON: []byte{}, Shape: PluginMCPDirect,
	})
	if !hasDiagnostic(result, "invalid-json") {
		t.Fatalf("empty non-nil inline bytes incorrectly triggered file mode: %+v", result)
	}
}

func TestParsePluginMCPExtractsInlineManifestField(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "work"))
	manifest := filepath.Join(root, ".codex-plugin", "plugin.json")
	mustMkdir(t, filepath.Dir(manifest))
	mustWrite(t, manifest, `{
  "name":"tools",
  "version":"1.0.0",
  "mcpServers":{
    "inline":{"type":"stdio","command":"runner","cwd":"./work"},
    "remote":{"type":"streamable_http","url":"https://example.test/mcp","oauth":{"clientId":"public-client","callbackPort":4321}}
  },
  "skills":"./skills"
}`)
	result := ParsePluginMCP(PluginMCPOptions{
		Dialect: DialectCodex, PluginID: "tools", PluginRoot: root,
		Path: manifest, ManifestField: "mcpServers",
	})
	server := serverNamed(t, result, "codex:plugin:tools:inline")
	if !server.Supported || expose(server.Command) != "runner" || expose(server.CWD) != filepath.Join(server.Provenance.PluginRoot, "work") ||
		server.Provenance.ConfigKey != "mcpServers.inline" {
		t.Fatalf("manifest field extraction = %+v diagnostics=%+v", server, result.Diagnostics)
	}
	remote := serverNamed(t, result, "codex:plugin:tools:remote")
	if !remote.Supported || remote.CodexOAuth == nil || remote.CodexOAuth.CallbackPort != 4321 ||
		remote.CodexOAuth.ClientID == nil || remote.CodexOAuth.ClientID.raw() != "public-client" {
		t.Fatalf("Codex plugin normalization = %+v diagnostics=%+v", remote, result.Diagnostics)
	}
}

func TestPluginActivationBindsResolvedExecutablePaths(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "plugin.json")
	mustWrite(t, manifest, `{}`)
	for _, target := range []string{"one", "two"} {
		mustMkdir(t, filepath.Join(root, target))
		mustWrite(t, filepath.Join(root, target, "server"), target)
	}
	current := filepath.Join(root, "current")
	if err := os.Symlink("one", current); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	parse := func() Result {
		return ParsePluginMCP(PluginMCPOptions{
			Dialect: DialectClaude, PluginID: "tools", PluginRoot: root, Path: manifest,
			InlineJSON: []byte(`{"server":{"command":"${CLAUDE_PLUGIN_ROOT}/current/server"}}`),
			Shape:      PluginMCPDirect,
		})
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	first := parse()
	firstRequest, err := first.ActivationRequest("claude:plugin:tools:server")
	if err != nil {
		t.Fatalf("first activation: %v; diagnostics=%+v servers=%+v", err, first.Diagnostics, first.Servers)
	}
	firstIdentity, err := firstRequest.Identity(key)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("two", current); err != nil {
		t.Fatal(err)
	}
	second := parse()
	secondRequest, err := second.ActivationRequest("claude:plugin:tools:server")
	if err != nil {
		t.Fatalf("second activation: %v; diagnostics=%+v servers=%+v", err, second.Diagnostics, second.Servers)
	}
	secondIdentity, err := secondRequest.Identity(key)
	if err != nil {
		t.Fatal(err)
	}
	if firstIdentity.Digest == secondIdentity.Digest {
		t.Fatal("retargeted plugin path did not invalidate the activation identity")
	}
	if firstServer, secondServer := serverNamed(t, first, firstIdentity.ID), serverNamed(t, second, secondIdentity.ID); expose(firstServer.Command) == expose(secondServer.Command) {
		t.Fatalf("plugin paths were not resolved before binding: first=%+v second=%+v", firstServer, secondServer)
	}
}

func TestNativeServerIDsCannotCollideAcrossDirectAndPluginNamespaces(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mustMkdir(t, home)
	mustWrite(t, filepath.Join(home, ".claude.json"), `{"mcpServers":{"plugin:a:b":{"command":"direct"}}}`)
	direct := Discover(Options{HomeDir: home})
	directServer := serverNamed(t, direct, "claude:plugin%3Aa%3Ab")

	manifest := filepath.Join(root, "plugin.json")
	mustWrite(t, manifest, `{}`)
	plugin := ParsePluginMCP(PluginMCPOptions{
		Dialect: DialectClaude, PluginID: "a", PluginRoot: root, Path: manifest,
		InlineJSON: []byte(`{"b":{"command":"plugin"}}`), Shape: PluginMCPDirect,
	})
	pluginServer := serverNamed(t, plugin, "claude:plugin:a:b")
	if directServer.ID == pluginServer.ID {
		t.Fatalf("direct and plugin server IDs collide: %q", directServer.ID)
	}
	request, err := plugin.ActivationRequest(pluginServer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if request.Dialect != DialectClaude || request.Scope != ScopePlugin || request.PluginID != "a" || request.Name != "b" {
		t.Fatalf("activation request lost structured plugin identity: %+v", request)
	}

	namespaced := ParsePluginMCP(PluginMCPOptions{
		Dialect: DialectClaude, PluginID: "a:b", PluginRoot: root, Path: manifest,
		InlineJSON: []byte(`{"c":{"command":"plugin"}}`), Shape: PluginMCPDirect,
	})
	if got := serverNamed(t, namespaced, "claude:plugin:a%3Ab:c"); !got.Supported {
		t.Fatalf("namespaced plugin ID was not encoded safely: %+v", got)
	}
	if id, err := PluginServerID(DialectClaude, "a:b", "c"); err != nil || id != "claude:plugin:a%3Ab:c" {
		t.Fatalf("PluginServerID = %q, %v", id, err)
	}
}
