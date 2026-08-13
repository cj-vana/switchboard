//go:build unix

package execution

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunArgv(t *testing.T) {
	res, err := Run(context.Background(), Command{Argv: []string{"echo", "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Output) != "hello" {
		t.Errorf("output = %q", res.Output)
	}
	if res.ExitCode != 0 || res.Truncated || res.TimedOut {
		t.Errorf("result = %+v", res)
	}
}

func TestRunCapturesFailureOutput(t *testing.T) {
	res, err := Run(context.Background(), Command{
		Argv:  []string{"sh -c 'echo to-stdout; echo to-stderr >&2; exit 3'"},
		Shell: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	// The model needs both streams interleaved: a build writes its diagnosis to
	// stderr and its progress to stdout, and either alone is misleading.
	if !strings.Contains(res.Output, "to-stdout") || !strings.Contains(res.Output, "to-stderr") {
		t.Errorf("output lost a stream: %q", res.Output)
	}
}

// Direct argv execution must not word-split or expand. This is the difference
// between running a program and handing untrusted model output to a shell.
func TestArgvModeDoesNotInterpretShellSyntax(t *testing.T) {
	res, err := Run(context.Background(), Command{Argv: []string{"echo", "a; rm -rf /tmp/nope", "$HOME", "*"}})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(res.Output)
	want := "a; rm -rf /tmp/nope $HOME *"
	if got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestShellModeInterpretsPipes(t *testing.T) {
	res, err := Run(context.Background(), Command{
		Argv:  []string{"printf 'b\\na\\nc\\n' | sort | tr '\\n' ' '"},
		Shell: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Output) != "a b c" {
		t.Errorf("output = %q, want %q", res.Output, "a b c")
	}
}

func TestShellModeRejectsMultipleArguments(t *testing.T) {
	_, err := Run(context.Background(), Command{Argv: []string{"echo", "hi"}, Shell: true})
	if err == nil {
		t.Fatal("shell mode takes a single script string")
	}
}

// A timeout must reach the whole process group. Killing only the direct child
// leaves the compiler or test runner it spawned holding the workspace.
func TestTimeoutKillsDescendants(t *testing.T) {
	res, err := Run(context.Background(), Command{
		Argv:    []string{"sleep 60 & echo CHILD:$!; wait"},
		Shell:   true,
		Timeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatalf("expected a timeout, got %+v", res)
	}
	if res.ExitCode != -1 {
		t.Errorf("exit code = %d, want -1 for a timeout", res.ExitCode)
	}

	_, rest, ok := strings.Cut(res.Output, "CHILD:")
	if !ok {
		t.Fatalf("test fixture did not report the grandchild pid: %q", res.Output)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0]))
	if err != nil {
		t.Fatalf("parsing grandchild pid from %q: %v", rest, err)
	}

	// Signal 0 asks whether the process exists without delivering anything.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("grandchild %d survived the timeout: the group was not signalled", pid)
}

func TestCancellationReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_, err := Run(ctx, Command{Argv: []string{"sleep", "30"}, Timeout: time.Minute})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestOutputCapKeepsBothEnds(t *testing.T) {
	res, err := Run(context.Background(), Command{
		Argv:      []string{"printf 'START'; for i in $(seq 1 4000); do printf 'xxxxxxxxxx'; done; printf 'END'"},
		Shell:     true,
		MaxOutput: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("40KB of output through a 2KB cap must report truncation")
	}
	if !strings.HasPrefix(res.Output, "START") {
		t.Error("the head of the output was lost")
	}
	if !strings.HasSuffix(res.Output, "END") {
		t.Error("the tail was lost; a compiler puts its errors there")
	}
	if !strings.Contains(res.Output, "bytes of output omitted") {
		t.Error("truncation must be visible in the output the model reads")
	}
	if len(res.Output) > 2000+200 {
		t.Errorf("captured %d bytes against a 2000 byte cap", len(res.Output))
	}
}

func TestProviderCredentialsAreNotInherited(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-not-leak")
	t.Setenv("SB_HARMLESS", "visible")

	res, err := Run(context.Background(), Command{
		Argv:  []string{"echo \"key=[${ANTHROPIC_API_KEY}] other=[${SB_HARMLESS}]\""},
		Shell: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "sk-should-not-leak") {
		t.Error("a model-requested command read the harness's provider credential")
	}
	if !strings.Contains(res.Output, "other=[visible]") {
		t.Errorf("the rest of the environment must pass through: %q", res.Output)
	}
}

func TestRunInWorkspaceDirectory(t *testing.T) {
	dir := t.TempDir()
	res, err := Run(context.Background(), Command{Argv: []string{"pwd"}, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	// macOS hands out temp directories behind a symlink, so compare resolved
	// paths rather than the literal strings.
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(res.Output); got != want {
		t.Errorf("cwd = %q, want %q", got, want)
	}
}

func TestMissingBinaryIsAnError(t *testing.T) {
	_, err := Run(context.Background(), Command{Argv: []string{"switchboard-no-such-binary"}})
	if err == nil {
		t.Fatal("expected an error for a binary that does not exist")
	}
}
