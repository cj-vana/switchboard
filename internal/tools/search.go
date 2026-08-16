package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/cj-vana/switchboard/internal/permission"
)

// The caps below bound what a search can put into the context. A search that
// returns everything it found is a search that evicts the conversation that
// asked for it, so over-large results truncate with a note saying how to
// narrow, and the note is part of the contract rather than an apology.
const (
	maxGlobResults  = 500
	maxGrepMatches  = 200
	maxGrepOutput   = 64 << 10
	maxGrepFileSize = 4 << 20
	maxGrepLine     = 500
)

// walkFiles visits every regular file under base in lexical order, skipping
// .git and anything reachable only through a symlink. Symlinked directories
// are not traversed and symlinked files are not visited, because a link is a
// door out of the workspace and resolve-per-file would price every search by
// its slowest lstat. The read tool remains the way through a link, and it
// checks the boundary properly.
func (r *Registry) walkFiles(ctx context.Context, base string, visit func(abs string, d fs.DirEntry) error) error {
	return filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that vanished or refused permission mid-walk is not
			// the search's problem; skip it and report what was readable.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return visit(p, d)
	})
}

// matchGlob reports whether a slash-separated relative path matches pattern.
// Segments match with path.Match; a segment of ** matches any number of
// segments, including none. A pattern with no slash matches the base name
// anywhere in the tree, because that is what every caller who writes *.go
// means, and making them write **/*.go teaches nothing.
func matchGlob(pattern, rel string) (bool, error) {
	if !strings.Contains(pattern, "/") {
		return path.Match(pattern, path.Base(rel))
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(rel, "/"))
}

func matchSegments(pat, segs []string) (bool, error) {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				ok, err := matchSegments(pat[1:], segs[i:])
				if ok || err != nil {
					return ok, err
				}
			}
			return false, nil
		}
		if len(segs) == 0 {
			return false, nil
		}
		ok, err := path.Match(pat[0], segs[0])
		if !ok || err != nil {
			return false, err
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0, nil
}

// checkGlob validates a pattern at Plan time, so a malformed one is reported
// as a bad call instead of surfacing as zero matches.
func checkGlob(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("pattern is required")
	}
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, "x"); err != nil {
			return fmt.Errorf("bad pattern %q: %w", pattern, err)
		}
	}
	return nil
}

type globTool struct{ r *Registry }

func (t *globTool) Name() string { return "glob" }

func (t *globTool) Description() string {
	return "Find workspace files by name. A pattern without a slash matches file names " +
		"anywhere: *.go finds every Go file. A pattern with a slash matches the whole " +
		"workspace-relative path, with ** crossing directories: internal/**/*_test.go. " +
		"Results are sorted paths. Scope with path to search one directory."
}

func (t *globTool) ParallelSafe() bool { return true }

func (t *globTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Glob pattern. Without a slash it matches file names anywhere; with a slash it matches the path relative to the searched directory, and ** matches across directories."},
    "path": {"type": "string", "description": "Directory to search, relative to the workspace root. Defaults to the whole workspace."}
  },
  "required": ["pattern"]
}`)
}

type globInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

func (t *globTool) Plan(input json.RawMessage) (Plan, error) {
	var in globInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("glob: %w", err)
	}
	if err := checkGlob(in.Pattern); err != nil {
		return Plan{}, fmt.Errorf("glob: %w", err)
	}
	base := in.Path
	if base == "" {
		base = "."
	}
	abs, err := t.r.resolve(base)
	if err != nil {
		return Plan{}, err
	}

	return Plan{
		Request: permission.Request{Tool: t.Name(), Effect: permission.EffectRead, Path: t.r.display(abs)},
		Run: func(ctx context.Context) (Result, error) {
			return t.glob(ctx, abs, in.Pattern)
		},
	}, nil
}

func (t *globTool) glob(ctx context.Context, base, pattern string) (Result, error) {
	if info, err := os.Stat(base); err != nil || !info.IsDir() {
		return errorf("%s is not a searchable directory", t.r.display(base))
	}

	var matches []string
	err := t.r.walkFiles(ctx, base, func(abs string, _ fs.DirEntry) error {
		rel, err := filepath.Rel(base, abs)
		if err != nil {
			return nil
		}
		ok, err := matchGlob(pattern, filepath.ToSlash(rel))
		if err != nil || !ok {
			return err
		}
		matches = append(matches, t.r.display(abs))
		return nil
	})
	if err != nil {
		return Result{}, err
	}

	if len(matches) == 0 {
		return Result{Content: fmt.Sprintf("no files match %s under %s", pattern, t.r.display(base))}, nil
	}
	sort.Strings(matches)

	truncated := false
	if len(matches) > maxGlobResults {
		matches = matches[:maxGlobResults]
		truncated = true
	}
	var b strings.Builder
	b.WriteString(strings.Join(matches, "\n"))
	if truncated {
		fmt.Fprintf(&b, "\n[first %d matches; narrow the pattern or set path]", maxGlobResults)
	}
	return Result{Content: b.String()}, nil
}

type grepTool struct{ r *Registry }

func (t *grepTool) Name() string { return "grep" }

func (t *grepTool) Description() string {
	return "Search file contents with a regular expression (Go RE2 syntax). Returns " +
		"matching lines as path:line: text; mode \"files\" lists only the files that " +
		"match. Scope with path (a directory or one file) and glob (a file name " +
		"pattern). Binary files and .git are skipped."
}

func (t *grepTool) ParallelSafe() bool { return true }

func (t *grepTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Regular expression, Go RE2 syntax."},
    "path": {"type": "string", "description": "Directory or file to search, relative to the workspace root. Defaults to the whole workspace."},
    "glob": {"type": "string", "description": "Only search files matching this glob, e.g. *.go or src/**/*.ts."},
    "ignore_case": {"type": "boolean", "description": "Match case-insensitively."},
    "mode": {"type": "string", "enum": ["content", "files"], "description": "content (default) returns matching lines; files returns one line per matching file with its match count."}
  },
  "required": ["pattern"]
}`)
}

