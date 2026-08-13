package execution

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func workspaceFor(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func confined(t *testing.T) *Confinement {
	t.Helper()
	path, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bubblewrap is not installed")
	}
	bwrapPath = path

	// Unprivileged user namespaces are a kernel setting some distributions turn
	// off. Without them bubblewrap cannot build a namespace at all, which is a
	// property of the host rather than a defect in the construction.
	probe := exec.Command(path, "--ro-bind", "/", "/", "--unshare-user", "/bin/true")
	if err := probe.Run(); err != nil {
		t.Skipf("bubblewrap cannot create a namespace here: %v", err)
	}
	return &Confinement{mechanism: MechanismBubblewrap, wrap: wrapBubblewrap}
}

func runConfined(t *testing.T, ws string, network NetworkAccess, argv []string, shell bool) Result {
	t.Helper()
	res, err := Run(context.Background(), Command{
		Argv:    argv,
		Shell:   shell,
		Dir:     ws,
		Timeout: 60 * time.Second,
		Confine: confined(t),
		Policy:  Policy{Workspace: ws, Network: network},
	})
	if err != nil {
		t.Fatalf("running %v: %v", argv, err)
	}
	return res
}

func TestSelfTestPassesOnThisHost(t *testing.T) {
	confined(t)
	ok, detail := linuxSelfTest()
	if !ok {
		t.Fatalf("self-test failed on this host: %s", detail)
	}
	if !strings.Contains(detail, "verified") {
		t.Errorf("detail = %q, want it to say what was verified", detail)
	}
}

