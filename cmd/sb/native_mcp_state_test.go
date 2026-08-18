package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/switchboard-code/switchboard/internal/mcpnative"
)

func TestNativeMCPActivationBindsWholeDefinitionAndPersists(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	configPath := filepath.Join(home, ".codex", "config.toml")
	writeNativeMCPConfig(t, configPath, `[mcp_servers.docs]
command = "docs-server"
args = ["--mode", "safe"]
`)
	request := nativeMCPRequest(t, home, workspace, "codex:docs")
	path := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	state, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.NativeMCPActivated(request) {
		t.Fatal("native declaration activated itself")
	}
	if err := state.enable(request); err != nil {
		t.Fatal(err)
	}
	if status := state.status(request); !status.Enabled || status.Changed {
		t.Fatalf("activation status = %#v", status)
	}
	reopened, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.NativeMCPActivated(request) {
		t.Fatal("activation did not survive restart")
	}

	writeNativeMCPConfig(t, configPath, `[mcp_servers.docs]
command = "different-server"
args = ["--mode", "safe"]
`)
	changed := nativeMCPRequest(t, home, workspace, "codex:docs")
	if status := reopened.status(changed); status.Enabled || !status.Changed {
		t.Fatalf("changed definition retained authority: %#v", status)
	}
	if err := reopened.enable(changed); err != nil {
		t.Fatal(err)
	}
	if !reopened.NativeMCPActivated(changed) || reopened.NativeMCPActivated(request) {
		t.Fatal("reapproval did not replace the exact definition identity")
	}
	if err := reopened.disable(changed); err != nil {
		t.Fatal(err)
	}
	if reopened.NativeMCPActivated(changed) {
		t.Fatal("disabled definition remained active")
	}
}

func TestNativeMCPActivationStateFailsClosedOnUnsafeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	writeNativeMCPConfig(t, path, `{"version":1,"version":1,"key":"x","activations":[]}`)
	if _, err := openNativeMCPActivationStateFile(path); err == nil || !strings.Contains(err.Error(), "duplicate JSON key") {
		t.Fatalf("duplicate state error = %v", err)
	}

	if runtime.GOOS != "windows" {
		writeNativeMCPConfig(t, path, `{"version":1,"key":"x","activations":[]}`)
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := openNativeMCPActivationStateFile(path); err == nil || !strings.Contains(err.Error(), "want 0600") {
			t.Fatalf("loose-permission error = %v", err)
		}

		target := filepath.Join(t.TempDir(), "target.json")
		writeNativeMCPConfig(t, target, `{"version":1,"key":"x","activations":[]}`)
		link := filepath.Join(t.TempDir(), nativeMCPStateFileName)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := openNativeMCPActivationStateFile(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("symlink state error = %v", err)
		}
	}
}

