//go:build unix

package execution

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func backgroundSet(t *testing.T) *BackgroundSet {
	t.Helper()
	s := NewBackgroundSet()
	t.Cleanup(s.StopAll)
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The shape exec could not express at all: something that keeps running while
// the turn goes on.
func TestABackgroundCommandOutlivesTheCallThatStartedIt(t *testing.T) {
	s := backgroundSet(t)
	status, err := s.Start(context.Background(), Command{
		Argv: []string{"sh", "-c", "echo listening; sleep 30"}, Shell: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !status.Running {
		t.Fatal("the command was not running when Start returned")
	}

	waitFor(t, "the command's first output", func() bool {
		out, _, _, err := s.Output(status.ID)
		return err == nil && strings.Contains(out, "listening")
	})

	// Reading does not consume: a caller that lost a server's startup line has
	// no way to ask for it again.
	first, _, _, err := s.Output(status.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, _, live, err := s.Output(status.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("a second read returned different bytes: %q then %q", first, second)
	}
	if !live.Running {
		t.Error("the command stopped on its own")
	}
}

// Stopping has to reach the whole group, or a shell's children keep the port.
func TestStoppingKillsTheGroupAndIsVisibleImmediately(t *testing.T) {
	s := backgroundSet(t)
	status, err := s.Start(context.Background(), Command{
		Argv: []string{"sh", "-c", "sleep 60 & echo started; wait"},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the command to start", func() bool {
		out, _, _, err := s.Output(status.ID)
		return err == nil && strings.Contains(out, "started")
	})

	stopped, err := s.Stop(status.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Running {
		t.Error("Stop returned while the command was still running; a caller that lists next sees a lie")
	}
	if !stopped.Killed {
		t.Error("the stop was not recorded, so a non-zero exit reads as a mystery")
	}
}

// A finished command is still an answer to "what did I start", so the list
// keeps it.
func TestTheListKeepsFinishedCommands(t *testing.T) {
	s := backgroundSet(t)
	status, err := s.Start(context.Background(), Command{Argv: []string{"sh", "-c", "exit 3"}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the command to finish", func() bool {
		_, _, live, err := s.Output(status.ID)
		return err == nil && !live.Running
	})

	list := s.List()
	if len(list) != 1 {
		t.Fatalf("list = %+v, want the finished command", list)
	}
	if list[0].Running {
		t.Error("a finished command is still listed as running")
	}
	if list[0].ExitCode != 3 {
		t.Errorf("exit code = %d, want the status the command actually returned", list[0].ExitCode)
	}
}

// A model that has learned to start a server will start one per attempt.
func TestTheNumberRunningAtOnceIsCapped(t *testing.T) {
	s := backgroundSet(t)
	for i := range MaxBackground {
		if _, err := s.Start(context.Background(), Command{Argv: []string{"sleep", "30"}}); err != nil {
			t.Fatalf("start %d failed: %v", i, err)
		}
	}
	_, err := s.Start(context.Background(), Command{Argv: []string{"sleep", "30"}})
	if err == nil {
		t.Fatal("the cap did not hold")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error = %q, which does not say what the problem is", err)
	}
}

// The session's exit is the last moment this program can be sure these
// processes are still its own to signal.
func TestStopAllEndsEverythingAndRefusesMore(t *testing.T) {
	s := NewBackgroundSet()
	if _, err := s.Start(context.Background(), Command{Argv: []string{"sleep", "60"}}); err != nil {
		t.Fatal(err)
	}
	s.StopAll()

	for _, status := range s.List() {
		if status.Running {
			t.Errorf("%s survived StopAll", status.ID)
		}
	}
	if _, err := s.Start(context.Background(), Command{Argv: []string{"sleep", "1"}}); err == nil {
		t.Error("a stopped set started a new process")
	}
}

// A sandbox that quietly fell back to running the command would leave the UI
// reporting containment that is not there.
func TestAnUnappliableConfinementRefusesToStart(t *testing.T) {
	s := backgroundSet(t)
	// A confinement whose wrap refuses is the shape a host that cannot apply
	// one produces. The value carries its own wrap because a *Confinement is
	// itself the evidence the self-test passed; there is no empty one.
	refusing := &Confinement{
		mechanism: MechanismNone,
		wrap: func(Policy, []string) ([]string, error) {
			return nil, errors.New("the sandbox helper is missing")
		},
	}
	_, err := s.Start(context.Background(), Command{
		Argv:    []string{"echo", "hi"},
		Confine: refusing,
	})
	if err == nil {
		t.Fatal("a command started with a confinement that could not be applied")
	}
	if !strings.Contains(err.Error(), "unconfined") {
		t.Errorf("error = %q, which does not say the refusal was about containment", err)
	}
}

func TestAnUnknownIDIsNamedRatherThanIgnored(t *testing.T) {
	s := backgroundSet(t)
	if _, _, _, err := s.Output("bg99"); err == nil {
		t.Error("reading an unknown command succeeded")
	}
	if _, err := s.Stop("bg99"); err == nil {
		t.Error("stopping an unknown command succeeded")
	}
}
