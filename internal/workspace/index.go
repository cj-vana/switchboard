package workspace

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
)

const (
	DefaultFileLimit    = 200_000
	DefaultSearchLimit  = 500
	DefaultSearchBytes  = 4 << 20
	defaultGitListBytes = 64 << 20
	defaultGitPathBytes = 64 << 10
)

var skippedDirectories = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "vendor": true, "dist": true, "build": true,
	"target": true, ".venv": true, "__pycache__": true,
}

type File struct {
	Path string `json:"path"`
	Size int64  `json:"size"`

	searchKey string
	baseKey   string
}

type Snapshot struct {
	Generation uint64 `json:"generation"`
	Files      []File `json:"files"`
	Truncated  bool   `json:"truncated"`
	// Skipped counts individual entries or excluded subtrees observed but not
	// indexed. Truncated separately says the collector stopped before EOF.
	Skipped int `json:"skipped"`
}

func (s Snapshot) Clone() Snapshot {
	s.Files = append([]File(nil), s.Files...)
	return s
}

type FileMatch struct {
	File  File `json:"file"`
	Score int  `json:"score"`
}

// Filter is allocation-bounded by limit and performs no filesystem I/O, so
// it is safe to call for every command-palette keystroke.
func (s Snapshot) Filter(query string, limit int) []FileMatch {
	if limit <= 0 {
		limit = 50
	}
	query = strings.TrimSpace(strings.ToLower(query))
	matches := make(matchHeap, 0, min(limit, len(s.Files)))
	for _, file := range s.Files {
		key, base := file.searchKey, file.baseKey
		if key == "" {
			key = strings.ToLower(file.Path)
			base = strings.ToLower(filepath.Base(file.Path))
		}
		score, ok := fuzzyScore(query, key, base)
		if !ok {
			continue
		}
		match := FileMatch{File: file, Score: score}
		if len(matches) < limit {
			heap.Push(&matches, match)
		} else if betterMatch(match, matches[0]) {
			heap.Pop(&matches)
			heap.Push(&matches, match)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return betterMatch(matches[i], matches[j])
	})
	return []FileMatch(matches)
}

func fuzzyScore(query, candidate, base string) (int, bool) {
	if query == "" {
		return len(candidate), true
	}
	switch {
	case candidate == query:
		return 0, true
	case base == query:
		return 1, true
	case strings.HasPrefix(base, query):
		return 10 + len(base) - len(query), true
	case strings.HasPrefix(candidate, query):
		return 20 + len(candidate) - len(query), true
	case strings.Contains(base, query):
		return 40 + strings.Index(base, query), true
	case strings.Contains(candidate, query):
		return 60 + strings.Index(candidate, query), true
	}
	qi, gaps := 0, 0
	for i := 0; i < len(candidate) && qi < len(query); i++ {
		if candidate[i] == query[qi] {
			qi++
		} else if qi > 0 {
			gaps++
		}
	}
	if qi != len(query) {
		return 0, false
	}
	return 100 + gaps + len(candidate) - len(query), true
}

func betterMatch(a, b FileMatch) bool {
	if a.Score != b.Score {
		return a.Score < b.Score
	}
	if len(a.File.Path) != len(b.File.Path) {
		return len(a.File.Path) < len(b.File.Path)
	}
	return a.File.Path < b.File.Path
}

// matchHeap keeps the worst retained match at index zero, so Filter stores at
// most the requested number instead of allocating for every file in a large
// repository.
type matchHeap []FileMatch

func (h matchHeap) Len() int           { return len(h) }
func (h matchHeap) Less(i, j int) bool { return betterMatch(h[j], h[i]) }
func (h matchHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *matchHeap) Push(value any)    { *h = append(*h, value.(FileMatch)) }
func (h *matchHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

type Index struct {
	root *Root
	cap  int

	gitCommand   func(context.Context, ...string) *exec.Cmd
	gitListBytes int64

	mu       sync.RWMutex
	snapshot Snapshot
	dirty    bool
	next     atomic.Uint64
}

func NewIndex(root *Root, fileLimit int) *Index {
	if fileLimit <= 0 {
		fileLimit = DefaultFileLimit
	}
	return &Index{
		root: root, cap: fileLimit, dirty: true,
		gitCommand: defaultGitCommand, gitListBytes: defaultGitListBytes,
	}
}

func defaultGitCommand(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "git", args...)
}

