package delegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeWorkflow(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadWorkflowsReadsAndValidates(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	writeWorkflow(t, filepath.Join(workspace, ".switchboard", "workflows"), "review.toml", `
description = "survey, then propose"

[[stage]]
name = "survey"
[[stage.task]]
task = "List every call site."
[[stage.task]]
task = "List every test."

[[stage]]
name = "propose"
carry = true
[[stage.task]]
tier = "t2"
task = "Propose the minimal edit."
`)

	got, notes := LoadWorkflows(workspace)
	if len(got) != 1 {
		t.Fatalf("workflows = %v, notes = %v", got, notes)
	}
	wf := got[0]
	if wf.Name != "review" || wf.Description != "survey, then propose" || len(wf.Stages) != 2 {
		t.Fatalf("loaded as %+v", wf)
	}
	if len(wf.Stages[0].Tasks) != 2 || wf.Stages[0].Carry {
		t.Fatalf("first stage = %+v", wf.Stages[0])
	}
	if !wf.Stages[1].Carry || wf.Stages[1].Tasks[0].Tier != "t2" {
		t.Fatalf("second stage = %+v", wf.Stages[1])
	}
}

// A definition that cannot execute fails when it is read, not halfway through
// a run that has already spent money.
func TestWorkflowCapsAndMistakesAreRefusedAtLoad(t *testing.T) {
	stage := "\n[[stage]]\n[[stage.task]]\ntask = \"x\"\n"
	for _, tc := range []struct{ name, body, want string }{
		{"nostages", `description = "x"`, "no stages"},
		{"toomanystages", strings.Repeat(stage, MaxWorkflowStages+1), "more than the"},
		{"emptytask", "\n[[stage]]\n[[stage.task]]\ntask = \"  \"\n", "no task text"},
		{"carryfirst", "\n[[stage]]\ncarry = true\n[[stage.task]]\ntask = \"x\"\n", "nothing ran before it"},
		{"typo", "\n[[stage]]\nnaem = \"oops\"\n[[stage.task]]\ntask = \"x\"\n", "unrecognized"},
	} {
		workspace := t.TempDir()
		t.Setenv("HOME", t.TempDir())
		writeWorkflow(t, filepath.Join(workspace, ".switchboard", "workflows"), tc.name+".toml", tc.body)
		got, notes := LoadWorkflows(workspace)
		if len(got) != 0 {
			t.Errorf("%s loaded despite being invalid: %+v", tc.name, got)
			continue
		}
		if len(notes) != 1 || !strings.Contains(notes[0], tc.want) {
			t.Errorf("%s notes = %v, want one saying %q", tc.name, notes, tc.want)
		}
	}
}

// A stage that fans out and carries everything would hand the next stage four
// transcripts to re-read on every one of its own calls.
func TestCarryTruncatesEachAnswer(t *testing.T) {
	long := strings.Repeat("x", MaxCarriedAnswerRune*2)
	got := Carry([]string{long, "short"}, "do the thing")
	if !strings.Contains(got, "[truncated]") {
		t.Error("a long carried answer was not truncated")
	}
	if !strings.Contains(got, "short") || !strings.Contains(got, "do the thing") {
		t.Errorf("carry lost content:\n%s", got)
	}
	if got := Carry(nil, "do the thing"); got != "do the thing" {
		t.Errorf("carrying nothing should leave the task alone, got %q", got)
	}
}
