package bisect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cj-vana/switchboard/internal/checkpoint"
)

func state(content string) checkpoint.FileState {
	return checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte(content)}
}

// probeByContent verifies by reading a file, the way a real verifier reads
// the tree: it passes until the file says "broken".
func probeByContent(t *testing.T, path string) func(context.Context) Verdict {
	return func(context.Context) Verdict {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			return Verdict{Err: err}
		}
		if string(data) == "broken\n" {
			return Verdict{FirstFail: "the file says broken"}
		}
		return Verdict{Passed: true}
	}
}

func TestRunFindsTheTurnThatWentRed(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f.go")
	if err := os.WriteFile(f, []byte("broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Four turns: before 0 and 1 the file was fine, turn 2 broke it, and
	// it has been broken since.
	r := &Runner{
		States: []map[string]checkpoint.FileState{
			{f: state("fine v0\n")},
			{f: state("fine v1\n")},
			{f: state("fine v2\n")},
			{f: state("broken\n")}, // before turn 3 it was already broken
		},
		Verify: probeByContent(t, f),
	}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Found || res.Culprit != 2 {
		t.Errorf("outcome=%v culprit=%d, want turn 2 named", res.Outcome, res.Culprit)
	}
	if res.Fail.FirstFail != "the file says broken" {
		t.Errorf("the failing line did not ride the result: %+v", res.Fail)
	}
	if got, _ := os.ReadFile(f); string(got) != "broken\n" {
		t.Errorf("the tree was not restored: %q", got)
	}
}

func TestRunSaysGreenWhenNothingFails(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f.go")
	if err := os.WriteFile(f, []byte("fine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runner{
		States: []map[string]checkpoint.FileState{{f: state("fine v0\n")}},
		Verify: probeByContent(t, f),
	}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != AlreadyGreen {
		t.Errorf("outcome = %v, want AlreadyGreen", res.Outcome)
	}
	if res.Probes != 1 {
		t.Errorf("a green tree needs one probe, took %d", res.Probes)
	}
}

func TestRunSaysWhenTheBreakPredatesTheRecord(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f.go")
	if err := os.WriteFile(f, []byte("broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runner{
		States: []map[string]checkpoint.FileState{{f: state("broken\n")}},
		Verify: probeByContent(t, f),
	}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != RedBeforeRecord {
		t.Errorf("outcome = %v, want RedBeforeRecord", res.Outcome)
	}
}

// A file a turn created restores to absent when probing before it, and
// comes back when the bisect is done.
func TestRunRemovesAndRestoresCreatedFiles(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f.go")
	created := filepath.Join(dir, "created.go")
	if err := os.WriteFile(f, []byte("broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(created, []byte("made by turn 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sawAbsent := false
	r := &Runner{
		States: []map[string]checkpoint.FileState{
			{f: state("fine\n"), created: {}}, // before turn 0, created.go was not there
			{f: state("broken\n"), created: {}},
		},
		Verify: func(context.Context) Verdict {
			if _, err := os.Stat(created); os.IsNotExist(err) {
				sawAbsent = true
			}
			data, err := os.ReadFile(f)
			if err != nil {
				return Verdict{Err: err}
			}
			if string(data) == "broken\n" {
				return Verdict{FirstFail: "broken"}
			}
			return Verdict{Passed: true}
		},
	}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Found || res.Culprit != 0 {
		t.Errorf("outcome=%v culprit=%d, want turn 0", res.Outcome, res.Culprit)
	}
	if !sawAbsent {
		t.Error("probing before the creating turn should see the file absent")
	}
	if got, _ := os.ReadFile(created); string(got) != "made by turn 1\n" {
		t.Errorf("the created file did not come back: %q", got)
	}
}

func TestRunRestoresOnCancellation(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f.go")
	if err := os.WriteFile(f, []byte("broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	probes := 0
	r := &Runner{
		States: []map[string]checkpoint.FileState{
			{f: state("fine v0\n")},
			{f: state("fine v1\n")},
			{f: state("fine v2\n")},
		},
		Verify: func(context.Context) Verdict {
			probes++
			if probes == 2 {
				cancel() // walk away mid-bisect
			}
			data, _ := os.ReadFile(f)
			if string(data) == "broken\n" {
				return Verdict{FirstFail: "broken"}
			}
			return Verdict{Passed: true}
		},
	}
	_, err := r.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation surfaced", err)
	}
	if got, _ := os.ReadFile(f); string(got) != "broken\n" {
		t.Errorf("a cancelled bisect left the tree in the past: %q", got)
	}
}

func TestRunWithNoTurnsRefuses(t *testing.T) {
	r := &Runner{Verify: func(context.Context) Verdict { return Verdict{Passed: true} }}
	if _, err := r.Run(context.Background()); err == nil {
		t.Fatal("no recorded turns must refuse, not probe")
	}
}
