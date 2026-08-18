package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/mcpnative"
	"github.com/switchboard-code/switchboard/internal/mcppolicy"
)

type allowAllNativeMCPPolicy struct{}

func (allowAllNativeMCPPolicy) NativeMCPAllowed(mcpnative.PolicyRequest) (bool, error) {
	return true, nil
}

func allowAllNativeMCPAssemblyPolicy(t *testing.T, environment ...string) nativeMCPAssemblyPolicy {
	t.Helper()
	policy, _, _ := nativeMCPTestPolicy(t, environment, nil)
	return policy
}

func nativeMCPTestPolicy(t *testing.T, environment []string, configure func(home, workspace string, paths *mcppolicy.Paths)) (nativeMCPAssemblyPolicy, string, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "workspace")
	system := filepath.Join(root, "system")
	for _, directory := range []string{home, workspace, system, filepath.Join(home, ".codex"), filepath.Join(home, ".claude")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	paths := &mcppolicy.Paths{
		CodexRequirements: filepath.Join(system, "requirements.toml"), CodexAuth: filepath.Join(home, ".codex", "auth.json"),
		ClaudeManagedSettings: filepath.Join(system, "managed-settings.json"), ClaudeManagedDropIns: filepath.Join(system, "managed-settings.d"),
		ClaudeManagedMCP: filepath.Join(system, "managed-mcp.json"), ClaudeRemoteSettings: filepath.Join(home, ".claude", "remote-settings.json"),
		ClaudeState: filepath.Join(home, ".claude.json"), ClaudeUserSettings: filepath.Join(home, ".claude", "settings.json"),
		ClaudeProjectSettings: filepath.Join(workspace, ".claude", "settings.json"), ClaudeLocalSettings: filepath.Join(workspace, ".claude", "settings.local.json"),
	}
	if configure != nil {
		configure(home, workspace, paths)
	}
	checker, diagnostics, err := mcppolicy.Load(mcppolicy.Options{
		HomeDir: home, Workspace: workspace, GOOS: runtime.GOOS, Paths: paths,
		CloudRequirementsChecked: true, StartupEnv: append([]string(nil), environment...),
	})
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("test policy: diagnostics=%#v err=%v", diagnostics, err)
	}
	expansion, err := checker.ClaudeRuntimeExpansion()
	if err != nil {
		t.Fatal(err)
	}
	return nativeMCPAssemblyPolicy{
		checker: checker, claudeExpansion: expansion,
		claudeManagedExclusive: checker.ClaudeManagedExclusive(),
	}, home, workspace
}

func TestNativeMCPRuntimeMapsClaudeStdioExpansionTimeoutsAndFilters(t *testing.T) {
	home := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {"worker": {
    "command":"${BIN}",
    "args":["--tenant", "${TENANT:-default-tenant}"],
    "env":{"STATIC":"${STATIC_VALUE}", "FALLBACK":"${EMPTY:-fallback}"},
    "timeout":1500,
    "alwaysLoad":true
  }}
}`)
	result := mcpnative.Discover(nativeMCPTestOptions(t, home, ""))
	request, err := result.ActivationRequest("claude:worker")
	if err != nil {
		t.Fatalf("activation request: %v; diagnostics=%#v", err, result.Diagnostics)
	}
	state, err := openNativeMCPActivationStateFile(filepath.Join(t.TempDir(), nativeMCPStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.enable(request); err != nil {
		t.Fatal(err)
	}
	server, err := result.Materialize("claude:worker", nil, allowAllNativeMCPPolicy{}, state, nativeMCPRuntimeFeatures...)
	if err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{
		"BIN":          "/opt/tools/mcp-worker",
		"TENANT":       "acme",
		"STATIC_VALUE": "static-value",
		"EMPTY":        "",
	}
	spec, err := nativeMCPRuntimeSpecWithExpansion(server, func(value string) (string, error) {
		return expandClaudeEnvironment(value, func(name string) (string, bool) {
			expanded, ok := environment[name]
			return expanded, ok
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "claude:worker" || spec.Command != "/opt/tools/mcp-worker" ||
		!reflect.DeepEqual(spec.Args, []string{"--tenant", "acme"}) ||
		spec.Env["STATIC"] != "static-value" || spec.Env["FALLBACK"] != "fallback" ||
		!spec.RestrictedEnv || spec.ToolTimeout != 1500*time.Millisecond {
		t.Fatalf("mapped stdio spec = %#v", spec)
	}
}

func TestNativeMCPRuntimeMapsCodexToolFilters(t *testing.T) {
	home := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".codex", "config.toml"), `
