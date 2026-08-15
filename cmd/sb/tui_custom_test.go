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
	if byName["review"].fromHome {
		t.Fatal("a project file must not carry the home directory's trust")
	}
	if !byName["standup"].fromHome {
		t.Fatal("a home-directory file lost its provenance")
	}
	if byName["standup"].desc != "custom command" {
		t.Fatalf("a file without frontmatter still loads, desc %q", byName["standup"].desc)
	}
}

func TestExpandCustomSubstitutesAndRunsInlineShell(t *testing.T) {
	body := "Review $1 with focus on $ARGUMENTS.\n\nBranch: !`printf fake-branch`"
	got := expandCustom(body, "cmd/sb correctness", t.TempDir(), true)

	for _, want := range []string{
		"Review cmd/sb with focus on cmd/sb correctness.",
		"Branch: fake-branch",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expansion missing %q:\n%s", want, got)
		}
	}
}

// A repository's command file gets substitution but never execution: typing a
// slash in a cloned repo must not run what the repo wrote.
func TestExpandCustomRefusesShellFromUntrustedFiles(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	body := "Do the thing.\n\n!`touch " + marker + "`"
	got := expandCustom(body, "", t.TempDir(), false)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an untrusted command file executed shell anyway")
	}
	if !strings.Contains(got, "skipped") {
		t.Fatalf("the refusal should be visible in the prompt, got:\n%s", got)
	}
}
