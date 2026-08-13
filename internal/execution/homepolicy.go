package execution

import (
	"os"
	"path/filepath"
	"runtime"
)

// The home directory is denied wholesale and opened back up only where a
// toolchain needs it. Everywhere else on the filesystem stays broadly readable.
//
// The split is deliberate. System directories hold no user secrets, and an
// allowlist over them would break every compiler for no gain. Home is the
// opposite: it is where credentials actually live, and enumerating what leaks
// there is a race nobody wins. A survey of one ordinary developer machine found
// 51 top-level entries, of which a hand-written deny list covered six; the
// readable remainder included an npm auth token, shell history, and the
// credential directories of five different CLI tools.
//
// So reads follow the risk. Broad where secrets are not, closed where they are.

// homeReadable lists paths under the home directory a confined command may
// read. A version manager keeps the actual compiler under home, so denying
// these removes the tool rather than protecting anything.
var homeReadable = []string{
	// Build caches. These are also granted for writing.
	".cache",
	".npm",
	".cargo",
	filepath.Join("go", "pkg", "mod"),

	// Toolchains installed per-user.
	".rustup",
	".nvm",
	".asdf",
	".pyenv",
	".rbenv",
	".bun",
	".deno",
	".volta",
	".sdkman",
	".ghcup",
	".local",

	// Configuration a build legitimately reads.
	".gitconfig",
	filepath.Join(".config", "git"),
}

// homeSecrets are denied even though they sit inside something homeReadable
// opened. A toolchain directory is not uniformly safe: cargo keeps registry
// tokens beside its package cache, and the XDG data directory holds the Linux
// keyring beside legitimately shared files.
var homeSecrets = []string{
	filepath.Join(".cargo", "credentials"),
	filepath.Join(".cargo", "credentials.toml"),
	filepath.Join(".config", "git", "credentials"),
	filepath.Join(".local", "share", "keyrings"),
	filepath.Join(".local", "share", "containers", "auth.json"),
}

// platformHomeReadable adds what a specific system puts under home. macOS keeps
// the Go build cache inside Library, which the shared list would otherwise
// leave denied.
func platformHomeReadable() []string {
	if runtime.GOOS == "darwin" {
		return []string{filepath.Join("Library", "Caches", "go-build")}
	}
	return nil
}

// existingHomePaths resolves a relative list against home and drops what is not
// there. Both mechanisms reject a rule naming a path that does not exist, and a
// path that is absent has nothing to protect or to open.
func existingHomePaths(home string, rels []string) []string {
	var out []string
	for _, rel := range rels {
		p := filepath.Join(home, rel)
		if _, err := os.Lstat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func readableHomePaths(home string) []string {
	return existingHomePaths(home, append(append([]string{}, homeReadable...), platformHomeReadable()...))
}

func secretHomePaths(home string) []string {
	return existingHomePaths(home, homeSecrets)
}
