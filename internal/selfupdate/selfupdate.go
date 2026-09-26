// Package selfupdate checks GitHub Releases for a newer jobtail build and
// can replace the currently running binary with one. It never blocks normal
// CLI use on network access: every check is best-effort and cached.
package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/mod/semver"
)

// Repo is the GitHub repo releases are published to.
const Repo = "dalogax/jobtail"

// checkInterval is how often CheckForUpdate actually hits the network; in
// between, it trusts its cached last result.
const checkInterval = 24 * time.Hour

func apiBase() string {
	if b := os.Getenv("JOBTAIL_UPDATE_API"); b != "" {
		return b
	}
	return "https://api.github.com"
}

type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// AssetName is the release-asset naming convention the release workflow
// publishes to and this package looks for: jobtail-<os>-<arch>.
func AssetName(goos, goarch string) string {
	return fmt.Sprintf("jobtail-%s-%s", goos, goarch)
}

func LatestRelease(ctx context.Context) (Release, error) {
	url := apiBase() + "/repos/" + Repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return Release{}, fmt.Errorf("decode release: %w", err)
	}
	return rel, nil
}

func FindAsset(rel Release, goos, goarch string) (Asset, bool) {
	want := AssetName(goos, goarch)
	for _, a := range rel.Assets {
		if a.Name == want {
			return a, true
		}
	}
	return Asset{}, false
}

// checkState is the on-disk cache CheckForUpdate uses to avoid hitting the
// network on every invocation.
type checkState struct {
	LastChecked time.Time `json:"last_checked"`
	LatestKnown string    `json:"latest_known"`
}

func statePath(dataDir string) string { return filepath.Join(dataDir, "update-check.json") }

func readState(dataDir string) checkState {
	var st checkState
	data, err := os.ReadFile(statePath(dataDir))
	if err != nil {
		return st
	}
	_ = json.Unmarshal(data, &st)
	return st
}

func writeState(dataDir string, st checkState) {
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	_ = os.WriteFile(statePath(dataDir), data, 0o644)
}

// CheckForUpdate returns a one-line upgrade suggestion if a newer release
// than currentVersion is known, or "" if not (no newer release, a dev
// build with nothing to compare against, or the network is unreachable and
// nothing is cached yet). It re-hits the network at most once every
// checkInterval, caching the result in dataDir.
func CheckForUpdate(ctx context.Context, dataDir, currentVersion string) string {
	if !semver.IsValid(currentVersion) {
		return "" // "dev" or any non-release build: nothing to compare against
	}

	st := readState(dataDir)
	latest := st.LatestKnown
	if time.Since(st.LastChecked) > checkInterval {
		rel, err := LatestRelease(ctx)
		if err == nil {
			latest = rel.TagName
			writeState(dataDir, checkState{LastChecked: time.Now(), LatestKnown: latest})
		}
	}
	if latest == "" || !semver.IsValid(latest) {
		return ""
	}
	if semver.Compare(latest, currentVersion) > 0 {
		return fmt.Sprintf("a newer jobtail is available: %s -> %s (run `jobtail upgrade`)", currentVersion, latest)
	}
	return ""
}

// Install downloads the release asset for goos/goarch and atomically
// replaces execPath with it (same directory, rename over the original —
// safe even for the binary that's currently running).
func Install(ctx context.Context, asset Asset, execPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.BrowserDownloadURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", asset.BrowserDownloadURL, resp.Status)
	}

	dir := filepath.Dir(execPath)
	tmp, err := os.CreateTemp(dir, ".jobtail-upgrade-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, execPath)
}

// CurrentPlatformAsset is a convenience wrapper for the running GOOS/GOARCH.
func CurrentPlatformAsset(rel Release) (Asset, bool) {
	return FindAsset(rel, runtime.GOOS, runtime.GOARCH)
}
