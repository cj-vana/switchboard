package credential

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// HelperStore runs a command the user configured and takes its standard output
// as the credential.
//
// This is the second headless path in §5.3, and the one that lets a password
// manager or a cloud credential chain be the source of truth without this
// program learning about either. The contract is deliberately small:
//
//	stdout   the credential, and nothing else. Never logged, never included in
//	         an error, not even on failure.
//	stderr   diagnostics. Included in errors, so a helper must not write the
//	         credential here.
//	exit 0   success. Any other status is a configuration error that stops the
//	         chain rather than falling through, because a helper that is present
//	         and broken is not the same as no helper at all.
type HelperStore struct {
	// Command is argv, not a shell line. There is no shell, so nothing in a
	// reference or a provider name can be read as shell syntax.
	Command []string

	// Env supplies the variables named in the contract below, in addition to
	// the parent environment.
	Env []string
}

func (s *HelperStore) Name() string {
	if len(s.Command) == 0 {
		return "credential helper"
	}
	return "credential helper (" + s.Command[0] + ")"
}

func (s *HelperStore) Get(ctx context.Context, ref Ref) (Secret, error) {
	if len(s.Command) == 0 {
		return Secret{}, ErrNotFound
	}

	cmd := exec.CommandContext(ctx, s.Command[0], s.Command[1:]...)
	// The helper is told which credential is wanted, so one command can serve
	// every provider without the config repeating itself.
	cmd.Env = append(cmd.Environ(),
		"SB_CREDENTIAL_PROVIDER="+ref.Provider,
		"SB_CREDENTIAL_ACCOUNT="+ref.Account,
	)
	cmd.Env = append(cmd.Env, s.Env...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Secret{}, ctx.Err()
		}
		if execErr := new(exec.Error); errors.As(err, &execErr) {
			return Secret{}, &Unavailable{
				Store:  s.Name(),
				Reason: fmt.Sprintf("%s is not on PATH", s.Command[0]),
			}
		}
		// stdout is the credential channel and is never quoted back, including
		// here: a helper that fails partway may well have written part of a
		// secret to it.
		return Secret{}, fmt.Errorf("%s failed: %w%s", s.Name(), err, diagnostics(stderr.String()))
	}

	value := strings.TrimSpace(stdout.String())
	if value == "" {
		return Secret{}, ErrNotFound
	}
	return New(value, SourceHelper, s.Command[0]), nil
}

// diagnostics formats a helper's stderr for an error message, capped so a
// helper that dumps a page of output does not bury the failure.
func diagnostics(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	const limit = 400
	if len(stderr) > limit {
		stderr = stderr[:limit] + "..."
	}
	return ": " + strings.ReplaceAll(stderr, "\n", "; ")
}
