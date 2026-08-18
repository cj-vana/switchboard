package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCodexRequirementsAbsentIsStrict(t *testing.T) {
	if checked, err := codexRequirementsAbsent([]byte(`{"requirements":null}`)); err != nil || !checked {
		t.Fatalf("null requirements = %v, %v", checked, err)
	}
	if checked, err := codexRequirementsAbsent([]byte(`{"requirements":{}}`)); err != nil || checked {
		t.Fatalf("present requirements = %v, %v", checked, err)
	}
	for _, raw := range []string{
		`{"requirements":null,"extra":true}`,
		`{"requirements":null,"requirements":{}}`,
		`{}`,
	} {
		if _, err := codexRequirementsAbsent([]byte(raw)); err == nil {
			t.Fatalf("invalid requirements snapshot %s was accepted", raw)
		}
	}
}

func TestCodexAppServerSnapshotUsesBoundedAuthoritativeResponses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX test launcher")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.toml")
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	name := map[string]any{"type": "user", "file": configPath, "profile": nil}
	configResult, err := json.Marshal(map[string]any{
		"config":  map[string]any{"mcp_servers": map[string]any{}},
		"origins": map[string]any{},
		"layers": []any{map[string]any{
			"name": name, "version": "test-v1", "config": map[string]any{},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	responses := []string{
		`{"id":1,"result":{"ok":true}}`,
		fmt.Sprintf(`{"id":2,"result":%s}`, configResult),
		`{"id":3,"result":{"requirements":null}}`,
	}
	script := "#!/bin/sh\n"
	for _, response := range responses {
		script += "IFS= read -r request || exit 1\nprintf '%s\\n' '" + strings.ReplaceAll(response, "'", "'\"'\"'") + "'\n"
	}
	codexPath := filepath.Join(bin, "codex")
	if err := os.WriteFile(codexPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	snapshot, err := readCodexAppServerSnapshot(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.config == nil || !snapshot.requirementsChecked {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestCodexExecutableCannotComeFromWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX executable permissions")
	}
	workspace := t.TempDir()
	codexPath := filepath.Join(workspace, "codex")
	if err := os.WriteFile(codexPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", workspace)
	if _, err := trustedCodexExecutable(workspace); err == nil || !strings.Contains(err.Error(), "workspace-local") {
		t.Fatalf("workspace-local Codex executable = %v", err)
	}
}
