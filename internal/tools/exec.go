package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

const maxExecTimeout = 10 * time.Minute

type execTool struct{ r *Registry }

func (t *execTool) Name() string { return "exec" }

func (t *execTool) Description() string {
	return "Run a command with the session's current execution reach. Sandbox is off by default, " +
		"so an approved command can access the host filesystem outside the workspace and the network. " +
		"When a verified sandbox is active, writes are limited to the workspace, temp, and build caches; broad system and outside-home paths remain readable, and network requests are gated. " +
		"By default the command array is executed directly, " +
		"with no shell, so quoting, globs, pipes, and variables are not interpreted. " +
		"Set shell to true to run through /bin/sh, and then pass the whole script as a " +
		"single element. Combined stdout and stderr are returned; long output has its " +
		"middle removed and says so."
}

func (t *execTool) ParallelSafe() bool { return false }

func (t *execTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Program and arguments, for example [\"go\",\"test\",\"./...\"]. When shell is true, pass exactly one element holding the whole script, for example [\"grep -r foo . | head -20\"]."
    },
    "shell": {"type": "boolean", "description": "Run the single command element through /bin/sh. Only needed for pipes, redirection, or expansion."},
	"network": {"type": "boolean", "description": "Request internet access when a sandbox is active. With the default sandbox-off posture, approved commands already have the host's full network reach regardless of this hint."},
    "timeout_seconds": {"type": "integer", "description": "Wall-clock limit. Defaults to 120."}
  },
  "required": ["command"]
}`)
}

type execInput struct {
	Command        []string `json:"command"`
	Shell          bool     `json:"shell"`
	Network        bool     `json:"network"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

func (t *execTool) Plan(input json.RawMessage) (Plan, error) {
	var in execInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("exec: %w", err)
	}
	if len(in.Command) == 0 {
		return Plan{}, fmt.Errorf("exec: command is empty")
	}
	if in.Shell && len(in.Command) != 1 {
		// The model met this twice in one session and corrected twice, which
		// means the sentence it read was not the shape it needed. An error
		// that shows the call is one it can act on without a second attempt.
		return Plan{}, fmt.Errorf(
			`exec: shell mode takes the whole script as one element, got %d; `+
				`join them: {"command": [%q], "shell": true}`,
			len(in.Command), strings.Join(in.Command, " "))
	}

	timeout := time.Duration(in.TimeoutSeconds) * time.Second
	if in.TimeoutSeconds <= 0 {
		timeout = execution.DefaultTimeout
	}
	if timeout > maxExecTimeout {
		return Plan{}, fmt.Errorf("exec: timeout_seconds %d exceeds the %s ceiling",
			in.TimeoutSeconds, maxExecTimeout)
	}

	policy := t.r.execution.CommandPolicy(in.Network)
	requestPolicy := policy
	runPolicy := policy
	// Keep the auditable request and the executable closure on distinct
	// backing arrays. Permission/reviewer code may inspect Request.Argv, but
	// cannot rewrite what Run will execute after approval.
	runArgv := append([]string(nil), in.Command...)
	requestArgv := append([]string(nil), runArgv...)
	return Plan{
		Request: permission.Request{
			Tool:      t.Name(),
			Effect:    permission.EffectExecute,
			Path:      ".", // command working directory, relative to the workspace
			Argv:      requestArgv,
			Shell:     in.Shell,
			Network:   in.Network,
			Execution: &requestPolicy,
		},
		Run: func(ctx context.Context) (Result, error) {
			release, err := t.r.execution.Hold(runPolicy, in.Network)
			if err != nil {
				return errorf("exec: %v", err)
			}
			defer release()
			res, err := execution.Run(ctx, execution.Command{
				Argv:    runArgv,
				Shell:   in.Shell,
				Dir:     t.r.root,
				Timeout: timeout,
				// The confinement and the permission decision come from one
				// capability, so a command approved as contained cannot then run
				// unconfined.
				Confine: runPolicy.Confinement,
				Policy: execution.Policy{
					Workspace: t.r.root,
					Network:   runPolicy.Network,
				},
			})
			if err != nil {
				// A context error is the user cancelling, which the loop handles
				// as a cancellation rather than a tool failure.
				if ctx.Err() != nil {
					return Result{}, err
				}
				return errorf("could not run %s: %v", Describe(in.Command, in.Shell), err)
			}
			return execResult(res), nil
		},
	}, nil
}

// execResult renders a command's outcome. A timeout is reported as a tool error
// rather than aborting the turn: the model is the one who chose the command and
// is best placed to decide whether to narrow it, retry, or give up (§10.3).
func execResult(res execution.Result) Result {
	var b strings.Builder
	if res.Output != "" {
		b.WriteString(res.Output)
		if !strings.HasSuffix(res.Output, "\n") {
			b.WriteByte('\n')
		}
	}

	switch {
	case res.TimedOut:
		fmt.Fprintf(&b, "[timed out after %s; the process group was terminated]", res.Duration.Round(time.Millisecond))
		return Result{Content: b.String(), IsError: true}
	case res.ExitCode != 0:
		fmt.Fprintf(&b, "[exit status %d]", res.ExitCode)
		return Result{Content: b.String(), IsError: true}
	}

	if b.Len() == 0 {
		return Result{Content: "[no output, exit status 0]"}
	}
	return Result{Content: strings.TrimSuffix(b.String(), "\n")}
}

// Describe renders a command for a permission prompt or a log line. It is not a
// quoting round trip and must never be fed back to a shell.
func Describe(argv []string, shell bool) string {
	if shell {
		return "sh -c " + strconv.Quote(strings.Join(argv, " "))
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		escaped := terminaltext.Escape(a)
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\$`;|&()<>*?[]{}!~") {
			quoted[i] = strconv.Quote(escaped)
		} else {
			quoted[i] = escaped
		}
	}
	return strings.Join(quoted, " ")
}
