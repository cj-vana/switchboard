package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

const (
	turnReviewDefaultMaxLines = 1200
	turnReviewDefaultMaxBytes = 256 << 10
	turnReviewLoadMaxFiles    = 256
	turnReviewLoadMaxBytes    = turnReviewDefaultMaxBytes
)

type turnReviewKind string

const (
	turnReviewCreated     turnReviewKind = "created"
	turnReviewModified    turnReviewKind = "modified"
	turnReviewDeleted     turnReviewKind = "deleted"
	turnReviewTruncated   turnReviewKind = "truncated"
	turnReviewModeOnly    turnReviewKind = "mode"
	turnReviewUnchanged   turnReviewKind = "unchanged"
	turnReviewStale       turnReviewKind = "stale"
	turnReviewUnavailable turnReviewKind = "unavailable"
)

type turnReviewMode struct {
	Before  fs.FileMode
	After   fs.FileMode
	Changed bool
}

// turnReviewFile is one path in a checkpoint turn. Before comes only from the
// recorder's exact pre-image. After is populated only when the checkpoint has
// safely read the current file and matched it against the committed post-image;
// stale and unsafe entries never expose whatever happens to be at Path now.
type turnReviewFile struct {
	Path        string
	DisplayPath string
	Kind        turnReviewKind
	Stale       bool
	Binary      bool
	Mode        turnReviewMode
	Before      checkpoint.FileState
	After       checkpoint.FileState
	Large       bool
	Skipped     bool
	Error       string
}

// turnReview is a stable, read-only image. Index is the recorder's one-based
// chronological turn number at load time.
type turnReview struct {
	Index     int
	Label     string
	Open      bool
	Partial   bool
	Workspace string
	Files     []turnReviewFile
	Omitted   int
}

// loadTurnReview loads one checkpoint turn without consuming it. Positive turn
// numbers are one-based and oldest-first, matching /changes; zero or negative
// selects the currently open user turn and never falls back to an older
// mutating turn. The recorder owns every post-image read so its captured parent
// identity and committed fingerprint remain authoritative.
func loadTurnReview(rec *checkpoint.Recorder, turn int, workspace string) (turnReview, error) {
	if rec == nil {
		return turnReview{}, errors.New("turn review is unavailable: no checkpoint recorder")
	}
	cursor, index, err := selectTurnReview(rec, turn)
	if err != nil {
		return turnReview{}, err
	}
	return loadTurnReviewCursor(rec, cursor, index, workspace)
}

func selectTurnReview(rec *checkpoint.Recorder, turn int) (checkpoint.ReviewCursor, int, error) {
	if rec == nil {
		return checkpoint.ReviewCursor{}, 0, errors.New("turn review is unavailable: no checkpoint recorder")
	}
	if turn <= 0 {
		cursor, index, hasMutations, ok := rec.CurrentReviewCursor()
		if !ok || !hasMutations {
			return checkpoint.ReviewCursor{}, 0, errors.New("current turn has no recorded write/edit mutations")
		}
		return cursor, index, nil
	}
	cursor, total, ok := rec.ReviewCursorAt(turn)
	if total == 0 {
		return checkpoint.ReviewCursor{}, 0, errors.New("no recorded mutation turns")
	}
	if !ok {
		return checkpoint.ReviewCursor{}, 0, fmt.Errorf("turn %d is out of range; recorded turns are 1-%d", turn, total)
	}
	return cursor, turn, nil
}

