// Phase 5 (distribution): version reporting, the background "a newer
// jobtail is available" suggestion, and `jobtail upgrade` actually
// replacing a binary on disk. All against a fake GitHub Releases server
// (JOBTAIL_UPDATE_API) — never the real GitHub API — so this suite has no
// network dependency and never depends on what's actually been released.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeRelease starts an httptest server that serves one GitHub-Releases
// -shaped "latest release" response (tag latestTag) plus a downloadable
// asset for the current GOOS/GOARCH whose content is assetBody. It reports
// how many times /releases/latest was actually hit.
func fakeRelease(t *testing.T, latestTag, assetBody string) (baseURL string, hits *int32) {
	t.Helper()
	hits = new(int32)
	assetName := fmt.Sprintf("jobtail-%s-%s", runtime.GOOS, runtime.GOARCH)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/repos/dalogax/jobtail/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": latestTag,
			"assets": []map[string]string{
				{"name": assetName, "browser_download_url": srv.URL + "/" + assetName},
			},
		})
	})
	mux.HandleFunc("/"+assetName, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, assetBody)
	})
	// The redirect-based lookup hits /releases/latest on the same fake
	// server first; it must fail (404) so tests exercise the API endpoint
	// they explicitly stub with releases/latest JSON.
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	return srv.URL, hits
}

