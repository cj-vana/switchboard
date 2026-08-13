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

// writableCaches are build caches granted so a second build is not cold. The
// list holds what has actually been exercised under confinement; extending it
// is documented in docs/sandbox.md.
//
// Granting them is also a persistence vector: a command can leave a config or a
// compiled artifact here that a later, separately approved command executes.
// Confinement is per command and is not a durable boundary between commands.
var writableCaches = []string{
	// The XDG base, which Go, pip, and uv all build on.
	".cache",
	".npm",
	".cargo",
	filepath.Join("go", "pkg", "mod"),
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

	// An empty mount over the home directory closes it wholesale. Everything
	// below reopens only what a build needs, so a credential file nobody
	// thought to enumerate is denied by default rather than by luck.
	out = append(out, "--tmpfs", home)

	// Readable, but not writable: toolchains a version manager installed.
	caches := map[string]bool{}
	for _, rel := range writableCaches {
		caches[filepath.Join(home, rel)] = true
	}
	for _, path := range readableHomePaths(home) {
		if !caches[path] {
			out = append(out, "--ro-bind", path, path)
		}
	}

	// Build caches, writable. The directory has to exist before it can be
	// bound: --bind-try would skip a missing one, and the tool inside cannot
	// create it either, so a user who has never run a build would meet
	// "mkdir ~/.cache: read-only file system" on their first confined command.
	for _, rel := range writableCaches {
		dir := filepath.Join(home, rel)
		os.MkdirAll(dir, 0o700)
		out = append(out, "--bind-try", dir, dir)
	}

	// The workspace usually sits inside home, so it comes after the tmpfs.
	out = append(out, "--bind", workspace, workspace)
	if tmp != "/" && !strings.HasPrefix(tmp, home+string(filepath.Separator)) {
		out = append(out, "--bind", tmp, tmp)
	}

	// Secrets that live inside a directory just reopened: cargo keeps registry
	// tokens beside its package cache, and the XDG data directory holds the
	// keyring beside legitimately shared files.
	for _, path := range secretHomePaths(home) {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			out = append(out, "--tmpfs", path)
		} else if err == nil {
			out = append(out, "--ro-bind", os.DevNull, path)
		}
	}

	out = append(out, agentSocketFlags()...)
	out = append(out, sessionBusFlags()...)

	// A tmpfs is writable, so without this the home directory would accept
	// writes into a filesystem that evaporates: the real home stays untouched,
	// but the command sees success and a later one finds nothing. Remounting
	// read-only turns that into the refusal it should have been.
	//
	// It has to come after every mount placed inside home. Earlier, and
	// bubblewrap cannot create the mountpoints for them.
	out = append(out, "--remount-ro", home)

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