func loadTurnReviewCursor(rec *checkpoint.Recorder, cursor checkpoint.ReviewCursor, index int, workspace string) (turnReview, error) {
	logicalRoot, realRoot, err := turnReviewWorkspace(workspace)
	if err != nil {
		return turnReview{}, err
	}
	snapshot, omitted, err := rec.ReviewSnapshot(cursor, turnReviewLoadMaxFiles, turnReviewLoadMaxBytes)
	if err != nil {
		return turnReview{}, err
	}
	review := turnReview{
		Index:     index,
		Label:     snapshot.Label,
		Open:      snapshot.Open,
		Partial:   snapshot.Partial || omitted > 0,
		Workspace: logicalRoot,
		Omitted:   omitted,
	}
	remainingBytes := turnReviewLoadMaxBytes
	for _, mutation := range snapshot.Files {
		remainingBytes -= len(mutation.Before.Content)
	}

	for _, mutation := range snapshot.Files {
		file := turnReviewFile{
			Path:        mutation.Path,
			DisplayPath: turnReviewDisplayPath(logicalRoot, realRoot, mutation.Path),
			Before:      mutation.Before,
		}
		if pathErr := validateTurnReviewPath(logicalRoot, realRoot, mutation.Path); pathErr != nil {
			file.Kind = turnReviewStale
			file.Stale = true
			file.Error = pathErr.Error()
			review.Files = append(review.Files, file)
			continue
		}

		current, readErr := rec.ReadSnapshotCurrentBounded(mutation, remainingBytes)
		file.After = current
		switch {
		case readErr == nil:
			remainingBytes -= len(current.Content)
			file.Kind = classifyTurnReviewFile(file.Before, file.After)
		case errors.Is(readErr, checkpoint.ErrSnapshotTooLarge):
			if !file.Before.Existed && file.After.Existed {
				file.Kind = turnReviewCreated
			} else {
				file.Kind = turnReviewModified
			}
			file.Large = true
			file.Error = readErr.Error()
		default:
			file.Kind = turnReviewStale
			file.Stale = true
			file.Error = readErr.Error()
			review.Files = append(review.Files, file)
			continue
		}
		file.Mode = turnReviewMode{
			Before:  file.Before.Mode,
			After:   file.After.Mode,
			Changed: file.Before.Existed && file.After.Existed && file.Before.Mode != file.After.Mode,
		}
		file.Binary = !file.Large && (turnReviewBinary(file.Before.Content) || turnReviewBinary(file.After.Content))
		review.Files = append(review.Files, file)
	}
	for _, path := range snapshot.Skipped {
		file := turnReviewFile{
			Path:        path,
			DisplayPath: turnReviewDisplayPath(logicalRoot, realRoot, path),
			Kind:        turnReviewUnavailable,
			Skipped:     true,
			Error:       "pre-image exceeded the checkpoint capture limit; exact review is unavailable",
		}
		if pathErr := validateTurnReviewPath(logicalRoot, realRoot, path); pathErr != nil {
			file.Stale = true
			file.Error = pathErr.Error()
		}
		review.Files = append(review.Files, file)
	}
	sort.Slice(review.Files, func(i, j int) bool { return review.Files[i].Path < review.Files[j].Path })
	if !rec.ReviewCursorValid(cursor) {
		return turnReview{}, fmt.Errorf("%w: review turn changed while it was loaded", checkpoint.ErrStale)
	}
	return review, nil
}

func classifyTurnReviewFile(before, after checkpoint.FileState) turnReviewKind {
	switch {
	case !before.Existed && after.Existed:
		return turnReviewCreated
	case before.Existed && !after.Existed:
		return turnReviewDeleted
	case before.Existed && after.Existed && len(before.Content) > 0 && len(after.Content) == 0:
		return turnReviewTruncated
	case bytes.Equal(before.Content, after.Content) && before.Mode != after.Mode:
		return turnReviewModeOnly
	case bytes.Equal(before.Content, after.Content):
		return turnReviewUnchanged
	default:
		return turnReviewModified
	}
}

func turnReviewBinary(content []byte) bool {
	return bytes.IndexByte(content, 0) >= 0 || !utf8.Valid(content)
}

func turnReviewWorkspace(workspace string) (logical, real string, err error) {
	if strings.TrimSpace(workspace) == "" {
		return "", "", errors.New("turn review needs a workspace")
	}
	logical, err = filepath.Abs(workspace)
	if err != nil {
		return "", "", fmt.Errorf("resolving review workspace: %w", err)
	}
	logical = filepath.Clean(logical)
	real, err = filepath.EvalSymlinks(logical)
	if err != nil {
		return "", "", fmt.Errorf("resolving review workspace: %w", err)
	}
	info, err := os.Lstat(real)
	if err != nil {
		return "", "", fmt.Errorf("inspecting review workspace: %w", err)
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return "", "", fmt.Errorf("review workspace %s is not a real directory", logical)
	}
	return logical, filepath.Clean(real), nil
}

