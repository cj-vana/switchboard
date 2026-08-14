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
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func fetchLatestRelease(ctx context.Context) (*ghRelease, error) {
	url := "https://api.github.com/repos/" + updateRepo + "/releases/latest"
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
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

var errNoRelease = errors.New("no releases published yet")

// newerVersion reports whether candidate outranks current, comparing the
// numeric core and ignoring prerelease suffixes.
func newerVersion(candidate, current string) bool {
	c, ok1 := semverCore(candidate)
	u, ok2 := semverCore(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range c {
		if c[i] != u[i] {
			return c[i] > u[i]
		}
	}
	return false
}

func semverCore(v string) ([3]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "-")
	parts := strings.Split(v, ".")
	var out [3]int
	if len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// checkForUpdate runs once at TUI startup. Failure is silent: a tool that
// nags about its update check failing is worse than one that skips it.
func checkForUpdate() tea.Cmd {
	return func() tea.Msg {
		current := currentVersion()
		if current == "" {
			return updateCheckMsg{}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		rel, err := fetchLatestRelease(ctx)
		if err != nil {
			return updateCheckMsg{}
		}
		if newerVersion(rel.TagName, current) {
			return updateCheckMsg{latest: rel.TagName}
		}
		return updateCheckMsg{}
	}
}

func cmdUpdate(m *tuiModel, _ string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		rel, err := fetchLatestRelease(ctx)
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
	// beside the binary rather than in /tmp.
	return os.Rename(tmpPath, exe)
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
