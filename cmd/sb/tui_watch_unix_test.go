//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/checkpoint"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/watch"
)

// The round boundary is the whole contract: the verifier runs only when the
// turn has captured files it has not checked, and what it returns rides the
// injection seam as a user-role message.
func TestWatchRoundRunsOnlyAfterCapturedEdits(t *testing.T) {
	m := testModel(t)
	rec := checkpoint.NewRecorder()
	m.app.undo = rec
	m.app.watchSt.arm(watch.New("echo '--- FAIL: TestAlpha'; exit 1", t.TempDir()))
	m.app.watchSt.beginTurn(context.Background())

	if msgs := m.app.watchRound(); len(msgs) != 0 {
		t.Fatal("the verifier ran with nothing captured")
	}

	rec.Begin("a turn")
	f := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec.Record(f)

	msgs := m.app.watchRound()
	if len(msgs) != 1 {
		t.Fatalf("want one injected message, got %d", len(msgs))
	}
	text := ""
	for _, b := range msgs[0].Content {
		if tb, ok := b.(provider.Text); ok {
			text = tb.Text
		}
	}
	if msgs[0].Role != provider.RoleUser || !strings.Contains(text, "--- FAIL: TestAlpha") {
		t.Fatalf("the injection is not a user-role failure report: %+v", msgs[0])
	}

	// The same evidence does not run the verifier twice.
	if msgs := m.app.watchRound(); len(msgs) != 0 {
		t.Fatal("the verifier ran again with no new captures")
	}
}
