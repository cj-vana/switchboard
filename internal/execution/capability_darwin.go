package execution

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// seatbeltProfile is the confinement policy. Its comments explain each rule and
// docs/sandbox.md records how the rules were arrived at.
//
//go:embed seatbelt.sb
var seatbeltProfile string

// sandboxExec is the Seatbelt front end. It has carried a deprecation warning
// in the headers since 10.8 and is still what Apple's own software and Chromium
// use, so the posture is to depend on it while keeping per-action approval as
// the always-available path (see docs/sandbox.md).
const sandboxExec = "/usr/bin/sandbox-exec"

func detectPlatform() Capability {
	c := Capability{Mechanism: MechanismSeatbelt}

	if _, err := os.Stat(sandboxExec); err != nil {
		return Capability{
			Mechanism: MechanismNone,
			Detail:    "sandbox-exec is not present on this system",
		}
	}
	c.MechanismPresent = true

	verified, detail := cachedVerification(darwinProfileKey(), darwinHostKey(), darwinSelfTest)
	c.Detail = detail
	if verified {
		c.confinement = &Confinement{mechanism: MechanismSeatbelt, wrap: wrapSeatbelt}
	}
	return c
}

// wrapSeatbelt turns a command into the same command under the profile.
//
// Parameters are passed as separate argv elements rather than substituted into
// the profile text, so a workspace path containing a quote or a parenthesis
// cannot rewrite the policy.
func wrapSeatbelt(p Policy, argv []string) ([]string, error) {
	params, err := profileParams(p)
	if err != nil {
		return nil, err
	}

	out := []string{sandboxExec, "-p", profileText(p)}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, "-D", k+"="+params[k])
	}
	return append(out, argv...), nil
}

// profileText appends the rules that cannot be expressed with a fixed set of
// parameters, because their number depends on what exists on this machine.
//
// Everything here is emitted after the static profile, and later rules win, so
// this section closes the home directory rather than opening anything the base
// profile denied.
func profileText(p Policy) string {
	var b strings.Builder
	b.WriteString(seatbeltProfile)

	if home, err := os.UserHomeDir(); err == nil {
		if resolved, err := filepath.EvalSymlinks(home); err == nil {
			home = resolved
		}
		b.WriteString("\n;; Home is denied wholesale, then reopened where a toolchain needs it.\n")
		fmt.Fprintf(&b, "(deny file-read* (subpath %s))\n", seatbeltString(home))

		// The workspace is usually inside home, so it has to come back first.
		if ws, err := filepath.EvalSymlinks(p.Workspace); err == nil {
			fmt.Fprintf(&b, "(allow file-read* (subpath %s))\n", seatbeltString(ws))
		}
		for _, path := range readableHomePaths(home) {
			fmt.Fprintf(&b, "(allow file-read* (subpath %s))\n", seatbeltString(path))
		}
		// Denied last, because some of them sit inside the paths just opened.
		for _, path := range secretHomePaths(home) {
			fmt.Fprintf(&b, "(deny file-read* (subpath %s))\n", seatbeltString(path))
		}
	}

	if p.Network == NetworkFull {
		b.WriteString("\n; Egress granted explicitly for this command.\n(allow network*)\n")
	}
	return b.String()
}

// seatbeltString renders a path as a profile string literal.
//
// Parameters passed with -D cannot be used for these rules because their count
// varies by machine, so the path goes into the policy text and has to be
// escaped. A workspace directory named with a quote would otherwise close the
// literal and let the rest of the name be read as policy.
func seatbeltString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func profileParams(p Policy) (map[string]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	// Resolving the home directory once covers every path built from it. Every
	// parameter must be fully resolved, because Seatbelt resolves a path before
	// matching and a rule naming an unresolved symlink never fires.
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return nil, fmt.Errorf("resolving home directory: %w", err)
	}

	workspace, err := filepath.EvalSymlinks(p.Workspace)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace %s: %w", p.Workspace, err)
	}
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return nil, fmt.Errorf("resolving temp directory: %w", err)
	}

	under := func(rest ...string) string {
		return filepath.Join(append([]string{home}, rest...)...)
	}

	return map[string]string{
		"WORKSPACE": workspace,
		"TMPDIR":    tmp,

		"CACHE_GO_BUILD": under("Library", "Caches", "go-build"),
		"CACHE_GO_MOD":   under("go", "pkg", "mod"),
		"CACHE_NPM":      under(".npm"),
		"CACHE_CARGO":    under(".cargo"),
		"CACHE_XDG":      under(".cache"),
		"CACHE_LIBRARY":  under("Library", "Caches"),

		"DENY_SSH":           under(".ssh"),
		"DENY_AWS":           under(".aws"),
		"DENY_GCLOUD":        under(".config", "gcloud"),
		"DENY_KUBE":          under(".kube"),
		"DENY_DOCKER":        under(".docker"),
		"DENY_GNUPG":         under(".gnupg"),
		"DENY_KEYCHAINS":     under("Library", "Keychains"),
		"DENY_KEYCHAINS_SYS": "/Library/Keychains",
		"DENY_CONFIG_SSH":    under(".config", "ssh"),
		"DENY_SWITCHBOARD":   under(".switchboard"),
	}, nil
}

// darwinProfileKey covers the effective profile, not just the embedded file.
// The generated section depends on which toolchain directories exist, so a
// cached pass must not survive the user installing one.
func darwinProfileKey() string {
	return shortHash(profileText(Policy{Workspace: os.TempDir(), Network: NetworkLoopback}))
}

// darwinHostKey pins the verdict to this OS build. What the kernel enforces for
// a given profile can change across releases.
func darwinHostKey() string {
	return commandOutput("/usr/bin/sw_vers", "-buildVersion")
}
