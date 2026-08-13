package execution

import (
	"os"
	"path/filepath"
)

// darwinSelfTest proves the Seatbelt profile confines a command on this
// machine, right now.
//
// It checks the security-critical direction, that things which must be refused
// are refused, plus enough of the allowed direction to catch a profile that
// simply breaks everything. Whether real toolchains still work is checked by
// the darwin-tagged tests rather than at startup, since it needs compilers the
// user may not have installed.
func darwinSelfTest() (bool, string) {
	env, cleanup, err := newSelfTestEnv()
	if err != nil {
		return false, err.Error()
	}
	defer cleanup()

	policy := Policy{Workspace: env.Workspace, Network: NetworkLoopback}

	cases := []selfTestCase{
		{
			name:    "write inside the workspace",
			mustRun: true,
			argv:    []string{"/bin/sh", "-c", "echo ok > " + shellQuote(filepath.Join(env.Workspace, "probe"))},
		},
		{
			name:    "read inside the workspace",
			mustRun: true,
			argv:    []string{"/bin/cat", filepath.Join(env.Workspace, "readable")},
		},
		{
			name: "write into the home directory",
			argv: []string{"/bin/sh", "-c", "echo escaped > " + shellQuote(env.Escape)},
		},
		{
			name: "write into /private/tmp",
			argv: []string{"/bin/sh", "-c", "echo escaped > /private/tmp/switchboard-sandbox-escape-probe"},
		},
		{
			// Reads the file directly rather than listing the directory, so an
			// empty listing cannot be mistaken for a working deny.
			name:          "read Switchboard's own session state",
			argv:          []string{"/bin/cat", env.Canary},
			mustNotOutput: canaryToken,
		},
		{
			// securityd answers over mach IPC whether or not the keychain files
			// are readable, so this is the check that catches a mach-lookup
			// grant reopening the credential store.
			name: "query the keychain through securityd",
			argv: []string{"/usr/bin/security", "list-keychains"},
		},
		{
			name: "reach the network off this machine",
			argv: []string{"/usr/bin/curl", "-s", "-m", "3", "http://1.1.1.1", "-o", "/dev/null"},
		},
	}

	if os.Getenv("SSH_AUTH_SOCK") != "" {
		cases = append(cases, selfTestCase{
			name: "use the ssh agent",
			argv: []string{"/usr/bin/ssh-add", "-l"},
		})
	}

	ok, detail := runSelfTestCases(policy, wrapSeatbelt, cases)
	if ok {
		if _, err := os.Stat(env.Escape); err == nil {
			return false, "sandbox self-test failed: a denied write still reached the home directory"
		}
	}
	return ok, detail
}
