package main

import (
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/checkpoint"

	route "github.com/cj-vana/switchboard/internal/router"
	"github.com/cj-vana/switchboard/internal/watch"
)

func TestWatchCommandArmsReportsAndDisarms(t *testing.T) {
	m := testModel(t)
	m.app.undo = nil

	// Arming needs the recorder: without it the watch would never be due
	// and would sit armed while silently doing nothing.
	cmdWatch(m, "go test ./...")
	if m.app.watchSt.armed() != nil {
		t.Fatal("armed without a checkpoint recorder")
	}

	m2 := testModel(t)
	m2.app.undo = checkpoint.NewRecorder()
	cmdWatch(m2, "go test ./...")
	w := m2.app.watchSt.armed()
	if w == nil || w.Command() != "go test ./..." {
		t.Fatalf("arming did not take: %+v", w)
	}
	view := m2.View()
	if !strings.Contains(view, "watch ✓") {
		t.Error("the status chip does not show the armed watch")
	}

	if cmd := cmdWatch(m2, "off"); cmd == nil {
		t.Fatal("disarming said nothing")
	}
	if m2.app.watchSt.armed() != nil {
		t.Error("off left the watch armed")
	}
	if strings.Contains(m2.View(), "watch ✓") {
		t.Error("the status chip survived disarming")
	}
}

func TestWatchInjectSpeaksOnlyOnAChange(t *testing.T) {
	// A repeat verdict is silence.
	if got := watchInjectText("go test", watch.Report{Persisting: 2, Signatures: []string{"a", "b"}}); got != "" {
		t.Errorf("a repeat verdict injected: %q", got)
	}

	// New failures name the command, the exit, and the lines.
	rep := watch.Report{
		ExitCode:   1,
		New:        []route.Failure{{Signature: "s1", Line: "--- FAIL: TestAlpha"}},
		Persisting: 1,
		Signatures: []string{"s1", "s0"},
	}
	text := watchInjectText("go test ./...", rep)
	for _, want := range []string{"[watch]", "go test ./...", "--- FAIL: TestAlpha", "1 earlier failure(s) persist"} {
		if !strings.Contains(text, want) {
			t.Errorf("inject text is missing %q:\n%s", want, text)
		}
	}

	// Green after red is the one green worth telling the model.
	green := watchInjectText("go test", watch.Report{Passed: true, WentGreen: true})
	if !strings.Contains(green, "now passes") {
		t.Errorf("going green was not announced: %q", green)
	}
}

func TestWatchInjectRedactsWhatTheGateWouldHold(t *testing.T) {
	rep := watch.Report{
		ExitCode: 1,
		New: []route.Failure{{
			Signature: "s1",
			Line:      "FAIL: env leaked sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefghijklmnop",
		}},
		Signatures: []string{"s1"},
	}
	text := watchInjectText("env", rep)
	if strings.Contains(text, "sk-ant-api03-abcdefghijklmnop") {
		t.Fatalf("a key rode the watch report to the model:\n%s", text)
	}
}

func TestWatchInjectCapsTheLinesItCarries(t *testing.T) {
	rep := watch.Report{ExitCode: 1}
	for _, l := range []string{"--- FAIL: A", "--- FAIL: B", "--- FAIL: C", "--- FAIL: D", "--- FAIL: E"} {
		rep.New = append(rep.New, route.Failure{Signature: l, Line: l})
		rep.Signatures = append(rep.Signatures, l)
	}
	text := watchInjectText("go test", rep)
	if strings.Contains(text, "--- FAIL: D") {
		t.Error("the cap did not hold")
	}
	if !strings.Contains(text, "2 more new failures") {
		t.Errorf("the dropped lines were not counted:\n%s", text)
	}
}

func TestWatchReportRendersOnceAndKeepsTheChipCurrent(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	cmdWatch(m, "go test")

	before := len(m.tr.entries)
	m.onWatchReport(watchReportMsg{command: "go test", rep: watch.Report{
		ExitCode:   1,
		New:        []route.Failure{{Signature: "s1", Line: "--- FAIL: TestAlpha"}},
		Signatures: []string{"s1"},
	}})
	if len(m.tr.entries) == before {
		t.Error("a new failure rendered nothing")
	}
	if !strings.Contains(m.View(), "watch ✗1") {
		t.Error("the chip does not show the failure count")
	}

	// The same verdict again: chip stays, transcript stays quiet.
	before = len(m.tr.entries)
	m.onWatchReport(watchReportMsg{command: "go test", rep: watch.Report{
		ExitCode: 1, Persisting: 1, Signatures: []string{"s1"},
	}})
	if len(m.tr.entries) != before {
		t.Error("a repeat verdict rendered a notice")
	}

	m.onWatchReport(watchReportMsg{command: "go test", rep: watch.Report{Passed: true, WentGreen: true}})
	if !strings.Contains(m.View(), "watch ✓") {
		t.Error("the chip did not go green")
	}
}

func TestWatchTurnEndVerdictFoldsIntoTheNextPrompt(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	cmdWatch(m, "go test")

	m.onWatchReport(watchReportMsg{command: "go test", turnEnd: true, rep: watch.Report{
		ExitCode:   1,
		New:        []route.Failure{{Signature: "s1", Line: "--- FAIL: TestAlpha"}},
		Signatures: []string{"s1"},
	}})
	prompt := m.watchContext("fix it")
	// The typed prompt leads and the report follows, so an opening never
	// leads with the injection label /retry's shape check reads.
	if !strings.Contains(prompt, "--- FAIL: TestAlpha") || !strings.HasPrefix(prompt, "fix it") {
		t.Fatalf("the verdict did not fold in behind the prompt:\n%s", prompt)
	}
	if again := m.watchContext("next"); again != "next" {
		t.Errorf("the fold was not drained: %q", again)
	}
}

// A turn-end run can outlive its turn; its stale count must not blind the
// next turn's due check.
func TestAStaleTurnEndRunCannotBlindTheNextTurn(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	cmdWatch(m, "go test")
	ws := m.app.watchSt

	// Round one of turn one: three files captured, run due.
	_, _, gen, ok := ws.due(3)
	if !ok {
		t.Fatal("three fresh captures were not due")
	}

	// The next turn begins before the run reports back.
	ws.beginTurn(nil)
	ws.ran(gen, 3)

	// One capture into the new turn must be news.
	if _, _, _, ok := ws.due(1); !ok {
		t.Fatal("a stale ran() blinded the new turn")
	}
}
