package service

import (
	"context"
	"net/http"

	"nodectl/internal/githubrelease"
)

func fetchLatestGitHubRelease(client *http.Client, latestURL, userAgent string) (tag, finalURL string, err error) {
	return githubrelease.FetchLatestRelease(context.Background(), client, latestURL, userAgent)
}
