package mcpnative

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustUserCodexSnapshot(t *testing.T, configPath, cwd string) *CodexSnapshot {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	realPath, err := filepath.EvalSymlinks(configPath)
	if err != nil {
		t.Fatal(err)
	}
	base := Provenance{
		Dialect:  DialectCodex,
		Scope:    ScopeUser,
		Source:   SourceCodexUser,
		Path:     filepath.Clean(configPath),
		RealPath: filepath.Clean(realPath),
	}
	servers, diagnostics, valid := decodeCodexServers(data, base)
	if !valid || len(diagnostics) != 0 {
		t.Fatalf("test Codex config is invalid: valid=%v diagnostics=%+v", valid, diagnostics)
	}
	servers, diagnostics, valid = normalizeCodexLayerPaths(servers, filepath.Dir(realPath), base)
	if !valid || len(diagnostics) != 0 {
		t.Fatalf("test Codex paths are invalid: valid=%v diagnostics=%+v", valid, diagnostics)
	}
	name := map[string]any{"type": "user", "file": filepath.Clean(realPath), "profile": nil}
	version := "test-user-layer-v1"
	origins := make(map[string]any, len(servers))
	for serverName, raw := range servers {
		entry, ok := asMap(raw)
		if !ok {
			t.Fatalf("test server %q is not an object", serverName)
		}
		field := "command"
		if _, exists := entry[field]; !exists {
			field = "url"
		}
		origins["mcp_servers."+serverName+"."+field] = map[string]any{
			"name": name, "version": version,
		}
	}
	config := map[string]any{"mcp_servers": servers}
	return mustCodexSnapshot(t, cwd, map[string]any{
		"config":  config,
		"origins": origins,
		"layers": []any{map[string]any{
			"name": name, "version": version, "config": config,
		}},
	})
}

func mustCodexSnapshot(t *testing.T, cwd string, result any) *CodexSnapshot {
	t.Helper()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewCodexSnapshot(encoded, cwd)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestDirectCodexInventoryRequiresAuthoritativeLayerSnapshot(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mustMkdir(t, filepath.Join(home, ".codex"))
	mustWrite(t, filepath.Join(home, ".codex", "config.toml"), "[mcp_servers.visible]\ncommand='server'\n")

	result := Discover(Options{HomeDir: home})
	server := serverNamed(t, result, "codex:visible")
	if !server.Supported || !server.Enabled {
		t.Fatalf("inventory summary was lost: %+v", server)
	}
	var quarantine *DiscoveryQuarantinedError
	if _, err := result.ActivationRequest(server.ID); !errors.As(err, &quarantine) ||
		quarantine.Quarantine.Code != "codex-layer-stack-unavailable" {
		t.Fatalf("incomplete Codex layer stack did not fail closed: %T %v", err, err)
	}
}

func TestCodexSnapshotPreservesLowerSystemDisable(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mustMkdir(t, home)
	systemPath := filepath.Join(root, "system-config.toml")
	userPath := filepath.Join(home, "config.toml")
	mustWrite(t, systemPath, "")
	mustWrite(t, userPath, "")

	systemName := map[string]any{"type": "system", "file": systemPath}
	userName := map[string]any{"type": "user", "file": userPath, "profile": nil}
	systemEntry := map[string]any{"enabled": false}
	userEntry := map[string]any{"command": "user-command"}
	effectiveEntry := map[string]any{"command": "user-command", "enabled": false}
	payload := map[string]any{
		"config": map[string]any{"mcp_servers": map[string]any{"same": effectiveEntry}},
		"origins": map[string]any{
			"mcp_servers.same.command": map[string]any{"name": userName, "version": "user-v1"},
		},
		// app-server returns layers from highest to lowest precedence.
		"layers": []any{
			map[string]any{"name": userName, "version": "user-v1", "config": map[string]any{"mcp_servers": map[string]any{"same": userEntry}}},
			map[string]any{"name": systemName, "version": "system-v1", "config": map[string]any{"mcp_servers": map[string]any{"same": systemEntry}}},
		},
	}
	snapshot := mustCodexSnapshot(t, home, payload)
	result := Discover(Options{HomeDir: home, CodexSnapshot: snapshot})
	server := serverNamed(t, result, "codex:same")
	if server.Enabled || !server.EnabledSet {
		t.Fatalf("lower system disable was lost: %+v", server)
	}
	if got := server.Provenance.ContributingLayers; len(got) != 2 ||
		got[0].Source != SourceCodexSystem || got[1].Source != SourceCodexUser {
		t.Fatalf("contributing layers = %+v", got)
	}
	if _, err := result.ActivationRequest(server.ID); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled effective entry became activatable: %v", err)
	}

	// The result object's effective view must agree with its returned layers;
	// merely labeling a response authoritative cannot erase a lower deny.
	effectiveEntry["enabled"] = true
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewCodexSnapshot(encoded, home); !errors.Is(err, ErrInvalidCodexSnapshot) {
		t.Fatalf("inconsistent effective enablement was accepted: %T %v", err, err)
	}
}

