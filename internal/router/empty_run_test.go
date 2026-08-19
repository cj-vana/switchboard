package router

import "testing"

// Every other signal here is lagging: it reports something that has already
// gone wrong. A turn can spend its whole budget without tripping one, because
// searching the wrong place succeeds every time and says so politely. This is
// what that failure looks like while it is still happening.
func TestARunOfEmptySuccessesIsEvidence(t *testing.T) {
	d := NewDetector()

	// Successes that return something reset the run, however many there are.
	for i := 0; i < 10; i++ {
		if sigs := d.ToolResult("grep", "grep -r foo .", "internal/x.go:12: foo", false); len(sigs) != 0 {
			t.Fatalf("a productive call produced signals: %v", sigs)
		}
	}

	var fired []Signal
	for i := 0; i < DefaultEmptyRunAt; i++ {
		fired = append(fired, d.ToolResult("grep", "grep -r bar .", "", false)...)
	}
	if len(fired) != 1 || fired[0] != EmptyResultRun {
		t.Fatalf("a run of %d empty successes produced %v, want one EmptyResultRun", DefaultEmptyRunAt, fired)
	}

	// Once per turn: the same run reported again is one observation counted
	// twice, which is what the error spike already refuses to do.
	if more := d.ToolResult("grep", "grep -r baz .", "   \n ", false); len(more) != 0 {
		t.Fatalf("the run reported itself twice: %v", more)
	}

	// A fresh turn starts clean.
	d.Reset()
	if sigs := d.ToolResult("grep", "grep -r qux .", "", false); len(sigs) != 0 {
		t.Fatalf("the run survived a turn boundary: %v", sigs)
	}
}

// One empty result is a fact about the workspace, not about the model. It
// takes more of them in a row than it takes failures, and it is worth less
// than a failure when it does fire.
func TestAnEmptyResultOnItsOwnDoesNotMove(t *testing.T) {
	d := NewDetector()
	if sigs := d.ToolResult("grep", "grep -r nothing .", "", false); len(sigs) != 0 {
		t.Fatalf("one empty result was treated as evidence: %v", sigs)
	}
	if DefaultEmptyRunAt <= DefaultErrorSpikeAt {
		t.Fatalf("an empty run (%d) should need more evidence than an error spike (%d)",
			DefaultEmptyRunAt, DefaultErrorSpikeAt)
	}
	if weights[EmptyResultRun] >= weights[ToolErrorSpike] {
		t.Fatalf("an empty run (%v) should weigh less than a failure spike (%v)",
			weights[EmptyResultRun], weights[ToolErrorSpike])
	}
	// It cannot escalate alone, which is the point of weighing it at half.
	var p Policy
	if move := p.Assess([]Signal{EmptyResultRun}, DefaultMinimumDwell+1); move.Direction != 0 {
		t.Fatalf("an empty run escalated on its own: %+v", move)
	}
}
