package main

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/config"
)

func pressureApp(t *testing.T) *tuiApp {
	t.Helper()
	m := testModel(t)
	m.app.config = &config.Config{CompactAuto: true, CompactAtPercent: 85}
	return m.app
}

// A warning that arrived at the compaction threshold would be advice with no
// turn left to take it.
func TestNoWarningWhileThereIsRoom(t *testing.T) {
	app := pressureApp(t)
	app.publishOccupancy(1000, 10000)
	if msgs := app.pressureRound(); len(msgs) != 0 {
		t.Errorf("a warning fired at 10%% full: %+v", msgs)
	}
}

// The point of the notice is that the model still has the whole picture when
// it reads it.
func TestTheBoundaryIsAnnouncedBeforeItArrives(t *testing.T) {
	app := pressureApp(t)
	app.publishOccupancy(7500, 10000)

	msgs := app.pressureRound()
	if len(msgs) != 1 {
		t.Fatalf("msgs = %+v, want one warning", msgs)
	}
	text := textOf(msgs[0])
	for _, want := range []string{"75%", "85%", "todo", "objective"} {
		if !strings.Contains(text, want) {
			t.Errorf("warning missing %q:\n%s", want, text)
		}
	}
}

// A warning repeated at every round boundary is one the model stops reading.
func TestTheBoundaryIsAnnouncedOnce(t *testing.T) {
	app := pressureApp(t)
	app.publishOccupancy(9000, 10000)

	if msgs := app.pressureRound(); len(msgs) != 1 {
		t.Fatalf("the first round did not warn: %+v", msgs)
	}
	if msgs := app.pressureRound(); len(msgs) != 0 {
		t.Errorf("the warning repeated: %+v", msgs)
	}
}

// With auto-compaction off there is no boundary coming, so there is nothing
// to announce.
func TestNoWarningWhenNothingWillCompact(t *testing.T) {
	app := pressureApp(t)
	app.config.CompactAuto = false
	app.publishOccupancy(9500, 10000)
	if msgs := app.pressureRound(); len(msgs) != 0 {
		t.Errorf("a warning fired with auto-compaction off: %+v", msgs)
	}
}

// An unknown window is not a full one, and warning on it would fire on every
// endpoint that reports no usage.
func TestNoWarningWithoutAKnownWindow(t *testing.T) {
	app := pressureApp(t)
	app.publishOccupancy(9000, 0)
	if msgs := app.pressureRound(); len(msgs) != 0 {
		t.Errorf("a warning fired with no known window: %+v", msgs)
	}
}