func TestCodexSnapshotUsesRawLayersBehindNormalizedEffectiveConfig(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	configPath := filepath.Join(home, ".codex", "config.toml")
	mustMkdir(t, filepath.Dir(configPath))
	mustWrite(t, configPath, "")

	name := map[string]any{"type": "user", "file": configPath, "profile": nil}
	rawEntry := map[string]any{
		"command":             "demo",
		"args":                []any{"one", "one"},
		"cwd":                 "relative-dir",
		"enabled":             false,
		"startup_timeout_ms":  float64(0),
		"startup_timeout_sec": 0.5,
		"enabled_tools":       []any{},
	}
	// config/read returns a typed, normalized effective view while retaining
	// native input spelling and presence in layers[].config. In particular it
	// inserts defaults/nulls and drops the shadowed millisecond timeout alias.
	effectiveEntry := map[string]any{
		"command":             "demo",
		"args":                []any{"one", "one"},
		"cwd":                 "relative-dir",
		"environment_id":      "local",
		"enabled":             false,
		"startup_timeout_sec": 0.5,
		"tool_timeout_sec":    nil,
		"enabled_tools":       []any{},
	}
	snapshot := mustCodexSnapshot(t, root, map[string]any{
		"config": map[string]any{"mcp_servers": map[string]any{"normalized": effectiveEntry}},
		"origins": map[string]any{
			"mcp_servers.normalized.command": map[string]any{"name": name, "version": "user-v1"},
		},
		"layers": []any{map[string]any{
			"name": name, "version": "user-v1",
			"config": map[string]any{"mcp_servers": map[string]any{"normalized": rawEntry}},
		}},
	})

	result := Discover(Options{HomeDir: home, CodexSnapshot: snapshot})
	server := serverNamed(t, result, "codex:normalized")
	if !server.Supported || server.Enabled || !server.EnabledSet {
		t.Fatalf("raw native enablement was not preserved: %+v", server)
	}
	expectedCWD := filepath.Join(filepath.Dir(canonicalSnapshotPath(configPath)), "relative-dir")
	if server.CWD == nil || server.CWD.raw() != expectedCWD {
		t.Fatalf("layer-relative cwd = %v, want %q", server.CWD, expectedCWD)
	}
	if len(server.Args) != 2 || server.Args[0].raw() != "one" || server.Args[1].raw() != "one" {
		t.Fatalf("ordered duplicate argv was not preserved: %+v", server.Args)
	}
	if !server.Timeouts.StartupMillisSet || !server.Timeouts.StartupSet || server.Timeouts.StartupSeconds != 0.5 {
		t.Fatalf("raw timeout presence was not preserved: %+v", server.Timeouts)
	}
	if !server.Tools.EnabledSet || len(server.Tools.Enabled) != 0 {
		t.Fatalf("explicit empty tool allow-list was not preserved: %+v", server.Tools)
	}
	if server.EnvironmentID != "local" || server.EnvironmentIDSet {
		t.Fatalf("native local default was not represented correctly: value=%q set=%v", server.EnvironmentID, server.EnvironmentIDSet)
	}
}

