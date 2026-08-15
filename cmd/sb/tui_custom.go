package main

// Custom commands: a markdown file per command, the format the neighboring
// tools converged on, so what a user already wrote for one of them ports
// here by copying a file. .switchboard/commands/review.md becomes /review;
// the project directory wins over ~/.switchboard/commands on a name clash
// because the project speaks for itself.
//
// The body is a prompt template. $ARGUMENTS is everything after the command,
// $1..$9 are its fields, a backtick-quoted !`cmd` runs a shell command at
// expansion time and inlines its output, and @path attachments ride the same
// expansion every prompt gets.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type customCommand struct {
	name string
	desc string
	body string
}

// loadCustomCommands reads both directories once at startup. Project first:
// on a name clash the repository's version wins, because the project speaks
// for its own workflows.
func loadCustomCommands(workspace string) []customCommand {
	var out []customCommand
	seen := map[string]bool{}

	dirs := []string{filepath.Join(workspace, ".switchboard", "commands")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".switchboard", "commands"))
	}

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".md")
			if seen[name] {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			desc, body := splitFrontmatter(string(data))
			if strings.TrimSpace(body) == "" {
				continue
			}
			seen[name] = true
			out = append(out, customCommand{name: name, desc: desc, body: body})
		}
	}
	return out
}

// splitFrontmatter reads an optional YAML block for its description line.
// Anything else in the frontmatter is ignored rather than erroring, so a file
// written for another tool loads here without editing.
func splitFrontmatter(content string) (desc, body string) {
	body = content
	if !strings.HasPrefix(content, "---\n") {
		return "custom command", body
	}
	rest := content[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "custom command", body
	}
	front := rest[:end]
	body = strings.TrimPrefix(rest[end+4:], "\n")
	desc = "custom command"
	for _, line := range strings.Split(front, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "description:"); ok {
			desc = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return desc, body
}

var inlineShell = regexp.MustCompile("!`([^`]+)`")

// expandCustom renders a command body against its arguments. Inline shell
// runs now, at expansion, because a command that pastes today's diff into the
// prompt is the entire point of having one.
func expandCustom(body, args, workspace string) string {
	fields := strings.Fields(args)
	body = strings.ReplaceAll(body, "$ARGUMENTS", args)
	for i := 9; i >= 1; i-- {
		val := ""
		if i <= len(fields) {
			val = fields[i-1]
		}
		body = strings.ReplaceAll(body, fmt.Sprintf("$%d", i), val)
	}

	return inlineShell.ReplaceAllStringFunc(body, func(match string) string {
		command := inlineShell.FindStringSubmatch(match)[1]
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		cmd.Dir = workspace
		out, err := cmd.CombinedOutput()
		text := strings.TrimRight(string(out), "\n")
		if len(text) > 8<<10 {
			text = text[:8<<10] + "\n[truncated]"
		}
		if err != nil {
			text += "\n[" + err.Error() + "]"
		}
		return text
	})
}

func runCustom(m *tuiModel, c customCommand, args string) tea.Cmd {
	if m.busy {
		return noticeCmd("warn", "a turn is running; esc to interrupt it first")
	}
	return m.enqueue(expandCustom(c.body, args, m.app.workspace), "")
}
