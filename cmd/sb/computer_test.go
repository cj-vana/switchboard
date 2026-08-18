package main

import (
	"runtime"
	"testing"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// The platform gate is the whole registration decision: on macOS the tool
// is present because osascript always is, and everywhere else the model
// never sees it — absent, not broken.
func TestComputerJoinsTheSuiteOnlyOnDarwin(t *testing.T) {
	registry, err := tools.NewRegistry(t.TempDir(), execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	addComputerUse(registry)
	_, present := registry.Get("computer")
	if want := runtime.GOOS == "darwin"; present != want {
		t.Fatalf("computer present=%v on %s, want %v", present, runtime.GOOS, want)
	}
}