func TestConfinedWritesStayInTheWorkspace(t *testing.T) {
	ws := workspaceFor(t)

	res := runConfined(t, ws, NetworkLoopback,
		[]string{"echo confined > " + filepath.Join(ws, "inside.txt")}, true)
	if res.ExitCode != 0 {
		t.Fatalf("a write inside the workspace must succeed: %s", res.Output)
	}
	if data, err := os.ReadFile(filepath.Join(ws, "inside.txt")); err != nil || !strings.Contains(string(data), "confined") {
		t.Errorf("file = %q, err = %v", data, err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// Not t.TempDir(): that lives under $TMPDIR, which is granted on purpose
	// because build tools are unusable without it.
	for _, escape := range []string{
		filepath.Join(home, ".switchboard-write-escape-probe"),
		"/etc/switchboard-write-escape-probe",
	} {
		os.Remove(escape)
		res = runConfined(t, ws, NetworkLoopback, []string{"echo out > " + escape}, true)
		if res.ExitCode == 0 {
			t.Errorf("a write to %s succeeded", escape)
		}
		if _, err := os.Stat(escape); err == nil {
			os.Remove(escape)
			t.Errorf("the write to %s actually landed", escape)
		}
	}
}

// The counterpart of the macOS keychain finding: hiding credential files is
// pointless if the daemon handing out those credentials is still reachable.
func TestConfinedCommandCannotReadCredentials(t *testing.T) {
	ws := workspaceFor(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	canary := filepath.Join(home, ".switchboard", "cred-canary")
	if err := os.MkdirAll(filepath.Dir(canary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canary, []byte(canaryToken), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(canary)

	res := runConfined(t, ws, NetworkLoopback, []string{"/bin/cat", canary}, false)
	if strings.Contains(res.Output, canaryToken) {
		t.Error("a confined command read Switchboard's session state, which holds other projects' prompts and code")
	}

	fake := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(fake, 0o700); err == nil {
		key := filepath.Join(fake, "switchboard-test-key")
		if err := os.WriteFile(key, []byte(canaryToken), 0o600); err == nil {
			defer os.Remove(key)
			res := runConfined(t, ws, NetworkLoopback, []string{"/bin/cat", key}, false)
			if strings.Contains(res.Output, canaryToken) {
				t.Error("a confined command read a key out of ~/.ssh")
			}
		}
	}
}

// The policy mapping is checked without touching the network, so it holds on a
// host with no egress and no HTTP client. An assertion that needs the internet
// to detect a missing flag is an assertion that quietly stops running.
func TestNetworkNamespaceFlags(t *testing.T) {
	ws := workspaceFor(t)

	loopback, err := wrapBubblewrap(Policy{Workspace: ws, Network: NetworkLoopback}, []string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(loopback, "--unshare-net") {
		t.Error("the default policy must take a private network namespace")
	}

	full, err := wrapBubblewrap(Policy{Workspace: ws, Network: NetworkFull}, []string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(full, "--unshare-net") {
		t.Error("a granted-network command must keep the host's network namespace")
	}
}

func TestNetworkPolicy(t *testing.T) {
	ws := workspaceFor(t)

	probe := egressProbeArgv()
	if probe == nil {
		t.Skip("no curl, wget, or nc to attempt a connection with")
	}

	// The granted case runs first: without it, a denial proves nothing, because
	// a host with no internet refuses either way.
	if granted := runConfined(t, ws, NetworkFull, probe, false); granted.ExitCode != 0 {
		t.Skipf("this host has no egress even when granted (exit %d); nothing to compare against", granted.ExitCode)
	}
	if denied := runConfined(t, ws, NetworkLoopback, probe, false); denied.ExitCode == 0 {
		t.Error("the default policy allowed egress off the machine")
	}
}

// A private network namespace comes with a working loopback interface, so
// fixture servers bind while egress stays unreachable. That is the property the
// whole loopback policy rests on.
func TestLoopbackServersStillWork(t *testing.T) {
	ws := workspaceFor(t)

	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build the probe with")
	}
	writeProbe(t, ws)

	build := runConfined(t, ws, NetworkLoopback, []string{"go", "build", "-o", "probe", "."}, false)
	if build.ExitCode != 0 {
		t.Fatalf("building the probe under the sandbox failed: %s", build.Output)
	}

	res := runConfined(t, ws, NetworkLoopback, []string{filepath.Join(ws, "probe")}, false)
	if res.ExitCode != 0 {
		t.Fatalf("loopback probe failed under the sandbox: %s", res.Output)
	}
	if !strings.Contains(res.Output, "listen ok") || !strings.Contains(res.Output, "dial ok") {
		t.Errorf("probe output = %q, want both a bind and a connect to succeed", res.Output)
	}
}

func writeProbe(t *testing.T, dir string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.26\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
)

func main() {
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0"} {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			fmt.Printf("listen failed %s: %v\n", addr, err)
			return
		}
		l.Close()
	}
	fmt.Println("listen ok")

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer s.Close()
	if _, err := http.Get(s.URL); err != nil {
		fmt.Printf("dial failed: %v\n", err)
		return
	}
	fmt.Println("dial ok")
}
`), 0o644)
}

// bubblewrap puts the command in a new PID namespace where it is init, so
// killing the wrapper has to tear the namespace down with it. If that stopped
// working, a timed-out build would leave its compiler running.
func TestProcessGroupKillSurvivesTheWrap(t *testing.T) {
	ws := workspaceFor(t)
	marker := filepath.Join(ws, "still-running")

	res, err := Run(context.Background(), Command{
		Argv:    []string{"(while true; do touch " + marker + "; sleep 0.1; done) & wait"},
		Shell:   true,
		Dir:     ws,
		Timeout: 500 * time.Millisecond,
		Confine: confined(t),
		Policy:  Policy{Workspace: ws, Network: NetworkLoopback},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatalf("expected a timeout, got %+v", res)
	}

	// A pid from inside a PID namespace means nothing out here, so liveness is
	// measured by whether the descendant keeps touching a file after the
	// wrapper is gone.
	os.Remove(marker)
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a descendant survived the timeout and is still writing")
	}
}

func TestUnapplicableConfinementRefusesToRun(t *testing.T) {
	res, err := Run(context.Background(), Command{
		Argv:    []string{"/bin/echo", "should not run"},
		Timeout: 5 * time.Second,
		Confine: confined(t),
		Policy:  Policy{Workspace: "/nonexistent/workspace/path", Network: NetworkLoopback},
	})
	if err == nil {
		t.Fatalf("expected a refusal, got %+v", res)
	}
	if !strings.Contains(err.Error(), "refusing to run unconfined") {
		t.Errorf("err = %v, want it to say the command was not run", err)
	}
}

func TestInstalledToolchainsWorkConfined(t *testing.T) {
	if testing.Short() {
		t.Skip("toolchain matrix is slow")
	}
	ws := workspaceFor(t)

	cases := []struct {
		name  string
		bin   string
		setup func(dir string)
		argv  []string
	}{
		{"go", "go", func(d string) {
			os.WriteFile(filepath.Join(d, "go.mod"), []byte("module m\n\ngo 1.26\n"), 0o644)
			os.WriteFile(filepath.Join(d, "main.go"), []byte("package main\nfunc main(){}\n"), 0o644)
		}, []string{"go", "build", "-o", "out", "."}},

		{"node", "node", func(d string) {
			os.WriteFile(filepath.Join(d, "i.js"), []byte("console.log('ok')\n"), 0o644)
		}, []string{"node", "i.js"}},

		{"python3", "python3", func(d string) {
			os.WriteFile(filepath.Join(d, "m.py"), []byte("print('ok')\n"), 0o644)
		}, []string{"python3", "m.py"}},

		{"clang", "clang", func(d string) {
			os.WriteFile(filepath.Join(d, "m.c"), []byte("int main(){return 0;}\n"), 0o644)
		}, []string{"clang", "m.c", "-o", "m"}},

		{"git", "git", func(string) {}, []string{"git", "init", "-q", "."}},

		{"cargo", "cargo", func(d string) {
			os.MkdirAll(filepath.Join(d, "src"), 0o755)
			os.WriteFile(filepath.Join(d, "Cargo.toml"),
				[]byte("[package]\nname=\"m\"\nversion=\"0.1.0\"\nedition=\"2021\"\n"), 0o644)
			os.WriteFile(filepath.Join(d, "src", "main.rs"), []byte("fn main(){}\n"), 0o644)
		}, []string{"cargo", "build", "--offline"}},
	}

	ran := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := exec.LookPath(tc.bin); err != nil {
				t.Skipf("%s is not installed", tc.bin)
			}
			dir := filepath.Join(ws, tc.name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.setup(dir)

			res, err := Run(context.Background(), Command{
				Argv:    tc.argv,
				Dir:     dir,
				Timeout: 3 * time.Minute,
				Confine: confined(t),
				Policy:  Policy{Workspace: ws, Network: NetworkLoopback},
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.ExitCode != 0 {
				t.Errorf("%s does not work confined: %s", tc.name, res.Output)
			}
			ran++
		})
	}
	if ran == 0 {
		t.Skip("no toolchains installed to check")
	}
}
