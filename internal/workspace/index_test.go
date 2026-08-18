package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIndexRefreshFilterAndSearch(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("cmd/server/main.go", "package main\n// Needle here\n")
	write("internal/parser/parse.go", "package parser\n// needle there\n")
	write("vendor/hidden.go", "needle hidden\n")
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	index := NewIndex(w, 0)
	snapshot, err := index.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Files) != 2 {
		t.Fatalf("files = %+v", snapshot.Files)
	}
	matches := snapshot.Filter("psgo", 10)
	if len(matches) == 0 || matches[0].File.Path != "internal/parser/parse.go" {
		t.Fatalf("fuzzy matches = %+v", matches)
	}
	text, status, err := index.Search(context.Background(), "needle", SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(text) != 2 || !status.Partial() || status.Skipped != 1 {
		t.Fatalf("search = %+v status=%+v", text, status)
	}
	if text[0].Location.Path != "cmd/server/main.go" || text[0].Location.Range.Start.Line != 2 {
		t.Fatalf("first match = %+v", text[0])
	}
}

func TestIndexInvalidateObservesExternalChanges(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.go"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, _ := Open(root)
	index := NewIndex(w, 0)
	first, err := index.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "two.go"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cached, _ := index.Ensure(context.Background()); len(cached.Files) != 1 {
		t.Fatalf("unexpected implicit refresh: %+v", cached.Files)
	}
	index.Invalidate()
	second, err := index.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Files) != 2 || second.Generation <= first.Generation {
		t.Fatalf("refreshed snapshot = %+v (first %+v)", second, first)
	}
}

func TestSearchUnicodeCaseFoldAndRuneColumn(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "unicode.txt"), []byte("🙂 Éclair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, _ := Open(root)
	index := NewIndex(w, 0)
	matches, _, err := index.Search(context.Background(), "éCLAIR", SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Location.Range.Start.Column != 3 {
		t.Fatalf("unicode matches = %+v", matches)
	}
	if err := w.Verify(matches[0].Location); err != nil {
		t.Fatalf("search returned unverifiable location: %v", err)
	}
}

func TestSearchHonorsCancelledContext(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.txt"), []byte("needle"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, _ := Open(root)
	index := NewIndex(w, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := index.Search(ctx, "needle", SearchOptions{}); err != context.Canceled {
		t.Fatalf("cancelled search = %v", err)
	}
}

func TestGitIndexIncludesTrackedConventionalDirectoriesAndReportsSkippedUntrackedOnes(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		"vendor/kept.go":      "needle tracked\n",
		"vendor/untracked.go": "needle untracked\n",
		"root.go":             "package root\n",
		"loose.go":            "package loose\n",
	} {
		writeIndexFile(t, root, name, content)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	index := NewIndex(w, 0)
	index.gitCommand = fakeGitCommand("tracked-vendor", nil)
	snapshot, err := index.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, file := range snapshot.Files {
		paths = append(paths, file.Path)
	}
	if got, want := strings.Join(paths, ","), "loose.go,root.go,vendor/kept.go"; got != want {
		t.Fatalf("indexed paths = %q, want %q", got, want)
	}
	if snapshot.Truncated || snapshot.Skipped != 1 {
		t.Fatalf("snapshot accounting = %+v, want one skipped untracked vendor path", snapshot)
	}
	matches, status, err := index.Search(context.Background(), "needle", SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Location.Path != "vendor/kept.go" {
		t.Fatalf("tracked vendor search = %+v", matches)
	}
	if !status.Partial() || status.Skipped != 1 || status.Oversized != 0 {
		t.Fatalf("tracked vendor search status = %+v", status)
	}
}

func TestGitIndexStreamsAndReapsAtEntryAndByteCaps(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fileLimit int
		byteLimit int64
	}{
		{name: "entries", fileLimit: 2, byteLimit: defaultGitListBytes},
		{name: "bytes", fileLimit: 10, byteLimit: int64(len("one\x00two\x00"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"one", "two", "three"} {
				writeIndexFile(t, root, name, name)
			}
			w, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			index := NewIndex(w, tc.fileLimit)
			index.gitListBytes = tc.byteLimit
			var commands []*exec.Cmd
			index.gitCommand = fakeGitCommand("bounded", &commands)
			started := time.Now()
			snapshot, err := index.Refresh(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(started); elapsed >= 5*time.Second {
				t.Fatalf("bounded git collection took %s; child was not stopped promptly", elapsed)
			}
			if !snapshot.Truncated || len(snapshot.Files) != 2 {
				t.Fatalf("bounded snapshot = %+v", snapshot)
			}
			if len(commands) != 1 || commands[0].ProcessState == nil {
				t.Fatalf("git child was not reaped: commands=%d state=%v", len(commands), processState(commands))
			}
		})
	}
}

func TestSearchReportsOversizedTextAsPartial(t *testing.T) {
	root := t.TempDir()
	large := make([]byte, DefaultSearchBytes+1)
	copy(large, "needle in a large text file")
	if err := os.WriteFile(filepath.Join(root, "large.txt"), large, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	index := NewIndex(w, 0)
	matches, status, err := index.Search(context.Background(), "needle", SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 || !status.Partial() || status.Oversized != 1 || status.Skipped != 0 {
		t.Fatalf("oversized search = %+v status=%+v", matches, status)
	}
}

func writeIndexFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fakeGitCommand(mode string, commands *[]*exec.Cmd) func(context.Context, ...string) *exec.Cmd {
	return func(ctx context.Context, args ...string) *exec.Cmd {
		helperArgs := []string{"-test.run=^TestGitListHelperProcess$", "--"}
		helperArgs = append(helperArgs, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], helperArgs...)
		cmd.Env = append(os.Environ(), "SB_WORKSPACE_GIT_HELPER="+mode)
		if commands != nil {
			*commands = append(*commands, cmd)
		}
		return cmd
	}
}

func TestGitListHelperProcess(t *testing.T) {
	mode := os.Getenv("SB_WORKSPACE_GIT_HELPER")
	if mode == "" {
		return
	}
	tracked := false
	for _, arg := range os.Args {
		if arg == "--cached" {
			tracked = true
			break
		}
	}
	switch mode {
	case "tracked-vendor":
		if tracked {
			_, _ = os.Stdout.Write([]byte("vendor/kept.go\x00root.go\x00"))
		} else {
			_, _ = os.Stdout.Write([]byte("vendor/untracked.go\x00loose.go\x00"))
		}
	case "bounded":
		if tracked {
			_, _ = os.Stdout.Write([]byte("one\x00two\x00three\x00"))
			time.Sleep(30 * time.Second)
		}
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func processState(commands []*exec.Cmd) *os.ProcessState {
	if len(commands) == 0 {
		return nil
	}
	return commands[0].ProcessState
}

func BenchmarkFilterHundredThousandFiles(b *testing.B) {
	files := make([]File, 100_000)
	for i := range files {
		files[i] = indexedFile(filepath.ToSlash(filepath.Join("internal", "package", string(rune('a'+i%26)), "file.go")), 0)
	}
	snapshot := Snapshot{Files: files}
	b.ResetTimer()
	for range b.N {
		_ = snapshot.Filter("ipfg", 50)
	}
}
