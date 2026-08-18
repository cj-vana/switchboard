package main

import (
	"context"
	"os/exec"

	"github.com/switchboard-code/switchboard/internal/childenv"
)

// sanitizedCommand is the common launch boundary for user-configured editors
// and other untrusted interactive children. Environment filtering is
// credential hygiene only; it does not confine the process.
func sanitizedCommand(name string, args ...string) *exec.Cmd {
	return sanitizedCommandContext(context.Background(), name, args...)
}

func sanitizedCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = childenv.Current()
	return command
}
