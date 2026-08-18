package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/extensions"
	"github.com/switchboard-code/switchboard/internal/mcpnative"
)

type denyAllNativeMCPPolicy struct{}

func (denyAllNativeMCPPolicy) NativeMCPAllowed(mcpnative.PolicyRequest) (bool, error) {
	return false, nil
}

type exactPluginIDPolicy string

func (want exactPluginIDPolicy) NativeMCPAllowed(request mcpnative.PolicyRequest) (bool, error) {
	return request.PluginID == string(want), nil
}

func TestPluginMCPNeedsActivationExecutableTrustAndPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	workspace := t.TempDir()
	root := makeClaudePluginFixture(t, home, "tools")
	mustWritePluginTest(t, filepath.Join(root, ".mcp.json"), `{
  "mcpServers":{"review":{"command":"review-server","args":["--safe"]}}
}`)

	state, candidate := installPluginMCPFixture(t, home, workspace, root, extensions.DialectClaude)
	inv := discoverPlugins(home, workspace, state, false)
	policy := allowAllNativeMCPAssemblyPolicy(t)
	if specs, notes, err := enabledPluginMCPSpecs(inv, workspace, policy); err != nil || len(specs) != 0 || !mcpNotesContain(notes, "stays off until") {
		t.Fatalf("untrusted plugin MCP = specs %#v, notes %#v, err %v", specs, notes, err)
	}

	if err := state.TrustExecutable(candidate, workspace); err != nil {
		t.Fatal(err)
	}
	inv = discoverPlugins(home, workspace, state, false)
	specs, notes, err := enabledPluginMCPSpecs(inv, workspace, policy)
	if err != nil || len(notes) != 0 || len(specs) != 1 {
		t.Fatalf("trusted plugin MCP = specs %#v, notes %#v, err %v", specs, notes, err)
	}
	if got := specs[0]; got.Name != "claude:plugin:tools:review" || got.Command != "review-server" ||
		!got.RestrictedEnv || len(got.Args) != 1 || got.Args[0] != "--safe" {
		t.Fatalf("plugin runtime spec = %#v", got)
	}

	deniedPolicy := policy
	deniedPolicy.checker = denyAllNativeMCPPolicy{}
	if denied, deniedNotes, err := enabledPluginMCPSpecs(inv, workspace, deniedPolicy); err != nil || len(denied) != 0 || !mcpNotesContain(deniedNotes, "policy") {
		t.Fatalf("policy-denied plugin MCP = specs %#v, notes %#v, err %v", denied, deniedNotes, err)
	}
}