func validateTurnReviewPath(logicalRoot, realRoot, path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("refusing unsafe checkpoint path %q", path)
	}
	base, rel, ok := turnReviewRelative(logicalRoot, path)
	if !ok {
		base, rel, ok = turnReviewRelative(realRoot, path)
	}
	if !ok || rel == "." {
		return fmt.Errorf("refusing checkpoint path outside workspace: %s", path)
	}

	parentRel := filepath.Dir(rel)
	cursor := base
	if parentRel != "." {
		for _, part := range strings.Split(parentRel, string(filepath.Separator)) {
			cursor = filepath.Join(cursor, part)
			info, err := os.Lstat(cursor)
			if err != nil {
				return fmt.Errorf("refusing unsafe parent of %s: %w", path, err)
			}
			if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
				return fmt.Errorf("refusing symlink or non-directory parent of %s", path)
			}
		}
	}
	parentReal, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("refusing unsafe parent of %s: %w", path, err)
	}
	if _, _, ok := turnReviewRelative(realRoot, parentReal); !ok && filepath.Clean(parentReal) != realRoot {
		return fmt.Errorf("refusing parent outside workspace for %s", path)
	}
	return nil
}

func turnReviewRelative(root, path string) (string, string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", false
	}
	return root, rel, true
}

func turnReviewDisplayPath(logicalRoot, realRoot, path string) string {
	if _, rel, ok := turnReviewRelative(logicalRoot, path); ok {
		return rel
	}
	if _, rel, ok := turnReviewRelative(realRoot, path); ok {
		return rel
	}
	return path
}

// Render returns a deterministic, bounded unified review. It is a rendering
// of checkpoint bytes, not Git state: no repository command runs and the index
// is never read or changed.
func (r turnReview) Render(maxLines, maxBytes int) string {
	if maxLines <= 0 {
		maxLines = turnReviewDefaultMaxLines
	}
	if maxBytes <= 0 {
		maxBytes = turnReviewDefaultMaxBytes
	}
	w := newTurnReviewWriter(maxLines, maxBytes)
	w.line(fmt.Sprintf("turn %d: %s\n", r.Index, terminaltext.Escape(firstLine(r.Label))))
	if r.Open {
		w.line("state: current turn (open checkpoint)\n")
	} else {
		w.line("state: recorded turn\n")
	}
	w.line("scope: write and edit mutations only; shell and external changes are not captured\n")
	if r.Omitted > 0 {
		w.line(fmt.Sprintf("coverage: %d additional recorded path(s) omitted by the review load limit\n", r.Omitted))
	}
	for i := range r.Files {
		section := newTurnReviewWriter(maxLines-w.lineCount, maxBytes-w.byteCount)
		if !renderTurnReviewFile(section, r.Files[i]) {
			w.truncated = true
			break
		}
		if !w.append(section) {
			break
		}
	}
	return w.finish()
}

func renderTurnReviewFile(w *turnReviewWriter, file turnReviewFile) bool {
	path := turnReviewQuotedLabel(file.DisplayPath)
	if !w.line(fmt.Sprintf("\nfile %s  [%s]\n", path, file.Kind)) {
		return false
	}
	if file.Stale || file.Skipped {
		return w.line("refused: " + terminaltext.Escape(file.Error) + "\n")
	}
	beforeLabel := turnReviewQuotedLabel("a/" + filepath.ToSlash(file.DisplayPath))
	afterLabel := turnReviewQuotedLabel("b/" + filepath.ToSlash(file.DisplayPath))
	if file.Kind == turnReviewCreated {
		beforeLabel = "/dev/null"
		w.line("new file mode " + turnReviewModeString(file.After.Mode) + "\n")
	} else if file.Kind == turnReviewDeleted {
		afterLabel = "/dev/null"
		w.line("deleted file mode " + turnReviewModeString(file.Before.Mode) + "\n")
	} else if file.Mode.Changed {
		w.line("old mode " + turnReviewModeString(file.Mode.Before) + "\n")
		w.line("new mode " + turnReviewModeString(file.Mode.After) + "\n")
	}
	if file.Large {
		return w.line("current file exceeds the review byte limit; content and digest were not reverified\n")
	}
	if file.Kind == turnReviewModeOnly {
		return true
	}
	if file.Kind == turnReviewUnchanged {
		return w.line("no net byte or mode change\n")
	}
	if file.Binary {
		switch file.Kind {
		case turnReviewCreated:
			return w.line("Binary file " + afterLabel + " created\n")
		case turnReviewDeleted:
			return w.line("Binary file " + beforeLabel + " deleted\n")
		default:
			return w.line("Binary files " + beforeLabel + " and " + afterLabel + " differ\n")
		}
	}
	if file.Kind == turnReviewCreated && len(file.After.Content) == 0 {
		return w.line("empty file created\n")
	}
	if file.Kind == turnReviewDeleted && len(file.Before.Content) == 0 {
		return w.line("empty file deleted\n")
	}
	return renderTurnUnified(w, beforeLabel, afterLabel, file.Before.Content, file.After.Content)
}

