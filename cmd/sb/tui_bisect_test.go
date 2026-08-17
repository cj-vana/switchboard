package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/bisect"
	"github.com/cj-vana/switchboard/internal/checkpoint"
)

func TestBisectRefusesWithoutADeclaredVerifier(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	os.WriteFile(path, []byte("v0"), 0o644)
	m.app.undo.Begin("a turn")
	m.app.undo.Record(path)
	os.WriteFile(path, []byte("v1"), 0o644)

	if cmd := cmdBisect(m, ""); cmd == nil {
		t.Fatal("refusal said nothing")
	} else {
		msg := cmd().(noticeMsg)
		if !strings.Contains(msg.text, "/watch") {
			t.Errorf("the refusal does not name the way to declare one: %q", msg.text)
		}
	}
	if m.bisect != nil {
		t.Error("a refused bisect is running anyway")
	}
}

func TestBisectRefusesWithNothingRecorded(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	cmd := cmdBisect(m, "go test ./...")
	if cmd == nil {
		t.Fatal("refusal said nothing")
	}
	msg := cmd().(noticeMsg)
	if !strings.Contains(msg.text, "nothing to bisect") {
		t.Errorf("an empty record should say so: %q", msg.text)
	}
}

func TestBisectRefusesAPartialTurn(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	dir := t.TempDir()
	big := filepath.Join(dir, "big.bin")
	os.WriteFile(big, []byte(strings.Repeat("x", 4<<20+1)), 0o644)
	m.app.undo.Begin("bulk change")
	m.app.undo.Record(big)

	cmd := cmdBisect(m, "go test ./...")
	if cmd == nil {
		t.Fatal("refusal said nothing")
	}
	msg := cmd().(noticeMsg)
	if !strings.Contains(msg.text, "snapshot cap") {
		t.Errorf("a partial turn must be refused by name: %q", msg.text)
	}
}

func TestBisectDoneNamesTheCulpritAndClearsBusy(t *testing.T) {
	m := testModel(t)
	run := &bisectRun{
		command: "go test ./...",
		labels:  []string{"first", "add the cache header", "third"},
		cancel:  func() {},
		rail:    m.tr.add(&entry{kind: kindInfo, text: "bisect"}),
	}
	m.bisect = run
	m.busy = true

	m.onBisectDone(bisectDoneMsg{res: bisect.Result{
		Outcome: bisect.Found,
		Culprit: 1,
		Fail:    bisect.Verdict{FirstFail: "--- FAIL: TestCache"},
		Probes:  4,
	}})

	if m.busy || m.bisect != nil {
		t.Error("a finished bisect left the session busy")
	}
	view := strings.Join(m.tr.flat, "\n")
	for _, want := range []string{"turn 2 of 3", "add the cache header", "--- FAIL: TestCache", "write and edit captured"} {
		if !strings.Contains(view, want) {
			t.Errorf("the report is missing %q:\n%s", want, view)
		}
	}
}

func TestBisectDoneOnCancellationSaysRestored(t *testing.T) {
	m := testModel(t)
	run := &bisectRun{
		command:   "go test ./...",
		labels:    []string{"only"},
		cancel:    func() {},
		cancelled: true,
		rail:      m.tr.add(&entry{kind: kindInfo, text: "bisect"}),
	}
	m.bisect = run
	m.busy = true

	m.onBisectDone(bisectDoneMsg{})
	if m.busy {
		t.Error("cancellation left the session busy")
	}
	if !strings.Contains(strings.Join(m.tr.flat, "\n"), "workspace is restored") {
		t.Errorf("a cancelled bisect must say the tree is back:\n%s", strings.Join(m.tr.flat, "\n"))
	}
}
