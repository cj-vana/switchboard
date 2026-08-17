package checkpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readBack(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestUndoRestoresEditedAndRemovesCreated(t *testing.T) {
	dir := t.TempDir()
	edited := filepath.Join(dir, "edited.go")
	created := filepath.Join(dir, "created.go")
	write(t, edited, "original\n")

	r := NewRecorder()
	r.Begin("fix the parser")
	r.Record(edited) // captured before mutation
	write(t, edited, "mutated\n")
	r.Record(created) // did not exist
	write(t, created, "brand new\n")

	restored, removed, skipped, failed, label, err := r.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if label != "fix the parser" {
		t.Errorf("label = %q", label)
	}
	if len(restored) != 1 || restored[0] != edited {
		t.Errorf("restored = %v", restored)
	}
	if len(removed) != 1 || removed[0] != created {
		t.Errorf("removed = %v", removed)
	}
	if len(skipped) != 0 || len(failed) != 0 {
		t.Errorf("skipped=%v failed=%v", skipped, failed)
	}
	if got := readBack(t, edited); got != "original\n" {
		t.Errorf("edited file = %q, want the pre-turn content", got)
	}
	if _, statErr := os.Stat(created); !os.IsNotExist(statErr) {
		t.Error("a file the turn created must be removed by undo")
	}
}

func TestFirstCaptureWinsWithinATurn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	write(t, path, "v1\n")

	r := NewRecorder()
	r.Begin("churn")
	r.Record(path)
	write(t, path, "v2\n")
	r.Record(path) // second mutation in the same turn
	write(t, path, "v3\n")

	if _, _, _, _, _, err := r.Undo(); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, path); got != "v1\n" {
		t.Errorf("file = %q, want the pre-turn state, not intra-turn churn", got)
	}
}

func TestEmptyTurnsAreNotStacked(t *testing.T) {
	r := NewRecorder()
	r.Begin("asked a question")
	r.Begin("asked another")

	if _, _, _, _, _, err := r.Undo(); err == nil {
		t.Fatal("undo with no captured turns must say there is nothing to undo")
	}
	if turns := r.Turns(); len(turns) != 0 {
		t.Errorf("turns = %v, want none", turns)
	}
}

func TestUndoWalksBackTurnByTurn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	write(t, path, "v1\n")

	r := NewRecorder()
	r.Begin("first")
	r.Record(path)
	write(t, path, "v2\n")

	r.Begin("second")
	r.Record(path)
	write(t, path, "v3\n")
	r.Begin("") // commit the open scope

	if _, _, _, _, label, err := r.Undo(); err != nil || label != "second" {
		t.Fatalf("first undo: label=%q err=%v", label, err)
	}
	if got := readBack(t, path); got != "v2\n" {
		t.Errorf("after one undo, file = %q, want v2", got)
	}
	if _, _, _, _, label, err := r.Undo(); err != nil || label != "first" {
		t.Fatalf("second undo: label=%q err=%v", label, err)
	}
	if got := readBack(t, path); got != "v1\n" {
		t.Errorf("after two undos, file = %q, want v1", got)
	}
}

func TestOversizeFilesAreNamedNotHalfCovered(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.bin")
	small := filepath.Join(dir, "small.txt")
	write(t, big, strings.Repeat("x", maxFileBytes+1))
	write(t, small, "small\n")

	r := NewRecorder()
	r.Begin("bulk change")
	r.Record(big)
	r.Record(small)
	write(t, small, "changed\n")

	turns := r.Turns()
	if len(turns) != 1 || !turns[0].Partial {
		t.Fatalf("turns = %+v, want one partial turn", turns)
	}

	_, _, skipped, _, _, err := r.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != big {
		t.Errorf("skipped = %v, want the oversize file named", skipped)
	}
	if got := readBack(t, small); got != "small\n" {
		t.Errorf("small = %q, want restored despite the oversize sibling", got)
	}
}

func TestRecordOutsideATurnIsIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	write(t, path, "v1\n")

	r := NewRecorder()
	r.Record(path)
	if turns := r.Turns(); len(turns) != 0 {
		t.Errorf("a capture with no turn scope must not invent a checkpoint: %v", turns)
	}
}

func TestPendingFilesCountsTheOpenScope(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	write(t, a, "one")
	write(t, b, "two")

	r := NewRecorder()
	if r.PendingFiles() != 0 {
		t.Error("counted files with no scope open")
	}

	r.Begin("first turn")
	if r.PendingFiles() != 0 {
		t.Error("counted files before any capture")
	}
	r.Record(a)
	r.Record(a) // the same file twice is one capture
	if got := r.PendingFiles(); got != 1 {
		t.Errorf("want 1 pending after one capture, got %d", got)
	}
	r.Record(b)
	if got := r.PendingFiles(); got != 2 {
		t.Errorf("want 2 pending, got %d", got)
	}

	// A new turn opens a fresh scope; the committed turn no longer counts.
	r.Begin("second turn")
	if got := r.PendingFiles(); got != 0 {
		t.Errorf("the previous turn's captures leaked into the new scope: %d", got)
	}
}

// Details is Turns with the paths attached: the same evidence Undo restores
// from, shaped for a surface that says what a session touched.
func TestDetailsNamesThePathsPerTurn(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	os.WriteFile(a, []byte("a"), 0o644)

	r := NewRecorder()
	r.Begin("first turn")
	r.Record(a)
	r.Begin("second turn")
	r.Record(a)
	r.Record(b)

	details := r.Details()
	if len(details) != 2 {
		t.Fatalf("got %d turns, want 2", len(details))
	}
	if details[0].Label != "first turn" || len(details[0].Paths) != 1 {
		t.Fatalf("first turn: %+v", details[0])
	}
	if details[1].Label != "second turn" || len(details[1].Paths) != 2 {
		t.Fatalf("second turn: %+v", details[1])
	}
	if details[1].Paths[0] != a {
		t.Fatalf("paths are not sorted: %v", details[1].Paths)
	}
}

// UndoFile takes back one file, not the turn: the newest capture of that
// file restores, the capture is consumed only on success, the turn's other
// files stay, and a turn left with nothing disappears from the stack.
func TestUndoFileRestoresOneAndConsumesTheCapture(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	os.WriteFile(a, []byte("a before"), 0o644)

	r := NewRecorder()
	r.Begin("the turn")
	r.Record(a) // existed: restore rewrites it
	r.Record(b) // absent: restore removes it
	os.WriteFile(a, []byte("a after"), 0o644)
	os.WriteFile(b, []byte("b created"), 0o644)

	removed, label, err := r.UndoFile(a)
	if err != nil || removed || label != "the turn" {
		t.Fatalf("UndoFile(a) = %v %q %v", removed, label, err)
	}
	if got, _ := os.ReadFile(a); string(got) != "a before" {
		t.Fatalf("a holds %q, want its pre-turn content", got)
	}
	if _, err := os.Stat(b); err != nil {
		t.Fatal("taking back a took b with it")
	}
	if details := r.Details(); len(details) != 1 || len(details[0].Paths) != 1 {
		t.Fatalf("the turn should still hold b alone: %+v", details)
	}

	if _, _, err := r.UndoFile(a); err == nil {
		t.Fatal("a consumed capture restored twice")
	}

	removed, _, err = r.UndoFile(b)
	if err != nil || !removed {
		t.Fatalf("UndoFile(b) = %v %v, want removed", removed, err)
	}
	if _, err := os.Stat(b); !os.IsNotExist(err) {
		t.Fatal("the created file survived its undo")
	}
	if details := r.Details(); len(details) != 0 {
		t.Fatalf("an emptied turn stayed on the stack: %+v", details)
	}
}