func (i *Index) Snapshot() Snapshot {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.snapshot.Clone()
}

func (i *Index) Invalidate() {
	i.mu.Lock()
	i.dirty = true
	i.mu.Unlock()
}

func (i *Index) Refresh(ctx context.Context) (Snapshot, error) {
	files, truncated, skipped, err := i.list(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{Generation: i.next.Add(1), Files: files, Truncated: truncated, Skipped: skipped}
	i.mu.Lock()
	i.snapshot = snapshot
	i.dirty = false
	i.mu.Unlock()
	return snapshot.Clone(), nil
}

func (i *Index) Ensure(ctx context.Context) (Snapshot, error) {
	i.mu.RLock()
	dirty := i.dirty
	snapshot := i.snapshot.Clone()
	i.mu.RUnlock()
	if !dirty && snapshot.Generation != 0 {
		return snapshot, nil
	}
	return i.Refresh(ctx)
}

func (i *Index) list(ctx context.Context) ([]File, bool, int, error) {
	if files, truncated, skipped, err := i.listGit(ctx); err == nil {
		return files, truncated, skipped, nil
	}
	return i.listWalk(ctx)
}

type gitListBudget struct {
	entries int
	bytes   int64
}

func (i *Index) listGit(ctx context.Context) ([]File, bool, int, error) {
	seen := make(map[string]struct{}, min(i.cap, 4096))
	files := make([]File, 0, min(i.cap, 4096))
	budget := gitListBudget{}
	skipped := 0
	consume := func(raw []byte, tracked bool) {
		if len(raw) == 0 || !utf8.Valid(raw) {
			skipped++
			return
		}
		name := filepath.ToSlash(string(raw))
		// Conventional generated trees stay out of the untracked scan, but a
		// path Git already tracks is source and must remain searchable.
		if !tracked && excludedPath(name) {
			skipped++
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		abs, err := i.root.Resolve(name)
		if err != nil {
			skipped++
			return
		}
		info, err := os.Lstat(abs)
		if err != nil || !info.Mode().IsRegular() {
			skipped++
			return
		}
		seen[name] = struct{}{}
		files = append(files, indexedFile(name, info.Size()))
	}

	trackedArgs := []string{"-C", i.root.Path(), "ls-files", "--cached", "-z", "--"}
	truncated, omitted, err := i.streamGitNames(ctx, trackedArgs, &budget, func(raw []byte) {
		consume(raw, true)
	})
	skipped += omitted
	if err != nil {
		return nil, false, 0, err
	}
	if !truncated {
		untrackedArgs := []string{"-C", i.root.Path(), "ls-files", "--others", "--exclude-standard", "-z", "--"}
		truncated, omitted, err = i.streamGitNames(ctx, untrackedArgs, &budget, func(raw []byte) {
			consume(raw, false)
		})
		skipped += omitted
		if err != nil {
			return nil, false, 0, err
		}
	}
	sort.Slice(files, func(a, b int) bool { return files[a].Path < files[b].Path })
	return files, truncated, skipped, nil
}

// streamGitNames consumes Git's NUL-delimited output without first retaining
// it all. The entry and byte budgets cover both tracked and untracked calls.
// Once either budget has evidence of more output, the child is killed and
// reaped; the retained prefix remains a usable, explicitly truncated snapshot.
func (i *Index) streamGitNames(
	ctx context.Context,
	args []string,
	budget *gitListBudget,
	consume func([]byte),
) (truncated bool, skipped int, err error) {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := i.gitCommand(commandCtx, args...)
	if cmd.Env == nil {
		cmd.Env = stableGitEnv()
	} else {
		cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat", "LC_ALL=C")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, 0, err
	}
	if err := cmd.Start(); err != nil {
		return false, 0, err
	}

	reader := bufio.NewReaderSize(stdout, 32<<10)
	var name []byte
	oversizedName := false
	for {
		if err := ctx.Err(); err != nil {
			cancel()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return false, skipped, err
		}
		fragment, readErr := reader.ReadSlice(0)
		if len(fragment) > 0 {
			remaining := i.gitListBytes - budget.bytes
			if remaining < int64(len(fragment)) {
				truncated = true
				break
			}
			budget.bytes += int64(len(fragment))
			if budget.entries >= i.cap {
				truncated = true
				break
			}
			if !oversizedName {
				if len(name)+len(fragment) > defaultGitPathBytes {
					name = nil
					oversizedName = true
				} else {
					name = append(name, fragment...)
				}
			}
		}

		switch {
		case readErr == nil:
			budget.entries++
			if oversizedName {
				skipped++
			} else {
				consume(name[:len(name)-1])
			}
			name = name[:0]
			oversizedName = false
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF):
			if len(name) != 0 || oversizedName {
				cancel()
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return false, skipped, errors.New("git ls-files returned an unterminated path")
			}
			waitErr := cmd.Wait()
			if err := ctx.Err(); err != nil {
				return false, skipped, err
			}
			return false, skipped, waitErr
		default:
			cancel()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return false, skipped, readErr
		}
	}

	cancel()
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if err := ctx.Err(); err != nil {
		return false, skipped, err
	}
	return truncated, skipped, nil
}

func stableGitEnv() []string {
	env := os.Environ()
	env = append(env, "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat", "LC_ALL=C")
	return env
}

func excludedPath(name string) bool {
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if skippedDirectories[part] {
			return true
		}
	}
	return false
}

