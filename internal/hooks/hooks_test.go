package hooks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/switchboard-code/switchboard/internal/permission"
)

func reqFor(tool string) permission.Request {
	return permission.Request{Tool: tool, Effect: permission.EffectExecute, Argv: []string{"go", "test"}}
}

func writeHooks(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadParsesEventsInOrder(t *testing.T) {
	path := writeHooks(t, `
[[hooks.pre_tool]]
tools = ["exec"]
run = "./audit.sh"
timeout_seconds = 5

[[hooks.post_tool]]
tools = ["write", "edit"]
run = "gofmt -l ."
`)
	s, err := Load(path, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hs := s.Hooks()
	if len(hs) != 2 {
		t.Fatalf("hooks = %+v, want 2", hs)
	}
	if hs[0].Event != PostTool || hs[1].Event != PreTool {
		// Events load in sorted order; within one event, file order holds.
		t.Errorf("events = %s, %s", hs[0].Event, hs[1].Event)
	}
	for _, h := range hs {
		if h.Event == PreTool && (h.TimeoutSeconds != 5 || len(h.Tools) != 1) {
			t.Errorf("pre_tool hook lost its settings: %+v", h)
		}
	}
}

func TestLoadRejectsTypos(t *testing.T) {
	path := writeHooks(t, `
[[hooks.pretool]]
run = "x"
`)
	if _, err := Load(path, t.TempDir()); err == nil {
		t.Fatal("an unknown event must be an error, not a silently dead gate")
	}

	path = writeHooks(t, `
[[hooks.pre_tool]]
tools = ["exec"]
`)
	if _, err := Load(path, t.TempDir()); err == nil {
		t.Fatal("a hook without a run command must be an error")
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "absent.toml"), t.TempDir())
	if err != nil || !s.Empty() {
		t.Fatalf("missing file: set=%+v err=%v, want empty and nil", s, err)
	}
}

func TestMergeKeepsOrderAndSkipsNil(t *testing.T) {
	a := &Set{hooks: []Hook{{Event: PreTool, Run: "first"}}}
	b := &Set{hooks: []Hook{{Event: PreTool, Run: "second"}}}
	m := Merge("/ws", a, nil, b)
	hs := m.Hooks()
	if len(hs) != 2 || hs[0].Run != "first" || hs[1].Run != "second" {
		t.Errorf("merged = %+v", hs)
	}
}

func TestNilAndEmptySetsAreInert(t *testing.T) {
	var s *Set
	if msg, blocked := s.PreTool(nil, reqFor("exec")); blocked || msg != "" {
		t.Error("a nil set must not block")
	}
	if note := s.PostTool(nil, reqFor("exec"), "out", false); note != "" {
		t.Error("a nil set must not annotate")
	}
}
