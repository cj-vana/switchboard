package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/execution"
)

func TestExecutionSandboxDefaultsOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.toml")
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox != execution.SandboxOff {
		t.Errorf("sandbox = %q, want off", cfg.Sandbox)
	}
}

func TestExecutionSandboxLoadsBooleanAndNamedForms(t *testing.T) {
	for input, want := range map[string]execution.SandboxMode{
		"true":   execution.SandboxOn,
		"false":  execution.SandboxOff,
		`"on"`:   execution.SandboxOn,
		`"off"`:  execution.SandboxOff,
		`"auto"`: execution.SandboxAuto,
	} {
		t.Run(input, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte("[execution]\nsandbox = "+input+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Sandbox != want {
				t.Errorf("sandbox = %q, want %q", cfg.Sandbox, want)
			}
		})
	}
}

func TestExecutionSandboxRejectsUnknownOrWrongType(t *testing.T) {
	for _, value := range []string{`"maybe"`, `17`, `["on"]`} {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte("[execution]\nsandbox = "+value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "execution.sandbox") {
			t.Errorf("value %s: err=%v", value, err)
		}
	}
}

func TestExecutionSandboxSaveRoundTrip(t *testing.T) {
	for _, mode := range []execution.SandboxMode{execution.SandboxOff, execution.SandboxOn, execution.SandboxAuto} {
		t.Run(string(mode), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			cfg := &Config{Path: path, Sandbox: mode, Slots: map[string]string{}, Auth: map[string]credential.Settings{}, Providers: map[string]ProviderSettings{}, UpdateCheck: true, UpdateAuto: true, CompactAuto: true, CompactAtPercent: 85}
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}
			reloaded, err := LoadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.Sandbox != mode {
				t.Errorf("sandbox = %q, want %q", reloaded.Sandbox, mode)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if mode == execution.SandboxOff && strings.Contains(string(data), "[execution]") {
				t.Error("default sandbox-off setting should be omitted")
			}
		})
	}
}
