package githubrelease

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchLatestReleaseFollowsRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/owner/repo/releases/latest":
			http.Redirect(w, r, "/owner/repo/releases/tag/v1.2.3-custom.4", http.StatusFound)
		case "/owner/repo/releases/tag/v1.2.3-custom.4":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tag, finalURL, err := FetchLatestRelease(context.Background(), server.Client(), server.URL+"/owner/repo/releases/latest", "nodectl-test")
	if err != nil {
		t.Fatalf("FetchLatestRelease() error = %v", err)
	}
	if tag != "v1.2.3-custom.4" {
		t.Fatalf("tag = %q, want %q", tag, "v1.2.3-custom.4")
	}
	if finalURL != server.URL+"/owner/repo/releases/tag/v1.2.3-custom.4" {
		t.Fatalf("finalURL = %q", finalURL)
	}
}

func TestTagFromURLUnescapesTag(t *testing.T) {
	tag, err := TagFromURL("https://github.com/owner/repo/releases/tag/v1.2.3%2Fcustom")
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v1.2.3/custom" {
		t.Fatalf("tag = %q, want %q", tag, "v1.2.3/custom")
	}
}

func TestExpandedAssetsURL(t *testing.T) {
	got, err := ExpandedAssetsURL("https://github.com/owner/repo/releases/tag/v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://github.com/owner/repo/releases/expanded_assets/v1.2.3"
	if got != want {
		t.Fatalf("ExpandedAssetsURL() = %q, want %q", got, want)
	}
}

func TestAssetsExtractsMatchingReleaseLinks(t *testing.T) {
	body := []byte(`<a href="/owner/repo/releases/download/v1.2.3/nodectl-agent-linux-amd64-v0.2.77">binary</a>
<a href="https://github.com/owner/repo/releases/download/v1.2.3/nodectl-agent-linux-amd64-v0.2.77.sha256">checksum</a>
<a href="https://example.com/owner/repo/releases/download/v1.2.3/not-a-release-asset">external</a>`)
	assets := Assets(body, "https://github.com/owner/repo/releases/tag/v1.2.3")
	if len(assets) != 2 {
		t.Fatalf("got %d assets, want 2: %#v", len(assets), assets)
	}
	if assets[0].Name != "nodectl-agent-linux-amd64-v0.2.77" {
		t.Fatalf("unexpected asset name: %q", assets[0].Name)
	}
	if assets[0].URL != "https://github.com/owner/repo/releases/download/v1.2.3/nodectl-agent-linux-amd64-v0.2.77" {
		t.Fatalf("unexpected asset URL: %q", assets[0].URL)
	}
}
