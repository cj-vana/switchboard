package main

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func TestPermissionRememberHintMatchesRememberScope(t *testing.T) {
	execHint := permissionRememberHint(permission.Request{Tool: "exec", Effect: permission.EffectExecute})
	if !strings.Contains(execHint, "exact command") {
		t.Fatalf("exec remember hint = %q", execHint)
	}
	mcpHint := permissionRememberHint(permission.Request{Tool: "mcp__github__delete_issue", Effect: permission.EffectExternal})
	if strings.Contains(mcpHint, "exact") || !strings.Contains(mcpHint, "this tool") {
		t.Fatalf("MCP remember hint misstates per-tool scope: %q", mcpHint)
	}
	hostHint := permissionRememberHint(permission.Request{Tool: "webfetch", Effect: permission.EffectExternal, Path: "example.com"})
	if !strings.Contains(hostHint, "example.com") || strings.Contains(hostHint, "exact") {
		t.Fatalf("host-scoped remember hint = %q", hostHint)
	}
}

func TestDescribeRequestEscapesExternalControlSequences(t *testing.T) {
	got := describeRequest(permission.Request{Effect: permission.EffectExternal, Detail: "\x1b[2JAPPROVED\x07"})
	if strings.ContainsAny(got, "\x1b\x07") || !strings.Contains(got, `\x1b`) {
		t.Fatalf("unsafe external request description %q", got)
	}
}

func TestREPLToolResultCannotWriteTerminalControls(t *testing.T) {
	var buf bytes.Buffer
	r := &renderer{w: bufio.NewWriter(&buf), atLineTop: true}
	r.ToolEnd(provider.ToolUse{Name: "exec"}, permission.Request{}, tools.Result{
		Content: "ok\x1b[2J\x1b]52;c;Y2xpcGJvYXJk\x07\rSPOOF",
	}, time.Millisecond)
	got := buf.String()
	if strings.ContainsAny(got, "\x1b\x07\r") || !strings.Contains(got, `\x1b`) {
		t.Fatalf("REPL rendered unsafe tool output: %q", got)
	}
}

func TestREPLModelAndNoticeTextCannotWriteTerminalControls(t *testing.T) {
	var buf bytes.Buffer
	r := &renderer{w: bufio.NewWriter(&buf), atLineTop: true}
	r.TextDelta("answer\x1b[2J")
	r.ThinkingDelta("thought\x1b]52;c;Y2xpcGJvYXJk\x07")
	r.Notice("error", "provider\rSPOOF")
	got := buf.String()
	if strings.ContainsAny(got, "\x1b\x07\r") || !strings.Contains(got, `\x1b`) {
		t.Fatalf("REPL rendered unsafe model/notice text: %q", got)
	}
}

func TestApprovalViewsMakeFullNetworkRequestExplicit(t *testing.T) {
	req := permission.Request{Tool: "exec", Effect: permission.EffectExecute, Argv: []string{"go", "test", "./..."}, Network: true}
	outcome := permission.Outcome{Decision: permission.Ask, Reason: "runs a command"}

	var buf bytes.Buffer
	r := &renderer{w: bufio.NewWriter(&buf), atLineTop: true}
	asker := terminalAsker{in: bufio.NewReader(strings.NewReader("n\n")), out: r}
	if _, err := asker.Ask(context.Background(), req, outcome); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, "FULL NETWORK ACCESS REQUESTED") {
		t.Fatalf("REPL network prompt hid egress reach: %q", got)
	}

	m := testModel(t)
	dialog := newPermissionDialog(req, outcome, make(chan permission.Response, 1))
	if got := stripANSI(dialog.view(80, m.th)); !strings.Contains(got, "FULL NETWORK ACCESS REQUESTED") {
		t.Fatalf("TUI network prompt hid egress reach: %q", got)
	}
}

func TestTUIApprovalBoundsHugeArgvWithoutHidingExecutableOrChoices(t *testing.T) {
	req := permission.Request{
		Tool: "exec", Effect: permission.EffectExecute,
		Argv: []string{"dangerous-command", strings.Repeat("padding ", 600), "--target", "/outside/workspace"},
	}
	outcome := permission.Outcome{Decision: permission.Ask, Reason: strings.Repeat("review detail ", 100), SandboxAbsent: true}
	m := testModel(t)
	dialog := newPermissionDialog(req, outcome, make(chan permission.Response, 1))
	plain := stripANSI(dialog.view(80, m.th))
	if lines := strings.Count(plain, "\n") + 1; lines > 24 {
		t.Fatalf("approval modal is %d lines and can scroll its identity off a 24-line terminal:\n%s", lines, plain)
	}
	for _, visible := range []string{"dangerous-command", "chars omitted", "/outside/workspace", "FULL HOST ACCESS", "yes", "no"} {
		if !strings.Contains(plain, visible) {
			t.Fatalf("bounded modal hid %q:\n%s", visible, plain)
		}
	}
}
