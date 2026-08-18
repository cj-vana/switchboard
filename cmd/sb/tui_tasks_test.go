package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/delegate"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func TestTasksSurfaceFiltersAndLabelsParentSession(t *testing.T) {
	manager := delegate.NewTaskManager(2)
	current := "primary-current"
	currentRef := manager.Reserve("review changes", "", "t2", current)
	_, err := manager.Execute(context.Background(), currentRef, func(_ context.Context, handle *delegate.TaskHandle) (tools.Result, error) {
		handle.AttachSession("delegate-current")
		handle.RecordUsage(2, 8_765)
		return tools.Result{Content: "done"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	oldRef := manager.Reserve("old work", "", "t1", "primary-old")
	if _, err := manager.Execute(context.Background(), oldRef, func(context.Context, *delegate.TaskHandle) (tools.Result, error) {
		return tools.Result{Content: "old"}, nil
	}); err != nil {
		t.Fatal(err)
	}

	visible := tasksForSession(manager, current)
	if len(visible) != 1 || visible[0].ID != currentRef.ID {
		t.Fatalf("current-session tasks = %+v", visible)
	}
	got := renderTasks(visible, manager.MaxParallel(), current)
	for _, want := range []string{currentRef.ID, "succeeded", "t2", "$0.0088", "primary-current", "delegate-current", "2 calls"} {
		if !strings.Contains(got, want) {
			t.Fatalf("task surface hid %q:\n%s", want, got)
		}
	}
	for _, stale := range []string{oldRef.ID, "primary-old", "old work"} {
		if strings.Contains(got, stale) {
			t.Fatalf("task surface leaked another session's %q:\n%s", stale, got)
		}
	}
}

func TestTasksCancelTargetsOneDelegate(t *testing.T) {
	m := testModel(t)
	manager := delegate.NewTaskManager(2)
	previous := subagentTasks
	subagentTasks = manager
	t.Cleanup(func() { subagentTasks = previous })
	parent := m.app.loop.Session.ID()
	first := manager.Reserve("cancel me", "", "t1", parent)
	second := manager.Reserve("keep me", "", "t1", parent)
	started := make(chan string, 2)
	release := make(chan struct{})
	done := make(chan error, 2)
	for _, ref := range []delegate.TaskRef{first, second} {
		ref := ref
		go func() {
			_, err := manager.Execute(context.Background(), ref, func(ctx context.Context, _ *delegate.TaskHandle) (tools.Result, error) {
				started <- ref.ID
				select {
				case <-release:
					return tools.Result{Content: "done"}, nil
				case <-ctx.Done():
					return tools.Result{}, ctx.Err()
				}
			})
			done <- err
		}()
	}
	<-started
	<-started
	cmd := cmdTasks(m, "cancel "+first.ID)
	if cmd == nil {
		t.Fatal("cancel command returned no confirmation")
	}
	msg, ok := cmd().(noticeMsg)
	if !ok || !strings.Contains(msg.text, "other delegate tasks keep running") {
		t.Fatalf("cancel notice = %#v", msg)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("target task error = %v", err)
	}
	byID := map[string]delegate.TaskSnapshot{}
	for _, task := range manager.List() {
		byID[task.ID] = task
	}
	if byID[first.ID].Status != delegate.TaskCanceled || byID[second.ID].Status != delegate.TaskRunning {
		t.Fatalf("statuses after targeted cancel = %+v", byID)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
