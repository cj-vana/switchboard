package main

// @path mentions: completion while typing, attachment on submit. The
// completion saves the typing; the attachment saves a tool round trip, which
// on a small local model is the difference between answering from the file
// and hallucinating it. A token that resolves to nothing is left alone — an
// email address is not a file, and the agent can still read paths itself.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
)

const (
	mentionMaxResults = 8
	mentionMaxFiles   = 8
	mentionFileCap    = 32 << 10
	mentionWalkCap    = 20000
	mentionListTTL    = 5 * time.Second

	// mentionImageCap is the strictest per-image limit among the surfaces
	// this program speaks; refusing above it here beats a provider error
	// after the upload.
	mentionImageCap = 5 << 20
)

// mentionImageTypes maps the extensions every reachable surface accepts to
// their media types. A mentioned image attaches as an image block rather
// than as text, when the target has evidence of taking one.
var mentionImageTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// skipDirs are never offered and never walked. They are where completion goes
// to die on a real repository.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"target": true, ".venv": true, "__pycache__": true,
}

// workspaceFiles lists relative paths, cached briefly: the agent writes files
// mid-session, so the list cannot be forever, and a walk per keystroke would
// make typing lag exactly when the repository is large (§14's 16ms).
func (m *tuiModel) workspaceFiles() []string {
	if time.Since(m.mentionListAt) < mentionListTTL && m.mentionList != nil {
		return m.mentionList
	}
	var files []string
	root := m.app.workspace
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] || (strings.HasPrefix(d.Name(), ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(files) >= mentionWalkCap {
			return filepath.SkipAll
		}
		if rel, err := filepath.Rel(root, path); err == nil {
			files = append(files, rel)
		}
		return nil
	})
	m.mentionList, m.mentionListAt = files, time.Now()
	return files
}

// mentionToken returns the @-token the cursor is completing, or "".
func mentionToken(value string) string {
	fields := strings.Fields(value)
	if len(fields) == 0 || !strings.HasSuffix(value, fields[len(fields)-1]) {
		return "" // the cursor is past a space, not inside the last token
	}
	last := fields[len(fields)-1]
	if !strings.HasPrefix(last, "@") || len(last) < 2 {
		return ""
	}
	return strings.TrimPrefix(last, "@")
}

func (m *tuiModel) mentionMatches() []string {
	frag := mentionToken(m.ta.Value())
	if frag == "" || strings.HasPrefix(m.ta.Value(), "/") {
		return nil
	}
	fragLower := strings.ToLower(frag)
	var exact, contains []string
	for _, f := range m.workspaceFiles() {
		lower := strings.ToLower(f)
		base := strings.ToLower(filepath.Base(f))
		switch {
		case strings.HasPrefix(base, fragLower) || strings.HasPrefix(lower, fragLower):
			exact = append(exact, f)
		case strings.Contains(lower, fragLower):
			contains = append(contains, f)
		}
	}
	sort.Slice(exact, func(i, j int) bool { return len(exact[i]) < len(exact[j]) })
	out := append(exact, contains...)
	if len(out) > mentionMaxResults {
		out = out[:mentionMaxResults]
	}
	return out
}

func (m *tuiModel) mentionsVisible() bool {
	return m.dlg == nil && !m.sugClosed && len(m.mentionMatches()) > 0
}

func (m *tuiModel) mentionsView() string {
	matches := m.mentionMatches()
	if len(matches) == 0 {
		return ""
	}
	if m.mentionSel >= len(matches) {
		m.mentionSel = len(matches) - 1
	}
	var rows []string
	for i, f := range matches {
		row := " @" + f
		if i == m.mentionSel {
			row = m.th.selected.Render(row)
		} else {
			row = m.th.dim.Render(row)
		}
		rows = append(rows, row)
	}
	return strings.Join(rows, "\n")
}

func (m *tuiModel) acceptMention() {
	matches := m.mentionMatches()
	if len(matches) == 0 {
		return
	}
	if m.mentionSel >= len(matches) {
		m.mentionSel = 0
	}
	v := m.ta.Value()
	frag := mentionToken(v)
	v = strings.TrimSuffix(v, "@"+frag) + "@" + matches[m.mentionSel] + " "
	m.ta.SetValue(v)
	m.ta.CursorEnd()
	m.mentionSel = 0
}

// expandMentions attaches the contents of every mentioned file to the prompt.
// The prompt keeps the @path where the user typed it — that is what they
// said — and the attachments follow, labelled, so the model knows why they
// are there. The augmented prompt is what the session records. A mentioned
// image comes back as an image block instead of text, with a labelled line
// in the prompt tying the attachment to the mention.
func (m *tuiModel) expandMentions(prompt string) (string, []provider.Image) {
	return expandPromptMentions(m.app.workspace, prompt)
}

// expandPromptMentions is shared by the interactive surfaces so `/tN prompt`
// and an ordinary prompt assemble identical text and image blocks.
func expandPromptMentions(workspace, prompt string) (string, []provider.Image) {
	var attached []string
	var images []provider.Image
	seen := map[string]bool{}
	for _, field := range strings.Fields(prompt) {
		token := strings.TrimPrefix(strings.Trim(field, ".,;:!?"), "@")
		if token == field || token == "" || seen[token] || len(attached) >= mentionMaxFiles {
			continue
		}
		path := filepath.Join(workspace, token)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}

		if mediaType, isImage := mentionImageTypes[strings.ToLower(filepath.Ext(token))]; isImage {
			if info.Size() > mentionImageCap {
				seen[token] = true
				attached = append(attached, fmt.Sprintf("%s (mentioned above) was not attached: %d bytes is over the %d-byte image cap.",
					token, info.Size(), mentionImageCap))
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			seen[token] = true
			images = append(images, provider.Image{MediaType: mediaType, Data: data})
			attached = append(attached, fmt.Sprintf("Image %s (mentioned above) is attached.", token))
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		seen[token] = true
		text := string(data)
		if len(text) > mentionFileCap {
			text = text[:mentionFileCap] + fmt.Sprintf("\n[truncated at %d bytes; read the file for the rest]", mentionFileCap)
		}
		attached = append(attached, fmt.Sprintf("Contents of %s (mentioned above):\n```\n%s\n```", token, strings.TrimRight(text, "\n")))
	}
	if len(attached) == 0 {
		return prompt, nil
	}
	return prompt + "\n\n" + strings.Join(attached, "\n\n"), images
}