func turnReviewModeString(mode fs.FileMode) string {
	bits := uint32(mode.Perm())
	if mode&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if mode&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if mode&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return fmt.Sprintf("%06o", 0o100000|bits)
}

func turnReviewQuotedLabel(label string) string {
	label = filepath.ToSlash(label)
	if label == "" {
		label = "."
	}
	quoted := false
	for i := 0; i < len(label); i++ {
		c := label[i]
		if c <= ' ' || c >= 0x7f || c == '"' || c == '\\' {
			quoted = true
			break
		}
	}
	if !quoted {
		return label
	}
	var out strings.Builder
	out.Grow(len(label) + 2)
	out.WriteByte('"')
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch c {
		case '\a':
			out.WriteString(`\a`)
		case '\b':
			out.WriteString(`\b`)
		case '\t':
			out.WriteString(`\t`)
		case '\n':
			out.WriteString(`\n`)
		case '\v':
			out.WriteString(`\v`)
		case '\f':
			out.WriteString(`\f`)
		case '\r':
			out.WriteString(`\r`)
		case '"', '\\':
			out.WriteByte('\\')
			out.WriteByte(c)
		default:
			if c >= 0x20 && c < 0x7f {
				out.WriteByte(c)
				continue
			}
			out.WriteByte('\\')
			out.WriteByte('0' + (c >> 6))
			out.WriteByte('0' + ((c >> 3) & 7))
			out.WriteByte('0' + (c & 7))
		}
	}
	out.WriteByte('"')
	return out.String()
}

func renderTurnUnified(w *turnReviewWriter, beforeLabel, afterLabel string, before, after []byte) bool {
	lineBudget := max(0, w.maxLines-w.lineCount)
	byteBudget := max(0, w.maxBytes-w.byteCount)
	oldLines, oldOK := splitTurnReviewLines(before, lineBudget, byteBudget)
	newLines, newOK := splitTurnReviewLines(after, lineBudget, byteBudget)
	if !oldOK || !newOK {
		w.truncated = true
		return false
	}
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}
	if prefix == len(oldLines) && prefix == len(newLines) {
		return true
	}
	const contextLines = 3
	oldStart := max(0, prefix-contextLines)
	newStart := max(0, prefix-contextLines)
	oldMiddleEnd := len(oldLines) - suffix
	newMiddleEnd := len(newLines) - suffix
	oldEnd := min(len(oldLines), oldMiddleEnd+contextLines)
	newEnd := min(len(newLines), newMiddleEnd+contextLines)

	if !w.line("--- "+beforeLabel+"\n") || !w.line("+++ "+afterLabel+"\n") {
		return false
	}
	if !w.line(fmt.Sprintf("@@ -%s +%s @@\n",
		turnReviewRange(oldStart, oldEnd-oldStart), turnReviewRange(newStart, newEnd-newStart))) {
		return false
	}
	for _, line := range oldLines[oldStart:prefix] {
		if !turnReviewDiffLine(w, ' ', line) {
			return false
		}
	}
	for _, line := range oldLines[prefix:oldMiddleEnd] {
		if !turnReviewDiffLine(w, '-', line) {
			return false
		}
	}
	for _, line := range newLines[prefix:newMiddleEnd] {
		if !turnReviewDiffLine(w, '+', line) {
			return false
		}
	}
	for _, line := range newLines[newMiddleEnd:newEnd] {
		if !turnReviewDiffLine(w, ' ', line) {
			return false
		}
	}
	return true
}

