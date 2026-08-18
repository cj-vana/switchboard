package mcpnative

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/trust"
)

type fixedPolicy struct {
	allowed bool
	err     error
}

func (p fixedPolicy) NativeMCPAllowed(PolicyRequest) (bool, error) {
	return p.allowed, p.err
}

type exactActivation struct {
	key      []byte
	identity ActivationIdentity
}

func (a exactActivation) NativeMCPActivated(request ActivationRequest) bool {
	identity, err := request.Identity(a.key)
	return err == nil && identity == a.identity
}

func approve(t *testing.T, result Result, id string) exactActivation {
	t.Helper()
	request, err := result.ActivationRequest(id)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	identity, err := request.Identity(key)
	if err != nil {
		t.Fatal(err)
	}
	return exactActivation{key: key, identity: identity}
}

func TestMaterializeRequiresPolicyActivationAndImmutableDefinition(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mustMkdir(t, home)
	const secret = "materialization-secret"
	mustWrite(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {"safe": {"command":"`+secret+`","args":["--token=`+secret+`"]}}
}`)
	result := Discover(Options{HomeDir: home})
	activation := approve(t, result, "claude:safe")

	if _, err := result.Materialize("claude:safe", nil, nil, activation); !errors.Is(err, ErrPolicyRequired) {
		t.Fatalf("missing managed-policy gate = %v", err)
	}
	if _, err := result.Materialize("claude:safe", nil, fixedPolicy{}, activation); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("managed-policy deny = %v", err)
	}
	if _, err := result.Materialize("claude:safe", nil, fixedPolicy{err: errors.New("unsafe detail")}, activation); !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("managed-policy error = %v", err)
	}
	if _, err := result.Materialize("claude:safe", nil, fixedPolicy{allowed: true}, nil); !errors.Is(err, ErrActivationRequired) {
		t.Fatalf("missing Switchboard activation = %v", err)
	}

	// Mutating every public execution-bearing field must not alter the private
	// authoritative winner used by Materialize.
	result.Servers[0].Command = nil
	result.Servers[0].Args = nil
	result.Servers[0].Enabled = false
	result.Servers[0].TrustRoot = "/forged"
	materialized, err := result.Materialize("claude:safe", nil, fixedPolicy{allowed: true}, activation)
	if err != nil {
		t.Fatal(err)
	}
	if materialized.Command == nil || materialized.Command.Expose() != secret ||
		len(materialized.Args) != 1 || materialized.Args[0].Expose() != "--token="+secret {
		t.Fatalf("public summary mutation changed materialization: %+v", materialized)
	}

	request, err := result.ActivationRequest("claude:safe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := request.Identity([]byte("too short")); !errors.Is(err, ErrInvalidActivationKey) {
		t.Fatalf("short activation key = %v", err)
	}
	assertNeverRenders(t, secret, result, result.Servers[0], request, materialized,
		sensitive(secret), MaterializedValue{value: secret})
}

func TestProjectMaterializationAlsoRequiresWorkspaceTrust(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "repo")
	mustMkdir(t, home)
	mustMkdir(t, workspace)
	mustWrite(t, filepath.Join(workspace, ".mcp.json"), `{
  "mcpServers": {"project": {"command":"project-server"}}
}`)
	result := Discover(Options{HomeDir: home, Workspace: workspace})
	activation := approve(t, result, "claude:project")
	policy := fixedPolicy{allowed: true}
	if _, err := result.Materialize("claude:project", nil, policy, activation); err == nil {
		t.Fatal("project server ran without trust")
	} else {
		var trustErr *TrustRequiredError
		if !errors.As(err, &trustErr) {
			t.Fatalf("project trust error = %T %v", err, err)
		}
	}
	store, err := trust.OpenFile(filepath.Join(root, "state", "trust.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Materialize("claude:project", store, policy, activation); err == nil {
		t.Fatal("untrusted workspace materialized")
	}
	if err := store.Grant(workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := result.Materialize("claude:project", store, policy, activation); err != nil {
		t.Fatalf("trusted and activated project did not materialize: %v", err)
	}
}

func TestActivationIdentityChangesWithWholeEntryDefinition(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mustMkdir(t, home)
	path := filepath.Join(home, ".claude.json")
	mustWrite(t, path, `{"mcpServers":{"server":{"command":"one","args":["a","a"]}}}`)
	first := Discover(Options{HomeDir: home})
	firstRequest, err := first.ActivationRequest("claude:server")
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	firstID, err := firstRequest.Identity(key)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, `{"mcpServers":{"server":{"command":"one","args":["a","b"]}}}`)
	second := Discover(Options{HomeDir: home})
	secondRequest, err := second.ActivationRequest("claude:server")
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := secondRequest.Identity(key)
	if err != nil {
		t.Fatal(err)
	}
	if firstID.ID != secondID.ID || firstID.RealPath != secondID.RealPath || firstID.Digest == secondID.Digest {
		t.Fatalf("definition change did not invalidate activation: first=%+v second=%+v", firstID, secondID)
	}
}

func TestAuthoritativeDefinitionsDeepCloneEveryCompositeField(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mustMkdir(t, filepath.Join(home, ".codex"))
	mustWrite(t, filepath.Join(home, ".codex", "config.toml"), `
[mcp_servers.local]
command = "original-command"
args = ["first", "first"]
cwd = "original-cwd"
env = { TOKEN = "original-env" }
env_vars = [{ name = "INHERITED", source = "local" }]
enabled_tools = []
disabled_tools = ["danger"]
default_tools_approval_mode = "prompt"
startup_timeout_sec = 1.25
required = true

[mcp_servers.local.tools.read]
approval_mode = "approve"

[mcp_servers.remote]
url = "https://example.test/original"
http_headers = { Authorization = "original-header" }
env_http_headers = { X_Token = "HEADER_TOKEN" }
bearer_token_env_var = "BEARER_TOKEN"
oauth_resource = "original-resource"
scopes = []
oauth = { client_id = "original-client", callback_port = 4321 }
http_headers_helper = "original-helper"
omit_tools_from = ["direct"]
supports_parallel_tool_calls = true
`)
	snapshot := mustUserCodexSnapshot(t, filepath.Join(home, ".codex", "config.toml"), home)
	result := Discover(Options{HomeDir: home, CodexSnapshot: snapshot})

	localFeatures := serverNamed(t, result, "codex:local").RequiredFeatures()
	remoteFeatures := serverNamed(t, result, "codex:remote").RequiredFeatures()
	localActivation := approve(t, result, "codex:local")
	remoteActivation := approve(t, result, "codex:remote")

	for index := range result.Servers {
		server := &result.Servers[index]
		switch server.ID {
		case "codex:local":
			server.Provenance.ContributingLayers[0].Source = Source("forged-source")
			server.Command = sensitivePointer("forged-command")
			server.Args[0] = sensitive("forged-arg")
			server.CWD = nil
			server.Env["TOKEN"] = sensitive("forged-env")
			server.ForwardedEnv[0].Name = "FORGED_INHERIT"
			server.Tools.Enabled = []string{"forged-tool"}
			server.Tools.Disabled[0] = "forged-danger"
			server.Approvals.Tools["read"] = ApprovalAuto
			server.Required = false
			server.Timeouts.StartupSeconds = 99
		case "codex:remote":
			server.URL = nil
			server.Headers["Authorization"] = sensitive("forged-header")
			server.HeaderEnv["X_Token"] = "FORGED_HEADER_ENV"
			server.BearerTokenEnvVar = "FORGED_BEARER"
			server.OAuthResource = sensitivePointer("forged-resource")
			server.OAuthScopes = []string{"forged-scope"}
			server.CodexOAuth.ClientID = sensitivePointer("forged-client")
			server.CodexOAuth.CallbackPort = 9999
			server.HeadersHelper = sensitivePointer("forged-helper")
			server.OmitToolsFrom[0] = ToolExposureCodeMode
			server.SupportsParallelToolCalls = false
		}
	}

	local, err := result.Materialize(
		"codex:local", nil, fixedPolicy{allowed: true}, localActivation, localFeatures...,
	)
	if err != nil {
		t.Fatal(err)
	}
	if local.Command.Expose() != "original-command" || local.Args[0].Expose() != "first" ||
		local.CWD.Expose() != filepath.Join(filepath.Dir(local.Provenance.RealPath), "original-cwd") || local.Env["TOKEN"].Expose() != "original-env" ||
		local.ForwardedEnv[0].Name != "INHERITED" || local.Tools.Disabled[0] != "danger" ||
		local.Approvals.Tools["read"] != ApprovalApprove || !local.Required ||
		local.Timeouts.StartupSeconds != 1.25 ||
		local.Provenance.ContributingLayers[0].Source != SourceCodexUser {
		t.Fatalf("local authoritative definition shared public storage: %+v", local)
	}
	remote, err := result.Materialize(
		"codex:remote", nil, fixedPolicy{allowed: true}, remoteActivation, remoteFeatures...,
	)
	if err != nil {
		t.Fatal(err)
	}
	if remote.URL.Expose() != "https://example.test/original" ||
		remote.Headers["Authorization"].Expose() != "original-header" ||
		remote.HeaderEnv["X_Token"] != "HEADER_TOKEN" ||
		remote.BearerTokenEnvVar != "BEARER_TOKEN" ||
		remote.OAuthResource.Expose() != "original-resource" || len(remote.OAuthScopes) != 0 ||
		remote.CodexOAuth.ClientID.Expose() != "original-client" || remote.CodexOAuth.CallbackPort != 4321 ||
		remote.HeadersHelper.Expose() != "original-helper" || remote.OmitToolsFrom[0] != ToolExposureDirect ||
		!remote.SupportsParallelToolCalls {
		t.Fatalf("remote authoritative definition shared public storage: %+v", remote)
	}
}

func TestMalformedHigherLayerQuarantinesLowerWinner(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "repo")
	mustMkdir(t, filepath.Join(home, ".codex"))
	mustMkdir(t, filepath.Join(workspace, ".codex"))
	mustWrite(t, filepath.Join(home, ".codex", "config.toml"), "[mcp_servers.same]\ncommand='user'\n")
	mustWrite(t, filepath.Join(workspace, ".codex", "config.toml"), "not valid = [")
	result := Discover(Options{HomeDir: home, Workspace: workspace})
	server := serverNamed(t, result, "codex:same")
	if server.Provenance.Scope != ScopeUser {
		t.Fatalf("unexpected winner: %+v", server)
	}
	var quarantine *DiscoveryQuarantinedError
	if _, err := result.ActivationRequest("codex:same"); !errors.As(err, &quarantine) {
		t.Fatalf("lower winner was not quarantined: %v", err)
	}

	nested := filepath.Join(workspace, "nested")
	mustMkdir(t, filepath.Join(nested, ".codex"))
	mustWrite(t, filepath.Join(nested, ".codex", "config.toml"), "[mcp_servers.same]\ncommand='nested'\n")
	result = Discover(Options{HomeDir: home, Workspace: workspace, CurrentDir: nested})
	if _, err := result.ActivationRequest("codex:same"); !errors.As(err, &quarantine) {
		t.Fatalf("valid higher partial layer bypassed an unknown recursively merged layer: %v", err)
	}
}

func TestMalformedLowerCodexLayerQuarantinesValidProjectEntry(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "repo")
	mustMkdir(t, filepath.Join(home, ".codex"))
	mustMkdir(t, filepath.Join(workspace, ".codex"))
	mustWrite(t, filepath.Join(home, ".codex", "config.toml"), "not valid = [")
	mustWrite(t, filepath.Join(workspace, ".codex", "config.toml"), "[mcp_servers.same]\ncommand='project'\n")
	result := Discover(Options{HomeDir: home, Workspace: workspace})
	server := serverNamed(t, result, "codex:same")
	if expose(server.Command) != "project" {
		t.Fatalf("project summary was not discovered: %+v", server)
	}
	var quarantine *DiscoveryQuarantinedError
	if _, err := result.ActivationRequest("codex:same"); !errors.As(err, &quarantine) {
		t.Fatalf("unknown lower Codex layer did not quarantine merged winner: %v", err)
	}
}

func TestInvalidExplicitRootsQuarantineUnknownNativeLayers(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "repo")
	mustMkdir(t, filepath.Join(workspace, ".codex"))
	mustWrite(t, filepath.Join(workspace, ".codex", "config.toml"), "[mcp_servers.codex]\ncommand='server'\n")
	mustWrite(t, filepath.Join(workspace, ".mcp.json"), `{"mcpServers":{"claude":{"command":"server"}}}`)
	result := Discover(Options{HomeDir: filepath.Join(root, "missing-home"), Workspace: workspace})
	for _, id := range []string{"codex:codex", "claude:claude"} {
		var quarantine *DiscoveryQuarantinedError
		if _, err := result.ActivationRequest(id); !errors.As(err, &quarantine) {
			t.Fatalf("%s bypassed an unresolved explicitly supplied home: %v", id, err)
		}
	}

	home := filepath.Join(root, "home")
	mustMkdir(t, filepath.Join(home, ".codex"))
	mustWrite(t, filepath.Join(home, ".codex", "config.toml"), "[mcp_servers.user]\ncommand='server'\n")
	outside := filepath.Join(root, "outside")
	mustMkdir(t, outside)
	result = Discover(Options{HomeDir: home, Workspace: workspace, CurrentDir: outside})
	var quarantine *DiscoveryQuarantinedError
	if _, err := result.ActivationRequest("codex:user"); !errors.As(err, &quarantine) {
		t.Fatalf("user Codex server bypassed invalid current-directory project chain: %v", err)
	}
}

func assertNeverRenders(t *testing.T, secret string, values ...any) {
	t.Helper()
	formats := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d", "%20v"}
	for _, value := range values {
		for _, format := range formats {
			if rendered := fmt.Sprintf(format, value); strings.Contains(rendered, secret) {
				t.Fatalf("%T rendered secret with %s: %s", value, format, rendered)
			}
		}
		encoded, err := json.Marshal(value)
		if err == nil && strings.Contains(string(encoded), secret) {
			t.Fatalf("%T JSON rendered secret: %s", value, encoded)
		}
	}
}
