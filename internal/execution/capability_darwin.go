package execution

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// seatbeltProfile is the confinement policy. Its comments explain each rule and
// docs/sandbox-macos.md records how the rules were arrived at.
//
//go:embed seatbelt.sb
var seatbeltProfile string

// sandboxExec is the Seatbelt front end. It has carried a deprecation warning
// in the headers since 10.8 and is still what Apple's own software and Chromium
// use, so the posture is to depend on it while keeping per-action approval as
// the always-available path (see docs/sandbox-macos.md).
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

	verified, detail := confinementVerified()
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

// profileText appends the egress grant for a command the user explicitly gave
// network access to. It goes after the deny block, which only covers reads, so
// nothing above is weakened.
func profileText(p Policy) string {
	if p.Network == NetworkFull {
		return seatbeltProfile + "\n; Egress granted explicitly for this command.\n(allow network*)\n"
	}
	return seatbeltProfile
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

// checkRecord caches a self-test verdict. It is keyed by the profile and the OS
// build, because a profile edit or an OS update can change what the kernel
// enforces and neither should inherit an old pass.
type checkRecord struct {
	ProfileHash string    `json:"profile_hash"`
	OSBuild     string    `json:"os_build"`
	Verified    bool      `json:"verified"`
	Detail      string    `json:"detail"`
	CheckedAt   time.Time `json:"checked_at"`
}

func confinementVerified() (bool, string) {
	hash := sha256.Sum256([]byte(seatbeltProfile))
	key := hex.EncodeToString(hash[:8])
	build := osBuild()

	path, err := checkCachePath()
	if err == nil {
		if rec, readErr := readCheck(path); readErr == nil &&
			rec.ProfileHash == key && rec.OSBuild == build {
			return rec.Verified, rec.Detail
		}
	}

	verified, detail := runSelfTest()
	if path != "" {
		writeCheck(path, checkRecord{
			ProfileHash: key,
			OSBuild:     build,
			Verified:    verified,
			Detail:      detail,
			CheckedAt:   time.Now().UTC(),
		})
	}
	return verified, detail
}

func osBuild() string {
	out, err := exec.Command("/usr/bin/sw_vers", "-buildVersion").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func checkCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".switchboard")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "sandbox-check.json"), nil
}

func readCheck(path string) (checkRecord, error) {
	var rec checkRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return rec, err
	}
	return rec, json.Unmarshal(data, &rec)
}

func writeCheck(path string, rec checkRecord) {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(path, data, 0o600)
}
