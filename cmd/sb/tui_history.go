package main

// Prompt history that survives the session, per workspace. Muscle memory is
// the whole feature: up-arrow reaches last week's prompt in this repository,
// ctrl+r searches it incrementally, and none of it is shared across projects,
// because the prompt that made sense in one repository is noise in another.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

const historyKeep = 500

func historyPath(workspace string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(workspace))
	return filepath.Join(home, ".switchboard", "history", hex.EncodeToString(sum[:8])+".hist"), nil
}

// loadHistory reads the workspace's prompt history, oldest first. Newlines
// are escaped on disk so one line is one prompt whatever the prompt held.
func loadHistory(workspace string) []string {
	path, err := historyPath(workspace)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line != "" {
			out = append(out, strings.ReplaceAll(line, "\\n", "\n"))
		}
	}
	if len(out) > historyKeep {
		out = out[len(out)-historyKeep:]
	}
	return out
}

// appendHistory writes one prompt through to disk, rewriting the file only
// when it has grown far past the cap. Failure is silent: history is a
// convenience, and a warning about it would outweigh it.
func appendHistory(workspace, prompt string) {
	path, err := historyPath(workspace)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(strings.ReplaceAll(prompt, "\n", "\\n") + "\n")
}

// --- reverse search ----------------------------------------------------------

// startHistorySearch is ctrl+r at the prompt: incremental search over this
// workspace's history, newest match first, ctrl+r again for the next older.
func (m *tuiModel) startHistorySearch() {
	m.histSearch = true
	m.histQuery = ""
	m.histMatch = -1
}

// historySearchKey handles one keypress while searching. It returns false
// when the search is over and the key should fall through to normal handling.
func (m *tuiModel) historySearchKey(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.histSearch = false
		return true
	case "enter":
		if hit := m.historyMatch(m.histMatch); hit != "" {
			m.ta.SetValue(hit)
			m.ta.CursorEnd()
			m.growInput()
		}
		m.histSearch = false
		return true
	case "ctrl+r":
		if next := m.historyMatch(m.histMatch + 1); next != "" {
			m.histMatch++
		}
		return true
	case "backspace":
		if m.histQuery != "" {
			// By rune, not by byte: deleting the last byte of a multi-byte
			// character would leave the query invalid UTF-8.
			runes := []rune(m.histQuery)
			m.histQuery = string(runes[:len(runes)-1])
			m.histMatch = m.firstMatch()
		}
		return true
	}
	if msg.Type == tea.KeyRunes {
		m.histQuery += string(msg.Runes)
		m.histMatch = m.firstMatch()
		return true
	}
	return true // swallow everything else; search owns the keyboard
}

func (m *tuiModel) firstMatch() int {
	if m.historyMatch(0) != "" {
		return 0
	}
	return -1
}

// historyMatch returns the nth newest history entry containing the query,
// or "" when there is no such match.
func (m *tuiModel) historyMatch(n int) string {
	if n < 0 {
		return ""
	}
	query := strings.ToLower(m.histQuery)
	seen := 0
	for i := len(m.history) - 1; i >= 0; i-- {
		if query == "" || strings.Contains(strings.ToLower(m.history[i]), query) {
			if seen == n {
				return m.history[i]
			}
			seen++
		}
	}
	return ""
}

func (m *tuiModel) historySearchView() string {
	hit := m.historyMatch(m.histMatch)
	line := "(reverse-search) " + m.histQuery
	if hit != "" {
		line += m.th.dim.Render("  → " + firstLine(strings.ReplaceAll(hit, "\n", " ")))
	} else if m.histQuery != "" {
		line += m.th.dim.Render("  no match")
	}
	return m.th.accent.Render(line)
}
