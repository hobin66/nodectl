package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"nodectl/internal/database"
)

type agentStartupTransport func(*http.Request) (*http.Response, error)

func (transport agentStartupTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func agentStartupResponse(request *http.Request, status int, body, location string) *http.Response {
	header := make(http.Header)
	if location != "" {
		header.Set("Location", location)
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func testStableAgentReleaseClient(t *testing.T, apiCalls *int) *http.Client {
	t.Helper()
	const panelTag = "v0.4.76-custom.4"
	const assetHTML = `<a href="/illria/nodectl/releases/download/v0.4.76-custom.4/nodectl-agent-linux-amd64-v0.2.77">amd64</a>
<a href="/illria/nodectl/releases/download/v0.4.76-custom.4/nodectl-agent-linux-amd64-v0.2.77.sha256">amd64 sha</a>
<a href="/illria/nodectl/releases/download/v0.4.76-custom.4/nodectl-agent-linux-arm64-v0.2.77">arm64</a>
<a href="/illria/nodectl/releases/download/v0.4.76-custom.4/nodectl-agent-linux-arm64-v0.2.77.sha256">arm64 sha</a>`
	return &http.Client{Transport: agentStartupTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "api.github.com" {
			(*apiCalls)++
			return agentStartupResponse(request, http.StatusForbidden, `{}`, ""), nil
		}
		if request.URL.Host != "github.com" {
			return nil, fmt.Errorf("unexpected host %q", request.URL.Host)
		}
		switch request.URL.Path {
		case "/illria/nodectl/releases/latest":
			return agentStartupResponse(request, http.StatusFound, "", "https://github.com/illria/nodectl/releases/tag/"+panelTag), nil
		case "/illria/nodectl/releases/tag/" + panelTag:
			return agentStartupResponse(request, http.StatusOK, "release page", ""), nil
		case "/illria/nodectl/releases/expanded_assets/" + panelTag:
			return agentStartupResponse(request, http.StatusOK, assetHTML, ""), nil
		default:
			return nil, fmt.Errorf("unexpected release path %q", request.URL.Path)
		}
	})}
}

func TestAgentStartupStableUsesAssetVersionWithoutAPI(t *testing.T) {
	apiCalls := 0
	client := testStableAgentReleaseClient(t, &apiCalls)
	versions := fetchLatestAgentVersionsForNodes(
		context.Background(), client, agentStartupLatestURL, githubReleasesListAPI,
		[]database.NodePool{{AgentVersion: "v0.2.76"}},
	)
	if versions.StableErr != nil || versions.AlphaErr != nil {
		t.Fatalf("unexpected release errors: stable=%v alpha=%v", versions.StableErr, versions.AlphaErr)
	}
	if versions.Stable != "v0.2.77" || versions.Alpha != "" {
		t.Fatalf("agent versions = stable %q alpha %q", versions.Stable, versions.Alpha)
	}
	if apiCalls != 0 {
		t.Fatalf("stable startup requested GitHub API %d times", apiCalls)
	}
	if !agentVersionNeedsUpdate("v0.2.76", versions.Stable) {
		t.Fatal("v0.2.76 agent did not detect v0.2.77 as an upgrade")
	}
}

func TestAgentStartupStableContinuesWhenAlphaAPIFails(t *testing.T) {
	apiCalls := 0
	client := testStableAgentReleaseClient(t, &apiCalls)
	versions := fetchLatestAgentVersionsForNodes(
		context.Background(), client, agentStartupLatestURL, githubReleasesListAPI,
		[]database.NodePool{{AgentVersion: "v0.2.76"}, {AgentVersion: "v0.2.76-alpha"}},
	)
	if versions.StableErr != nil || versions.Stable != "v0.2.77" || versions.AlphaErr == nil {
		t.Fatalf("Alpha API failure affected Stable: %#v", versions)
	}
	if apiCalls != 1 {
		t.Fatalf("Alpha API requests = %d, want 1", apiCalls)
	}
}

func TestAgentStartupAlphaAlsoUsesAgentAssetVersion(t *testing.T) {
	stableCalls := 0
	client := &http.Client{Transport: agentStartupTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "api.github.com" {
			stableCalls++
			return nil, fmt.Errorf("unexpected Stable request %q", request.URL.String())
		}
		body := `[{"tag_name":"v0.4.76-alpha","assets":[{"name":"nodectl-agent-linux-amd64-v0.2.77-alpha"},{"name":"nodectl-agent-linux-amd64-v0.2.77-alpha.sha256"}]}]`
		return agentStartupResponse(request, http.StatusOK, body, ""), nil
	})}
	versions := fetchLatestAgentVersionsForNodes(
		context.Background(), client, agentStartupLatestURL, githubReleasesListAPI,
		[]database.NodePool{{AgentVersion: "v0.2.76-alpha"}},
	)
	if versions.AlphaErr != nil || versions.Alpha != "v0.2.77-alpha" || versions.Stable != "" || stableCalls != 0 {
		t.Fatalf("unexpected Alpha result: versions=%#v stableCalls=%d", versions, stableCalls)
	}
}
