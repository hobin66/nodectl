//go:build linux

package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/mod/semver"
)

type stableAgentTransport func(*http.Request) (*http.Response, error)

func (transport stableAgentTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestExistingStableAgentFindsNewerRelease(t *testing.T) {
	const panelTag = "v0.4.76-custom.4"
	const assetHTML = `<a href="/illria/nodectl/releases/download/v0.4.76-custom.4/nodectl-agent-linux-amd64-v0.2.77">amd64</a>
<a href="/illria/nodectl/releases/download/v0.4.76-custom.4/nodectl-agent-linux-amd64-v0.2.77.sha256">amd64 sha</a>
<a href="/illria/nodectl/releases/download/v0.4.76-custom.4/nodectl-agent-linux-arm64-v0.2.77">arm64</a>
<a href="/illria/nodectl/releases/download/v0.4.76-custom.4/nodectl-agent-linux-arm64-v0.2.77.sha256">arm64 sha</a>`
	client := &http.Client{Transport: stableAgentTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "github.com" {
			return nil, fmt.Errorf("unexpected host %q", request.URL.Host)
		}
		status, body, location := http.StatusOK, "release page", ""
		switch request.URL.Path {
		case "/illria/nodectl/releases/latest":
			status, body, location = http.StatusFound, "", "https://github.com/illria/nodectl/releases/tag/"+panelTag
		case "/illria/nodectl/releases/tag/" + panelTag:
		case "/illria/nodectl/releases/expanded_assets/" + panelTag:
			body = assetHTML
		default:
			return nil, fmt.Errorf("unexpected path %q", request.URL.Path)
		}
		header := make(http.Header)
		if location != "" {
			header.Set("Location", location)
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}

	updater := &Updater{client: client}
	version, binaryURL, shaURL, err := updater.findLatestStableRelease(context.Background())
	if err != nil {
		t.Fatalf("find Stable release: %v", err)
	}
	wantBinary := fmt.Sprintf("https://github.com/illria/nodectl/releases/download/%s/nodectl-agent-linux-%s-v0.2.77", panelTag, runtime.GOARCH)
	if version != "v0.2.77" || binaryURL != wantBinary || shaURL != wantBinary+".sha256" {
		t.Fatalf("release = %q, %q, %q; want binary %q", version, binaryURL, shaURL, wantBinary)
	}
	if semver.Compare(version, "v0.2.76") <= 0 {
		t.Fatal("existing v0.2.76 Agent did not see v0.2.77 as newer")
	}
}
