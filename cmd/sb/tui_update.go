package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/config"
)

// Update machinery (§18). The check names nothing but the running version and
// is separate from telemetry; [updates] check = false or SB_NO_UPDATE_CHECK=1
// turns it off.
//
// Releases are expected to publish, per tag, one archive per platform named
// sb_<version>_<goos>_<goarch>.tar.gz containing the sb binary, plus a
// checksums.txt of sha256 sums. The checksum is verified before anything is
// replaced. §18 additionally calls for signed update metadata: that needs a
// signing key in the release pipeline, which does not exist yet, so this
// verifies integrity only. A checksum served beside a compromised binary is
// not authenticity, and this comment is not a substitute for fixing that.
var (
	// version is set at release time: -ldflags "-X main.version=v0.3.0".
	version = "dev"

	// updateRepo is the GitHub owner/repo releases are fetched from.
	updateRepo = "cj-vana/switchboard"
)

// currentVersion is the release version, or "" for a dev build, which has
// nothing meaningful to compare against.
func currentVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return ""
}

var updateHTTP = &http.Client{Timeout: 8 * time.Second}

type ghRelease struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// fetchLatest resolves the newest release the channel accepts. Stable asks
// GitHub's /latest, which already excludes prereleases; beta has to list and
// choose, because "latest including prereleases" is not an endpoint.
func fetchLatest(ctx context.Context, channel string) (*ghRelease, error) {
	if channel != "beta" {
		return fetchJSON[ghRelease](ctx, "https://api.github.com/repos/"+updateRepo+"/releases/latest")
	}
	releases, err := fetchJSON[[]ghRelease](ctx, "https://api.github.com/repos/"+updateRepo+"/releases?per_page=20")
	if err != nil {
		return nil, err
	}
	var best *ghRelease
	for i := range *releases {
		rel := &(*releases)[i]
		if best == nil || newerVersion(rel.TagName, best.TagName) {
			best = rel
		}
	}
	if best == nil {
		return nil, errNoRelease
	}
	return best, nil
}

func fetchJSON[T any](ctx context.Context, url string) (*T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "switchboard/"+version)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := updateHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release check answered %s", resp.Status)
	}
	var out T
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

var errNoRelease = errors.New("no releases published yet")

// newerVersion reports whether candidate outranks current under semver
// precedence. Prerelease ordering matters because the beta channel moves
// v0.4.0-beta.1 → v0.4.0-beta.2 → v0.4.0, and a comparison that strips the
// suffix would refuse the second step and repeat the third forever.
func newerVersion(candidate, current string) bool {
	c, ok1 := parseSemver(candidate)
	u, ok2 := parseSemver(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range c.core {
		if c.core[i] != u.core[i] {
			return c.core[i] > u.core[i]
		}
	}
	// Equal cores: a release outranks any prerelease of it.
	if c.pre == "" || u.pre == "" {
		return c.pre == "" && u.pre != ""
	}
	return comparePrerelease(c.pre, u.pre) > 0
}

type semver struct {
	core [3]int
	pre  string
}

func parseSemver(v string) (semver, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	var out semver
	v, out.pre, _ = strings.Cut(v, "-")
	parts := strings.Split(v, ".")
	if len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out.core[i] = n
	}
	return out, true
}

// comparePrerelease is semver §11.4: dot-separated identifiers, numeric ones
// compared numerically and ranking below alphanumeric ones, fewer identifiers
// ranking lower when all shared ones are equal.
func comparePrerelease(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aNum := strconv.Atoi(as[i])
		bn, bNum := strconv.Atoi(bs[i])
		switch {
		case aNum == nil && bNum == nil:
			if an != bn {
				if an > bn {
					return 1
				}
				return -1
			}
		case aNum == nil:
			return -1
		case bNum == nil:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(as) > len(bs):
		return 1
	case len(as) < len(bs):
		return -1
	}
	return 0
}

// startupUpdate runs once at TUI startup. With auto on it goes all the way:
// download, verify, replace, and say so; the running process is untouched and
// the next start runs the new binary. Failure is silent beyond falling back
// to the notice, because a tool that nags about its own update check failing
// is worse than one that skips it.
func startupUpdate(cfg *config.Config) tea.Cmd {
	channel, auto := cfg.UpdateChannel, cfg.UpdateAuto
	return func() tea.Msg {
		current := currentVersion()
		if current == "" {
			return updateCheckMsg{}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		rel, err := fetchLatest(ctx, channel)
		if err != nil || !newerVersion(rel.TagName, current) {
			return updateCheckMsg{}
		}
		if !auto {
			return updateCheckMsg{latest: rel.TagName}
		}
		if err := selfUpdate(ctx, rel); err != nil {
			// Including installs a package manager owns: those fall back to
			// the notice, which /update explains rather than fights.
			return updateCheckMsg{latest: rel.TagName}
		}
		return updateAppliedMsg{version: rel.TagName}
	}
}

const updateUsage = "usage: /update, /update channel [stable|beta], or /update auto [on|off]"

func cmdUpdate(m *tuiModel, args string) tea.Cmd {
	if args != "" {
		return updateSettings(m, args)
	}
	channel := m.app.config.UpdateChannel
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		rel, err := fetchLatest(ctx, channel)
		if errors.Is(err, errNoRelease) {
			return noticeMsg{text: "no releases published yet; nothing to update to"}
		}
		if err != nil {
			return noticeMsg{level: "error", text: "update check failed: " + err.Error()}
		}
		if current := currentVersion(); current != "" && !newerVersion(rel.TagName, current) {
			return noticeMsg{text: "already on the latest (" + current + ")"}
		}
		if err := selfUpdate(ctx, rel); err != nil {
			return noticeMsg{level: "error", text: "update failed: " + err.Error()}
		}
		return noticeMsg{text: "updated to " + rel.TagName + "; restart sb to run it"}
	}
}