func (i *Index) listWalk(ctx context.Context) ([]File, bool, int, error) {
	files := make([]File, 0, min(i.cap, 4096))
	truncated := false
	skipped := 0
	err := filepath.WalkDir(i.root.Path(), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			skipped++
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if path != i.root.Path() && skippedDirectories[entry.Name()] {
				skipped++
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			skipped++
			return nil
		}
		if len(files) >= i.cap {
			truncated = true
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(i.root.Path(), path)
		if err != nil {
			skipped++
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			skipped++
			return nil
		}
		files = append(files, indexedFile(filepath.ToSlash(rel), info.Size()))
		return nil
	})
	if err != nil {
		return nil, false, 0, err
	}
	sort.Slice(files, func(a, b int) bool { return files[a].Path < files[b].Path })
	return files, truncated, skipped, nil
}

func indexedFile(path string, size int64) File {
	key := strings.ToLower(path)
	return File{Path: path, Size: size, searchKey: key, baseKey: strings.ToLower(filepath.Base(path))}
}

type SearchOptions struct {
	Limit         int
	MaxFileBytes  int64
	CaseSensitive bool
}

type TextMatch struct {
	Location Location `json:"location"`
	Preview  string   `json:"preview"`
}

type SearchStatus struct {
	Truncated bool `json:"truncated"`
	// Skipped includes index omissions plus binary or disappeared files seen
	// while searching. Oversized is separate because its configured limit is
	// useful evidence to the caller.
	Skipped   int `json:"skipped"`
	Oversized int `json:"oversized"`
}

func (s SearchStatus) Partial() bool {
	return s.Truncated || s.Skipped > 0 || s.Oversized > 0
}

