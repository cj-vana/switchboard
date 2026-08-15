package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCustomCommandsLoadAndProjectWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := filepath.Join(home, "project")

	global := filepath.Join(home, ".switchboard", "commands")
	local := filepath.Join(ws, ".switchboard", "commands")
	for _, dir := range []string{global, local} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(global, "review.md"), []byte("---\ndescription: the global one\n---\nglobal body"), 0o644)
	os.WriteFile(filepath.Join(local, "review.md"), []byte("---\ndescription: the project one\n---\nproject body"), 0o644)
	os.WriteFile(filepath.Join(global, "standup.md"), []byte("what changed since yesterday?"), 0o644)

	cmds := loadCustomCommands(ws)
	if len(cmds) != 2 {
		t.Fatalf("loaded %d commands, want 2: %+v", len(cmds), cmds)
	}
	byName := map[string]customCommand{}
	for _, c := range cmds {
		byName[c.name] = c
	}
	if byName["review"].body != "project body" {
		t.Fatalf("on a name clash the project must win, got %q", byName["review"].body)
	}
	if byName["standup"].desc != "custom command" {
		t.Fatalf("a file without frontmatter still loads, desc %q", byName["standup"].desc)
	}
}

func TestExpandCustomSubstitutesAndRunsInlineShell(t *testing.T) {
	body := "Review $1 with focus on $ARGUMENTS.\n\nBranch: !`printf fake-branch`"
	got := expandCustom(body, "cmd/sb correctness", t.TempDir())

	for _, want := range []string{
		"Review cmd/sb with focus on cmd/sb correctness.",
		"Branch: fake-branch",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expansion missing %q:\n%s", want, got)
		}
	}
}
