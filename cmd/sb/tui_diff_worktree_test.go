package main

import (
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/scm"
)

func TestOpenDiffIncludesOnlyUntrackedFile(t *testing.T) {
	root := initTUIDiffRepo(t)
	writeTUIDiffFile(t, root, "base.txt", []byte("base\n"))
	commitTUIDiffFiles(t, root)
	writeTUIDiffFile(t, root, "notes.txt", []byte("untracked text\n"))

	msg := runOpenDiff(t, root)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	text := plainTUIDiff(msg)
	if strings.Contains(text, "working tree clean") {
		t.Fatalf("an untracked-only worktree was reported clean:\n%s", text)
	}
	for _, want := range []string{"diff --git", "notes.txt", "+untracked text"} {
		if !strings.Contains(text, want) {
			t.Fatalf("untracked diff is missing %q:\n%s", want, text)
		}
	}
}

func TestOpenDiffIncludesMixedChangesAndScopesNestedWorkspace(t *testing.T) {
	root := initTUIDiffRepo(t)
	writeTUIDiffFile(t, root, "inside/mixed.txt", []byte("base\n"))
	writeTUIDiffFile(t, root, "outside.txt", []byte("outside base\n"))
	commitTUIDiffFiles(t, root)

	writeTUIDiffFile(t, root, "inside/mixed.txt", []byte("staged value\n"))
	runTUIDiffGit(t, root, "add", "--", "inside/mixed.txt")
	writeTUIDiffFile(t, root, "inside/mixed.txt", []byte("worktree value\n"))
	writeTUIDiffFile(t, root, "inside/notes.txt", []byte("notes value\n"))
	writeTUIDiffFile(t, root, "inside/image.bin", []byte{0, 1, 2, 3, 0xff, 0xfe})
	writeTUIDiffFile(t, root, "outside.txt", []byte("must stay out\n"))
	wantIndex := tuiDiffIndexChecksum(t, root)

	msg := runOpenDiff(t, filepath.Join(root, "inside"))
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	if got := tuiDiffIndexChecksum(t, root); got != wantIndex {
		t.Fatalf("/diff changed the Git index: got %x, want %x", got, wantIndex)
	}
	text := plainTUIDiff(msg)
	for _, want := range []string{
		"inside/mixed.txt",
		"+worktree value",
		"inside/notes.txt",
		"+notes value",
		"inside/image.bin",
		"GIT binary patch",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("mixed diff is missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"staged value", "outside.txt", "must stay out"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("nested workspace diff unexpectedly contains %q:\n%s", unwanted, text)
		}
	}
}

func TestOpenDiffReportsCleanWorktree(t *testing.T) {
	root := initTUIDiffRepo(t)
	writeTUIDiffFile(t, root, "clean.txt", []byte("clean\n"))
	commitTUIDiffFiles(t, root)

	msg := runOpenDiff(t, root)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	if got := strings.TrimSpace(plainTUIDiff(msg)); got != "working tree clean" {
		t.Fatalf("clean diff = %q", got)
	}
}

func TestOpenDiffNonRepositoryRetainsGitDiagnostic(t *testing.T) {
	msg := runOpenDiff(t, t.TempDir())
	if msg.err == nil {
		t.Fatal("non-repository diff unexpectedly succeeded")
	}
	got := strings.ToLower(msg.err.Error())
	for _, want := range []string{"not a git worktree", "not a git repository"} {
		if !strings.Contains(got, want) {
			t.Fatalf("non-repository error %q does not contain %q", msg.err, want)
		}
	}
}

func TestRenderSCMDiffMarksTruncationAndNonTextChanges(t *testing.T) {
	truncated := renderSCMDiff(scm.DiffResult{Text: []byte("partial"), Truncated: true})
	if !strings.Contains(truncated, "diff truncated at 1 MiB") {
		t.Fatalf("truncated diff has no explicit marker: %q", truncated)
	}

	withoutPatch := renderSCMDiff(scm.DiffResult{Files: []scm.PathState{{
		Path:      "empty.txt",
		Untracked: true,
	}}})
	if strings.Contains(withoutPatch, "working tree clean") || !strings.Contains(withoutPatch, "untracked  empty.txt") {
		t.Fatalf("non-text change was rendered dishonestly: %q", withoutPatch)
	}
}

func runOpenDiff(t *testing.T, workspace string) diffLoadedMsg {
	t.Helper()
	cmd := openDiff(workspace, false)
	if cmd == nil {
		t.Fatal("openDiff returned no command")
	}
	msg, ok := cmd().(diffLoadedMsg)
	if !ok {
		t.Fatalf("openDiff returned %T, want diffLoadedMsg", msg)
	}
	return msg
}

func plainTUIDiff(msg diffLoadedMsg) string {
	return stripANSI(strings.Join(msg.lines, "\n"))
}

func initTUIDiffRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	runTUIDiffGit(t, root, "init", "--quiet")
	runTUIDiffGit(t, root, "symbolic-ref", "HEAD", "refs/heads/main")
	runTUIDiffGit(t, root, "config", "user.name", "Switchboard Test")
	runTUIDiffGit(t, root, "config", "user.email", "switchboard@example.invalid")
	return root
}

func runTUIDiffGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeTUIDiffFile(t *testing.T, root, name string, content []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitTUIDiffFiles(t *testing.T, root string) {
	t.Helper()
	runTUIDiffGit(t, root, "add", "--all")
	runTUIDiffGit(t, root, "commit", "--quiet", "-m", "fixture")
}

func tuiDiffIndexChecksum(t *testing.T, root string) [sha256.Size]byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}
