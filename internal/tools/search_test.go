package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobBasenamePatternMatchesAnywhere(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "main.go"), "package main\n")
	writeFile(t, filepath.Join(root, "internal", "deep", "x.go"), "package deep\n")
	writeFile(t, filepath.Join(root, "notes.txt"), "notes\n")

	res := run(t, r, "glob", map[string]any{"pattern": "*.go"})
	if res.IsError {
		t.Fatalf("glob failed: %s", res.Content)
	}
	for _, want := range []string{"main.go", filepath.Join("internal", "deep", "x.go")} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("missing %s in %q", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "notes.txt") {
		t.Errorf("notes.txt should not match *.go: %q", res.Content)
	}
}

func TestGlobPathPatternWithDoublestar(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "a_test.go"), "package a\n")
	writeFile(t, filepath.Join(root, "internal", "pkg", "b_test.go"), "package pkg\n")
	writeFile(t, filepath.Join(root, "internal", "pkg", "b.go"), "package pkg\n")

	res := run(t, r, "glob", map[string]any{"pattern": "internal/**/*_test.go"})
	if res.IsError {
		t.Fatalf("glob failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, filepath.Join("internal", "pkg", "b_test.go")) {
		t.Errorf("doublestar missed nested test file: %q", res.Content)
	}
	// A path pattern is anchored: the root-level test file is outside internal/.
	if strings.Contains(res.Content, "a_test.go\n") || strings.HasPrefix(res.Content, "a_test.go") {
		t.Errorf("anchored pattern matched outside its prefix: %q", res.Content)
	}
	if strings.Contains(res.Content, "b.go\n") {
		t.Errorf("matched a non-test file: %q", res.Content)
	}
}

func TestGlobSkipsGitAndReportsEmpty(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, ".git", "config.go"), "not really go\n")

	res := run(t, r, "glob", map[string]any{"pattern": "*.go"})
	if res.IsError {
		t.Fatalf("glob failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "no files match") {
		t.Errorf("want the no-match message, got %q", res.Content)
	}
}

func TestGlobRejectsBadPatternAndOutsidePath(t *testing.T) {
	r, _ := newRegistry(t)

	if _, err := tryRun(r, "glob", map[string]any{"pattern": "[unclosed"}); err == nil {
		t.Error("malformed pattern must fail at Plan time")
	}
	if _, err := tryRun(r, "glob", map[string]any{"pattern": "*.go", "path": "../elsewhere"}); err == nil {
		t.Error("a path outside the workspace must be refused")
	}
}

func TestGlobTruncatesAtTheCap(t *testing.T) {
	r, root := newRegistry(t)
	for i := 0; i < maxGlobResults+50; i++ {
		writeFile(t, filepath.Join(root, fmt.Sprintf("f%04d.txt", i)), "x")
	}

	res := run(t, r, "glob", map[string]any{"pattern": "*.txt"})
	if res.IsError {
		t.Fatalf("glob failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, fmt.Sprintf("first %d matches", maxGlobResults)) {
		t.Errorf("an over-cap result must say it truncated: %q", res.Content[len(res.Content)-200:])
	}
	if got := strings.Count(res.Content, "\n") + 1; got > maxGlobResults+1 {
		t.Errorf("returned %d lines, cap is %d plus the notice", got, maxGlobResults)
	}
}

func TestGrepContentModeReturnsPathLineText(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "a.go"), "package a\n\nfunc Hello() {}\n")
	writeFile(t, filepath.Join(root, "sub", "b.go"), "package b\n\nfunc hello() {}\n")

	res := run(t, r, "grep", map[string]any{"pattern": "func Hello"})
	if res.IsError {
		t.Fatalf("grep failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.go:3: func Hello() {}") {
		t.Errorf("want path:line: text, got %q", res.Content)
	}
	if strings.Contains(res.Content, "b.go") {
		t.Errorf("case-sensitive search matched the wrong case: %q", res.Content)
	}
}

func TestGrepIgnoreCase(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "a.go"), "// TODO: later\n// todo: sooner\n")

	res := run(t, r, "grep", map[string]any{"pattern": "todo", "ignore_case": true})
	if res.IsError {
		t.Fatalf("grep failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.go:1:") || !strings.Contains(res.Content, "a.go:2:") {
		t.Errorf("ignore_case must match both lines: %q", res.Content)
	}
}