func TestNativeMCPActivationMutationReloadFailsClosedOnUnsafeFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links is not generally available to unprivileged Windows tests")
	}
	home := t.TempDir()
	workspace := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {"docs": {"command":"docs"}}
}`)
	request := nativeMCPRequest(t, home, workspace, "claude:docs")
	directory := t.TempDir()
	path := filepath.Join(directory, nativeMCPStateFileName)
	state, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(directory, "target.json")
	target, err := openNativeMCPActivationStateFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.enable(request); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetPath, path); err != nil {
		t.Fatal(err)
	}

	err = state.enable(request)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("mutation through unsafe state error = %v", err)
	}
	if state.NativeMCPActivated(request) || len(state.references(workspace)) != 0 {
		t.Fatal("unsafe reload left cached activation authority usable")
	}
	reopened, err := openNativeMCPActivationStateFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.NativeMCPActivated(request) {
		t.Fatal("unsafe mutation changed the symlink target")
	}
}

func TestNativeMCPGlobalActivationAppliesWhenWorkspaceCannotResolve(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {"global": {"command":"global"}}
}`)
	request := nativeMCPRequest(t, home, workspace, "claude:global")
	state, err := openNativeMCPActivationStateFile(filepath.Join(t.TempDir(), nativeMCPStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.enableWithRequired(request, true); err != nil {
		t.Fatal(err)
	}
	missingWorkspace := filepath.Join(t.TempDir(), "missing")
	if !state.hasDialect(mcpnative.DialectClaude, missingWorkspace) {
		t.Fatal("unresolvable workspace hid a global activation")
	}
	if !state.snapshotFailureRequired(mcpnative.DialectClaude, missingWorkspace) {
		t.Fatal("unresolvable workspace hid global required semantics")
	}
	if references := state.references(missingWorkspace); len(references) != 1 || references[0].TrustRoot != "" {
		t.Fatalf("global references = %#v", references)
	}
}

func TestNativeMCPActivationMutationReloadsLatestAcrossOpenHandles(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {
    "x": {"command":"x"},
    "y": {"command":"y"}
  }
}`)
	x := nativeMCPRequest(t, home, workspace, "claude:x")
	y := nativeMCPRequest(t, home, workspace, "claude:y")
	path := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	first, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.enable(x); err != nil {
		t.Fatal(err)
	}
	if err := first.disable(x); err != nil {
		t.Fatal(err)
	}
	if err := stale.enable(y); err != nil {
		t.Fatal(err)
	}

	reopened, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.NativeMCPActivated(x) {
		t.Fatal("stale handle resurrected the activation another handle disabled")
	}
	if !reopened.NativeMCPActivated(y) {
		t.Fatal("stale handle's requested mutation was not persisted")
	}
	if references := reopened.references(workspace); len(references) != 1 || references[0].ID != "claude:y" {
		t.Fatalf("persisted references = %#v", references)
	}
}

func TestNativeMCPActivationConcurrentHandlesDoNotLoseUpdates(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	servers := make(map[string]any)
	const count = 32
	for index := range count {
		name := fmt.Sprintf("server_%02d", index)
		servers[name] = map[string]any{"command": name}
	}
	raw, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		t.Fatal(err)
	}
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), string(raw))
	discovered := mcpnative.Discover(nativeMCPTestOptions(t, home, workspace))
	requests := make([]mcpnative.ActivationRequest, count)
	for index := range count {
		requests[index], err = discovered.ActivationRequest(fmt.Sprintf("claude:server_%02d", index))
		if err != nil {
			t.Fatal(err)
		}
	}

	path := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	handles := make([]*nativeMCPActivationState, count)
	for index := range handles {
		handles[index], err = openNativeMCPActivationStateFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	runConcurrentNativeMCPMutations(t, count, func(index int) error {
		return handles[index].enableWithRequired(requests[index], index%3 == 0)
	})

	disableHandles := make([]*nativeMCPActivationState, count/2)
	for index := range disableHandles {
		disableHandles[index], err = openNativeMCPActivationStateFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	runConcurrentNativeMCPMutations(t, len(disableHandles), func(index int) error {
		return disableHandles[index].disable(requests[index*2])
	})

	reopened, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for index, request := range requests {
		if got, want := reopened.NativeMCPActivated(request), index%2 == 1; got != want {
			t.Errorf("activation %d enabled = %t, want %t", index, got, want)
		}
	}
	if references := reopened.references(workspace); len(references) != count/2 {
		t.Fatalf("remaining references = %d, want %d", len(references), count/2)
	}
}

func runConcurrentNativeMCPMutations(t *testing.T, count int, mutation func(int) error) {
	t.Helper()
	errorsByIndex := make([]error, count)
	var wait sync.WaitGroup
	for index := range count {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsByIndex[index] = mutation(index)
		}()
	}
	wait.Wait()
	for index, err := range errorsByIndex {
		if err != nil {
			t.Fatalf("mutation %d: %v", index, err)
		}
	}
}

func TestNativeMCPActivationCancellationWhileLockHeldDoesNotCommit(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {
    "x": {"command":"x"},
    "y": {"command":"y"}
  }
}`)
	x := nativeMCPRequest(t, home, workspace, "claude:x")
	y := nativeMCPRequest(t, home, workspace, "claude:y")
	path := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	state, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.enable(x); err != nil {
		t.Fatal(err)
	}
	held, err := acquireNativeMCPStateFileLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- state.enableWithRequiredContext(ctx, y, false) }()
	deadline := time.Now().Add(time.Second)
	for state.mu.TryLock() {
		state.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("mutation never began waiting for the interprocess lock")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled mutation error = %v", err)
	}
	if state.NativeMCPActivated(x) || state.NativeMCPActivated(y) {
		t.Fatal("ambiguous lock wait left cached activation authority usable")
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.NativeMCPActivated(x) || reopened.NativeMCPActivated(y) {
		t.Fatal("canceled lock wait changed persisted activation state")
	}
}

func nativeMCPRequest(t *testing.T, home, workspace, id string) mcpnative.ActivationRequest {
	t.Helper()
	result := mcpnative.Discover(nativeMCPTestOptions(t, home, workspace))
	request, err := result.ActivationRequest(id)
	if err != nil {
		t.Fatalf("activation request %s: %v; servers=%#v diagnostics=%#v", id, err, result.Servers, result.Diagnostics)
	}
	return request
}

// nativeMCPTestOptions seals the test's user Codex config into the same
// authoritative config/read shape production must obtain from Codex
// app-server. Tests must not accidentally exercise the quarantined fallback
// inventory when they intend to verify executable behavior.
func nativeMCPTestOptions(t *testing.T, home, workspace string) mcpnative.Options {
	t.Helper()
	options := mcpnative.Options{HomeDir: home, Workspace: workspace}
	configPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := os.Stat(configPath); err != nil {
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return options
	}
	var config map[string]any
	if _, err := toml.DecodeFile(configPath, &config); err != nil {
		t.Fatal(err)
	}
	realPath, err := filepath.EvalSymlinks(configPath)
	if err != nil {
		t.Fatal(err)
	}
	name := map[string]any{"type": "user", "file": filepath.Clean(realPath), "profile": nil}
	version := "cmd-test-user-v1"
	origins := map[string]any{}
	if servers, ok := config["mcp_servers"].(map[string]any); ok {
		for serverName, raw := range servers {
			server, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("test Codex server %q is not a table", serverName)
			}
			field := "command"
			if _, exists := server[field]; !exists {
				field = "url"
			}
			origins["mcp_servers."+serverName+"."+field] = map[string]any{"name": name, "version": version}
		}
	}
	result := map[string]any{
		"config": config, "origins": origins,
		"layers": []any{map[string]any{"name": name, "version": version, "config": config}},
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	cwd := workspace
	if cwd == "" {
		cwd = home
	}
	snapshot, err := mcpnative.NewCodexSnapshot(encoded, cwd)
	if err != nil {
		t.Fatalf("test Codex snapshot: %v", err)
	}
	options.CodexSnapshot = snapshot
	return options
}

func writeNativeMCPConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
