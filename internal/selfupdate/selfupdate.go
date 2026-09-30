// Package selfupdate checks GitHub Releases for a newer jobtail build and
// can replace the currently running binary with one. It never blocks normal
// CLI use on network access: every check is best-effort and cached.
//
// Release lookups avoid the GitHub API wherever possible: the API allows
// only 60 unauthenticated requests per hour per IP, which is routinely
// exhausted on shared networks — the redirect endpoints jobtail uses
// instead are unthrottled and need no account. The API remains as a
// fallback (e.g. for GitHub Enterprise-style overrides) and is given a
// token when one exists in the environment.
package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// releasesBase is the human-facing releases home (github.com/owner/repo by
// default). Its /releases/latest redirect and /releases/download/ asset
// URLs are served without any API call, unthrottled.
func releasesBase() string {
	if b := os.Getenv("JOBTAIL_UPDATE_BASE"); b != "" {
		return strings.TrimSuffix(b, "/")
	}
	return "https://github.com/" + Repo
}

// apiToken returns an optional GitHub token for the API fallback.(tokens
// only ever travel in request headers — never in errors or logs.)
func apiToken() string {
	for _, k := range []string{"JOBTAIL_UPDATE_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`

	// base is the releases home the tag was resolved from; asset URLs are
	// synthesized against it when the API didn't supply any (it's empty on
	// the API path, where assets carry their own download URLs).
	base string
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

// assetURL returns the download URL for a release asset: the explicit URL
// the API listed when it did, otherwise the public /releases/download/ URL
// (GitHub serves published release assets unauthenticated, no API involved).
func (rel Release) assetURL(name string) string {
	for _, a := range rel.Assets {
		if a.Name == name && a.BrowserDownloadURL != "" {
			return a.BrowserDownloadURL
		}
	}
	if rel.base != "" && rel.TagName != "" {
		return rel.base + "/releases/download/" + rel.TagName + "/" + name
	}
	return ""
}

// LatestRelease resolves the newest published release without touching the
// rate-limited API: GitHub permanently redirects releases/latest to
// releases/tag/<tag>, so the tag travels in plain sight. The API is only
// consulted when that redirect path fails.
func LatestRelease(ctx context.Context) (Release, error) {
	if rel, err := latestViaRedirect(ctx); err == nil {
		return rel, nil
	}
	return latestViaAPI(ctx)
}

// latestViaRedirect reads the tag out of the releases/latest redirect.
func latestViaRedirect(ctx context.Context) (Release, error) {
	home := releasesBase()
	endpoint := home + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Release{}, err
	}
	// Stop at the first response: we want the 302's Location itself, not
	// the tag page it points to.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return Release{}, fmt.Errorf("GET %s: %s", endpoint, resp.Status)
	}
	tag := latestTagFromLocation(resp.Header.Get("Location"))
	if tag == "" {
		return Release{}, fmt.Errorf("no release tag in the %s redirect", endpoint)
	}
	return Release{TagName: tag, base: home}, nil
}

// latestTagFromLocation extracts the tag from a releases/tag/<tag> URL
// (absolute or relative, host part irrelevant to us).
func latestTagFromLocation(loc string) string {
	i := strings.LastIndex(loc, "/releases/tag/")
	if i < 0 {
		return ""
	}
	tag, err := url.PathUnescape(loc[i+len("/releases/tag/"):])
	if err != nil || tag == "" || strings.Contains(tag, "/") {
		return ""
	}
	return tag
}

// latestViaAPI is the fallback: the REST releases/latest endpoint. It
// accepts an optional token (GH_TOKEN / GITHUB_TOKEN / JOBTAIL_UPDATE_TOKEN)
// — unauthenticated it's throttled to 60 requests/hour per IP.
func latestViaAPI(ctx context.Context) (Release, error) {
	endpoint := apiBase() + "/repos/" + Repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := apiToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		if tok := apiToken(); tok != "" {
			return Release{}, fmt.Errorf("GET %s: %s (API rejected the provided token)", endpoint, resp.Status)
		}
		return Release{}, fmt.Errorf("GET %s: %s (GitHub API rate limit; the tokenless redirect lookup also failed — set GH_TOKEN to raise the limit, or retry later)", endpoint, resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GET %s: %s", endpoint, resp.Status)
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
		if rel, err := LatestRelease(ctx); err == nil {
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
	if asset.BrowserDownloadURL == "" {
		return fmt.Errorf("no download URL for asset %s", asset.Name)
	}
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
	name := AssetName(runtime.GOOS, runtime.GOARCH)
	if u := rel.assetURL(name); u != "" {
		return Asset{Name: name, BrowserDownloadURL: u}, true
	}
	return Asset{}, false
}
