package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/cj-vana/switchboard/internal/execution"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/provider"
)

// SystemPrompt builds the frozen-zone system blocks.
//
// It is short deliberately. The prompt sits at the head of every request for
// the life of the session, so each paragraph is paid for on every cold cache,
// and a small local model follows three clear rules better than fifteen.
//
// Nothing here varies within a session. Mode and budget change during a run and
// belong in the volatile tail, never in this text, because rewriting the frozen
// zone invalidates the cached prefix from that point on (§6.1).
func SystemPrompt(workspace string, mode permission.Mode, capability execution.Capability) []provider.Block {
	var b strings.Builder

	b.WriteString("You are Switchboard, a coding agent working in a terminal.\n\n")

	fmt.Fprintf(&b, "Workspace: %s\nPlatform: %s\n\n", workspace, runtime.GOOS)

	b.WriteString(`Working rules:

- Read a file before changing it. Both write and edit refuse to touch a file you have not read this session, and refuse again if it changed since you read it.
- Prefer edit over write. edit replaces an exact string, so include enough surrounding text to make the match unique.
- Paths are relative to the workspace root. Nothing outside it is reachable.
- Find files with glob and search contents with grep before reaching for exec. Both stay inside the workspace and cost no approval.
- exec runs a command directly with no shell, so pipes, globs, redirection, and variables are not interpreted. Set shell only when you need those, and then pass the whole script as one element.
- Use the tools to find things out rather than guessing. When a tool returns an error, read it: it usually says exactly what to do next.
- Say what you did and what you found. Do not describe a change you have not made.
`)

	if mode == permission.ModePlan {
		b.WriteString("\nThis session is in plan mode. Writes and commands are refused. " +
			"Investigate and propose; do not attempt changes.\n")
	}
	if !capability.AutomaticExecutionAllowed() {
		b.WriteString("\nThere is no verified sandbox on this host, so the user approves each " +
			"command individually. Keep commands few and specific; a long speculative " +
			"sequence is a long sequence of interruptions.\n")
	}

	blocks := []provider.Block{provider.Text{Text: b.String()}}
	if inst, ok := ProjectInstructions(workspace); ok {
		blocks = append(blocks, provider.Text{Text: inst})
	}
	return blocks
}

// maxInstructionBytes caps what a project file can add to the frozen zone.
// The prompt is paid for on every cold cache, and a repository that writes a
// novel into AGENTS.md should not silently triple every request.
const maxInstructionBytes = 16 << 10

// instructionFiles are consulted in order and the first hit wins. AGENTS.md
// is the convention this project itself follows; CLAUDE.md is honored because
// repositories that have one mean it as agent instructions, whoever the agent.
var instructionFiles = []string{"AGENTS.md", "CLAUDE.md"}

// ProjectInstructions reads the workspace's agent instructions, if any. They
// go in the frozen zone as their own block: stable for the session, cacheable,
// and separate from the tool rules so a truncation cannot eat those.
func ProjectInstructions(workspace string) (string, bool) {
	for _, name := range instructionFiles {
		data, err := os.ReadFile(filepath.Join(workspace, name))
		if err != nil || len(strings.TrimSpace(string(data))) == 0 {
			continue
		}
		text := string(data)
		truncated := false
		if len(text) > maxInstructionBytes {
			text = text[:maxInstructionBytes]
			truncated = true
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Project instructions from %s (maintained by the project, follow them):\n\n%s", name, text)
		if truncated {
			fmt.Fprintf(&b, "\n\n[%s truncated at %d bytes]", name, maxInstructionBytes)
		}
		return b.String(), true
	}
	return "", false
}
