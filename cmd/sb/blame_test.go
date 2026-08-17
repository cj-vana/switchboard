package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
)

// The full path: a log shaped the way the loop writes one, a file on disk
// that has since grown a hand-typed line, and the report naming the rung,
// the model, the turn, and the line no recorded call explains.
func TestBlameAttributesLinesAndNamesWhatItCannot(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	light := provider.RouteTargetID("ollama/local/qwen3:4b")
	heavy := provider.RouteTargetID("kimi/api/k2")

	sess, err := store.Create(workspace, light, "test")
	if err != nil {
		t.Fatal(err)
	}
	appendMessages(t, sess,
		provider.UserText("write the parser"),
		provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.ToolUse{ID: "w1", Name: "write", Input: json.RawMessage(`{"path":"parser.go","content":"one\ntwo\n"}`)},
		}},
	)
	if err := sess.AppendUsage(session.Usage{Target: string(light)}); err != nil {
		t.Fatal(err)
	}
	appendMessages(t, sess, provider.Message{Role: provider.RoleTool, Content: []provider.Block{
		provider.ToolResult{ToolUseID: "w1", Name: "write", Content: "wrote parser.go"},
	}})
	if err := sess.AppendRoute(session.Route{TurnDepth: 0, Tier: "t1", Target: light}); err != nil {
		t.Fatal(err)
	}
	appendMessages(t, sess,
		provider.UserText("shout the second line"),
		provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.ToolUse{ID: "e1", Name: "edit", Input: json.RawMessage(`{"path":"parser.go","old_string":"two","new_string":"TWO"}`)},
		}},
	)
	if err := sess.AppendUsage(session.Usage{Target: string(heavy)}); err != nil {
		t.Fatal(err)
	}
	appendMessages(t, sess, provider.Message{Role: provider.RoleTool, Content: []provider.Block{
		provider.ToolResult{ToolUseID: "e1", Name: "edit", Content: "edited parser.go"},
	}})
	id := sess.State().ID
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	abs := filepath.Join(workspace, "parser.go")
	if err := os.WriteFile(abs, []byte("one\nTWO\nhand-typed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := strings.Join(blameLines(store, workspace, abs, "parser.go"), "\n")

	if !strings.Contains(out, "3 lines: 2 from recorded turns, 1 outside the record") {
		t.Errorf("the header does not add up:\n%s", out)
	}
	if !strings.Contains(out, "t1 "+string(light)) {
		t.Errorf("the write's rung and model are missing:\n%s", out)
	}
	if !strings.Contains(out, string(heavy)) {
		t.Errorf("the edit's model is missing:\n%s", out)
	}
	if !strings.Contains(out, id+"#1") || !strings.Contains(out, id+"#2") {
		t.Errorf("the turns are not named the way /resume could reach them:\n%s", out)
	}
	if !strings.Contains(out, `"write the parser"`) {
		t.Errorf("the turn's own words are missing:\n%s", out)
	}
	if !strings.Contains(out, "outside the record") {
		t.Errorf("the hand-typed line is not named as such:\n%s", out)
	}
}

func TestBlameWithNoRecordSaysWhatItCovers(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	abs := filepath.Join(workspace, "untouched.go")
	if err := os.WriteFile(abs, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := strings.Join(blameLines(store, workspace, abs, "untouched.go"), "\n")
	if !strings.Contains(out, "no recorded turn has written untouched.go") {
		t.Errorf("an empty record should say so:\n%s", out)
	}
	if !strings.Contains(out, "hands and shell commands are outside it") {
		t.Errorf("the boundary of the claim is unstated:\n%s", out)
	}
}

func appendMessages(t *testing.T, sess *session.Session, messages ...provider.Message) {
	t.Helper()
	for _, m := range messages {
		if err := sess.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
	}
}
