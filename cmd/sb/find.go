package main

// /find and `sb find`: search the workspace's recorded sessions by content.
// "Which session did I fix the runner race in" is a question the picker's
// first-words labels cannot answer once the day is long; this greps what
// was actually said — the user's prompts and the model's answers — and
// hands back the ids /resume takes. Read-only over the logs, the open one
// included, the same posture as sb cost.

import (
	"fmt"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
)

const findMaxSessions = 20

func findLines(store *session.Store, workspace, query string) []string {
	infos, err := store.List(workspace)
	if err != nil {
		return []string{"  " + err.Error()}
	}
	if len(infos) == 0 {
		return []string{"  no sessions recorded for this workspace yet"}
	}

	needle := strings.ToLower(query)
	var lines []string
	matched := 0
	for _, info := range infos {
		state, err := session.ReadState(info.Path)
		if err != nil {
			continue
		}
		hits, snippet := searchMessages(state.Messages, needle)
		if hits == 0 {
			continue
		}
		matched++
		if matched > findMaxSessions {
			lines = append(lines, fmt.Sprintf("  … more sessions match; a narrower query sees past the first %d", findMaxSessions))
			break
		}
		label, _ := session.ReadOpening(info.Path)
		if label == "" {
			label = "(no prompt recorded)"
		}
		word := "match"
		if hits > 1 {
			word = "matches"
		}
		lines = append(lines,
			fmt.Sprintf("  %s  %s  %d %s", state.ID, info.Modified.Local().Format("Jan 2 15:04"), hits, word),
			"    "+truncate(label, 70),
			"    "+snippet)
	}
	if matched == 0 {
		return []string{fmt.Sprintf("  nothing in %d sessions says %q", len(infos), query)}
	}
	lines = append(lines, "  /resume <id> picks one up")
	return lines
}

// searchMessages counts case-insensitive hits across what was said — user
// and assistant text, not tool payloads, because the question is about the
// conversation — and returns the first matching line as the snippet.
func searchMessages(messages []provider.Message, needle string) (int, string) {
	hits := 0
	snippet := ""
	for _, msg := range messages {
		if msg.Role != provider.RoleUser && msg.Role != provider.RoleAssistant {
			continue
		}
		for _, b := range msg.Content {
			text, ok := b.(provider.Text)
			if !ok {
				continue
			}
			for _, line := range strings.Split(text.Text, "\n") {
				n := strings.Count(strings.ToLower(line), needle)
				if n == 0 {
					continue
				}
				hits += n
				if snippet == "" {
					snippet = truncate(strings.TrimSpace(line), 70)
				}
			}
		}
	}
	return hits, snippet
}

func cmdFind(m *tuiModel, args string) tea.Cmd {
	query := strings.TrimSpace(args)
	if query == "" {
		return noticeCmd("error", "/find takes text to search this workspace's sessions for")
	}
	m.addInfo(fmt.Sprintf("sessions saying %q\n", query) +
		strings.Join(findLines(m.app.store, m.app.workspace, query), "\n"))
	return nil
}

func runFindCLI(w io.Writer, store *session.Store, workspace, query string) error {
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("sb find takes text to search this workspace's sessions for")
	}
	for _, line := range findLines(store, workspace, strings.TrimSpace(query)) {
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
	return nil
}
