//go:build darwin || linux

package execution

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The home directory is closed by default rather than by enumeration. This is
// the property the deny-list posture could not provide: a survey of one
// ordinary machine found 51 top-level entries in home, of which a hand-written
// deny list covered six, leaving an npm auth token, shell history, and five
// CLI tools' credential directories readable by any confined command.
//
// Each name below is one that no list mentions.
func TestUnlistedHomeFilesAreNotReadable(t *testing.T) {
	ws := workspaceFor(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	names := []string{
		".npmrc",                   // registry auth tokens
		".sb-test-unlisted-secret", // a name nothing could have anticipated
		".config/sb-test-tool/credentials",
	}

	for _, name := range names {
		path := filepath.Join(home, filepath.FromSlash(name))
		if _, err := os.Lstat(path); err == nil {
			// Never write over something the user already has.
			res := runConfined(t, ws, NetworkLoopback, []string{"/bin/cat", path}, false)
			if res.ExitCode == 0 && len(res.Output) > 0 {
				t.Errorf("%s is readable from inside the sandbox", name)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Skipf("cannot stage %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(canaryToken), 0o600); err != nil {
			t.Skipf("cannot stage %s: %v", name, err)
		}
		t.Cleanup(func() { os.Remove(path) })

		res := runConfined(t, ws, NetworkLoopback, []string{"/bin/cat", path}, false)
		if res.ExitCode == 0 {
			t.Errorf("reading %s from inside the sandbox succeeded", name)
		}
		if strings.Contains(res.Output, canaryToken) {
			t.Errorf("%s leaked its contents into the sandbox", name)
		}
	}
}

// A build cache and the workspace have to stay reachable, or the closure is
// just a broken sandbox rather than a tighter one.
func TestAllowlistedHomePathsStillWork(t *testing.T) {
	ws := workspaceFor(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	cache := filepath.Join(home, ".cache", "sb-test-readback")
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		t.Skipf("cannot stage a cache file: %v", err)
	}
	if err := os.WriteFile(cache, []byte("cache-contents"), 0o600); err != nil {
		t.Skipf("cannot stage a cache file: %v", err)
	}
	defer os.Remove(cache)

	if res := runConfined(t, ws, NetworkLoopback, []string{"/bin/cat", cache}, false); res.ExitCode != 0 {
		t.Errorf("the build cache is unreadable from inside the sandbox: %s", res.Output)
	}

	probe := filepath.Join(ws, "in-workspace")
	if err := os.WriteFile(probe, []byte("workspace-contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := runConfined(t, ws, NetworkLoopback, []string{"/bin/cat", probe}, false); res.ExitCode != 0 {
		t.Errorf("the workspace is unreadable from inside the sandbox: %s", res.Output)
	}
}
