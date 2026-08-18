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
	DefaultFileLimit   = 200_000
	DefaultSearchLimit = 500
	DefaultSearchBytes = 4 << 20
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

	mu       sync.RWMutex
	snapshot Snapshot
	dirty    bool
	next     atomic.Uint64
}

func NewIndex(root *Root, fileLimit int) *Index {
	if fileLimit <= 0 {
		fileLimit = DefaultFileLimit
	}
	return &Index{root: root, cap: fileLimit, dirty: true}
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
	files, truncated, err := i.list(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{Generation: i.next.Add(1), Files: files, Truncated: truncated}
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

func (i *Index) list(ctx context.Context) ([]File, bool, error) {
	if files, truncated, err := i.listGit(ctx); err == nil {
		return files, truncated, nil
	}
	return i.listWalk(ctx)
}

func (i *Index) listGit(ctx context.Context) ([]File, bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", i.root.Path(), "ls-files", "-co", "--exclude-standard", "-z", "--")
	cmd.Env = stableGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, false, err
	}
	names := bytes.Split(out, []byte{0})
	seen := make(map[string]struct{}, min(len(names), i.cap))
	files := make([]File, 0, min(len(names), i.cap))
	truncated := false
	for _, raw := range names {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if len(raw) == 0 || !utf8.Valid(raw) {
			continue
		}
		name := filepath.ToSlash(string(raw))
		if excludedPath(name) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		abs, err := i.root.Resolve(name)
		if err != nil {
			continue
		}
		info, err := os.Lstat(abs)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if len(files) >= i.cap {
			truncated = true
			break
		}
		seen[name] = struct{}{}
		files = append(files, indexedFile(name, info.Size()))
	}
	sort.Slice(files, func(a, b int) bool { return files[a].Path < files[b].Path })
	return files, truncated, nil
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

func (i *Index) listWalk(ctx context.Context) ([]File, bool, error) {
	files := make([]File, 0, min(i.cap, 4096))
	truncated := false
	err := filepath.WalkDir(i.root.Path(), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
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
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil
		}
		if len(files) >= i.cap {
			truncated = true
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(i.root.Path(), path)
		if err != nil {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		files = append(files, indexedFile(filepath.ToSlash(rel), info.Size()))
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	sort.Slice(files, func(a, b int) bool { return files[a].Path < files[b].Path })
	return files, truncated, nil
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

// Search performs a bounded literal search over the current file snapshot.
// It is intended to run in a tea.Cmd, never in Bubble Tea's Update method.
func (i *Index) Search(ctx context.Context, query string, options SearchOptions) ([]TextMatch, bool, error) {
	if strings.TrimSpace(query) == "" {
		return nil, false, errors.New("search query is required")
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
		return nil, false, err
	}
	snapshot, err := i.Ensure(ctx)
	if err != nil {
		return nil, false, err
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
	truncated := false
	for result := range results {
		if result.err != nil && !errors.Is(result.err, ErrBinary) && !errors.Is(result.err, ErrTooLarge) && !errors.Is(result.err, fs.ErrNotExist) && !errors.Is(result.err, context.Canceled) {
			cancel()
			return nil, false, result.err
		}
		matches = append(matches, result.matches...)
		if len(matches) >= options.Limit {
			truncated = true
			cancel()
		}
	}
	if err := parentCtx.Err(); err != nil {
		return nil, false, err
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
		truncated = true
	}
	return matches, truncated || snapshot.Truncated, nil
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