// runWithUpdateAPI runs the binary with the fake GitHub Releases server
// set for both the redirect-based lookup (JOBTAIL_UPDATE_BASE) and the API
// fallback (JOBTAIL_UPDATE_API).
func (e *env) runWithUpdateAPI(apiBase, versionOverride string, args ...string) (string, error) {
	e.t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Env = append(os.Environ(),
		"JOBTAIL_DATA_DIR="+e.dataDir,
		"JOBTAIL_DISABLE_NOTIFY=1",
		"JOBTAIL_UPDATE_API="+apiBase,
		"JOBTAIL_UPDATE_BASE="+apiBase,
		"JOBTAIL_VERSION_OVERRIDE="+versionOverride,
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func TestVersionFlagReportsCurrentVersion(t *testing.T) {
	e := newEnv(t)
	out, err := e.runAllowFail("--version")
	if err != nil {
		t.Fatalf("--version: %v\n%s", err, out)
	}
	if !strings.Contains(out, "jobtail dev") {
		t.Fatalf("expected the dev-build version string, got: %s", out)
	}
}

// TestBareInvocationOpensDashboardNotHelp confirms `jobtail` with no
// subcommand attempts to open the dashboard rather than printing help.
// There's no real tty in this test environment, so it can't render the
// TUI end to end here (that's verified separately, via a real pty) — but
// the distinct failure mode (openDashboard's "tui: could not open a new
// TTY", not cobra's usage/help text) proves which code path actually ran.
func TestBareInvocationOpensDashboardNotHelp(t *testing.T) {
	e := newEnv(t)
	out, err := e.runAllowFail()
	if err == nil {
		t.Fatalf("expected an error opening a tty-less dashboard, got success: %s", out)
	}
	if strings.Contains(out, "Available Commands") {
		t.Fatalf("bare invocation printed help instead of opening the dashboard: %s", out)
	}
	if !strings.Contains(out, "tui:") {
		t.Fatalf("expected a dashboard-open attempt in the error, got: %s", out)
	}
}

func TestUnknownSubcommandStillErrors(t *testing.T) {
	e := newEnv(t)
	out, err := e.runAllowFail("bogus")
	if err == nil {
		t.Fatalf("expected an error for an unknown subcommand, got success: %s", out)
	}
	if !strings.Contains(out, "unknown command") {
		t.Fatalf("expected a clear 'unknown command' error, got: %s", out)
	}
}

func TestBackgroundSuggestionAppearsOnStderrWhenNewer(t *testing.T) {
	e := newEnv(t)
	apiBase, hits := fakeRelease(t, "v2.0.0", "unused")

	e.run("add", "j", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", t.TempDir(), "--cmd", "true")

	out, err := e.runWithUpdateAPI(apiBase, "v1.0.0", "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v\n%s", err, out)
	}
	if !strings.Contains(out, "a newer jobtail is available: v1.0.0 -> v2.0.0") {
		t.Fatalf("expected an upgrade suggestion, got: %s", out)
	}
	// The suggestion must never land where --json output is parsed from.
	jsonPart := out[:strings.Index(out, "jobtail:")]
	var v []jobJSON
	if err := json.Unmarshal([]byte(jsonPart), &v); err != nil {
		t.Fatalf("--json output wasn't clean JSON ahead of the suggestion: %v\noutput: %s", err, out)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("want exactly 1 hit to /releases/latest, got %d", got)
	}
}

func TestBackgroundSuggestionCachesAndDoesNotRecheckEveryRun(t *testing.T) {
	e := newEnv(t)
	apiBase, hits := fakeRelease(t, "v2.0.0", "unused")
	e.run("add", "j", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", t.TempDir(), "--cmd", "true")

	e.runWithUpdateAPI(apiBase, "v1.0.0", "list")
	e.runWithUpdateAPI(apiBase, "v1.0.0", "list")
	e.runWithUpdateAPI(apiBase, "v1.0.0", "list")

	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("want the release check cached after the first run (1 network hit), got %d", got)
	}
}

func TestNoSuggestionWhenAlreadyLatest(t *testing.T) {
	e := newEnv(t)
	apiBase, _ := fakeRelease(t, "v1.0.0", "unused")
	e.run("add", "j", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", t.TempDir(), "--cmd", "true")

	out, err := e.runWithUpdateAPI(apiBase, "v1.0.0", "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if strings.Contains(out, "newer jobtail") {
		t.Fatalf("should not suggest an upgrade when already latest: %s", out)
	}
}

func TestNoSuggestionForDevBuild(t *testing.T) {
	e := newEnv(t)
	apiBase, hits := fakeRelease(t, "v2.0.0", "unused")
	e.run("add", "j", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", t.TempDir(), "--cmd", "true")

	out, err := e.runWithUpdateAPI(apiBase, "dev", "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if strings.Contains(out, "newer jobtail") {
		t.Fatalf("a dev build has nothing to compare against, should not suggest an upgrade: %s", out)
	}
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("a dev build shouldn't even hit the network to check, got %d hits", got)
	}
}

func TestUpgradeInstallsNewerRelease(t *testing.T) {
	e := newEnv(t)
	apiBase, _ := fakeRelease(t, "v2.0.0", "#!/bin/sh\necho FAKE-UPGRADED-V2\n")
	copyPath := e.copyBinary(t)

	cmd := exec.Command(copyPath, "upgrade")
	cmd.Env = append(os.Environ(),
		"JOBTAIL_DATA_DIR="+e.dataDir,
		"JOBTAIL_UPDATE_API="+apiBase,
		"JOBTAIL_UPDATE_BASE="+apiBase,
		"JOBTAIL_VERSION_OVERRIDE=v1.0.0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "upgraded v1.0.0 -> v2.0.0") {
		t.Fatalf("unexpected upgrade output: %s", out)
	}

	verify, err := exec.Command(copyPath).CombinedOutput()
	if err != nil {
		t.Fatalf("running the upgraded binary failed: %v\n%s", err, verify)
	}
	if !strings.Contains(string(verify), "FAKE-UPGRADED-V2") {
		t.Fatalf("the binary on disk wasn't actually replaced: %s", verify)
	}
}

func TestUpgradeSkipsWhenAlreadyLatest(t *testing.T) {
	e := newEnv(t)
	apiBase, _ := fakeRelease(t, "v1.0.0", "#!/bin/sh\necho SHOULD-NOT-BE-INSTALLED\n")
	copyPath := e.copyBinary(t)
	before, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(copyPath, "upgrade")
	cmd.Env = append(os.Environ(),
		"JOBTAIL_DATA_DIR="+e.dataDir,
		"JOBTAIL_UPDATE_API="+apiBase,
		"JOBTAIL_UPDATE_BASE="+apiBase,
		"JOBTAIL_VERSION_OVERRIDE=v1.0.0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "up to date") {
		t.Fatalf("expected an up-to-date message, got: %s", out)
	}

	after, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("binary was replaced even though it was already the latest version")
	}
}

func TestUpgradeCheckFlagNeverInstalls(t *testing.T) {
	e := newEnv(t)
	apiBase, _ := fakeRelease(t, "v2.0.0", "#!/bin/sh\necho SHOULD-NOT-BE-INSTALLED\n")
	copyPath := e.copyBinary(t)
	before, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(copyPath, "upgrade", "--check")
	cmd.Env = append(os.Environ(),
		"JOBTAIL_DATA_DIR="+e.dataDir,
		"JOBTAIL_UPDATE_API="+apiBase,
		"JOBTAIL_UPDATE_BASE="+apiBase,
		"JOBTAIL_VERSION_OVERRIDE=v1.0.0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("upgrade --check: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "v1.0.0 -> v2.0.0") {
		t.Fatalf("expected --check to report the available version, got: %s", out)
	}

	after, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("--check must never modify the binary on disk")
	}
}

func TestUpgradeErrorsWithoutMatchingAsset(t *testing.T) {
	e := newEnv(t)
	// A release returned by the API fallback with no asset at all for this
	// platform. The API endpoint is reached only after the redirect lookup
	// (served here as a 404) has failed.
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/repos/dalogax/jobtail/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tag_name": "v2.0.0", "assets": []map[string]string{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	copyPath := e.copyBinary(t)

	cmd := exec.Command(copyPath, "upgrade")
	cmd.Env = append(os.Environ(),
		"JOBTAIL_DATA_DIR="+e.dataDir,
		"JOBTAIL_UPDATE_API="+srv.URL,
		"JOBTAIL_UPDATE_BASE="+srv.URL,
		"JOBTAIL_VERSION_OVERRIDE=v1.0.0",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected an error when no asset matches this platform, output: %s", out)
	}
	if !strings.Contains(string(out), "no asset for") {
		t.Fatalf("expected a clear reason, got: %s", out)
	}
}
