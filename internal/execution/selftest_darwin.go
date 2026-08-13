package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runSelfTest proves the profile confines a command on this machine, right now.
//
// It checks the security-critical direction: that things which must be refused
// are refused. A profile that merely fails to break the build is not evidence
// of anything, and a rule that silently stopped matching after an OS update
// looks exactly like a rule that works until something is asked of it.
//
// Whether real toolchains still function under the profile is the other half,
// and it is checked by the darwin-tagged tests rather than at startup, because
// it needs compilers the user may not have installed.
func runSelfTest() (bool, string) {
	workspace, err := os.MkdirTemp("", "switchboard-sandbox-check")
	if err != nil {
		return false, "could not create a directory to verify the sandbox: " + err.Error()
	}
	defer os.RemoveAll(workspace)

	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return false, "could not resolve the verification directory: " + err.Error()
	}
	if err := os.WriteFile(filepath.Join(resolved, "readable"), []byte("ok"), 0o600); err != nil {
		return false, err.Error()
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return false, err.Error()
	}
	policy := Policy{Workspace: resolved, Network: NetworkLoopback}

	escapee := filepath.Join(home, ".switchboard-sandbox-escape-probe")
	defer os.Remove(escapee)

	checks := []struct {
		name    string
		mustRun bool
		argv    []string
	}{
		{"write inside the workspace", true,
			[]string{"/bin/sh", "-c", "echo ok > " + shellQuote(filepath.Join(resolved, "probe"))}},
		{"read inside the workspace", true,
			[]string{"/bin/cat", filepath.Join(resolved, "readable")}},

		{"write into the home directory", false,
			[]string{"/bin/sh", "-c", "echo escaped > " + shellQuote(escapee)}},
		{"write into /private/tmp", false,
			[]string{"/bin/sh", "-c", "echo escaped > /private/tmp/switchboard-sandbox-escape-probe"}},
		{"read Switchboard's own session logs", false,
			[]string{"/bin/ls", filepath.Join(home, ".switchboard")}},
		{"query the keychain through securityd", false,
			[]string{"/usr/bin/security", "list-keychains"}},
		{"reach the network off this machine", false,
			[]string{"/usr/bin/curl", "-s", "-m", "3", "http://1.1.1.1", "-o", "/dev/null"}},
	}

	if os.Getenv("SSH_AUTH_SOCK") != "" {
		checks = append(checks, struct {
			name    string
			mustRun bool
			argv    []string
		}{"use the ssh agent", false, []string{"/usr/bin/ssh-add", "-l"}})
	}

	var failures []string
	for _, c := range checks {
		ran, err := probeUnderProfile(policy, c.argv)
		if err != nil {
			return false, fmt.Sprintf("could not run the %q check: %v", c.name, err)
		}
		switch {
		case c.mustRun && !ran:
			failures = append(failures, "the profile blocked something it must permit: "+c.name)
		case !c.mustRun && ran:
			failures = append(failures, "the profile permitted something it must deny: "+c.name)
		}
	}

	// A rule that stopped matching is worse than a missing rule, because the
	// profile still reads as strict. Refuse the whole thing rather than grant
	// automatic execution on a partial boundary.
	if len(failures) > 0 {
		return false, "sandbox self-test failed: " + strings.Join(failures, "; ")
	}
	if _, err := os.Stat(escapee); err == nil {
		return false, "sandbox self-test failed: a denied write still reached the home directory"
	}
	return true, fmt.Sprintf("seatbelt profile verified against %d checks on this host", len(checks))
}

// probeUnderProfile reports whether the command succeeded under the profile. A
// non-zero exit counts as refused, which is what the kernel gives a denied
// syscall through these particular binaries.
func probeUnderProfile(policy Policy, argv []string) (bool, error) {
	wrapped, err := wrapSeatbelt(policy, argv)
	if err != nil {
		return false, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, wrapped[0], wrapped[1:]...)
	cmd.Env = childEnv()
	err = cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false, nil
	}
	return false, err
}

// shellQuote wraps a path for /bin/sh in the self-test. The paths are ones this
// process just created, so this is hygiene rather than a boundary.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
