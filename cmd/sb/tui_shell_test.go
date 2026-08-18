package main

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func runShellForTest(t *testing.T, command string) (*tuiModel, shellDoneMsg) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the TUI shell runner uses the user's platform shell")
	}
	m := testModel(t)
	m.app.workspace = t.TempDir()
	t.Setenv("SHELL", "/bin/sh")
	cmd := m.runShell(command)
	if cmd == nil || !m.busy || !m.operationActive {
		t.Fatalf("shell did not claim asynchronous operation ownership: cmd=%v busy=%v operation=%v", cmd != nil, m.busy, m.operationActive)
	}
	msg, ok := cmd().(shellDoneMsg)
	if !ok {
		t.Fatalf("shell command returned an unexpected message")
	}
	return m, msg
}

func shellEntry(t *testing.T, m *tuiModel) *entry {
	t.Helper()
	for i := len(m.tr.entries) - 1; i >= 0; i-- {
		if m.tr.entries[i].kind == kindTool && m.tr.entries[i].tool.name == "shell" {
			return m.tr.entries[i]
		}
	}
	t.Fatal("no shell transcript entry")
	return nil
}

func TestShellSuccessShowsExitZeroAndKeepsOutputExpandable(t *testing.T) {
	m, msg := runShellForTest(t, `printf "$PWD"`)
	if msg.err != nil {
		t.Fatalf("shell command failed: %v", msg.err)
	}
	m.onShellDone(msg)
	if m.busy || m.operationActive {
		t.Fatal("completed shell command did not release operation ownership")
	}

	e := shellEntry(t, m)
	if e.tool.failed || !strings.Contains(e.tool.detail, "exit 0 (success)") {
		t.Fatalf("successful command has no explicit verdict: %+v", e.tool)
	}
	collapsed := stripANSI(strings.Join(m.tr.flat, "\n"))
	if !strings.Contains(collapsed, "exit 0 (success)") {
		t.Fatalf("success is not visible while collapsed:\n%s", collapsed)
	}
	if strings.Contains(collapsed, m.app.workspace) {
		t.Fatalf("stdout should stay behind expansion on success:\n%s", collapsed)
	}
	e.expanded = true
	e.cache = nil
	m.tr.invalidate(m.tr.indexOf(e))
	expanded := stripANSI(strings.Join(m.tr.flat, "\n"))
	if !strings.Contains(expanded, m.app.workspace) {
		t.Fatalf("expansion did not reveal stdout:\n%s", expanded)
	}

	contextPrompt := m.shellContext("continue")
	if !strings.Contains(contextPrompt, "[shell result: success; exit_code=0]") {
		t.Fatalf("next-prompt context lost the structured outcome:\n%s", contextPrompt)
	}
}

func TestShellFailureShowsExactExitCodeBesideOutput(t *testing.T) {
	m, msg := runShellForTest(t, `printf "problem on stderr\n" >&2; exit 7`)
	m.onShellDone(msg)

	e := shellEntry(t, m)
	if !e.tool.failed {
		t.Fatal("nonzero command rendered as successful")
	}
	visible := stripANSI(strings.Join(m.tr.flat, "\n"))
	for _, want := range []string{"problem on stderr", "exit 7 (failure)"} {
		if !strings.Contains(visible, want) {
			t.Fatalf("collapsed failure is missing %q:\n%s", want, visible)
		}
	}
	contextPrompt := m.shellContext("diagnose it")
	if !strings.Contains(contextPrompt, "[shell result: failure; exit_code=7]") {
		t.Fatalf("next-prompt context lost the exit code:\n%s", contextPrompt)
	}
}

func TestShellSignalNamesTheCause(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal behavior")
	}
	m, msg := runShellForTest(t, `kill -TERM $$`)
	m.onShellDone(msg)
	visible := stripANSI(strings.Join(m.tr.flat, "\n"))
	if !strings.Contains(visible, "terminated by signal") || !strings.Contains(visible, "terminated") {
		t.Fatalf("signal termination was not named:\n%s", visible)
	}
}

