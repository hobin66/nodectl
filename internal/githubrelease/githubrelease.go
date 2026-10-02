package githubrelease

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
)

const maxReleasePageSize = 8 << 20

var downloadHrefPattern = regexp.MustCompile(`(?i)href\s*=\s*["']([^"'<>]*?/releases/download/[^"'<>]+)["']`)

// Asset is a named file linked from a GitHub release page.
type Asset struct {
	Name string
	URL  string
}

// FetchLatestRelease follows the releases/latest redirect and returns the tag
// and final release page URL without using the GitHub API.
func FetchLatestRelease(ctx context.Context, client *http.Client, latestURL, userAgent string) (tag, finalURL string, err error) {
	resp, err := get(ctx, client, latestURL, userAgent)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GitHub release 页面返回 HTTP %d", resp.StatusCode)
	}
	if resp.Request == nil || resp.Request.URL == nil {
		return "", "", fmt.Errorf("无法确定 GitHub release 页面最终地址")
	}

	finalURL = resp.Request.URL.String()
	tag, err = TagFromURL(finalURL)
	if err != nil {
		return "", "", err
	}
	return tag, finalURL, nil
}

// FetchReleasePage follows redirects and returns a bounded HTML release page.
func FetchReleasePage(ctx context.Context, client *http.Client, releaseURL, userAgent string) (body []byte, finalURL string, err error) {
	resp, err := get(ctx, client, releaseURL, userAgent)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("GitHub release 页面返回 HTTP %d", resp.StatusCode)
	}
	if resp.Request == nil || resp.Request.URL == nil {
		return nil, "", fmt.Errorf("无法确定 GitHub release 页面最终地址")
	}

	body, err = io.ReadAll(io.LimitReader(resp.Body, maxReleasePageSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("读取 GitHub release 页面失败: %w", err)
	}
	if len(body) > maxReleasePageSize {
		return nil, "", fmt.Errorf("GitHub release 页面超过 %d 字节", maxReleasePageSize)
	}
	return body, resp.Request.URL.String(), nil
}

// TagFromURL extracts a tag from a final /releases/tag/{tag} URL.
func TagFromURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("解析 GitHub release 地址失败: %w", err)
	}
	const marker = "/releases/tag/"
	escapedPath := u.EscapedPath()
	index := strings.LastIndex(escapedPath, marker)
	if index < 0 {
		return "", fmt.Errorf("GitHub release 最终地址没有 tag: %s", rawURL)
	}
	escapedTag := strings.Trim(strings.TrimSpace(escapedPath[index+len(marker):]), "/")
	if escapedTag == "" {
		return "", fmt.Errorf("GitHub release 最终地址中的 tag 为空")
	}
	tag, err := url.PathUnescape(escapedTag)
	if err != nil {
		return "", fmt.Errorf("解析 GitHub release tag 失败: %w", err)
	}
	return tag, nil
}

// ExpandedAssetsURL builds GitHub's ordinary HTML endpoint for release assets.
func ExpandedAssetsURL(releasePageURL string) (string, error) {
	u, err := url.Parse(releasePageURL)
	if err != nil {
		return "", fmt.Errorf("解析 GitHub release 地址失败: %w", err)
	}
	const marker = "/releases/tag/"
	if !strings.Contains(u.Path, marker) {
		return "", fmt.Errorf("GitHub release 地址没有 tag: %s", releasePageURL)
	}
	u.Path = strings.Replace(u.Path, marker, "/releases/expanded_assets/", 1)
	u.RawPath = ""
	return u.String(), nil
}

// Assets extracts release asset links from GitHub's rendered release page.
func Assets(body []byte, pageURL string) []Asset {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}

	seen := make(map[string]bool)
	assets := make([]Asset, 0)
	for _, match := range downloadHrefPattern.FindAllSubmatch(body, -1) {
		href := html.UnescapeString(string(match[1]))
		assetURL, err := url.Parse(href)
		if err != nil {
			continue
		}
		assetURL = base.ResolveReference(assetURL)
		if !strings.EqualFold(assetURL.Hostname(), base.Hostname()) {
			continue
		}
		if !strings.Contains(assetURL.Path, "/releases/download/") {
			continue
		}
		name := path.Base(assetURL.Path)
		if name == "." || name == "/" || name == "" || seen[name] {
			continue
		}
		seen[name] = true
		assets = append(assets, Asset{Name: name, URL: assetURL.String()})
	}
	return assets
}

func get(ctx context.Context, client *http.Client, requestURL, userAgent string) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建 GitHub release 请求失败: %w", err)
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 GitHub release 页面失败: %w", err)
	}
	return resp, nil
}
