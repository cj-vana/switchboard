package main

// The ! prefix runs a shell command as the user, immediately, with no model
// in the loop. It exists because "let me just check something" should not
// cost a turn, a permission dialog, or a context switch to another terminal.
// The output lands in the transcript right away and is carried into the next
// turn's prompt, so the model sees what the user saw — one user message per
// turn, which keeps every adapter's view of the conversation well-formed.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	shellTimeout   = 60 * time.Second
	shellOutputCap = 16 << 10
)

type shellDoneMsg struct {
	command string
	output  string
	err     error
	took    time.Duration
}

// runShell executes the command through the user's shell in the workspace.
// This is deliberately not the sandboxed tool path: the user typed it, which
// is the same authority as typing it into the terminal next door. The agent's
// own commands still go through permissions; this never becomes its escape
// hatch because the loop cannot reach it.
func (m *tuiModel) runShell(command string) tea.Cmd {
	if command == "" {
		return noticeCmd("error", "nothing to run after !")
	}
	if m.busy {
		return noticeCmd("warn", "a turn is running; ! commands wait for the prompt")
	}

	// rank -1: the user ran this, not a rung, so the rail stays neutral.
	m.tr.add(&entry{kind: kindTool, tool: toolEntry{name: "shell", desc: command}, rank: -1})
	m.tr.scrollToBottom()

	workspace := m.app.workspace
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), shellTimeout)
		defer cancel()

		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		cmd := exec.CommandContext(ctx, shell, "-c", command)
		cmd.Dir = workspace

		start := time.Now()
		out, err := cmd.CombinedOutput()
		took := time.Since(start)

		text := string(out)
		if len(text) > shellOutputCap {
			text = text[:shellOutputCap] + fmt.Sprintf("\n[truncated at %d bytes]", shellOutputCap)
		}
		if ctx.Err() != nil {
			text += fmt.Sprintf("\n[killed after %s]", shellTimeout)
		}
		return shellDoneMsg{command: command, output: text, err: err, took: took}
	}
}

func (m *tuiModel) onShellDone(msg shellDoneMsg) {
	if last := m.tr.last(); last != nil && last.kind == kindTool && !last.tool.done {
		last.tool.done = true
		last.tool.failed = msg.err != nil
		last.tool.took = msg.took
		last.tool.detail = msg.output
		m.tr.invalidate(len(m.tr.entries) - 1)
		m.tr.scrollToBottom()
	}

	var b strings.Builder
	fmt.Fprintf(&b, "$ %s\n%s", msg.command, strings.TrimRight(msg.output, "\n"))
	if msg.err != nil {
		fmt.Fprintf(&b, "\n[%v]", msg.err)
	}
	m.pendingShell = append(m.pendingShell, b.String())
}

// shellContext drains what ! commands produced into the next prompt. The
// session records the augmented prompt, so a replayed or resumed session
// carries the same context the model actually saw.
func (m *tuiModel) shellContext(prompt string) string {
	if len(m.pendingShell) == 0 {
		return prompt
	}
	var b strings.Builder
	b.WriteString("I ran these shell commands in the workspace just now:\n\n")
	for _, s := range m.pendingShell {
		b.WriteString(s + "\n\n")
	}
	b.WriteString(prompt)
	m.pendingShell = nil
	return b.String()
}