// updateSettings is /update channel and /update auto: the update posture is
// configuration, and configuration is set from inside the TUI.
func updateSettings(m *tuiModel, args string) tea.Cmd {
	cfg := m.app.config
	what, value, _ := strings.Cut(strings.TrimSpace(args), " ")
	value = strings.TrimSpace(value)
	switch what {
	case "channel":
		switch value {
		case "":
			ch := cfg.UpdateChannel
			if ch == "" {
				ch = "stable"
			}
			return noticeCmd("", "update channel is "+ch)
		case "stable", "beta":
			cfg.UpdateChannel = value
			if err := cfg.Save(); err != nil {
				return noticeCmd("error", "saving the channel failed: "+err.Error())
			}
			return noticeCmd("", "update channel is now "+value)
		default:
			return noticeCmd("error", updateUsage)
		}
	case "auto":
		switch value {
		case "":
			state := "on"
			if !cfg.UpdateAuto {
				state = "off"
			}
			return noticeCmd("", "auto-update is "+state)
		case "on", "off":
			cfg.UpdateAuto = value == "on"
			if err := cfg.Save(); err != nil {
				return noticeCmd("error", "saving the setting failed: "+err.Error())
			}
			return noticeCmd("", "auto-update is now "+value)
		default:
			return noticeCmd("error", updateUsage)
		}
	default:
		return noticeCmd("error", updateUsage)
	}
}

// selfUpdate downloads the archive for this platform, verifies it against the
// release's checksums, and atomically replaces the running binary.
func selfUpdate(ctx context.Context, rel *ghRelease) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}

	// §18: an install that came from a package manager defers to it rather
	// than fighting it.
	if managedBy, ok := packageManagerFor(exe); ok {
		return fmt.Errorf("this install is managed by %s; update through it", managedBy)
	}

	assetName := fmt.Sprintf("sb_%s_%s_%s.tar.gz",
		strings.TrimPrefix(rel.TagName, "v"), runtime.GOOS, runtime.GOARCH)
	assetURL, sumsURL := "", ""
	for _, a := range rel.Assets {
		switch a.Name {
		case assetName:
			assetURL = a.URL
		case "checksums.txt":
			sumsURL = a.URL
		}
	}
	if assetURL == "" {
		return fmt.Errorf("release %s has no build for %s/%s", rel.TagName, runtime.GOOS, runtime.GOARCH)
	}
	if sumsURL == "" {
		return errors.New("release has no checksums.txt; refusing to install unverified bits")
	}

	sums, err := download(ctx, sumsURL, 1<<16)
	if err != nil {
		return err
	}
	want, err := checksumFor(sums, assetName)
	if err != nil {
		return err
	}

	archive, err := download(ctx, assetURL, 128<<20)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archive)
	if !equalFoldHex(hex.EncodeToString(sum[:]), want) {
		return errors.New("checksum mismatch; nothing was installed")
	}

	binary, err := extractSB(archive)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(exe), ".sb-update-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(binary); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	// Rename is atomic on the same filesystem, which is why the temp file lives
	// beside the binary rather than in /tmp. Windows refuses to rename over a
	// running executable, so there the old binary steps aside first and the
	// leftover .old is swept on the next start.
	if runtime.GOOS == "windows" {
		old := exe + ".old"
		os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			return err
		}
		if err := os.Rename(tmpPath, exe); err != nil {
			os.Rename(old, exe)
			return err
		}
		return nil
	}
	return os.Rename(tmpPath, exe)
}

// sweepOldBinary removes the .old a Windows self-update leaves behind. Called
// at startup; every error is ignorable because the file either is not there,
// is still running, or will be swept next time.
func sweepOldBinary() {
	if runtime.GOOS != "windows" {
		return
	}
	if exe, err := os.Executable(); err == nil {
		os.Remove(exe + ".old")
	}
}

// packageManagerFor recognizes install layouts that belong to a package
// manager by where the binary lives.
func packageManagerFor(exe string) (string, bool) {
	switch {
	case strings.Contains(exe, "/Cellar/"), strings.Contains(exe, "/homebrew/"), strings.Contains(exe, "/linuxbrew/"):
		return "Homebrew", true
	case strings.Contains(exe, "scoop"):
		return "Scoop", true
	case strings.HasPrefix(exe, "/usr/local/bin/") && runtime.GOOS == "linux",
		strings.HasPrefix(exe, "/usr/bin/"):
		return "the system package manager", true
	}
	return "", false
}

func download(ctx context.Context, url string, cap int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "switchboard/"+version)
	resp, err := updateHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, cap+1))
}

// checksumFor reads a sha256sum-format checksums file.
func checksumFor(sums []byte, name string) (string, error) {
	for line := range strings.Lines(string(sums)) {
		sum, file, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if strings.TrimSpace(strings.TrimPrefix(file, "*")) == name {
			return sum, nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", name)
}

func equalFoldHex(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// extractSB pulls the sb binary out of the release archive.
func extractSB(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		base := filepath.Base(hdr.Name)
		if hdr.Typeflag == tar.TypeReg && (base == "sb" || base == "sb.exe") {
			return io.ReadAll(io.LimitReader(tr, 256<<20))
		}
	}
	return nil, errors.New("no sb binary in the release archive")
}
