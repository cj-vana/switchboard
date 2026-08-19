package agent

// Project instructions, composed the way the ecosystem writes them.
//
// AGENTS.md and CLAUDE.md are composition formats everywhere they are defined:
// a repository root states the house rules, a package states its own, a
// developer shadows a checked-in file locally without editing it, and a file
// pulls in another with an import. This program honored the filename and
// ignored the format — the workspace root's first hit, whole, and nothing
// else — so a monorepo's package instructions were invisible and a user's own
// standing preferences had nowhere to live.
//
// Order is general to specific, and it is the reading order for a reason: the
// last word should belong to the file closest to the work. The budget is one
// number shared by everything, because the prompt is paid for on every cold
// cache and four composed layers can triple a request as easily as one long
// file. When it binds, the most general layer is dropped first and the result
// says which, since dropping the package's own rules to keep the user's
// defaults would be exactly backwards.
//
// Two refusals. There is no `!cmd` substitution and there will not be: a
// checkout must not get a command executed by the act of being opened, which
// is the same rule that keeps a repository from declaring a /watch verifier.
// And a repository's import may not resolve outside the workspace, because an
// instruction file that can read any path is a file that can read a private
// key into a prompt.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	// maxInstructionBytes caps everything composed here, together. It is the
	// budget the frozen zone pays on every cold cache.
	maxInstructionBytes = 16 << 10

	// maxImportDepth bounds how far an import chain runs. Two hops covers a
	// root file pulling in a shared section that pulls in a fragment; past
	// that the file is a program, and this reader is not one.
	maxImportDepth = 2

	// maxLayers bounds how many directories are consulted between the
	// repository root and the working directory, so a deep tree cannot turn
	// assembly into a walk of arbitrary length.
	maxLayers = 8
)

// instructionFiles are the names read at each layer, in order. The first hit
// at a layer wins: a directory holding both means them as one instruction set,
// and reading both would double whatever they agree on.
var instructionFiles = []string{"AGENTS.md", "CLAUDE.md"}

// overrideFiles are the uncommitted siblings a developer can use to shadow a
// checked-in file without editing it. They are read after the file they
// shadow, so the local word is the later one.
var overrideFiles = []string{"AGENTS.override.md", "CLAUDE.local.md"}

// userInstructionDirs are the roots a person's own standing instructions live
// in. All three are read because a user who already keeps one for another tool
// means it for this one too, and the alternative is asking them to duplicate a
// file they already maintain.
var userInstructionDirs = []string{".switchboard", ".agents", ".claude"}

type instructionLayer struct {
	label string
	text  string
}

// ProjectInstructions composes the workspace's agent instructions.
//
// The bool reports whether anything was found at all, which is what the caller
// uses to decide whether the system prompt grows a block.
func ProjectInstructions(workspace string) (string, bool) {
	layers := collectInstructionLayers(workspace)
	if len(layers) == 0 {
		return "", false
	}
	return renderInstructionLayers(layers), true
}

func collectInstructionLayers(workspace string) []instructionLayer {
	var layers []instructionLayer
	if home, err := os.UserHomeDir(); err == nil {
		for _, dir := range userInstructionDirs {
			root := filepath.Join(home, dir)
			// The user's own roots are explicit import boundaries: a file
			// there may pull in a neighbour, which is how a person keeps one
			// set of rules in pieces.
			layers = append(layers, readInstructionDir(root, root)...)
		}
	}
	for _, dir := range instructionDirs(workspace) {
		layers = append(layers, readInstructionDir(dir, workspace)...)
	}
	return layers
}

