package tools

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/execution"
)

func driftRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	root := t.TempDir()
	r, err := NewRegistry(root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	return r, root
}

// readFile drives the read tool the way the loop does, so the test exercises
// the same recording path the feature depends on.
func readFile(t *testing.T, r *Registry, name string) {
	t.Helper()
	plan, err := r.tools["read"].Plan([]byte(`{"path":"` + name + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// Coarse filesystem timestamps would let a same-second rewrite look
	// unchanged to the stat gate, which is not what this test is measuring.
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

// The case the feature exists for: something outside the tool record changed a
// file the model was shown.
func TestAFileChangedOutsideTheToolRecordIsReported(t *testing.T) {
	r, root := driftRegistry(t)
	path := filepath.Join(root, "main.go")
	write(t, path, "package main\n")
	readFile(t, r, "main.go")

	if drifted := r.DriftedReads(); len(drifted) != 0 {
		t.Fatalf("an untouched file reported drift: %+v", drifted)
	}

	write(t, path, "package main\n// formatted\n")
	drifted := r.DriftedReads()
	if len(drifted) != 1 || drifted[0].Path != "main.go" {
		t.Fatalf("drifted = %+v, want the changed file", drifted)
	}
}

// The same sentence at every round boundary is noise the model learns to skip.
func TestOneChangeIsReportedOnce(t *testing.T) {
	r, root := driftRegistry(t)
	path := filepath.Join(root, "a.txt")
	write(t, path, "one")
	readFile(t, r, "a.txt")
	write(t, path, "two")

	if drifted := r.DriftedReads(); len(drifted) != 1 {
		t.Fatalf("first sweep = %+v, want the change", drifted)
	}
	if drifted := r.DriftedReads(); len(drifted) != 0 {
		t.Errorf("second sweep repeated the same change: %+v", drifted)
	}

	write(t, path, "three")
	if drifted := r.DriftedReads(); len(drifted) != 1 {
		t.Errorf("a further change was not reported: %+v", drifted)
	}
}

// Deletion is drift too, and the most confusing kind to meet through a failed
// edit rather than a sentence.
func TestADeletedFileIsReportedAsGone(t *testing.T) {
	r, root := driftRegistry(t)
	path := filepath.Join(root, "gone.txt")
	write(t, path, "here")
	readFile(t, r, "gone.txt")

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	drifted := r.DriftedReads()
	if len(drifted) != 1 || !drifted[0].Gone {
		t.Fatalf("drifted = %+v, want the deletion", drifted)
	}
	if again := r.DriftedReads(); len(again) != 0 {
		t.Errorf("the deletion was reported twice: %+v", again)
	}
}

// A file touched without its bytes moving is not drift, and saying it was would
// train the model to ignore the notice.
func TestATouchedButUnchangedFileIsNotDrift(t *testing.T) {
	r, root := driftRegistry(t)
	path := filepath.Join(root, "same.txt")
	write(t, path, "identical")
	readFile(t, r, "same.txt")

	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if drifted := r.DriftedReads(); len(drifted) != 0 {
		t.Errorf("a touched file with the same bytes reported drift: %+v", drifted)
	}
}

// A file the model has not read is not watched, because nothing is known about
// what it looked like.
func TestAnUnreadFileIsNotWatched(t *testing.T) {
	r, root := driftRegistry(t)
	write(t, filepath.Join(root, "never-read.txt"), "content")
	if drifted := r.DriftedReads(); len(drifted) != 0 {
		t.Errorf("a file that was never read reported drift: %+v", drifted)
	}
}

// The stale check at the write is the guarantee; this notice must not disarm it
// by refreshing what the model was shown.
func TestReportingDriftLeavesTheStaleCheckArmed(t *testing.T) {
	r, root := driftRegistry(t)
	path := filepath.Join(root, "guarded.txt")
	write(t, path, "original")
	readFile(t, r, "guarded.txt")
	write(t, path, "changed underneath")

	if drifted := r.DriftedReads(); len(drifted) != 1 {
		t.Fatalf("drift was not reported: %+v", drifted)
	}

	plan, err := r.tools["edit"].Plan([]byte(`{"path":"guarded.txt","old_string":"original","new_string":"mine"}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := plan.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("the edit was allowed after the file moved; the notice disarmed the guarantee")
	}
}