// Search performs a bounded literal search over the current file snapshot.
// It is intended to run in a tea.Cmd, never in Bubble Tea's Update method.
func (i *Index) Search(ctx context.Context, query string, options SearchOptions) ([]TextMatch, SearchStatus, error) {
	if strings.TrimSpace(query) == "" {
		return nil, SearchStatus{}, errors.New("search query is required")
	}
	if options.Limit <= 0 {
		options.Limit = DefaultSearchLimit
	}
	if options.MaxFileBytes <= 0 {
		options.MaxFileBytes = DefaultSearchBytes
	}
	expression := regexp.QuoteMeta(query)
	if !options.CaseSensitive {
		expression = "(?i:" + expression + ")"
	}
	matcher, err := regexp.Compile(expression)
	if err != nil {
		return nil, SearchStatus{}, err
	}
	snapshot, err := i.Ensure(ctx)
	if err != nil {
		return nil, SearchStatus{}, err
	}
	status := SearchStatus{Truncated: snapshot.Truncated, Skipped: snapshot.Skipped}
	for _, file := range snapshot.Files {
		if file.Size > options.MaxFileBytes {
			status.Oversized++
		}
	}

	type result struct {
		matches []TextMatch
		err     error
	}
	workers := min(max(runtime.GOMAXPROCS(0), 2), 8)
	jobs := make(chan File)
	results := make(chan result, workers)
	parentCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for file := range jobs {
				matches, err := i.searchFile(ctx, file, matcher, options)
				select {
				case results <- result{matches: matches, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, file := range snapshot.Files {
			if file.Size > options.MaxFileBytes {
				continue
			}
			select {
			case jobs <- file:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	var matches []TextMatch
	for result := range results {
		switch {
		case result.err == nil:
		case errors.Is(result.err, ErrTooLarge):
			status.Oversized++
		case errors.Is(result.err, ErrBinary), errors.Is(result.err, fs.ErrNotExist):
			status.Skipped++
		case errors.Is(result.err, context.Canceled):
			// Reaching the match cap cancels the remaining workers. Truncated
			// already carries that incomplete-coverage evidence.
		default:
			cancel()
			return nil, SearchStatus{}, result.err
		}
		matches = append(matches, result.matches...)
		if len(matches) >= options.Limit {
			status.Truncated = true
			cancel()
		}
	}
	if err := parentCtx.Err(); err != nil {
		return nil, SearchStatus{}, err
	}
	sort.Slice(matches, func(a, b int) bool {
		if matches[a].Location.Path != matches[b].Location.Path {
			return matches[a].Location.Path < matches[b].Location.Path
		}
		if matches[a].Location.Range.Start.Line != matches[b].Location.Range.Start.Line {
			return matches[a].Location.Range.Start.Line < matches[b].Location.Range.Start.Line
		}
		return matches[a].Location.Range.Start.Column < matches[b].Location.Range.Start.Column
	})
	if len(matches) > options.Limit {
		matches = matches[:options.Limit]
		status.Truncated = true
	}
	return matches, status, nil
}

func (i *Index) searchFile(ctx context.Context, file File, matcher *regexp.Regexp, options SearchOptions) ([]TextMatch, error) {
	if file.Size > options.MaxFileBytes {
		return nil, ErrTooLarge
	}
	doc, err := i.root.Read(file.Path, options.MaxFileBytes)
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReader(bytes.NewReader(doc.Content))
	lineNumber := 0
	var matches []TextMatch
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			lineNumber++
			text := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			remaining := options.Limit - len(matches)
			for _, bounds := range matcher.FindAllStringIndex(text, remaining) {
				byteCol := bounds[0]
				column := utf8.RuneCountInString(text[:byteCol]) + 1
				endColumn := column + utf8.RuneCountInString(text[bounds[0]:bounds[1]])
				loc := doc.Location
				loc.Range = Range{Start: Position{Line: lineNumber, Column: column}, End: Position{Line: lineNumber, Column: endColumn}}
				matches = append(matches, TextMatch{Location: loc, Preview: trimPreview(text, byteCol)})
			}
			if len(matches) >= options.Limit {
				return matches, nil
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	return matches, nil
}

func trimPreview(line string, focus int) string {
	const maxRunes = 240
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\t' {
			return ' '
		}
		return r
	}, line)
	if utf8.RuneCountInString(line) <= maxRunes {
		return line
	}
	runes := []rune(line)
	focusRunes := utf8.RuneCountInString(line[:min(focus, len(line))])
	start := max(focusRunes-maxRunes/3, 0)
	end := min(start+maxRunes, len(runes))
	if end-start < maxRunes {
		start = max(end-maxRunes, 0)
	}
	text := string(runes[start:end])
	if start > 0 {
		text = "…" + text
	}
	if end < len(runes) {
		text += "…"
	}
	return text
}

func (i *Index) String() string {
	snapshot := i.Snapshot()
	return fmt.Sprintf("workspace index generation %d (%d files)", snapshot.Generation, len(snapshot.Files))
}