// instructionDirs lists the directories to consult, general to specific: the
// repository root first, then each directory down to the workspace.
//
// The repository root is found by walking up for a .git entry. Without one the
// workspace is the only layer, which is the honest answer for a directory that
// is not a checkout.
func instructionDirs(workspace string) []string {
	workspace = filepath.Clean(workspace)
	root := workspace
	for dir := workspace; ; {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			root = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	var dirs []string
	for dir := workspace; ; {
		dirs = append(dirs, dir)
		if dir == root {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// Collected specific-first; the prompt wants general-first.
	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}
	if len(dirs) > maxLayers {
		// Keep the most specific, which are the ones closest to the work.
		dirs = dirs[len(dirs)-maxLayers:]
	}
	return dirs
}

// readInstructionDir reads one directory's instruction file and its override
// sibling. boundary is the root an import may not escape.
func readInstructionDir(dir, boundary string) []instructionLayer {
	var out []instructionLayer
	for _, name := range instructionFiles {
		path := filepath.Join(dir, name)
		if text, ok := readInstructionFile(path, boundary); ok {
			out = append(out, instructionLayer{label: path, text: text})
			break
		}
	}
	for _, name := range overrideFiles {
		path := filepath.Join(dir, name)
		if text, ok := readInstructionFile(path, boundary); ok {
			out = append(out, instructionLayer{label: path, text: text})
		}
	}
	return out
}

func readInstructionFile(path, boundary string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(data)) == "" {
		return "", false
	}
	return expandImports(string(data), filepath.Dir(path), boundary, map[string]bool{path: true}, 0), true
}

// expandImports replaces a line that is exactly an @path reference with the
// file it names.
//
// Only a whole line counts. An @path inside a sentence is prose about a file,
// and a reader that spliced a file in wherever the character appeared would
// make every mention of an email address an import.
func expandImports(text, dir, boundary string, seen map[string]bool, depth int) string {
	if depth >= maxImportDepth || !strings.Contains(text, "@") {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) < 2 || !strings.HasPrefix(trimmed, "@") || strings.ContainsAny(trimmed, " \t") {
			continue
		}
		target := filepath.Clean(filepath.Join(dir, strings.TrimPrefix(trimmed, "@")))
		if !withinBoundary(target, boundary) {
			lines[i] = fmt.Sprintf("[%s was not imported: it resolves outside %s]", trimmed, boundary)
			continue
		}
		if seen[target] {
			lines[i] = fmt.Sprintf("[%s was not imported: it is already part of this file]", trimmed)
			continue
		}
		data, err := os.ReadFile(target)
		if err != nil {
			lines[i] = fmt.Sprintf("[%s was not imported: %v]", trimmed, err)
			continue
		}
		seen[target] = true
		lines[i] = expandImports(string(data), filepath.Dir(target), boundary, seen, depth+1)
	}
	return strings.Join(lines, "\n")
}

// withinBoundary reports whether target sits inside root. It compares cleaned
// paths rather than resolving symlinks, because the check is about what the
// instruction file may name and a resolved path would let a symlink inside the
// workspace point anywhere.
func withinBoundary(target, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// renderInstructionLayers joins the layers under one budget.
//
// The budget is spent specific-first so the file closest to the work survives,
// and whatever did not fit is named. A layer that would not fit at all is
// dropped whole rather than half-quoted: half a rule reads as a rule.
func renderInstructionLayers(layers []instructionLayer) string {
	budget := maxInstructionBytes
	kept := make([]string, len(layers))
	var dropped []string

	for i := len(layers) - 1; i >= 0; i-- {
		layer := layers[i]
		header := fmt.Sprintf("Instructions from %s (follow them):\n\n", layer.label)
		if len(header) >= budget {
			dropped = append(dropped, layer.label)
			continue
		}
		body := layer.text
		if len(header)+len(body) > budget {
			body = truncateInstruction(body, budget-len(header))
			if strings.TrimSpace(body) == "" {
				dropped = append(dropped, layer.label)
				continue
			}
			body += fmt.Sprintf("\n\n[%s was cut here: the composed instructions reached the %d byte budget]",
				layer.label, maxInstructionBytes)
		}
		kept[i] = header + body
		budget -= len(header) + len(body)
	}

	var b strings.Builder
	for _, section := range kept {
		if section == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(section)
	}
	if len(dropped) > 0 {
		// Named rather than silent: a rule that did not arrive is a rule the
		// model will be judged against anyway.
		fmt.Fprintf(&b, "\n\n[These instruction files did not fit the %d byte budget and were not read: %s]",
			maxInstructionBytes, strings.Join(dropped, ", "))
	}
	return b.String()
}

// truncateInstruction cuts on a line boundary, and failing that on a rune.
//
// The old reader sliced bytes, which could cut a multi-byte character in half
// and hand the model an invalid string. Cutting at a line is better still: a
// sentence stopped mid-word reads as an instruction that means something else.
func truncateInstruction(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	cut := text[:limit]
	if newline := strings.LastIndexByte(cut, '\n'); newline > 0 {
		return cut[:newline]
	}
	for !utf8.ValidString(cut) && len(cut) > 0 {
		cut = cut[:len(cut)-1]
	}
	return cut
}