func TestShellTimeoutAndCancellationNameTheirCause(t *testing.T) {
	timedOut := classifyShellResult(errors.New("process killed"), context.DeadlineExceeded)
	wantTimeout := "timed out after " + shellTimeout.String()
	wantCancelled := "cancelled by user"
	if runtime.GOOS == "windows" {
		limit := "; only the direct shell was stopped; descendant processes may still be running"
		wantTimeout += limit
		wantCancelled += limit
	}
	if got := timedOut.summary(); got != wantTimeout {
		t.Fatalf("timeout summary = %q", got)
	}
	cancelled := classifyShellResult(errors.New("process killed"), context.Canceled)
	if got := cancelled.summary(); got != wantCancelled {
		t.Fatalf("cancellation summary = %q", got)
	}
	if record := cancelled.contextRecord(); (runtime.GOOS == "windows") != strings.Contains(record, "descendants_may_survive=true") {
		t.Fatalf("cancellation context did not match platform cleanup semantics: %q", record)
	}

	if runtime.GOOS == "windows" {
		return
	}
	m := testModel(t)
	m.app.workspace = t.TempDir()
	t.Setenv("SHELL", "/bin/sh")
	cmd := m.runShell(`printf "must not run"`)
	m.operationCancelling = true
	m.turnCancel()
	msg := cmd().(shellDoneMsg)
	m.onShellDone(msg)
	visible := stripANSI(strings.Join(m.tr.flat, "\n"))
	if !strings.Contains(visible, "cancelled by user") || m.busy || m.operationActive {
		t.Fatalf("cancelled operation was not visibly and completely settled: busy=%v operation=%v\n%s", m.busy, m.operationActive, visible)
	}
}

func TestShellOutputControlsStayInertWhenExpanded(t *testing.T) {
	m := testModel(t)
	m.tr.add(&entry{kind: kindTool, tool: toolEntry{name: "shell", desc: "unsafe output"}, rank: -1})
	m.onShellDone(shellDoneMsg{
		command: "unsafe output",
		output:  "ok\x1b[2J\x1b]52;c;Y2xpcGJvYXJk\x07\rSPOOF\nnext\tcolumn",
		result:  shellResult{kind: shellSucceeded},
		took:    time.Millisecond,
	})
	e := shellEntry(t, m)
	e.expanded = true
	e.cache = nil
	m.tr.invalidate(m.tr.indexOf(e))
	rendered := strings.Join(m.tr.flat, "\n")
	if strings.ContainsAny(rendered, "\x07\r") || strings.Contains(rendered, "\x1b[2J") || strings.Contains(rendered, "\x1b]52;") {
		t.Fatalf("shell output wrote terminal controls: %q", rendered)
	}
	plain := stripANSI(rendered)
	if !strings.Contains(plain, `\x1b`) || !strings.Contains(plain, `\x0d`) || !strings.Contains(plain, "next") {
		t.Fatalf("safe rendering lost visible escapes or output: %q", plain)
	}
}

func TestStaleShellCompletionCannotMutateTheCurrentSession(t *testing.T) {
	m, msg := runShellForTest(t, `exit 9`)
	e := shellEntry(t, m)
	m.operationSourceID = "a-new-session-owns-the-prompt"
	m.onShellDone(msg)
	if e.tool.done || len(m.pendingShell) != 0 {
		t.Fatalf("stale completion mutated live state: done=%v pending=%d", e.tool.done, len(m.pendingShell))
	}

	// Restore the synthetic guard change so test cleanup does not leave an
	// owned context behind.
	m.operationSourceID = msg.sourceID
	m.finishOperation(msg.operation, false)
}

func TestShellCompletionInvalidatesLiteralAndSemanticWorkspaceSnapshots(t *testing.T) {
	for _, test := range []struct {
		name   string
		result shellResult
	}{
		{name: "success", result: shellResult{kind: shellSucceeded}},
		// A failed shell may already have changed files before its non-zero exit.
		{name: "failure after partial mutation", result: shellResult{kind: shellExited, exitCode: 9}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := testModel(t)
			m.workspaceRuntime = newWorkspaceRuntime(m.app.workspace)
			semantic := &lspView{}
			m.full = semantic
			m.tr.add(&entry{kind: kindTool, tool: toolEntry{name: "shell", desc: test.name}, rank: -1})

			before := m.workspaceRuntime.epoch.Load()
			m.onShellDone(shellDoneMsg{command: test.name, result: test.result})

			if after := m.workspaceRuntime.epoch.Load(); after != before+1 {
				t.Fatalf("workspace epoch = %d, want %d", after, before+1)
			}
			if !semantic.stale {
				t.Fatal("shell completion left the open semantic result looking current")
			}
		})
	}
}