func splitTurnReviewLines(content []byte, maxLines, maxBytes int) ([]string, bool) {
	if len(content) == 0 {
		return nil, true
	}
	if maxLines <= 0 || maxBytes <= 0 || len(content) > maxBytes {
		return nil, false
	}
	capacity := min(bytes.Count(content, []byte{'\n'})+1, maxLines)
	lines := make([]string, 0, capacity)
	start := 0
	for i, b := range content {
		if b == '\n' {
			if len(lines) == maxLines {
				return nil, false
			}
			lines = append(lines, string(content[start:i+1]))
			start = i + 1
		}
	}
	if start < len(content) {
		if len(lines) == maxLines {
			return nil, false
		}
		lines = append(lines, string(content[start:]))
	}
	return lines, true
}

func turnReviewRange(start, count int) string {
	line := start + 1
	if count == 0 {
		line = start
	}
	if count == 1 {
		return strconv.Itoa(line)
	}
	return fmt.Sprintf("%d,%d", line, count)
}

func turnReviewDiffLine(w *turnReviewWriter, prefix byte, line string) bool {
	hasNewline := strings.HasSuffix(line, "\n")
	if hasNewline {
		line = strings.TrimSuffix(line, "\n")
	}
	// Avoid allocating an escaped copy of a single enormous source line when
	// the remaining output budget cannot possibly hold even its raw bytes.
	if len(line)+2 > w.maxBytes-w.byteCount {
		w.truncated = true
		return false
	}
	line = terminaltext.Display(line)
	line = strings.ReplaceAll(line, "\t", `\t`)
	if !w.line(string(prefix) + line + "\n") {
		return false
	}
	if !hasNewline {
		return w.line("\\ No newline at end of file\n")
	}
	return true
}

type turnReviewWriter struct {
	lines     []string
	sections  []turnReviewBoundary
	byteCount int
	lineCount int
	maxLines  int
	maxBytes  int
	truncated bool
}

type turnReviewBoundary struct {
	chunks int
	bytes  int
	lines  int
}

func newTurnReviewWriter(maxLines, maxBytes int) *turnReviewWriter {
	return &turnReviewWriter{maxLines: maxLines, maxBytes: maxBytes}
}

func (w *turnReviewWriter) line(line string) bool {
	if w.truncated {
		return false
	}
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	lineCount := strings.Count(line, "\n")
	if w.lineCount+lineCount > w.maxLines || w.byteCount+len(line) > w.maxBytes {
		w.truncated = true
		return false
	}
	w.lines = append(w.lines, line)
	w.byteCount += len(line)
	w.lineCount += lineCount
	return true
}

func (w *turnReviewWriter) append(section *turnReviewWriter) bool {
	if w.truncated {
		return false
	}
	if section == nil || section.truncated {
		w.truncated = true
		return false
	}
	if w.lineCount+section.lineCount > w.maxLines || w.byteCount+section.byteCount > w.maxBytes {
		w.truncated = true
		return false
	}
	w.sections = append(w.sections, turnReviewBoundary{
		chunks: len(w.lines),
		bytes:  w.byteCount,
		lines:  w.lineCount,
	})
	w.lines = append(w.lines, section.lines...)
	w.byteCount += section.byteCount
	w.lineCount += section.lineCount
	return true
}

func (w *turnReviewWriter) finish() string {
	if w.truncated {
		const marker = "... turn review truncated ...\n"
		for len(w.sections) > 0 && (w.lineCount+1 > w.maxLines || w.byteCount+len(marker) > w.maxBytes) {
			last := w.sections[len(w.sections)-1]
			w.sections = w.sections[:len(w.sections)-1]
			w.lines = w.lines[:last.chunks]
			w.byteCount = last.bytes
			w.lineCount = last.lines
		}
		for len(w.lines) > 0 && (w.lineCount+1 > w.maxLines || w.byteCount+len(marker) > w.maxBytes) {
			last := w.lines[len(w.lines)-1]
			w.lines = w.lines[:len(w.lines)-1]
			w.byteCount -= len(last)
			w.lineCount -= strings.Count(last, "\n")
		}
		if w.lineCount+1 <= w.maxLines && w.byteCount+len(marker) <= w.maxBytes {
			w.lines = append(w.lines, marker)
		}
	}
	return strings.Join(w.lines, "")
}
