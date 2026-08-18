package blame

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/session"
)

func write(sess string, turn int, target, content string) session.FileEdit {
	return session.FileEdit{SessionID: sess, Turn: turn, Target: target, Write: true, Content: content}
}

func edit(sess string, turn int, target, old, new string) session.FileEdit {
	return session.FileEdit{SessionID: sess, Turn: turn, Target: target, Old: old, New: new}
}

// lineOrigins renders an annotation as one rune per line for compact
// assertions: 'a' is Origins[0], 'b' is Origins[1], '.' is outside the
// record.
func lineOrigins(ann Annotation) string {
	var b strings.Builder
	for _, o := range ann.Lines {
		if o < 0 {
			b.WriteByte('.')
		} else {
			b.WriteByte(byte('a' + o))
		}
	}
	return b.String()
}

func TestAnnotateAttributesWriteThenEdit(t *testing.T) {
	edits := []session.FileEdit{
		write("s1", 1, "light", "one\ntwo\nthree\n"),
		edit("s1", 2, "heavy", "two", "TWO"),
	}
	disk := []byte("one\nTWO\nthree\n")

	ann := Annotate(disk, edits)
	if got := lineOrigins(ann); got != "aba" {
		t.Errorf("attribution reads %q, wanted the edit to own only its line", got)
	}
	if len(ann.Origins) != 2 || ann.Origins[0].Turn != 1 || ann.Origins[1].Turn != 2 {
		t.Errorf("origins drifted: %+v", ann.Origins)
	}
	if ann.Unplaced != 0 {
		t.Errorf("everything replayed, yet %d edits read as unplaced", ann.Unplaced)
	}
}

func TestAnnotateMarksOutOfBandLinesOutsideTheRecord(t *testing.T) {
	edits := []session.FileEdit{write("s1", 1, "light", "one\ntwo\n")}
	disk := []byte("one\nhand-typed\ntwo\n")

	ann := Annotate(disk, edits)
	if got := lineOrigins(ann); got != "a.a" {
		t.Errorf("attribution reads %q; a hand-typed line must not inherit an origin", got)
	}
}

// A full rewrite that keeps lines is not a fresh authorship claim on them:
// the kept lines stay with the turn that first wrote them, the way git
// blame survives a reformat that moves a function.
func TestAnnotateKeepsAttributionAcrossARewrite(t *testing.T) {
	edits := []session.FileEdit{
		write("s1", 1, "light", "alpha\nbeta\n"),
		write("s1", 3, "heavy", "alpha\ninserted\nbeta\n"),
	}
	disk := []byte("alpha\ninserted\nbeta\n")

	ann := Annotate(disk, edits)
	if got := lineOrigins(ann); got != "aba" {
		t.Errorf("attribution reads %q; the rewrite may own only what it added", got)
	}
}

func TestAnnotateCountsDriftedEditsAsUnplaced(t *testing.T) {
	edits := []session.FileEdit{
		write("s1", 1, "light", "one\n"),
		edit("s1", 2, "light", "never-there", "x"),
	}
	ann := Annotate([]byte("one\n"), edits)
	if ann.Unplaced != 1 {
		t.Errorf("one edit could not land, %d reported", ann.Unplaced)
	}
	if got := lineOrigins(ann); got != "a" {
		t.Errorf("the placed write should still hold its line: %q", got)
	}
}

func TestAnnotateHonorsReplaceAll(t *testing.T) {
	edits := []session.FileEdit{
		write("s1", 1, "light", "x\nx\n"),
		{SessionID: "s1", Turn: 2, Target: "light", Old: "x", New: "y", ReplaceAll: true},
	}
	ann := Annotate([]byte("y\ny\n"), edits)
	if got := lineOrigins(ann); got != "aa" {
		t.Errorf("replace_all touched every line, attribution reads %q", got)
	}
	if len(ann.Origins) != 1 || ann.Origins[0].Turn != 2 {
		t.Errorf("both lines belong to the replace_all turn: %+v", ann.Origins)
	}
	if ann.Unplaced != 0 {
		t.Errorf("a replayable replace_all read as unplaced")
	}
}

// An edit that would have been ambiguous against the reconstruction ran
// against a file that had drifted; placing it anywhere would be a guess.
func TestAnnotateRefusesAmbiguousPlacement(t *testing.T) {
	edits := []session.FileEdit{
		write("s1", 1, "light", "x\nx\n"),
		edit("s1", 2, "light", "x", "y"),
	}
	ann := Annotate([]byte("y\nx\n"), edits)
	if ann.Unplaced != 1 {
		t.Errorf("an ambiguous edit was placed anyway: %+v", ann)
	}
}

func TestAnnotateWithNoEditsExplainsNothing(t *testing.T) {
	ann := Annotate([]byte("a\nb\n"), nil)
	if got := lineOrigins(ann); got != ".." {
		t.Errorf("no record, yet attribution reads %q", got)
	}
	if len(ann.Origins) != 0 {
		t.Errorf("origins from nowhere: %+v", ann.Origins)
	}
}

// Origins the session itself overwrote do not survive into the result:
// only what still explains a current line makes the list.
func TestAnnotateDropsFullyOverwrittenOrigins(t *testing.T) {
	edits := []session.FileEdit{
		write("s1", 1, "light", "draft\n"),
		write("s1", 2, "heavy", "final\n"),
	}
	ann := Annotate([]byte("final\n"), edits)
	if len(ann.Origins) != 1 || ann.Origins[0].Turn != 2 {
		t.Errorf("an overwritten origin survived: %+v", ann.Origins)
	}
}

func TestAlignPairsAroundAnInsertion(t *testing.T) {
	got := align([]string{"a", "b", "c"}, []string{"a", "x", "b", "c"})
	want := []int{0, -1, 1, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("align = %v, want %v", got, want)
		}
	}
}