func TestPluginMCPInlineManifestAndDigestChange(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	workspace := t.TempDir()
	root := filepath.Join(home, "inline-tools")
	manifest := filepath.Join(root, ".codex-plugin", "plugin.json")
	mustWritePluginTest(t, manifest, `{
  "name":"inline-tools",
  "mcpServers":{"review":{"command":"inline-server"}}
}`)

	state, candidate := installPluginMCPFixture(t, home, workspace, root, extensions.DialectCodex)
	if err := state.TrustExecutable(candidate, workspace); err != nil {
		t.Fatal(err)
	}
	inv := discoverPlugins(home, workspace, state, false)
	policy := allowAllNativeMCPAssemblyPolicy(t)
	specs, notes, err := enabledPluginMCPSpecs(inv, workspace, policy)
	if err != nil || len(specs) != 1 || specs[0].Name != "codex:plugin:inline-tools:review" || specs[0].Command != "inline-server" {
		t.Fatalf("inline plugin MCP = specs %#v, notes %#v, err %v", specs, notes, err)
	}

	installed := inv.records[0].Plugin
	if err := os.WriteFile(installed.Manifest, []byte(`{
  "name":"inline-tools",
  "mcpServers":{"review":{"command":"changed-server"}}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := discoverPlugins(home, workspace, state, false)
	if specs, changedNotes, err := enabledPluginMCPSpecs(changed, workspace, policy); err != nil || len(specs) != 0 || !mcpNotesContain(changedNotes, "current executable bytes") {
		t.Fatalf("changed plugin MCP = specs %#v, notes %#v, err %v", specs, changedNotes, err)
	}
}

func TestManagedClaudeMCPExcludesRequiredPluginBeforeMaterialization(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	workspace := t.TempDir()
	root := makeClaudePluginFixture(t, home, "managed-out")
	mustWritePluginTest(t, filepath.Join(root, ".mcp.json"), `{
  "mcpServers":{"required":{"command":"must-not-run","required":true}}
}`)

	state, candidate := installPluginMCPFixture(t, home, workspace, root, extensions.DialectClaude)
	if err := state.TrustExecutable(candidate, workspace); err != nil {
		t.Fatal(err)
	}
	inv := discoverPlugins(home, workspace, state, false)
	policy := allowAllNativeMCPAssemblyPolicy(t)
	policy.claudeManagedExclusive = true
	specs, notes, err := enabledPluginMCPSpecs(inv, workspace, policy)
	if err != nil || len(specs) != 0 || len(notes) != 0 {
		t.Fatalf("managed-exclusive plugin MCP = specs %#v, notes %#v, err %v", specs, notes, err)
	}
}

func TestPluginMCPUsesPolicyEnvironmentAfterLiveEnvironmentChanges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	workspace := t.TempDir()
	root := makeClaudePluginFixture(t, home, "expansion")
	mustWritePluginTest(t, filepath.Join(root, ".mcp.json"), `{
  "mcpServers":{"worker":{"command":"${BIN}/server"}}
}`)

	state, candidate := installPluginMCPFixture(t, home, workspace, root, extensions.DialectClaude)
	if err := state.TrustExecutable(candidate, workspace); err != nil {
		t.Fatal(err)
	}
	policy := allowAllNativeMCPAssemblyPolicy(t, "BIN=/approved")
	t.Setenv("BIN", "/evil")

	inv := discoverPlugins(home, workspace, state, false)
	specs, notes, err := enabledPluginMCPSpecs(inv, workspace, policy)
	if err != nil || len(notes) != 0 || len(specs) != 1 {
		t.Fatalf("plugin specs=%#v notes=%#v err=%v", specs, notes, err)
	}
	if specs[0].Command != "/approved/server" {
		t.Fatalf("plugin runtime command = %q, want policy-authorized snapshot", specs[0].Command)
	}
}

func TestPolicyRestrictedCodexPluginRequiresCanonicalNativeIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	workspace := t.TempDir()
	root := filepath.Join(home, "spoof")
	mustWritePluginTest(t, filepath.Join(root, ".codex-plugin", "plugin.json"), `{
  "name":"review@official",
  "mcpServers":{"tool":{"command":"approved","args":["attacker-controlled"]}}
}`)
	state, candidate := installPluginMCPFixture(t, home, workspace, root, extensions.DialectCodex)
	if err := state.TrustExecutable(candidate, workspace); err != nil {
		t.Fatal(err)
	}
	inv := discoverPlugins(home, workspace, state, false)
	policy := allowAllNativeMCPAssemblyPolicy(t)
	policy.codexPluginRestricted = true

	specs, notes, err := enabledPluginMCPSpecs(inv, workspace, policy)
	if err != nil || len(specs) != 0 || !mcpNotesContain(notes, "no unique canonical Codex marketplace identity") {
		t.Fatalf("policy identity spoof = specs %#v, notes %#v, err %v", specs, notes, err)
	}
}

func TestPluginMCPAuthorityUsesExactEncodedServerIDs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	workspace := t.TempDir()
	root := filepath.Join(home, "colon")
	mustWritePluginTest(t, filepath.Join(root, ".codex-plugin", "plugin.json"), `{
  "name":"foo:bar",
  "mcpServers":{"review":{"command":"review-server"}}
}`)
	state, candidate := installPluginMCPFixture(t, home, workspace, root, extensions.DialectCodex)
	if err := state.TrustExecutable(candidate, workspace); err != nil {
		t.Fatal(err)
	}
	inv := discoverPlugins(home, workspace, state, false)

	specs, notes, err := enabledPluginMCPSpecs(inv, workspace, allowAllNativeMCPAssemblyPolicy(t))
	if err != nil || len(notes) != 0 || len(specs) != 1 {
		t.Fatalf("encoded plugin MCP = specs %#v, notes %#v, err %v", specs, notes, err)
	}
	if specs[0].Command != "review-server" || !strings.Contains(specs[0].Name, "foo%3Abar") {
		t.Fatalf("encoded plugin runtime spec = %#v", specs[0])
	}
}

func TestPolicyRestrictedCodexPluginUsesCanonicalNativeIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	workspace := t.TempDir()
	root := filepath.Join(home, "review")
	mustWritePluginTest(t, filepath.Join(root, ".codex-plugin", "plugin.json"), `{
  "name":"review",
  "mcpServers":{"tool":{"command":"approved"}}
}`)
	state, candidate := installPluginMCPFixture(t, home, workspace, root, extensions.DialectCodex)
	if err := state.TrustExecutable(candidate, workspace); err != nil {
		t.Fatal(err)
	}
	inv := discoverPlugins(home, workspace, state, false)
	if len(inv.records) != 1 {
		t.Fatalf("plugin inventory = %#v", inv.records)
	}
	inv.records[0].NativeIDs = []string{"review@official"}
	policy := allowAllNativeMCPAssemblyPolicy(t)
	policy.checker = exactPluginIDPolicy("review@official")
	policy.codexPluginRestricted = true

	specs, notes, err := enabledPluginMCPSpecs(inv, workspace, policy)
	if err != nil || len(notes) != 0 || len(specs) != 1 {
		t.Fatalf("canonical policy identity = specs %#v, notes %#v, err %v", specs, notes, err)
	}
	if !strings.Contains(specs[0].Name, "review@official") {
		t.Fatalf("canonical native ID not preserved in server ID: %s", specs[0].Name)
	}
}

func installPluginMCPFixture(t *testing.T, home, workspace, root string, dialect extensions.Dialect) (*extensions.State, *extensions.ActivationCandidate) {
	t.Helper()
	discovered := extensions.Discover([]extensions.Candidate{{
		Root: root, Scope: extensions.ScopeUser, Dialect: dialect,
	}})
	if len(discovered.Plugins) != 1 {
		t.Fatalf("plugin fixture discovery = %#v", discovered)
	}
	cacheRoot := filepath.Join(home, ".switchboard", "plugin-cache")
	candidate, err := extensions.InstallActivation(discovered.Plugins[0], cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	state, err := extensions.OpenStateFile(filepath.Join(home, ".switchboard", extensions.StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Enable(candidate, workspace); err != nil {
		t.Fatal(err)
	}
	return state, candidate
}

func mcpNotesContain(notes []mcpNote, want string) bool {
	for _, note := range notes {
		if strings.Contains(note.text, want) {
			return true
		}
	}
	return false
}
