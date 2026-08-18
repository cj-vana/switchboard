package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
)

func TestFailedAndNoopEditsCreateNoCheckpoint(t *testing.T) {
	r, root := newRegistry(t)
	rec := checkpoint.NewRecorder()
	r.SetCheckpoints(rec)
	path := filepath.Join(root, "target.txt")
	writeFile(t, path, "same\nsame\n")
	run(t, r, "read", map[string]any{"path": "target.txt"})

	tests := []struct {
		name  string
		input map[string]any
	}{
		{"missing match", map[string]any{"path": "target.txt", "old_string": "absent", "new_string": "new"}},
		{"ambiguous match", map[string]any{"path": "target.txt", "old_string": "same", "new_string": "new"}},
		{"missing file", map[string]any{"path": "missing.txt", "old_string": "old", "new_string": "new"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec.Begin(tt.name)
			res := run(t, r, "edit", tt.input)
			if !res.IsError {
				t.Fatalf("edit unexpectedly succeeded: %+v", res)
			}
			if turns := rec.Turns(); len(turns) != 0 {
				t.Fatalf("failed edit created checkpoint: %+v", turns)
			}
		})
	}

	rec.Begin("plan-time no-op")
	if _, err := tryRun(r, "edit", map[string]any{
		"path": "target.txt", "old_string": "same", "new_string": "same",
	}); err == nil {
		t.Fatal("identical edit must fail validation")
	}
	if turns := rec.Turns(); len(turns) != 0 {
		t.Fatalf("no-op edit created checkpoint: %+v", turns)
	}
}

func TestTransactionalEditPreservesCRLFAndNoFinalNewline(t *testing.T) {
	r, root := newRegistry(t)
	path := filepath.Join(root, "target.txt")
	writeFile(t, path, "one\r\ntwo\r\nlast")
	run(t, r, "read", map[string]any{"path": "target.txt"})

	res := run(t, r, "edit", map[string]any{
		"path": "target.txt", "old_string": "two", "new_string": "second",
	})
	if res.IsError {
		t.Fatalf("edit failed: %s", res.Content)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one\r\nsecond\r\nlast" {
		t.Fatalf("content=%q", got)
	}
}

func TestTransactionalWritePreservesExistingMode(t *testing.T) {
	r, root := newRegistry(t)
	path := filepath.Join(root, "script")
	writeFile(t, path, "old")
	if err := os.Chmod(path, 0o751); err != nil {
		t.Fatal(err)
	}
	run(t, r, "read", map[string]any{"path": "script"})
	if res := run(t, r, "write", map[string]any{"path": "script", "content": "new"}); res.IsError {
		t.Fatalf("write failed: %s", res.Content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o751 {
		t.Fatalf("mode=%o, want 751", info.Mode().Perm())
	}
}

func TestTransactionalCreationPublishesExactBytesWithoutTempLeak(t *testing.T) {
	r, root := newRegistry(t)
	content := "first\r\nlast-without-newline"
	if res := run(t, r, "write", map[string]any{
		"path": "nested/deeper/new.txt", "content": content,
	}); res.IsError {
		t.Fatalf("creation failed: %s", res.Content)
	}
	path := filepath.Join(root, "nested", "deeper", "new.txt")
	got, err := os.ReadFile(path)
	if err != nil || string(got) != content {
		t.Fatalf("content=%q err=%v", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".switchboard-write-") {
			t.Fatalf("temporary file leaked: %s", entry.Name())
		}
	}
}

func TestInjectedPrecommitRaceRefusesAndAbortsCheckpoint(t *testing.T) {
	r, root := newRegistry(t)
	rec := checkpoint.NewRecorder()
	r.SetCheckpoints(rec)
	path := filepath.Join(root, "target.txt")
	writeFile(t, path, "source")
	run(t, r, "read", map[string]any{"path": "target.txt"})
	rec.Begin("raced write")

	tx, res, ok := r.prepareFileMutation(path, false)
	if !ok {
		t.Fatalf("prepare failed: %s", res.Content)
	}
	defer tx.close()
	err := tx.publish(context.Background(), []byte("agent"), tx.before.mode, func() {
		if writeErr := os.WriteFile(path, []byte("external"), 0o644); writeErr != nil {
			t.Fatalf("injecting race: %v", writeErr)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "changed before commit") {
		t.Fatalf("publish error=%v, want source CAS refusal", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "external" {
		t.Fatalf("external bytes were overwritten: %q", got)
	}
	if turns := rec.Turns(); len(turns) != 0 {
		t.Fatalf("unpublished write created a checkpoint: %+v", turns)
	}
}

func TestConcurrentSamePathWritesSerialize(t *testing.T) {
	r, root := newRegistry(t)
	path := filepath.Join(root, "target.txt")
	writeFile(t, path, "seed")
	run(t, r, "read", map[string]any{"path": "target.txt"})

	const writers = 24
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	successes := make(chan struct{}, writers)
	contents := make(map[string]bool, writers)
	for i := range writers {
		content := fmt.Sprintf("writer-%02d-%s", i, strings.Repeat("x", 1024+i))
		contents[content] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := tryRun(r, "write", map[string]any{"path": "target.txt", "content": content})
			if err != nil {
				errs <- err
				return
			}
			if res.IsError {
				if !strings.Contains(res.Content, "changed since it was read") {
					errs <- fmt.Errorf("unexpected refusal: %s", res.Content)
				}
				return
			}
			successes <- struct{}{}
		}()
	}
	wg.Wait()
	close(errs)
	close(successes)
	for err := range errs {
		t.Error(err)
	}
	if len(successes) == 0 {
		t.Fatal("every serialized writer was refused")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !contents[string(got)] {
		t.Fatalf("final file is torn or unexpected (%d bytes)", len(got))
	}
}
