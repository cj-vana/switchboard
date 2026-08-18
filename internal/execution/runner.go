package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	DefaultTimeout   = 2 * time.Minute
	DefaultMaxOutput = 64 << 10

	// terminateGrace is how long a process group gets to exit on SIGTERM before
	// SIGKILL. Long enough for a test runner to flush, short enough that a
	// cancelled turn returns promptly.
	terminateGrace = 2 * time.Second

	// reapTimeout bounds the wait after a group kill. A descendant that survives
	// SIGKILL is stuck in the kernel, and blocking on it forever would hang the
	// agent instead of the command.
	reapTimeout = 2 * time.Second
)

// Command is one execution request. Shell mode is a separate field rather than
// an argv convention so permission rules can match on it: a shell string is
// untrusted model output that gets word splitting, expansion, and redirection,
// and that is a materially different request from running a binary directly.
type Command struct {
	Argv      []string
	Shell     bool
	Dir       string
	Timeout   time.Duration
	MaxOutput int

	// Confine, when non-nil, confines the command. It comes from
	// Capability.Confinement, which is the same value that decides whether
	// automatic execution was allowed, so a command cannot be approved as
	// contained and then run unconfined.
	//
	// If it is set and cannot be applied, Run fails. It never falls back to
	// running the command unconfined.
	Confine *Confinement
	Policy  Policy

	// ExtraEnv appends to the hygienic child environment. It exists for
	// hook payloads; it is not a way back in for the credential variables
	// childEnv strips, so those names are rejected here too.
	ExtraEnv []string

	// Stdin, when non-empty, is fed to the child's standard input. The
	// default remains a closed stdin: a command that waits for input would
	// otherwise wait forever.
	Stdin []byte
}

type Result struct {
	Output    string
	ExitCode  int
	TimedOut  bool
	Truncated bool
	Duration  time.Duration
}

// providerCredentialVars hold the keys the harness itself authenticates with.
// They are dropped from the child environment so a model-requested command
// cannot read them back out.
//
// This is credential hygiene and explicitly not containment. Any allowed
// interpreter, package manager, or compiler can still read a credential from
// disk, so the sandbox and the permission model remain the boundary (§10.2).
var providerCredentialVars = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"OPENAI_API_KEY",
	"OPENAI_ORG_ID",
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"KIMI_API_KEY",
	"SWITCHBOARD_TOKEN",
}

func credentialEnvKey(key string) bool {
	upper := strings.ToUpper(strings.TrimSpace(key))
	for _, known := range providerCredentialVars {
		if upper == known {
			return true
		}
	}
	// Env-backed provider references include dynamic
	// SB_<PROVIDER>[_<ACCOUNT>]_API_KEY names. These markers also cover a new
	// provider's conventional variable before the static list is updated.
	for _, marker := range []string{"_API_KEY", "_AUTH_TOKEN", "_ACCESS_TOKEN", "_SESSION_TOKEN", "_PASSWORD", "_PASSWD", "_PRIVATE_KEY", "_CREDENTIAL", "_CLIENT_SECRET"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

func Run(ctx context.Context, c Command) (Result, error) {
	if len(c.Argv) == 0 {
		return Result{}, errors.New("no command given")
	}
	if c.Shell && len(c.Argv) != 1 {
		return Result{}, fmt.Errorf("shell mode takes one script string, got %d arguments", len(c.Argv))
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxOutput <= 0 {
		c.MaxOutput = DefaultMaxOutput
	}

	name, args := c.Argv[0], c.Argv[1:]
	if c.Shell {
		name, args = shellCommand(c.Argv[0])
	}

	if c.Confine != nil {
		wrapped, err := c.Confine.apply(c.Policy, append([]string{name}, args...))
		if err != nil {
			// Failing closed is the whole point. A sandbox that quietly falls
			// back to running the command is worse than no sandbox, because the
			// UI goes on reporting containment.
			return Result{}, fmt.Errorf("refusing to run unconfined: %w", err)
		}
		name, args = wrapped[0], wrapped[1:]
	}

	runCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	// exec.CommandContext is deliberately not used: it kills only the direct
	// child, which leaves a shell's descendants running and holding the output
	// pipe open. The whole group has to go.
	cmd := exec.Command(name, args...)
	cmd.Dir = c.Dir
	envNetwork := NetworkFull
	if c.Confine != nil && c.Policy.Network == NetworkLoopback {
		envNetwork = NetworkLoopback
	}
	cmd.Env = commandEnv(envNetwork, c.ExtraEnv)
	if len(c.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	setProcessGroup(cmd)

	out := newCapture(c.MaxOutput)
	cmd.Stdout = out
	cmd.Stderr = out

	started := time.Now()
	if err := runCtx.Err(); err != nil {
		return Result{Duration: time.Since(started)}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{Duration: time.Since(started)}, err
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	var timedOut bool
	select {
	case waitErr = <-done:
	case <-runCtx.Done():
		timedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
		terminateGroup(cmd, terminateGrace)
		select {
		case waitErr = <-done:
		case <-time.After(reapTimeout):
			waitErr = errors.New("process group did not exit after SIGKILL")
		}
	}

	text, truncated := out.String()
	res := Result{
		Output:    text,
		TimedOut:  timedOut,
		Truncated: truncated,
		Duration:  time.Since(started),
	}

	switch {
	case timedOut:
		res.ExitCode = -1
	case waitErr == nil:
		res.ExitCode = 0
	default:
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
		} else {
			// The process ran but could not be reaped normally. Report it as a
			// failure with the output intact rather than discarding the turn.
			res.ExitCode = -1
			res.Output = appendLine(res.Output, "switchboard: "+waitErr.Error())
		}
	}

	// A cancelled turn is the user's decision, not a command failure.
	if ctx.Err() != nil && !timedOut {
		return res, ctx.Err()
	}
	return res, nil
}

func commandEnv(network NetworkAccess, extra []string) []string {
	keep := func(kv string) bool {
		key, _, ok := strings.Cut(kv, "=")
		if ok && credentialEnvKey(key) {
			return false
		}
		// macOS Seatbelt permits host loopback so test fixtures work. An
		// inherited HTTP(S) proxy on 127.0.0.1 would otherwise turn that into
		// off-machine egress without the separate network grant. Strip proxy-
		// related variables, including tool-specific variants and ExtraEnv,
		// whenever loopback is the effective confined policy.
		if network == NetworkLoopback && strings.Contains(strings.ToUpper(key), "PROXY") {
			return false
		}
		return true
	}

	env := os.Environ()
	kept := make([]string, 0, len(env)+len(extra))
	for _, kv := range env {
		if keep(kv) {
			kept = append(kept, kv)
		}
	}
	for _, kv := range extra {
		if keep(kv) {
			kept = append(kept, kv)
		}
	}
	return kept
}

func childEnv() []string { return commandEnv(NetworkFull, nil) }

func appendLine(s, line string) string {
	if s == "" {
		return line
	}
	if strings.HasSuffix(s, "\n") {
		return s + line
	}
	return s + "\n" + line
}