type grepInput struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	IgnoreCase bool   `json:"ignore_case"`
	Mode       string `json:"mode"`
}

func (t *grepTool) Plan(input json.RawMessage) (Plan, error) {
	var in grepInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("grep: %w", err)
	}
	if in.Pattern == "" {
		return Plan{}, fmt.Errorf("grep: pattern is required")
	}
	expr := in.Pattern
	if in.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return Plan{}, fmt.Errorf("grep: %w", err)
	}
	if in.Glob != "" {
		if err := checkGlob(in.Glob); err != nil {
			return Plan{}, fmt.Errorf("grep: %w", err)
		}
	}
	switch in.Mode {
	case "", "content", "files":
	default:
		return Plan{}, fmt.Errorf("grep: mode must be content or files, not %q", in.Mode)
	}
	base := in.Path
	if base == "" {
		base = "."
	}
	abs, err := t.r.resolve(base)
	if err != nil {
		return Plan{}, err
	}

	return Plan{
		Request: permission.Request{Tool: t.Name(), Effect: permission.EffectRead, Path: t.r.display(abs)},
		Run: func(ctx context.Context) (Result, error) {
			return t.grep(ctx, abs, re, in)
		},
	}, nil
}

// grepHit is one file's outcome, kept so the files mode can report counts
// without a second pass.
type grepHit struct {
	display string
	count   int
	lines   []string
}

func (t *grepTool) grep(ctx context.Context, base string, re *regexp.Regexp, in grepInput) (Result, error) {
	info, err := os.Stat(base)
	if err != nil {
		return errorf("cannot search %s: %v", t.r.display(base), err)
	}

	var hits []grepHit
	total := 0
	budget := maxGrepMatches

	scanOne := func(abs string) {
		if budget <= 0 {
			return
		}
		hit := t.scanFile(abs, re, budget)
		if hit.count == 0 {
			return
		}
		total += hit.count
		budget -= len(hit.lines)
		hits = append(hits, hit)
	}

	if info.IsDir() {
		err = t.r.walkFiles(ctx, base, func(abs string, _ fs.DirEntry) error {
			if in.Glob != "" {
				rel, err := filepath.Rel(base, abs)
				if err != nil {
					return nil
				}
				ok, err := matchGlob(in.Glob, filepath.ToSlash(rel))
				if err != nil || !ok {
					return err
				}
			}
			scanOne(abs)
			return nil
		})
		if err != nil {
			return Result{}, err
		}
	} else {
		scanOne(base)
	}

	if len(hits) == 0 {
		return Result{Content: fmt.Sprintf("no matches for %s in %s", in.Pattern, t.r.display(base))}, nil
	}

	var b strings.Builder
	if in.Mode == "files" {
		for _, h := range hits {
			fmt.Fprintf(&b, "%s (%d)\n", h.display, h.count)
		}
	} else {
		for _, h := range hits {
			for _, line := range h.lines {
				b.WriteString(line)
				b.WriteByte('\n')
				if b.Len() >= maxGrepOutput {
					b.WriteString("[output limit reached; narrow the pattern, set path, or use mode files]\n")
					return Result{Content: b.String()}, nil
				}
			}
		}
	}
	if budget <= 0 {
		fmt.Fprintf(&b, "[first %d matching lines; narrow the pattern, set path, or use mode files]\n", maxGrepMatches)
	}
	return Result{Content: strings.TrimRight(b.String(), "\n")}, nil
}

// scanFile reports up to lineBudget matching lines from one file. Binary
// files and files over the size cap count as clean rather than as errors,
// because a tree search that stops on the first unreadable object never
// finishes, and the model cannot act on "skipped" any better than on silence.
func (t *grepTool) scanFile(abs string, re *regexp.Regexp, lineBudget int) grepHit {
	info, err := os.Stat(abs)
	if err != nil || info.Size() > maxGrepFileSize {
		return grepHit{}
	}
	f, err := os.Open(abs)
	if err != nil {
		return grepHit{}
	}
	defer f.Close()

	head := make([]byte, 8<<10)
	n, _ := f.Read(head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return grepHit{}
	}
	if _, err := f.Seek(0, 0); err != nil {
		return grepHit{}
	}

	hit := grepHit{display: t.r.display(abs)}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	lineno := 0
	for sc.Scan() {
		lineno++
		line := sc.Text()
		if !re.MatchString(line) {
			continue
		}
		hit.count++
		if len(hit.lines) >= lineBudget {
			continue
		}
		if len(line) > maxGrepLine {
			line = line[:maxGrepLine] + "…"
		}
		hit.lines = append(hit.lines, fmt.Sprintf("%s:%d: %s", hit.display, lineno, line))
	}
	// A scanner error means a line outlonged the buffer; what matched before
	// it stands, and the rest of the file is not text worth reporting.
	return hit
}
