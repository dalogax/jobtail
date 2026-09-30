package selfupdate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestLatestTagFromLocation(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://github.com/dalogax/jobtail/releases/tag/v0.3.0", "v0.3.0"},
		{"/dalogax/jobtail/releases/tag/v1.2.3", "v1.2.3"},
		{"https://github.com/dalogax/jobtail/releases/tag/v1.0.0-rc.1", "v1.0.0-rc.1"},
		{"https://github.com/dalogax/jobtail/releases/tag/weird%20tag", "weird tag"},
		{"https://github.com/dalogax/jobtail/releases", ""},                       // not a tag URL
		{"https://example.com/releases/tag/v1.0.0/releases/tag/v2.0.0", "v2.0.0"}, // last one wins
		{"", ""},
	}
	for _, c := range cases {
		if got := latestTagFromLocation(c.in); got != c.want {
			t.Errorf("latestTagFromLocation(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A redirect to a tag URL yields the tag with no asset list; asset URLs are
// then synthesized against the releases base.
func TestLatestReleaseViaRedirect(t *testing.T) {
	var serveTag string // set below; handlers close over it
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Location", serveTag)
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	serveTag = srv.URL + "/releases/tag/v9.9.9"

	t.Setenv("JOBTAIL_UPDATE_BASE", srv.URL)
	t.Setenv("JOBTAIL_UPDATE_API", "http://127.0.0.1:1") // must never be reached

	rel, err := LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if rel.TagName != "v9.9.9" {
		t.Fatalf("tag = %q, want v9.9.9", rel.TagName)
	}

	asset, ok := CurrentPlatformAsset(rel)
	if !ok {
		t.Fatal("expected a synthesized platform asset")
	}
	// The URL shape is what matters (the asset name depends on the host
	// GOOS/GOARCH): base + /releases/download/<tag>/<asset>, absolute.
	if !strings.HasPrefix(asset.BrowserDownloadURL, srv.URL+"/releases/download/v9.9.9/jobtail-") {
		t.Fatalf("unexpected asset URL: %s", asset.BrowserDownloadURL)
	}
}

// The redirect path needs the tag page to have published assets; when the
// redirect lookup fails (here: a 404), the API fallback runs.
func TestLatestReleaseFallsBackToAPI(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/repos/dalogax/jobtail/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if tok := apiToken(); tok != "tok-123" {
			t.Errorf("API request carried token %q, want tok-123", tok)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tag_name":"v8.8.8","assets":[{"name":"jobtail-x-y","browser_download_url":"http://dl/x"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("JOBTAIL_UPDATE_BASE", srv.URL)
	t.Setenv("JOBTAIL_UPDATE_API", srv.URL)
	t.Setenv("GH_TOKEN", "tok-123")
	t.Setenv("JOBTAIL_UPDATE_TOKEN", "")

	rel, err := LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if rel.TagName != "v8.8.8" {
		t.Fatalf("tag = %q, want v8.8.8", rel.TagName)
	}
	a, ok := FindAsset(rel, "x", "y")
	if !ok || a.BrowserDownloadURL != "http://dl/x" {
		t.Fatalf("API-declared asset not found: %v %v", ok, a)
	}
}

// A token is picked up from any of the three environment variables.
func TestAPITokenSelection(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"JOBTAIL_UPDATE_TOKEN": "a", "GH_TOKEN": "b", "GITHUB_TOKEN": "c"}, "a"},
		{map[string]string{"JOBTAIL_UPDATE_TOKEN": "", "GH_TOKEN": "b", "GITHUB_TOKEN": "c"}, "b"},
		{map[string]string{"JOBTAIL_UPDATE_TOKEN": "", "GH_TOKEN": "", "GITHUB_TOKEN": "c"}, "c"},
		{map[string]string{"JOBTAIL_UPDATE_TOKEN": "", "GH_TOKEN": "", "GITHUB_TOKEN": ""}, ""},
	} {
		for k, v := range tc.env {
			t.Setenv(k, v)
		}
		if got := apiToken(); got != tc.want {
			t.Errorf("apiToken() with %v = %q, want %q", tc.env, got, tc.want)
		}
	}
}

// When both lookup paths fail with 403 the error names the rate limit and
// the GH_TOKEN escape hatch.
func TestRateLimitedErrorMentionsToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("/repos/dalogax/jobtail/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("JOBTAIL_UPDATE_BASE", srv.URL)
	t.Setenv("JOBTAIL_UPDATE_API", srv.URL)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("JOBTAIL_UPDATE_TOKEN", "")

	_, err := LatestRelease(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"rate limit", "GH_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// Tag extraction from the Location header must not accept junk that could
// smuggle a path into a synthetic asset URL.
func TestLatestTagFromLocationRejectsTraversal(t *testing.T) {
	u, err := url.PathUnescape("/releases/tag/..%2F..%2Fetc")
	if err != nil {
		t.Fatal(err)
	}
	if got := latestTagFromLocation(u); got != "" {
		t.Fatalf("traversal tag %q should have been rejected", got)
	}
}
