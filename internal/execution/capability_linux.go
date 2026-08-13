package execution

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Linux confinement is a namespace construction rather than a policy language.
// Instead of allowing and denying operations, bubblewrap builds a filesystem
// view: the whole tree read-only, a handful of writable binds on top, and empty
// mounts over the paths that must not be readable.
//
// Two ordering rules fall out of that and are easy to break:
//
//  1. Writable binds must come after the read-only root, and the deny mounts
//     must come after those. Mounts apply in order, so a deny placed before a
//     bind that covers the same path silently does nothing.
//  2. A deny mount can only be placed over a path that exists. bubblewrap
//     cannot create the mountpoint, because its parent is read-only by then,
//     and the whole invocation fails rather than that one flag being skipped.
//     Absent paths are dropped, which is safe: there is nothing there to hide.
func detectPlatform() Capability {
	c := Capability{Mechanism: MechanismBubblewrap}

	path, err := exec.LookPath("bwrap")
	if err != nil {
		return Capability{
			Mechanism: MechanismNone,
			Detail:    "bubblewrap is not installed; install it to enable automatic execution",
		}
	}
	c.MechanismPresent = true
	bwrapPath = path

	verified, detail := cachedVerification(linuxProfileKey(), linuxHostKey(), linuxSelfTest)
	c.Detail = detail
	if verified {
		c.confinement = &Confinement{mechanism: MechanismBubblewrap, wrap: wrapBubblewrap}
	}
	return c
}

// bwrapPath is resolved once by detectPlatform. Tests set it directly.
var bwrapPath = "bwrap"

// writableCaches are build caches granted so a second build is not cold.
//
// Granting them is also a persistence vector: a command can leave a config or a
// compiled artifact here that a later, separately approved command executes.
// Confinement is per command and is not a durable boundary between commands.
var writableCaches = []string{
	".cache",
	".npm",
	".cargo",
	".gradle",
	".m2",
	filepath.Join("go", "pkg", "mod"),
	filepath.Join(".local", "share", "virtualenvs"),
}

// hiddenPaths are covered with an empty mount. Anything not named here is
// readable, which is the same leak-by-default posture the macOS profile takes
// and carries the same open question.
var hiddenPaths = []string{
	".ssh",
	".aws",
	".kube",
	".docker",
	".gnupg",
	".config/gcloud",
	".config/gh",
	".password-store",
	".local/share/keyrings",

	// Switchboard's own logs hold prompts, diffs, and file contents from every
	// workspace on this machine.
	".switchboard",
}

// hiddenFiles are covered with /dev/null rather than an empty directory.
var hiddenFiles = []string{
	".netrc",
	".git-credentials",
	".pgpass",
}

func wrapBubblewrap(p Policy, argv []string) ([]string, error) {
	workspace, err := filepath.EvalSymlinks(p.Workspace)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	tmp := os.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}

	out := []string{
		bwrapPath,
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
	}

	// Writable roots, layered over the read-only tree.
	out = append(out, "--bind", workspace, workspace)
	if tmp != "/" {
		out = append(out, "--bind", tmp, tmp)
	}
	for _, rel := range writableCaches {
		dir := filepath.Join(home, rel)
		// --bind-try skips a cache the user does not have rather than failing
		// the whole command.
		out = append(out, "--bind-try", dir, dir)
	}

	// Deny mounts last, so they cover anything the binds above exposed.
	for _, rel := range hiddenPaths {
		dir := filepath.Join(home, filepath.FromSlash(rel))
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			out = append(out, "--tmpfs", dir)
		}
	}
	for _, rel := range hiddenFiles {
		file := filepath.Join(home, rel)
		if info, err := os.Stat(file); err == nil && !info.IsDir() {
			out = append(out, "--ro-bind", os.DevNull, file)
		}
	}
	out = append(out, agentSocketFlags()...)
	out = append(out, sessionBusFlags()...)

	out = append(out,
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--unshare-cgroup",
	)
	if p.Network != NetworkFull {
		// A fresh network namespace has a working loopback interface and no
		// route off the machine, which is exactly the contract: fixture servers
		// bind, egress is unreachable.
		out = append(out, "--unshare-net")
	}

	out = append(out,
		// The confined process must not outlive the runner that started it.
		"--die-with-parent",
		// A new session denies TIOCSTI, which would otherwise let a confined
		// process push characters into the parent's terminal.
		"--new-session",
	)

	return append(out, argv...), nil
}

// agentSocketFlags neutralizes the ssh-agent socket. Binding /dev/null over it
// is more surgical than hiding its directory, which on some systems is shared
// with unrelated sockets.
func agentSocketFlags() []string {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil
	}
	if _, err := os.Stat(sock); err != nil {
		return nil
	}
	return []string{"--ro-bind", os.DevNull, sock}
}

// sessionBusFlags hides the session bus, which is how gnome-keyring, kwallet,
// and anything else implementing the Secret Service API hand out credentials.
// It is the Linux counterpart of denying the Keychain on macOS: the files being
// unreadable accomplishes nothing while the daemon is still reachable.
func sessionBusFlags() []string {
	var flags []string
	seen := map[string]bool{}

	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		info, err := os.Stat(path)
		if err != nil {
			return
		}
		seen[path] = true
		if info.IsDir() {
			flags = append(flags, "--tmpfs", path)
		} else {
			flags = append(flags, "--ro-bind", os.DevNull, path)
		}
	}

	if addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); addr != "" {
		// The address looks like "unix:path=/run/user/1000/bus,guid=...".
		for _, part := range strings.Split(addr, ",") {
			if p, ok := strings.CutPrefix(part, "unix:path="); ok {
				add(p)
			}
			if p, ok := strings.CutPrefix(part, "path="); ok {
				add(p)
			}
		}
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		add(filepath.Join(dir, "bus"))
		add(filepath.Join(dir, "keyring"))
	}
	return flags
}

// linuxProfileKey changes whenever the constructed argument list changes, so a
// cached pass cannot survive an edit to the confinement.
func linuxProfileKey() string {
	sample, err := wrapBubblewrap(
		Policy{Workspace: os.TempDir(), Network: NetworkLoopback},
		[]string{"/probe"},
	)
	if err != nil {
		return "unbuildable"
	}
	return shortHash(strings.Join(sample, "\x00"))
}

// linuxHostKey pins the verdict to this kernel. Namespace and seccomp behavior
// is kernel-dependent, and a distribution upgrade should re-run the check.
func linuxHostKey() string {
	return commandOutput("uname", "-rm")
}