func TestCodexSnapshotProjectContributorRequiresWorkspaceTrust(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "repo")
	dotCodex := filepath.Join(workspace, ".codex")
	mustMkdir(t, home)
	mustMkdir(t, dotCodex)
	userPath := filepath.Join(home, "config.toml")
	mustWrite(t, userPath, "")

	userName := map[string]any{"type": "user", "file": userPath, "profile": nil}
	projectName := map[string]any{"type": "project", "dotCodexFolder": dotCodex}
	effectiveEntry := map[string]any{"command": "project-command", "env": map[string]any{"BASE": "one"}}
	snapshot := mustCodexSnapshot(t, workspace, map[string]any{
		"config": map[string]any{"mcp_servers": map[string]any{"project": effectiveEntry}},
		"origins": map[string]any{
			"mcp_servers.project.command": map[string]any{"name": projectName, "version": "project-v1"},
		},
		"layers": []any{
			map[string]any{"name": projectName, "version": "project-v1", "config": map[string]any{"mcp_servers": map[string]any{"project": map[string]any{"command": "project-command"}}}},
			map[string]any{"name": userName, "version": "user-v1", "config": map[string]any{"mcp_servers": map[string]any{"project": map[string]any{"env": map[string]any{"BASE": "one"}}}}},
		},
	})
	result := Discover(Options{HomeDir: home, Workspace: workspace, CodexSnapshot: snapshot})
	server := serverNamed(t, result, "codex:project")
	realWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !server.ExecutionTrustRequired || server.TrustRoot != filepath.Clean(realWorkspace) {
		t.Fatalf("project contributor did not retain Switchboard trust: %+v", server)
	}
	activation := approve(t, result, server.ID)
	if _, err := result.Materialize(server.ID, nil, fixedPolicy{allowed: true}, activation); err == nil {
		t.Fatal("project-contributed Codex server ran without workspace trust")
	} else {
		var trustErr *TrustRequiredError
		if !errors.As(err, &trustErr) {
			t.Fatalf("project trust error = %T %v", err, err)
		}
	}
}

func TestCodexSnapshotRejectsIncompleteOrMisorderedResults(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, root)
	path := filepath.Join(root, "config.toml")
	mustWrite(t, path, "")
	userName := map[string]any{"type": "user", "file": path, "profile": nil}
	systemName := map[string]any{"type": "system", "file": path}
	entry := map[string]any{"command": "server"}
	origin := map[string]any{"name": userName, "version": "user-v1"}
	validLayer := map[string]any{"name": userName, "version": "user-v1", "config": map[string]any{"mcp_servers": map[string]any{"one": entry}}}
	tests := []struct {
		name   string
		result map[string]any
	}{
		{
			name: "missing layers",
			result: map[string]any{
				"config":  map[string]any{"mcp_servers": map[string]any{"one": entry}},
				"origins": map[string]any{"mcp_servers.one.command": origin},
			},
		},
		{
			name: "origin absent from layers",
			result: map[string]any{
				"config":  map[string]any{"mcp_servers": map[string]any{"one": entry}},
				"origins": map[string]any{"mcp_servers.one.command": origin},
				"layers":  []any{},
			},
		},
		{
			name: "low before high",
			result: map[string]any{
				"config":  map[string]any{"mcp_servers": map[string]any{"one": entry}},
				"origins": map[string]any{"mcp_servers.one.command": origin},
				"layers": []any{
					map[string]any{"name": systemName, "version": "system-v1", "config": map[string]any{}},
					validLayer,
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.result)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewCodexSnapshot(encoded, root); !errors.Is(err, ErrInvalidCodexSnapshot) {
				t.Fatalf("NewCodexSnapshot error = %T %v", err, err)
			}
		})
	}
}

func TestCodexSnapshotNeverRendersConfigurationValues(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, root)
	path := filepath.Join(root, "config.toml")
	mustWrite(t, path, "")
	const secret = "snapshot-secret-sentinel"
	name := map[string]any{"type": "user", "file": path, "profile": nil}
	entry := map[string]any{"command": secret}
	snapshot := mustCodexSnapshot(t, root, map[string]any{
		"config": map[string]any{"mcp_servers": map[string]any{"one": entry}},
		"origins": map[string]any{
			"mcp_servers.one.command": map[string]any{"name": name, "version": "user-v1"},
		},
		"layers": []any{map[string]any{
			"name": name, "version": "user-v1", "config": map[string]any{"mcp_servers": map[string]any{"one": entry}},
		}},
	})
	renderings := []string{
		fmt.Sprint(snapshot), fmt.Sprintf("%v", snapshot), fmt.Sprintf("%+v", snapshot),
		fmt.Sprintf("%#v", snapshot), fmt.Sprintf("%q", snapshot), fmt.Sprintf("%x", snapshot),
		fmt.Sprint(*snapshot), fmt.Sprintf("%v", *snapshot), fmt.Sprintf("%+v", *snapshot),
		fmt.Sprintf("%#v", *snapshot), fmt.Sprintf("%q", *snapshot), fmt.Sprintf("%x", *snapshot),
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	renderings = append(renderings, string(encoded))
	encoded, err = json.Marshal(*snapshot)
	if err != nil {
		t.Fatal(err)
	}
	renderings = append(renderings, string(encoded))
	for _, rendered := range renderings {
		if strings.Contains(rendered, secret) {
			t.Fatalf("snapshot rendered a configuration value: %s", rendered)
		}
	}
}
