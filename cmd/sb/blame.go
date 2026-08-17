package main

// /blame and `sb blame`: which recorded turn wrote each line of a file, on
// which rung and model — and which lines no recorded turn wrote. git blame
// answers "who committed this"; in an agent session the missing half is
// "which model, asked what", and the session logs already hold it: every
// write's bytes and every edit's replacement, beside the usage record that
// names the target and the route record that names the rung. Replay is
// internal/blame's; this file is the two surfaces sharing one body, the
// cost/stats/find pattern. Read-only over the logs, the open one included.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/blame"
	"github.com/cj-vana/switchboard/internal/session"
)

const blameMaxRuns = 12

// blameLines annotates one file from every session the workspace has
// recorded. abs is the file already resolved; shown is how the user named
// it, for the report's own words.
func blameLines(store *session.Store, workspace, abs, shown string) []string {
	disk, err := os.ReadFile(abs)
	if err != nil {
		return []string{"  cannot read " + shown + ": " + err.Error()}
	}

	infos, err := store.List(workspace)
	if err != nil {
		return []string{"  " + err.Error()}
	}
	var edits []session.FileEdit
	for _, info := range infos {
		fromLog, err := session.ReadFileEdits(info.Path)
		if err != nil {
			continue
		}
		for _, e := range fromLog {
			if resolveEditPath(e) == abs {
				edits = append(edits, e)
			}
		}
	}
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].At.Before(edits[j].At) })

	if len(edits) == 0 {
		return []string{
			"  no recorded turn has written " + shown,
			"  blame sees what write and edit put in the log; hands and shell commands are outside it",
		}
	}

	ann := blame.Annotate(disk, edits)
	total := len(ann.Lines)
	if total == 0 {
		return []string{"  " + shown + " is empty"}
	}

	counts := make([]int, len(ann.Origins))
	outside := 0
	for _, o := range ann.Lines {
		if o < 0 {
			outside++
		} else {
			counts[o]++
		}
	}
	order := make([]int, len(ann.Origins))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		if counts[order[i]] != counts[order[j]] {
			return counts[order[i]] > counts[order[j]]
		}
		return ann.Origins[order[i]].At.Before(ann.Origins[order[j]].At)
	})

	recorded := total - outside
	lines := []string{fmt.Sprintf("  %d lines: %d from recorded turns, %d outside the record", total, recorded, outside)}

	for rank, origin := range order {
		if rank >= 26 {
			lines = append(lines, fmt.Sprintf("  … and %d more origins with fewer lines", len(order)-rank))
			break
		}
		o := ann.Origins[origin]
		who := o.Target
		if who == "" {
			who = "(target unrecorded)"
		}
		if o.Tier != "" {
			who = o.Tier + " " + who
		}
		turn := fmt.Sprintf("%s#%d", o.SessionID, o.Turn)
		prompt := ""
		if o.Prompt != "" {
			prompt = fmt.Sprintf("  %q", truncate(o.Prompt, 44))
		}
		word := "lines"
		if counts[origin] == 1 {
			word = "line"
		}
		lines = append(lines,
			fmt.Sprintf("  %c  %d %s  %s  %s%s", 'a'+rank, counts[origin], word, who, turn, prompt),
			"       "+lineRuns(ann.Lines, origin))
	}
	if outside > 0 {
		word := "lines"
		if outside == 1 {
			word = "line"
		}
		lines = append(lines,
			fmt.Sprintf("  ·  %d %s outside the record — typed, shell-made, or before the log", outside, word),
			"       "+lineRuns(ann.Lines, -1))
	}
	if ann.Unplaced > 0 {
		word := "edits"
		if ann.Unplaced == 1 {
			word = "edit"
		}
		lines = append(lines, fmt.Sprintf("  %d recorded %s could not be replayed against what the file became; those lines read as outside the record", ann.Unplaced, word))
	}
	return lines
}

// resolveEditPath maps a recorded call's path to the file it named:
// workspace-relative paths resolve against the workspace the log's own
// header recorded, not against whoever is asking today.
func resolveEditPath(e session.FileEdit) string {
	if filepath.IsAbs(e.Path) {
		return filepath.Clean(e.Path)
	}
	return filepath.Join(e.Workspace, e.Path)
}

// lineRuns renders which 1-based lines carry an origin, as compact runs:
// "12-48, 60". A heavily interleaved file is capped rather than scrolled.
func lineRuns(lines []int, origin int) string {
	var runs []string
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		if start == end {
			runs = append(runs, fmt.Sprintf("%d", start))
		} else {
			runs = append(runs, fmt.Sprintf("%d-%d", start, end))
		}
		start = -1
	}
	for i, o := range lines {
		if o == origin {
			if start < 0 {
				start = i + 1
			}
			continue
		}
		flush(i)
	}
	flush(len(lines))
	if len(runs) > blameMaxRuns {
		return strings.Join(runs[:blameMaxRuns], ", ") + fmt.Sprintf(" … and %d more runs", len(runs)-blameMaxRuns)
	}
	return strings.Join(runs, ", ")
}

func cmdBlame(m *tuiModel, args string) tea.Cmd {
	path := strings.TrimSpace(args)
	if path == "" {
		return noticeCmd("error", "/blame takes the file to annotate")
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(m.app.workspace, path)
	}
	m.addInfo(fmt.Sprintf("who wrote %s\n", path) +
		strings.Join(blameLines(m.app.store, m.app.workspace, filepath.Clean(abs), path), "\n"))
	return nil
}

func runBlameCLI(w io.Writer, store *session.Store, workspace, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "who wrote %s\n", path)
	for _, line := range blameLines(store, workspace, filepath.Clean(abs), path) {
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
	return nil
}
