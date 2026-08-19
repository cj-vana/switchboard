package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/mcp"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/tools"
)

type collisionTool struct {
	name        string
	description string
}

type staticExternalTool struct {
	name string
}

func (t *staticExternalTool) Name() string        { return t.name }
func (t *staticExternalTool) Description() string { return "test external tool" }
func (t *staticExternalTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (t *staticExternalTool) ParallelSafe() bool { return false }
func (t *staticExternalTool) Plan(json.RawMessage) (tools.Plan, error) {
	return tools.Plan{
		Request: permission.Request{Tool: t.name, Effect: permission.EffectExternal},
		Run: func(context.Context) (tools.Result, error) {
			return tools.Result{Content: "unused"}, nil
		},
	}, nil
}

// collisionServer is a legacy Streamable HTTP server with a deliberately
// adversarial tools/list result. connectMCP still performs the real transport
// negotiation and assembly; only the remote process is replaced by this
// deterministic fixture.
func collisionServer(t *testing.T, advertised ...collisionTool) *httptest.Server {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// An unrecognized HTTP 400 is the client's explicit signal that this is
		// an initialization-era MCP server.
		if req.Method == "server/discover" {
			http.Error(w, "initialize first", http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-06-18",
				"serverInfo": map[string]any{
					"name":    "collision-fixture",
					"version": "1",
				},
			}
		case "tools/list":
			definitions := make([]map[string]any, 0, len(advertised))
			for _, tool := range advertised {
				definitions = append(definitions, map[string]any{
					"name":        tool.name,
					"description": tool.description,
					"inputSchema": map[string]any{"type": "object"},
				})
			}
			result = map[string]any{"tools": definitions}
		default:
			result = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      *req.ID,
			"result":  result,
		})
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func connectConfiguredMCP(t *testing.T, config string) (*tools.Registry, *mcpState, []permission.Rule) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".switchboard")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	registry, err := tools.NewRegistry(workspace, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	state, rules, err := connectMCP(ctx, workspace, nil, registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(state.Close)
	if len(state.clientList()) == 0 {
		t.Fatal("the collision fixture did not connect")
	}
	return registry, state, rules
}

func permissionForRegisteredTool(t *testing.T, registry *tools.Registry, rules []permission.Rule, name string) permission.Decision {
	t.Helper()
	tool, ok := registry.Get(name)
	if !ok {
		t.Fatalf("tool %s was not registered", name)
	}
	plan, err := tool.Plan(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	engine := permission.NewEngine(permission.ModeDefault, execution.Capability{}, rules...)
	return engine.Check(plan.Request).Decision
}

func TestMCPIntraServerCollisionCannotBorrowAllowRule(t *testing.T) {
	srv := collisionServer(t,
		collisionTool{name: "read.file", description: "the dot read"},
		collisionTool{name: "read file", description: "the spaced read"},
		collisionTool{name: "safe", description: "an unambiguous tool"},
	)
	registry, state, rules := connectConfiguredMCP(t, fmt.Sprintf(`
[mcp.intra]
url = %q
allow = ["read file", "safe"]
`, srv.URL))

	// The two read identities are ambiguous after sanitizing, so neither is
	// registered and the losing allow cannot create a rule for the canonical
	// name. An unrelated exact allow remains effective.
	if _, ok := registry.Get("mcp__intra__read_file"); ok {
		t.Fatal("an intra-server sanitizer collision must register no survivor")
	}
	engine := permission.NewEngine(permission.ModeDefault, execution.Capability{}, rules...)
	if got := engine.Check(permission.Request{Tool: "mcp__intra__read_file", Effect: permission.EffectExternal}).Decision; got != permission.Ask {
		t.Fatalf("ambiguous exposed name decision = %s, want ask", got)
	}
	if got := permissionForRegisteredTool(t, registry, rules, "mcp__intra__safe"); got != permission.Allow {
		t.Fatalf("unambiguous exact allow decision = %s, want allow", got)
	}
	if len(rules) != 1 || rules[0].Tool != "mcp__intra__safe" {
		t.Fatalf("rules = %+v, want only the exact registered identity", rules)
	}

	notes := state.attach(nil)
	text := notesText(notes)
	for _, want := range []string{
		`server "intra" tool "read file"`,
		`server "intra" tool "read.file"`,
		`exposed name "mcp__intra__read_file"`,
		`none was registered`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("collision diagnostics missing %q:\n%s", want, text)
		}
	}
}

func TestMCPIntraServerCollisionIgnoresAdvertisementOrder(t *testing.T) {
	run := func(advertised ...collisionTool) string {
		t.Helper()
		srv := collisionServer(t, advertised...)
		registry, state, rules := connectConfiguredMCP(t, fmt.Sprintf(`
[mcp.intra]
url = %q
allow = ["read file"]
`, srv.URL))
		if _, ok := registry.Get("mcp__intra__read_file"); ok || len(rules) != 0 {
			t.Fatalf("ambiguous identity registered or gained rules: %+v", rules)
		}
		for _, note := range state.attach(nil) {
			if strings.Contains(note.text, "raw identity is ambiguous") {
				return note.text
			}
		}
		t.Fatal("no deterministic collision diagnostic")
		return ""
	}

	dot := collisionTool{name: "read.file", description: "dot"}
	space := collisionTool{name: "read file", description: "space"}
	forward := run(dot, space)
	reverse := run(space, dot)
	if forward != reverse {
		t.Fatalf("collision diagnostic depends on tools/list order:\n%s\n%s", forward, reverse)
	}
}

func TestMCPInterServerCollisionCannotBorrowAllowRule(t *testing.T) {
	srv := collisionServer(t,
		collisionTool{name: "read", description: "read from this server"},
		collisionTool{name: "write", description: "write from this server"},
	)
	registry, state, rules := connectConfiguredMCP(t, fmt.Sprintf(`
[mcp."a b"]
url = %q
allow = ["write"]

[mcp."a.b"]
url = %q
allow = ["read"]
`, srv.URL, srv.URL))

	// Both server names sanitize to a_b. Registration chooses the raw identity
	// deterministically: server "a b" sorts before server "a.b". An allow from
	// the losing server cannot cross that identity boundary.
	if got := permissionForRegisteredTool(t, registry, rules, "mcp__a_b__read"); got != permission.Ask {
		t.Fatalf("cross-server survivor decision = %s, want ask", got)
	}
	if got := permissionForRegisteredTool(t, registry, rules, "mcp__a_b__write"); got != permission.Allow {
		t.Fatalf("exact winner decision = %s, want allow", got)
	}
	if len(rules) != 1 || rules[0].Tool != "mcp__a_b__write" {
		t.Fatalf("rules = %+v, want only the deterministic winner's allow", rules)
	}
	if tool, _ := registry.Get("mcp__a_b__read"); !strings.Contains(tool.Description(), "[a b MCP]") {
		t.Fatalf("cross-server winner was not deterministic: %s", tool.Description())
	}

	notes := state.attach(nil)
	text := notesText(notes)
	for _, want := range []string{
		`server "a b" tool "read"`,
		`server "a.b" tool "read"`,
		`exposed name "mcp__a_b__read"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("collision diagnostics missing %q:\n%s", want, text)
		}
	}
}

func TestMCPAllowRuleRequiresSuccessfulRegistration(t *testing.T) {
	registry, err := tools.NewRegistry(t.TempDir(), execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	const exposed = "mcp__taken__read"
	if err := registry.AddExternal(&staticExternalTool{name: exposed}); err != nil {
		t.Fatal(err)
	}

	state := &mcpState{}
	rules, count := registerMCPTools(registry, state, []mcpToolRegistration{{
		server:  "taken",
		rawTool: "read",
		exposed: exposed,
		tool:    &staticExternalTool{name: exposed},
		allowed: true,
	}})
	if count != 0 || len(rules) != 0 {
		t.Fatalf("registration count %d, rules %+v; a failed registration must grant nothing", count, rules)
	}
	if got := permissionForRegisteredTool(t, registry, rules, exposed); got != permission.Ask {
		t.Fatalf("pre-existing tool decision = %s, want ask", got)
	}
	if text := notesText(state.attach(nil)); !strings.Contains(text, "already registered") {
		t.Fatalf("failed registration was not diagnosed: %s", text)
	}
}

func TestRequiredMCPFailureStopsSessionAssembly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".switchboard")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "mcp.toml")
	if err := os.WriteFile(configPath, []byte(`
[mcp.required]
command = "/definitely/missing/switchboard-mcp-server"
required = true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	registry, err := tools.NewRegistry(workspace, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	state, rules, err := connectMCP(context.Background(), workspace, nil, registry, nil)
	if err == nil || !strings.Contains(err.Error(), "required mcp server required did not connect") {
		t.Fatalf("required startup error = %v", err)
	}
	if state == nil || len(state.clientList()) != 0 || len(rules) != 0 {
		t.Fatalf("failed required assembly leaked clients or rules: state=%#v rules=%#v", state, rules)
	}
}

func TestMalformedApplicableMCPConfigCannotHideRequiredServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".switchboard")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.toml"), []byte(`
[mcp.critical]
command = "critical-server"
required = true

[mcp.invalid]
command = "other-server"
startup_timeout_seconds = -1
`), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	registry, err := tools.NewRegistry(workspace, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	state, rules, err := connectMCP(context.Background(), workspace, nil, registry, nil)
	if err == nil || !strings.Contains(err.Error(), "applicable MCP configuration is invalid") {
		t.Fatalf("malformed applicable config error = %v", err)
	}
	if state == nil || len(state.clientList()) != 0 || len(rules) != 0 {
		t.Fatalf("malformed config leaked clients or rules: state=%#v rules=%#v", state, rules)
	}
}

func TestOptionalMCPFailureDoesNotStopSessionAssembly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".switchboard")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.toml"), []byte(`
[mcp.optional]
command = "/definitely/missing/switchboard-mcp-server"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	registry, err := tools.NewRegistry(workspace, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	state, rules, err := connectMCP(context.Background(), workspace, nil, registry, nil)
	if err != nil {
		t.Fatalf("optional startup failure became fatal: %v", err)
	}
	if state == nil || len(state.clientList()) != 0 || len(rules) != 0 {
		t.Fatalf("optional failure leaked clients or rules: state=%#v rules=%#v", state, rules)
	}
}

func TestMCPAssemblyDoesNotTruncateConfiguredStartupTimeout(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	configured, configuredCancel := mcpAssemblyConnectContext(parent, mcp.Spec{
		Name: "slow-native", StartupTimeout: 60 * time.Second,
	})
	defer configuredCancel()
	if _, ok := configured.Deadline(); ok {
		t.Fatal("assembly added a deadline on top of the configured per-server startup timeout")
	}

	defaulted, defaultedCancel := mcpAssemblyConnectContext(parent, mcp.Spec{Name: "defaulted"})
	defer defaultedCancel()
	deadline, ok := defaulted.Deadline()
	if !ok {
		t.Fatal("server without a configured startup timeout has no assembly default")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > mcpConnectTimeout {
		t.Fatalf("default assembly timeout remaining = %v", remaining)
	}
}

func notesText(notes []mcpNote) string {
	lines := make([]string, 0, len(notes))
	for _, note := range notes {
		lines = append(lines, note.text)
	}
	return strings.Join(lines, "\n")
}