func TestGrepFilesModeCountsMatches(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "a.txt"), "x\nx\nx\n")
	writeFile(t, filepath.Join(root, "b.txt"), "x\n")

	res := run(t, r, "grep", map[string]any{"pattern": "x", "mode": "files"})
	if res.IsError {
		t.Fatalf("grep failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.txt (3)") || !strings.Contains(res.Content, "b.txt (1)") {
		t.Errorf("files mode must list files with counts: %q", res.Content)
	}
	if strings.Contains(res.Content, ":1:") {
		t.Errorf("files mode must not include line content: %q", res.Content)
	}
}

func TestGrepGlobFilterAndSingleFile(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "a.go"), "match\n")
	writeFile(t, filepath.Join(root, "a.md"), "match\n")

	res := run(t, r, "grep", map[string]any{"pattern": "match", "glob": "*.go"})
	if res.IsError {
		t.Fatalf("grep failed: %s", res.Content)
	}
	if strings.Contains(res.Content, "a.md") {
		t.Errorf("glob filter leaked a non-matching file: %q", res.Content)
	}

	res = run(t, r, "grep", map[string]any{"pattern": "match", "path": "a.md"})
	if res.IsError {
		t.Fatalf("grep on one file failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.md:1: match") {
		t.Errorf("single-file search missed: %q", res.Content)
	}
}

func TestGrepSkipsBinaryFiles(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "text.txt"), "needle\n")
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), []byte("needle\x00needle"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := run(t, r, "grep", map[string]any{"pattern": "needle"})
	if res.IsError {
		t.Fatalf("grep failed: %s", res.Content)
	}
	if strings.Contains(res.Content, "blob.bin") {
		t.Errorf("a file with NUL bytes must be skipped: %q", res.Content)
	}
	if !strings.Contains(res.Content, "text.txt:1: needle") {
		t.Errorf("the text file must still match: %q", res.Content)
	}
}

func TestGrepRejectsBadRegexAndBadMode(t *testing.T) {
	r, _ := newRegistry(t)

	if _, err := tryRun(r, "grep", map[string]any{"pattern": "("}); err == nil {
		t.Error("an invalid regex must fail at Plan time")
	}
	if _, err := tryRun(r, "grep", map[string]any{"pattern": "x", "mode": "lines"}); err == nil {
		t.Error("an unknown mode must fail at Plan time")
	}
}

func TestGrepTruncatesAtTheMatchCap(t *testing.T) {
	r, root := newRegistry(t)
	var b strings.Builder
	for i := 0; i < maxGrepMatches+100; i++ {
		b.WriteString("needle\n")
	}
	writeFile(t, filepath.Join(root, "big.txt"), b.String())

	res := run(t, r, "grep", map[string]any{"pattern": "needle"})
	if res.IsError {
		t.Fatalf("grep failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, fmt.Sprintf("first %d matching lines", maxGrepMatches)) {
		t.Errorf("an over-cap result must say it truncated: %q", res.Content[len(res.Content)-200:])
	}
}

func TestGrepReportsNoMatches(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "a.txt"), "hay\n")

	res := run(t, r, "grep", map[string]any{"pattern": "needle"})
	if res.IsError {
		t.Fatalf("grep failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "no matches") {
		t.Errorf("want the no-match message, got %q", res.Content)
	}
}

func TestSearchSkipsSymlinkedFiles(t *testing.T) {
	r, root := newRegistry(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "needle\n")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	res := run(t, r, "grep", map[string]any{"pattern": "needle"})
	if res.IsError {
		t.Fatalf("grep failed: %s", res.Content)
	}
	if strings.Contains(res.Content, "link.txt") {
		t.Errorf("a symlinked file is a door out of the workspace: %q", res.Content)
	}

	res = run(t, r, "glob", map[string]any{"pattern": "*.txt"})
	if strings.Contains(res.Content, "link.txt") {
		t.Errorf("glob must not list symlinked files: %q", res.Content)
	}
}

func TestMatchGlobSemantics(t *testing.T) {
	cases := []struct {
		pattern, rel string
		want         bool
	}{
		{"*.go", "deep/nested/x.go", true},
		{"*.go", "x.txt", false},
		{"**/*.go", "x.go", true},
		{"**/*.go", "a/b/x.go", true},
		{"internal/**/*_test.go", "internal/x_test.go", true},
		{"internal/**/*_test.go", "internal/a/b/x_test.go", true},
		{"internal/**/*_test.go", "cmd/x_test.go", false},
		{"a/*.go", "a/x.go", true},
		{"a/*.go", "a/b/x.go", false},
	}
	for _, c := range cases {
		got, err := matchGlob(c.pattern, c.rel)
		if err != nil {
			t.Errorf("matchGlob(%q, %q): %v", c.pattern, c.rel, err)
			continue
		}
		if got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.rel, got, c.want)
		}
	}
}
