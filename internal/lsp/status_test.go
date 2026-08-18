package lsp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestServerStatusNeverStartsRuntime(t *testing.T) {
	server := &Server{Argv: []string{filepath.Join(t.TempDir(), "fixture-ls"), "--stdio"}, Root: t.TempDir()}
	starts := 0
	server.startClient = func(context.Context, []string, string, *ProblemStore) (*Client, error) {
		starts++
		return nil, errors.New("unexpected start")
	}

	status := server.Status()
	if starts != 0 || status.State != ServerConfigured || status.Executable != "fixture-ls" {
		t.Fatalf("Status() = %+v, starts=%d", status, starts)
	}
	if server.problems != nil {
		t.Fatal("Status initialized the diagnostics store")
	}
}

func TestServerStatusTracksStartingRetryFailureRunningAndClosed(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	attempts := 0
	server := &Server{Argv: []string{"fixture-ls"}, Root: t.TempDir()}
	server.startClient = func(_ context.Context, _ []string, _ string, store *ProblemStore) (*Client, error) {
		attempts++
		if attempts == 1 {
			started <- struct{}{}
			<-release
			return nil, errors.New("temporary startup failure")
		}
		return testStartedClient(store, "fixture"), nil
	}

	done := make(chan error, 1)
	go func() {
		_, err := server.Capabilities(context.Background())
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("startup did not begin")
	}
	if status := server.Status(); status.State != ServerStarting {
		t.Fatalf("starting Status() = %+v", status)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("first startup unexpectedly succeeded")
	}
	if status := server.Status(); status.State != ServerConfigured || status.LastError != "temporary startup failure" {
		t.Fatalf("failed Status() = %+v", status)
	}

	capabilities, err := server.Capabilities(context.Background())
	if err != nil || capabilities.ServerName != "fixture" {
		t.Fatalf("retry capabilities = %+v, %v", capabilities, err)
	}
	if status := server.Status(); status.State != ServerRunning || status.LastError != "" || status.Capabilities.ServerName != "fixture" {
		t.Fatalf("running Status() = %+v", status)
	}
	server.Close()
	if status := server.Status(); status.State != ServerClosed {
		t.Fatalf("closed Status() = %+v", status)
	}
}
