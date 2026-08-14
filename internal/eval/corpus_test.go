package eval

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// The corpus is pinned to this repository's source. A spec whose text no longer
// matches is a task that would be handed out already solved, and a harness that
// reports a solve rate over already-solved tasks is worse than no harness.
func TestEverySpecStillMatchesTheSource(t *testing.T) {
	root := repoRoot(t)

	for _, s := range specs {
		t.Run(s.id, func(t *testing.T) {
			for _, b := range s.breaks {
				body, err := os.ReadFile(filepath.Join(root, b.file))
				if err != nil {
					t.Fatalf("%s: %v", b.file, err)
				}
				if !strings.Contains(string(body), b.old) {
					t.Errorf("%s no longer contains the text this task breaks:\n%q", b.file, b.old)
				}
				if strings.Count(string(body), b.old) > 1 {
					// Replace(…, 1) would break an arbitrary one of them, so the
					// task would not be the one it claims to be.
					t.Errorf("%s contains the broken text %d times, so the edit is ambiguous",
						b.file, strings.Count(string(body), b.old))
				}
			}
			for path, want := range s.mustContain {
				body, err := os.ReadFile(filepath.Join(root, path))
				if err != nil {
					t.Fatalf("%s: %v", path, err)
				}
				if !strings.Contains(string(body), want) {
					t.Errorf("%s does not contain the property this task re-checks: %q", path, want)
				}
			}
		})
	}
}

// §8.6 sets the floor at twenty to thirty hand-written tasks, and the gate
// refuses below it.
func TestTheCorpusMeetsTheFloor(t *testing.T) {
	tasks := Tier1(repoRoot(t))
	if len(tasks) < MinimumTier1Tasks {
		t.Errorf("the corpus has %d tasks and the gate needs %d", len(tasks), MinimumTier1Tasks)
	}
	seen := map[string]bool{}
	for _, task := range tasks {
		if task.Provenance != HandWritten {
			t.Errorf("%s is not tier 1", task.ID)
		}
		if seen[task.ID] {
			t.Errorf("duplicate task id %q", task.ID)
		}
		seen[task.ID] = true
	}
}

// Every task must break its package and be restored by the obvious fix. A task
// that passes before it is attempted measures nothing, and one that cannot be
// fixed measures nothing either.
func TestTasksBreakWhatTheyClaimAndPassWhenRestored(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and runs the suite for each task")
	}
	root := repoRoot(t)

	for _, s := range specs {
		t.Run(s.id, func(t *testing.T) {
			if s.goos != "" && s.goos != runtime.GOOS {
				// A breakage to a file this platform never compiles cannot fail
				// here, so the task would pass its verifier untouched.
				t.Skipf("task targets %s-only code", s.goos)
			}
			t.Parallel()
			dir := t.TempDir()

			task := taskFor(root, s)
			if err := task.Setup(dir); err != nil {
				t.Fatalf("setup: %v", err)
			}

			solved, detail, err := task.Verify(dir)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if solved {
				t.Fatalf("the task passes its own verifier before anything is done to it, so it measures nothing")
			}
			if detail == "" {
				t.Error("a failing verifier reported no detail, so a model would be told nothing")
			}
		})
	}
}
