package delegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var suite = []string{"edit", "exec", "glob", "grep", "read", "todo", "write"}

func writeAgent(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAgentsParsesTheFourFields(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	writeAgent(t, filepath.Join(workspace, ".switchboard", "agents"), "reviewer.md",
		"---\ndescription: reviews a diff\ntier: t2\ntools: read, grep, glob\n---\n\nYou review changes.\n")

	agents, notes := LoadAgents(workspace, suite)
	if len(notes) != 0 {
		t.Fatalf("notes = %v, want none", notes)
	}
	if len(agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(agents))
	}
	ag := agents[0]
	if ag.Name != "reviewer" {
		t.Errorf("Name = %q, want the filename", ag.Name)
	}
	if ag.Description != "reviews a diff" || ag.Tier != "t2" {
		t.Errorf("Description/Tier = %q/%q", ag.Description, ag.Tier)
	}
	if strings.Join(ag.Tools, ",") != "read,grep,glob" {
		t.Errorf("Tools = %v", ag.Tools)
	}
	if ag.Prompt != "You review changes." {
		t.Errorf("Prompt = %q", ag.Prompt)
	}
	if ag.FromHome {
		t.Error("a project file must not claim home provenance")
	}
}

func TestLoadAgentsProjectWinsAndOutputIsSorted(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeAgent(t, filepath.Join(workspace, ".switchboard", "agents"), "zeta.md", "project zeta\n")
	writeAgent(t, filepath.Join(home, ".switchboard", "agents"), "zeta.md", "home zeta\n")
	writeAgent(t, filepath.Join(home, ".switchboard", "agents"), "alpha.md", "home alpha\n")

	agents, _ := LoadAgents(workspace, suite)
	if len(agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(agents))
	}
	if agents[0].Name != "alpha" || agents[1].Name != "zeta" {
		t.Errorf("order = %s, %s: the schema needs a stable order", agents[0].Name, agents[1].Name)
	}
	if agents[1].Prompt != "project zeta" {
		t.Errorf("Prompt = %q, want the project's version to win the clash", agents[1].Prompt)
	}
	if !agents[0].FromHome || agents[1].FromHome {
		t.Error("provenance must record which directory spoke")
	}
}

func TestLoadAgentsRejectsAGrantOutsideTheSuite(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	writeAgent(t, filepath.Join(workspace, ".switchboard", "agents"), "bad.md",
		"---\ntools: read, telepathy\n---\nbody\n")

	agents, notes := LoadAgents(workspace, suite)
	if len(agents) != 0 {
		t.Fatalf("agents = %v, want the bad grant skipped", agents)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], `"telepathy"`) {
		t.Errorf("notes = %v, want the unknown tool named", notes)
	}
}

// A definition written for the neighboring tool describes the same thing: a
// prompt and two defaults. It is read from .claude/agents the way a native
// skill is, and the one capability both suites have under different names is
// translated rather than refused.
func TestLoadAgentsReadsNativeDefinitions(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	writeAgent(t, filepath.Join(workspace, ".claude", "agents"), "reviewer.md",
		"---\nname: reviewer\ndescription: reviews a diff\ntools: Read, Grep, Bash\nmodel: opus\n---\nReview the diff.\n")

	agents, notes := LoadAgents(workspace, suite)
	if len(agents) != 1 {
		t.Fatalf("agents = %v, notes = %v; want the native definition loaded", agents, notes)
	}
	got := agents[0]
	if got.Name != "reviewer" || got.Description != "reviews a diff" || got.Prompt != "Review the diff." {
		t.Fatalf("native definition loaded as %+v", got)
	}
	// Bash and exec are the same capability under two names, so the grant is
	// applied rather than the agent refused. An unmappable name would still
	// fail, because a tools list is a restriction.
	want := []string{"read", "grep", "exec"}
	if len(got.Tools) != len(want) {
		t.Fatalf("tools = %v, want %v", got.Tools, want)
	}
	for i, name := range want {
		if got.Tools[i] != name {
			t.Fatalf("tools = %v, want %v", got.Tools, want)
		}
	}
	// "model" is not a field here; it is ignored rather than refused, the way
	// every other foreign key is.
	if len(notes) != 0 {
		t.Errorf("a native definition should load without complaint: %v", notes)
	}
}

// Switchboard's own directory speaks first at the same scope.
func TestSwitchboardAgentWinsOverANativeOneOfTheSameName(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	writeAgent(t, filepath.Join(workspace, ".switchboard", "agents"), "scout.md",
		"---\nname: scout\n---\nmine\n")
	writeAgent(t, filepath.Join(workspace, ".claude", "agents"), "scout.md",
		"---\nname: scout\n---\ntheirs\n")

	agents, _ := LoadAgents(workspace, suite)
	if len(agents) != 1 || agents[0].Prompt != "mine" {
		t.Fatalf("agents = %v, want this project's definition to win", agents)
	}
}

func TestLoadAgentsRequiresABody(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	writeAgent(t, filepath.Join(workspace, ".switchboard", "agents"), "empty.md",
		"---\ndescription: nothing follows\n---\n\n")

	agents, notes := LoadAgents(workspace, suite)
	if len(agents) != 0 || len(notes) != 1 {
		t.Fatalf("agents = %v, notes = %v: a bodyless agent has no instructions", agents, notes)
	}
}

func TestParseAgentAcceptsBothListShapesAndFrontmatterName(t *testing.T) {
	valid := map[string]bool{"read": true, "grep": true}
	ag, err := parseAgent("file.md", "---\nname: scout\ntools: [Read, GREP]\n---\nbody\n", valid)
	if err != nil {
		t.Fatal(err)
	}
	if ag.Name != "scout" {
		t.Errorf("Name = %q, want the frontmatter to override the filename", ag.Name)
	}
	if strings.Join(ag.Tools, ",") != "read,grep" {
		t.Errorf("Tools = %v, want bracketed, cased input normalized", ag.Tools)
	}
}