[mcp_servers.worker]
command = "worker"
enabled_tools = ["read"]
disabled_tools = ["delete"]
`)
	result := mcpnative.Discover(nativeMCPTestOptions(t, home, ""))
	request, err := result.ActivationRequest("codex:worker")
	if err != nil {
		t.Fatal(err)
	}
	state, err := openNativeMCPActivationStateFile(filepath.Join(t.TempDir(), nativeMCPStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.enable(request); err != nil {
		t.Fatal(err)
	}
	server, err := result.Materialize("codex:worker", nil, allowAllNativeMCPPolicy{}, state, nativeMCPRuntimeFeatures...)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := nativeMCPRuntimeSpecWithExpansion(server, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !spec.EnabledToolsSet || !reflect.DeepEqual(spec.EnabledTools, []string{"read"}) ||
		!spec.DisabledToolsSet || !reflect.DeepEqual(spec.DisabledTools, []string{"delete"}) {
		t.Fatalf("tool filters = %#v", spec)
	}
}

func TestNativeMCPRuntimeMapsClaudeHTTPHeadersAndRejectsMissingExpansion(t *testing.T) {
	home := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {"remote": {
    "type":"http",
    "url":"https://${HOST}/mcp",
    "headers":{"Authorization":"Bearer ${TOKEN}"}
  }}
}`)
	result := mcpnative.Discover(nativeMCPTestOptions(t, home, ""))
	request, err := result.ActivationRequest("claude:remote")
	if err != nil {
		t.Fatal(err)
	}
	state, err := openNativeMCPActivationStateFile(filepath.Join(t.TempDir(), nativeMCPStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.enable(request); err != nil {
		t.Fatal(err)
	}
	server, err := result.Materialize("claude:remote", nil, allowAllNativeMCPPolicy{}, state, nativeMCPRuntimeFeatures...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nativeMCPRuntimeSpecWithExpansion(server, func(value string) (string, error) {
		return expandClaudeEnvironment(value, func(string) (string, bool) { return "", false })
	}); err == nil ||
		!stringsContainAll(err.Error(), "claude:remote", "url", "HOST") {
		t.Fatalf("missing expansion error = %v", err)
	}
	values := map[string]string{"HOST": "mcp.example.test", "TOKEN": "opaque-token"}
	spec, err := nativeMCPRuntimeSpecWithExpansion(server, func(value string) (string, error) {
		return expandClaudeEnvironment(value, func(name string) (string, bool) { expanded, ok := values[name]; return expanded, ok })
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.URL != "https://mcp.example.test/mcp" || spec.Headers["Authorization"] != "Bearer opaque-token" {
		t.Fatalf("mapped HTTP spec = %#v", spec)
	}
}

func TestNativeMCPAssemblyUsesPolicyEnvironmentAfterLiveEnvironmentChanges(t *testing.T) {
	policy, home, workspace := nativeMCPTestPolicy(t, []string{"BIN=/startup"}, func(home, _ string, paths *mcppolicy.Paths) {
		writeNativeMCPConfig(t, paths.ClaudeManagedSettings, `{
  "allowedMcpServers":[{"serverCommand":["/approved/server"]}]
}`)
		writeNativeMCPConfig(t, paths.ClaudeUserSettings, `{"env":{"BIN":"/approved"}}`)
		writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers":{"worker":{"command":"${BIN}/server"}}
}`)
	})
	// Reproduce the policy/runtime split: policy loaded /approved, while the
	// process environment changes before the server is assembled.
	t.Setenv("BIN", "/evil")

	state, err := openNativeMCPActivationStateFile(filepath.Join(t.TempDir(), nativeMCPStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	inv := discoverNativeMCP(nativeMCPTestOptions(t, home, workspace), state)
	request, err := inv.result.ActivationRequest("claude:worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.enable(request); err != nil {
		t.Fatal(err)
	}
	specs, notes, err := activatedNativeMCPSpecs(inv, nil, policy)
	if err != nil || len(notes) != 0 || len(specs) != 1 {
		t.Fatalf("assembled specs=%#v notes=%#v err=%v", specs, notes, err)
	}
	if specs[0].Command != "/approved/server" {
		t.Fatalf("runtime command = %q, want policy-authorized snapshot", specs[0].Command)
	}
}

func TestNativeMCPRuntimeDoesNotClaimOAuth(t *testing.T) {
	home := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".codex", "config.toml"), `
[mcp_servers.remote]
url = "https://example.test/mcp"
auth = "oauth"
`)
	result := mcpnative.Discover(nativeMCPTestOptions(t, home, ""))
	request, err := result.ActivationRequest("codex:remote")
	if err != nil {
		t.Fatal(err)
	}
	state, err := openNativeMCPActivationStateFile(filepath.Join(t.TempDir(), nativeMCPStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.enable(request); err != nil {
		t.Fatal(err)
	}
	_, err = result.Materialize("codex:remote", nil, allowAllNativeMCPPolicy{}, state, nativeMCPRuntimeFeatures...)
	var compatibility *mcpnative.CompatibilityError
	if !errors.As(err, &compatibility) {
		t.Fatalf("OAuth compatibility gate = %T %v", err, err)
	}
}

func TestActivatedNativeMCPSpecsSkipsUnactivatedAndFailsRequiredUnsupported(t *testing.T) {
	home := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".codex", "config.toml"), `
[mcp_servers.local]
command = "local-server"

[mcp_servers.remote]
url = "https://example.test/mcp"
auth = "oauth"
required = true
`)
	state, err := openNativeMCPActivationStateFile(filepath.Join(t.TempDir(), nativeMCPStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	inv := discoverNativeMCP(nativeMCPTestOptions(t, home, ""), state)
	policy := allowAllNativeMCPAssemblyPolicy(t)
	specs, notes, err := activatedNativeMCPSpecs(inv, nil, policy)
	if err != nil || len(specs) != 0 || len(notes) != 0 {
		t.Fatalf("unactivated inventory affected assembly: specs=%#v notes=%#v err=%v", specs, notes, err)
	}
	localRequest, err := inv.result.ActivationRequest("codex:local")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.enable(localRequest); err != nil {
		t.Fatal(err)
	}
	specs, _, err = activatedNativeMCPSpecs(inv, nil, policy)
	if err != nil || len(specs) != 1 || specs[0].Name != "codex:local" {
		t.Fatalf("activated local specs = %#v, %v", specs, err)
	}
	remoteRequest, err := inv.result.ActivationRequest("codex:remote")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.enable(remoteRequest); err != nil {
		t.Fatal(err)
	}
	if _, _, err := activatedNativeMCPSpecs(inv, nil, policy); err == nil ||
		!stringsContainAll(err.Error(), "codex:remote", "runtime does not implement") {
		t.Fatalf("required unsupported native server = %v", err)
	}
}

func TestExpandClaudeEnvironmentValidation(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	for _, value := range []string{"${}", "${1BAD}", "${OPEN"} {
		if _, err := expandClaudeEnvironment(value, lookup); err == nil {
			t.Fatalf("invalid expansion %q was accepted", value)
		}
	}
	if got, err := expandClaudeEnvironment("plain-$VALUE", lookup); err != nil || got != "plain-$VALUE" {
		t.Fatalf("literal expansion = %q, %v", got, err)
	}
}

func stringsContainAll(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(value, fragment) {
			return false
		}
	}
	return true
}
