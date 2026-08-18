package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/switchboard-code/switchboard/internal/scm"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

const tuiDiffMaxBytes = 1 << 20

// diffView is the /diff fullscreen. The diff is highlighted once when it
// loads — §14's "rendered once and cached" — and scrolling is a slice over
// the result.
type diffView struct {
	lines  []string
	offset int
}

type diffLoadedMsg struct {
	lines []string
	err   error
}

// openDiff diffs the workspace against HEAD. This is the harness running a
// read-only git command, not the agent: it does not pass through the
// permission engine.
func openDiff(workspace string, dark bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		repo, err := scm.Discover(ctx, workspace)
		if err != nil {
			return diffLoadedMsg{err: err}
		}
		paths, err := workspaceDiffPaths(workspace, repo.Root)
		if err != nil {
			return diffLoadedMsg{err: err}
		}
		result, err := repo.DiffHEAD(ctx, scm.DiffOptions{
			Paths:    paths,
			MaxBytes: tuiDiffMaxBytes,
		})
		if err != nil {
			return diffLoadedMsg{err: err}
		}
		text := terminaltext.Display(renderSCMDiff(result))
		return diffLoadedMsg{lines: highlightDiff(text, dark)}
	}
}

// workspaceDiffPaths keeps /diff scoped to the directory Switchboard opened,
// even when that directory is nested inside a larger Git worktree. The SCM
// layer forces literal pathspec semantics before this value reaches Git.
func workspaceDiffPaths(workspace, repoRoot string) ([]string, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace for diff: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(abs); resolveErr == nil {
		abs = resolved
	}
	rel, err := filepath.Rel(repoRoot, abs)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace within Git worktree: %w", err)
	}
	if rel == "." {
		return nil, nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("%w: workspace %s", scm.ErrOutsideRepo, workspace)
	}
	return []string{filepath.ToSlash(rel)}, nil
}

func renderSCMDiff(result scm.DiffResult) string {
	text := string(result.Text)
	if result.Truncated {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += "… diff truncated at 1 MiB; some changes are not shown …\n"
	}
	if strings.TrimSpace(text) != "" {
		return text
	}

	changed := make([]scm.PathState, 0, len(result.Files))
	for _, file := range result.Files {
		if !file.Ignored {
			changed = append(changed, file)
		}
	}
	if len(changed) == 0 {
		return "working tree clean\n"
	}

	var b strings.Builder
	b.WriteString("working tree has changes with no textual patch:\n")
	for _, file := range changed {
		fmt.Fprintf(&b, "  %s  %s\n", diffStateLabel(file), file.Path)
	}
	return b.String()
}

func diffStateLabel(file scm.PathState) string {
	switch {
	case file.Unmerged:
		return "unmerged"
	case file.Untracked:
		return "untracked"
	case file.Staged && file.Unstaged:
		return "staged+unstaged"
	case file.Staged:
		return "staged"
	case file.Unstaged:
		return "unstaged"
	default:
		return "changed"
	}
}

// highlightDiff syntax-highlights a unified diff. A highlighting failure falls
// back to plain lines: the diff matters more than its colors.
func highlightDiff(text string, dark bool) []string {
	plain := strings.Split(strings.TrimRight(text, "\n"), "\n")

	lexer := lexers.Get("diff")
	if lexer == nil {
		return plain
	}
	styleName := "github"
	if dark {
		styleName = "github-dark"
	}
	style := styles.Get(styleName)
	formatter := formatters.Get("terminal256")
	if style == nil || formatter == nil {
		return plain
	}
	it, err := lexer.Tokenise(nil, text)
	if err != nil {
		return plain
	}
	var buf bytes.Buffer
	if err := formatter.Format(&buf, style, it); err != nil {
		return plain
	}
	return strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
}

// key scrolls; it reports true when the view should close. The diff has no
// asynchronous key actions, so its command is always nil.
func (d *diffView) key(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		return true, nil
	case "up", "k":
		d.scroll(-1)
	case "down", "j":
		d.scroll(1)
	case "pgup", "ctrl+u":
		d.scroll(-20)
	case "pgdown", "ctrl+d":
		d.scroll(20)
	case "g":
		d.offset = 0
	case "G":
		d.offset = len(d.lines)
		d.scroll(0)
	}
	return false, nil
}

func (d *diffView) mouse(msg tea.MouseMsg) {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		d.scroll(-3)
	case tea.MouseButtonWheelDown:
		d.scroll(3)
	}
}

func (d *diffView) scroll(n int) {
	d.offset += n
	if d.offset < 0 {
		d.offset = 0
	}
}

func (d *diffView) view(width, height int, th *theme) string {
	header := th.bold.Render(" git diff HEAD") +
		th.faint.Render("  ↑↓ scroll · pgup/pgdn page · g/G ends · esc close")

	bodyH := height - 2
	if bodyH < 1 {
		bodyH = 1
	}
	if max := len(d.lines) - bodyH; d.offset > max {
		d.offset = max
	}
	if d.offset < 0 {
		d.offset = 0
	}

	end := d.offset + bodyH
	if end > len(d.lines) {
		end = len(d.lines)
	}
	visible := append([]string(nil), d.lines[d.offset:end]...)
	for len(visible) < bodyH {
		visible = append(visible, "")
	}

	footer := ""
	if len(d.lines) > bodyH {
		pct := (d.offset + bodyH) * 100 / len(d.lines)
		if pct > 100 {
			pct = 100
		}
		footer = th.faint.Render(" " + itoa(pct) + "%")
	}
	return header + "\n" + lipgloss.JoinVertical(lipgloss.Left, visible...) + "\n" + footer
}
