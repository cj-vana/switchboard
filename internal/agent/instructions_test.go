package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// repoTree builds a checkout with a root and a package directory, and points
// HOME somewhere empty so a developer's real files cannot change the result.
func repoTree(t *testing.T) (root, pkg string) {
	t.Helper()
	root = t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	pkg = filepath.Join(root, "services", "api")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	return root, pkg
}

// A monorepo states house rules at the root and specifics in a package, and
// both are meant.
func TestInstructionsComposeFromTheRepositoryRootDown(t *testing.T) {
	root, pkg := repoTree(t)
	writeFile(t, filepath.Join(root, "AGENTS.md"), "House rule: run gofmt.")
	writeFile(t, filepath.Join(pkg, "AGENTS.md"), "This package: never touch generated.go.")

	text, ok := ProjectInstructions(pkg)
	if !ok {
		t.Fatal("no instructions were found")
	}
	if !strings.Contains(text, "run gofmt") {
		t.Error("the repository root's rules were not read")
	}
	if !strings.Contains(text, "never touch generated.go") {
		t.Error("the package's own rules were not read")
	}
	// The last word belongs to the file closest to the work.
	if strings.Index(text, "run gofmt") > strings.Index(text, "never touch generated.go") {
		t.Error("the general layer came after the specific one")
	}
}

// A developer shadows a checked-in file without editing it.
func TestAnUncommittedOverrideIsReadAfterTheFileItShadows(t *testing.T) {
	root, _ := repoTree(t)
	writeFile(t, filepath.Join(root, "AGENTS.md"), "Committed rule.")
	writeFile(t, filepath.Join(root, "AGENTS.override.md"), "My local rule.")

	text, ok := ProjectInstructions(root)
	if !ok {
		t.Fatal("no instructions were found")
	}
	if strings.Index(text, "Committed rule") > strings.Index(text, "My local rule") {
		t.Error("the override was read before the file it shadows")
	}
}

// A directory holding both means them as one set, and reading both would
// double whatever they agree on.
func TestOnlyOneInstructionFilePerDirectory(t *testing.T) {
	root, _ := repoTree(t)
	writeFile(t, filepath.Join(root, "AGENTS.md"), "the agents file")
	writeFile(t, filepath.Join(root, "CLAUDE.md"), "the claude file")

	text, _ := ProjectInstructions(root)
	if strings.Contains(text, "the claude file") {
		t.Error("both files in one directory were read")
	}
	if !strings.Contains(text, "the agents file") {
		t.Error("the first-listed file was not the one read")
	}
}

// A person who already keeps standing instructions for another tool means
// them here too.
func TestTheUsersOwnInstructionsAreReadFirst(t *testing.T) {
	root, _ := repoTree(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, filepath.Join(home, ".claude", "CLAUDE.md"), "Always explain the why.")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "Project rule.")

	text, ok := ProjectInstructions(root)
	if !ok {
		t.Fatal("no instructions were found")
	}
	if !strings.Contains(text, "Always explain the why") {
		t.Fatal("the user's own instructions were not read")
	}
	if strings.Index(text, "Always explain the why") > strings.Index(text, "Project rule") {
		t.Error("the user's defaults came after the project's rules")
	}
}

// A whole-line @path pulls a file in; a mention inside a sentence does not.
func TestAWholeLineImportIsExpandedAndAMentionIsNot(t *testing.T) {
	root, _ := repoTree(t)
	writeFile(t, filepath.Join(root, "shared.md"), "The shared section.")
	writeFile(t, filepath.Join(root, "AGENTS.md"),
		"Top matter.\n@shared.md\nWrite to support@example.com when stuck.")

	text, _ := ProjectInstructions(root)
	if !strings.Contains(text, "The shared section") {
		t.Error("a whole-line import was not expanded")
	}
	if !strings.Contains(text, "support@example.com") {
		t.Error("an address in a sentence was treated as an import")
	}
}

// An instruction file that can read any path is a file that can read a private
// key into a prompt.
func TestAnImportOutsideTheWorkspaceIsRefusedAndNamed(t *testing.T) {
	root, _ := repoTree(t)
	outside := filepath.Join(t.TempDir(), "secrets.md")
	writeFile(t, outside, "the private thing")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "Rules.\n@"+outside)

	text, _ := ProjectInstructions(root)
	if strings.Contains(text, "the private thing") {
		t.Fatal("an import escaped the workspace")
	}
	if !strings.Contains(text, "not imported") {
		t.Error("the refusal was silent")
	}
}

// A file that imports itself is a loop, and a loop is named rather than run.
func TestAnImportCycleIsRefused(t *testing.T) {
	root, _ := repoTree(t)
	writeFile(t, filepath.Join(root, "a.md"), "A says:\n@AGENTS.md")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "Root says:\n@a.md")

	done := make(chan string, 1)
	go func() {
		text, _ := ProjectInstructions(root)
		done <- text
	}()
	select {
	case text := <-done:
		if !strings.Contains(text, "not imported") {
			t.Errorf("a cycle produced no refusal: %s", text)
		}
	case <-t.Context().Done():
		t.Fatal("the import walk did not terminate")
	}
}

// The old reader sliced bytes and could hand the model half a character.
func TestTruncationCutsOnARuneAndPrefersALine(t *testing.T) {
	if got := truncateInstruction("héllo", 2); !isValidUTF8(got) {
		t.Errorf("truncation produced invalid UTF-8: %q", got)
	}
	if got := truncateInstruction("first line\nsecond line", 15); got != "first line" {
		t.Errorf("truncation = %q, want the whole first line", got)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// A rule that did not arrive is a rule the model will be judged against anyway.
func TestTheBudgetKeepsTheSpecificLayerAndNamesWhatItDropped(t *testing.T) {
	root, pkg := repoTree(t)
	writeFile(t, filepath.Join(root, "AGENTS.md"), strings.Repeat("general filler line\n", 2000))
	writeFile(t, filepath.Join(pkg, "AGENTS.md"), "The package rule that matters.")

	text, ok := ProjectInstructions(pkg)
	if !ok {
		t.Fatal("no instructions were found")
	}
	if !strings.Contains(text, "The package rule that matters") {
		t.Error("the budget dropped the layer closest to the work")
	}
	if len(text) > maxInstructionBytes*2 {
		t.Errorf("composed instructions are %d bytes, far past the %d budget", len(text), maxInstructionBytes)
	}
	if !strings.Contains(text, "budget") {
		t.Error("the truncation was silent")
	}
}

// A directory that is not a checkout has one layer, which is the honest answer.
func TestADirectoryWithNoRepositoryHasOneLayer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "Just here.")

	text, ok := ProjectInstructions(dir)
	if !ok {
		t.Fatal("no instructions were found")
	}
	if !strings.Contains(text, "Just here") {
		t.Error("the only file was not read")
	}
}

// Nothing anywhere is not an empty block: the caller uses this to decide
// whether the system prompt grows at all.
func TestNoInstructionsAnywhereReportsAbsence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	if _, ok := ProjectInstructions(dir); ok {
		t.Error("an empty tree produced instructions")
	}
}
