package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/catalog"
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

// The bare form: surviving lines by target beside the target's own money
// word, the three meterings kept apart, and a target every line of which
// was overwritten still on the receipt at zero.
func TestBlameWorkspaceSumsSurvivorsAndKeepsMeteringsApart(t *testing.T) {
	cat, priced := pricedTarget(t)
	local := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3:4b"}
	if info, _, ok := cat.Lookup(local); !ok || info.Metering != catalog.Local {
		t.Fatal("the bundled catalog no longer meters ollama/local as local; pick another fixture")
	}

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	sess, err := store.Create(workspace, local.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}

	// Turn 1, local rung: writes two lines that survive.
	appendMessages(t, sess,
		provider.UserText("write the parser"),
		provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.ToolUse{ID: "w1", Name: "write", Input: json.RawMessage(`{"path":"kept.go","content":"one\ntwo\n"}`)},
		}},
	)
	if err := sess.AppendUsage(session.Usage{Target: string(local.ID())}); err != nil {
		t.Fatal(err)
	}
	appendMessages(t, sess, provider.Message{Role: provider.RoleTool, Content: []provider.Block{
		provider.ToolResult{ToolUseID: "w1", Name: "write", Content: "wrote kept.go"},
	}})

	// Turn 2, priced rung: writes a draft the next turn fully overwrites,
	// so it survives nowhere — and its dollars still show.
	appendMessages(t, sess,
		provider.UserText("draft the helper"),
		provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.ToolUse{ID: "w2", Name: "write", Input: json.RawMessage(`{"path":"churned.go","content":"draft\n"}`)},
		}},
	)
	if err := sess.AppendUsage(session.Usage{Target: string(priced.ID()), CostMicroUSD: 1_250_000}); err != nil {
		t.Fatal(err)
	}
	appendMessages(t, sess, provider.Message{Role: provider.RoleTool, Content: []provider.Block{
		provider.ToolResult{ToolUseID: "w2", Name: "write", Content: "wrote churned.go"},
	}})
	appendMessages(t, sess,
		provider.UserText("rewrite the helper"),
		provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.ToolUse{ID: "w3", Name: "write", Input: json.RawMessage(`{"path":"churned.go","content":"final\n"}`)},
		}},
	)
	if err := sess.AppendUsage(session.Usage{Target: string(local.ID())}); err != nil {
		t.Fatal(err)
	}
	appendMessages(t, sess, provider.Message{Role: provider.RoleTool, Content: []provider.Block{
		provider.ToolResult{ToolUseID: "w3", Name: "write", Content: "wrote churned.go"},
	}})
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(workspace, "kept.go"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "churned.go"), []byte("final\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := strings.Join(blameWorkspaceLines(store, cat, workspace), "\n")

	if !strings.Contains(out, "2 files the record touched") {
		t.Errorf("the file count is off:\n%s", out)
	}
	if !strings.Contains(out, "nothing to bill") {
		t.Errorf("the local rung lost its metering word:\n%s", out)
	}
	if !strings.Contains(out, "$1.25 as routed") {
		t.Errorf("the priced rung's dollars are missing:\n%s", out)
	}
	if !strings.Contains(out, "0 lines") {
		t.Errorf("a paid target with no surviving line must still show:\n%s", out)
	}
	if strings.Contains(out, "$0.00") {
		t.Errorf("something rendered as free money:\n%s", out)
	}
	if !strings.Contains(out, "surviving or not") {
		t.Errorf("the money scope is unstated:\n%s", out)
	}
}

// The drill-in: one line's story is its writer, the ask, the turn's other
// files, and how the turn signed off — with a line nobody wrote saying so.
func TestBlameLineTellsTheTurnsStory(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	target := provider.RouteTargetID("ollama/local/qwen3:4b")
	sess, err := store.Create(workspace, target, "test")
	if err != nil {
		t.Fatal(err)
	}
	appendMessages(t, sess,
		provider.UserText("wire the cache header"),
		provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.ToolUse{ID: "w1", Name: "write", Input: json.RawMessage(`{"path":"cache.go","content":"alpha\nbeta\n"}`)},
			provider.ToolUse{ID: "w2", Name: "write", Input: json.RawMessage(`{"path":"cache_test.go","content":"test\n"}`)},
		}},
	)
	if err := sess.AppendUsage(session.Usage{Target: string(target)}); err != nil {
		t.Fatal(err)
	}
	appendMessages(t, sess,
		provider.Message{Role: provider.RoleTool, Content: []provider.Block{
			provider.ToolResult{ToolUseID: "w1", Name: "write", Content: "wrote cache.go"},
			provider.ToolResult{ToolUseID: "w2", Name: "write", Content: "wrote cache_test.go"},
		}},
		provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.Text{Text: "The header rides every request now, with the test pinning it."},
		}},
	)
	if err := sess.AppendUsage(session.Usage{Target: string(target)}); err != nil {
		t.Fatal(err)
	}
	id := sess.State().ID
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	abs := filepath.Join(workspace, "cache.go")
	if err := os.WriteFile(abs, []byte("alpha\nbeta\nhand-typed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := strings.Join(blameLineLines(store, workspace, abs, "cache.go", 2), "\n")
	for _, want := range []string{
		"written by " + string(target),
		id + "#1",
		`"wire the cache header"`,
		"also touched: cache_test.go",
		"signed off",
		"pinning it",
		"/resume " + id,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the story is missing %q:\n%s", want, out)
		}
	}

	out = strings.Join(blameLineLines(store, workspace, abs, "cache.go", 3), "\n")
	if !strings.Contains(out, "outside the record") {
		t.Errorf("a hand-typed line must say nobody wrote it:\n%s", out)
	}

	out = strings.Join(blameLineLines(store, workspace, abs, "cache.go", 40), "\n")
	if !strings.Contains(out, "no line 40") {
		t.Errorf("a line past the end must be refused by count:\n%s", out)
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
