package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"nodectl/internal/database"
	"nodectl/internal/githubrelease"
	"nodectl/internal/logger"

	"golang.org/x/mod/semver"
)

const (
	agentStartupCheckDelay  = 60 * time.Second
	agentStartupCheckWindow = 60 * time.Second
	agentStartupLatestURL   = "https://github.com/illria/nodectl/releases/latest"
)

var agentStartupAssetPattern = regexp.MustCompile(`^nodectl-agent-linux-(amd64|arm64)-(v[0-9]+\.[0-9]+\.[0-9]+.*)$`)

var (
	agentStartupUpdateOnce sync.Once
)

// StartAgentStartupSilentUpdateCheck 启动时静默检查一次 Agent 版本并按需下发更新命令。
// 设计目标：
//  1. 不阻塞主流程（异步执行）
//  2. 只在进程生命周期内执行一次
//  3. 仅查询现有节点所需的发布渠道，并从 Agent 产物名提取版本号，
//     若版本号一致则不下发更新命令（避免无谓的命令下发）
//  4. 仅对在线节点且版本落后的 Agent 下发 check-agent-update
//  5. 汇总式日志输出，不逐节点打印
func StartAgentStartupSilentUpdateCheck() {
	agentStartupUpdateOnce.Do(func() {
		go func() {
			logger.Log.Debug("启动静默 Agent 更新检查任务已创建", "delay", agentStartupCheckDelay.String())

			timer := time.NewTimer(agentStartupCheckDelay)
			defer timer.Stop()

			select {
			case <-timer.C:
			case <-context.Background().Done():
				return
			}

			enabled, err := isStartupSilentAgentUpdateEnabled()
			if err != nil {
				logger.Log.Warn("读取启动静默 Agent 更新开关失败，按关闭处理", "error", err)
				return
			}
			if !enabled {
				logger.Log.Debug("启动静默 Agent 更新检查已关闭，跳过执行", "config_key", "agent_startup_silent_update_enabled")
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), agentStartupCheckWindow)
			defer cancel()

			if err := runAgentStartupSilentUpdateCheck(ctx); err != nil {
				logger.Log.Warn("启动静默 Agent 更新检查失败", "error", err)
			}
		}()
	})
}

func runAgentStartupSilentUpdateCheck(ctx context.Context) (retErr error) {
	var (
		latestStableVersion string
		latestAlphaVersion  string
		nodes               []database.NodePool
	)

	defer func() {
		// 显式释放临时内存引用，避免长生命周期 goroutine 持有大对象
		latestStableVersion = ""
		latestAlphaVersion = ""
		for i := range nodes {
			nodes[i] = database.NodePool{}
		}
		nodes = nil
	}()

	if err := database.DB.Select("uuid", "install_id", "name", "agent_version").Find(&nodes).Error; err != nil {
		return fmt.Errorf("查询节点列表失败: %w", err)
	}

	total := len(nodes)
	if total == 0 {
		logger.Log.Info("启动静默 Agent 更新检查完成：无节点")
		return nil
	}

	versions := fetchLatestAgentVersionsForNodes(
		ctx,
		&http.Client{Timeout: 15 * time.Second},
		agentStartupLatestURL,
		githubReleasesListAPI,
		nodes,
	)
	latestStableVersion, latestAlphaVersion = versions.Stable, versions.Alpha
	if versions.StableErr != nil {
		logger.Log.Warn("获取 Stable Agent 版本失败", "error", versions.StableErr)
	}
	if versions.AlphaErr != nil {
		logger.Log.Warn("获取 Alpha Agent 版本失败", "error", versions.AlphaErr)
	}
	if latestStableVersion == "" && latestAlphaVersion == "" {
		if err := errors.Join(versions.StableErr, versions.AlphaErr); err != nil {
			return fmt.Errorf("获取 GitHub 最新 Agent 版本失败: %w", err)
		}
		logger.Log.Info("启动静默 Agent 更新检查完成：无有效版本节点", "total_nodes", total)
		return nil
	}

	// ── 第一轮：纯版本号比较，筛选出需要更新的节点 ──
	type needUpdateNode struct {
		InstallID string
		Name      string
		DBVersion string
	}
	var needUpdate []needUpdateNode
	skippedUpToDate := 0
	skippedInvalid := 0

	for _, node := range nodes {
		installID := strings.TrimSpace(node.InstallID)
		nodeName := strings.TrimSpace(node.Name)
		if nodeName == "" {
			nodeName = "unknown"
		}

		dbVerRaw := strings.TrimSpace(node.AgentVersion)
		dbVer := normalizeSemver(dbVerRaw)

		// 版本未知/无效：不冒进触发，跳过
		if dbVer == "" || !semver.IsValid(dbVer) {
			skippedInvalid++
			continue
		}

		// 根据节点版本判断渠道，选择对应的目标版本
		var targetVersion string
		if strings.Contains(strings.ToLower(dbVer), "-alpha") {
			// Alpha 版本的节点，检查是否有更新的 alpha 版本
			targetVersion = latestAlphaVersion
		} else {
			// 正式版本的节点，检查正式版本
			targetVersion = latestStableVersion
		}

		// 如果目标版本为空（可能没有该渠道的发布），跳过
		if targetVersion == "" {
			continue
		}

		targetVer := normalizeSemver(targetVersion)
		if !semver.IsValid(targetVer) {
			continue
		}

		// 数据库版本 >= 目标版本：已是最新，无需下发
		if !agentVersionNeedsUpdate(dbVer, targetVer) {
			skippedUpToDate++
			continue
		}

		needUpdate = append(needUpdate, needUpdateNode{
			InstallID: installID,
			Name:      nodeName,
			DBVersion: dbVerRaw,
		})
	}

	// 所有节点版本均已是最新，直接结束，不下发任何命令
	if len(needUpdate) == 0 {
		logger.Log.Info("Agent 更新检查完成：所有节点版本均已是最新",
			"latest_stable", latestStableVersion,
			"latest_alpha", latestAlphaVersion,
			"total_nodes", total,
		)
		return nil
	}

	// ── 第二轮：仅对版本落后且在线的节点下发更新命令 ──
	triggered := 0
	skippedOffline := 0
	skippedFireErr := 0

	for _, n := range needUpdate {
		if !IsNodeOnline(n.InstallID) {
			skippedOffline++
			continue
		}

		_, fireErr := FireCommandToNode(n.InstallID, "check-agent-update", map[string]interface{}{})
		if fireErr != nil {
			skippedFireErr++
			logger.Log.Debug("启动静默 Agent 更新命令下发失败",
				"install_id", n.InstallID,
				"node_name", n.Name,
				"error", fireErr,
			)
			continue
		}

		triggered++
	}

	// ── 汇总日志：一行显示成功/失败数 ──
	logger.Log.Info("已成功下发 Agent 检查更新",
		"latest_stable", latestStableVersion,
		"latest_alpha", latestAlphaVersion,
		"成功", triggered,
		"失败", skippedFireErr,
		"离线跳过", skippedOffline,
		"已是最新", skippedUpToDate,
		"版本无效跳过", skippedInvalid,
	)

	return nil
}

type agentLatestVersions struct {
	Stable    string
	Alpha     string
	StableErr error
	AlphaErr  error
}

// 只查询数据库中实际存在的 Agent 渠道；纯 Stable 节点不会请求 GitHub API。
func fetchLatestAgentVersionsForNodes(ctx context.Context, client *http.Client, stableURL, alphaAPIURL string, nodes []database.NodePool) agentLatestVersions {
	needsStable, needsAlpha := false, false
	for _, node := range nodes {
		version := normalizeSemver(node.AgentVersion)
		if !semver.IsValid(version) {
			continue
		}
		if strings.Contains(strings.ToLower(semver.Prerelease(version)), "alpha") {
			needsAlpha = true
		} else {
			needsStable = true
		}
	}

	var versions agentLatestVersions
	if needsStable {
		versions.Stable, versions.StableErr = fetchLatestStableAgentVersion(ctx, client, stableURL)
	}
	if needsAlpha {
		versions.Alpha, versions.AlphaErr = fetchLatestAlphaAgentVersion(ctx, client, alphaAPIURL)
	}
	return versions
}

func fetchLatestStableAgentVersion(ctx context.Context, client *http.Client, latestURL string) (string, error) {
	_, releaseURL, err := githubrelease.FetchLatestRelease(ctx, client, latestURL, "nodectl-core-agent-startup-check")
	if err != nil {
		return "", err
	}
	assetsURL, err := githubrelease.ExpandedAssetsURL(releaseURL)
	if err != nil {
		return "", err
	}
	body, finalURL, err := githubrelease.FetchReleasePage(ctx, client, assetsURL, "nodectl-core-agent-startup-check")
	if err != nil {
		return "", err
	}
	version := latestAgentVersionFromAssets(githubrelease.Assets(body, finalURL), false)
	if version == "" {
		return "", fmt.Errorf("Stable release 中未找到带 SHA256 的 Agent 产物")
	}
	return version, nil
}

func fetchLatestAlphaAgentVersion(ctx context.Context, client *http.Client, apiURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "nodectl-core-agent-startup-check")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub Alpha releases API 返回 %s", resp.Status)
	}

	var releases []struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return "", err
	}

	latest := ""
	for _, release := range releases {
		if !strings.Contains(strings.ToLower(release.TagName), "alpha") {
			continue
		}
		assets := make([]githubrelease.Asset, 0, len(release.Assets))
		for _, asset := range release.Assets {
			assets = append(assets, githubrelease.Asset{Name: asset.Name})
		}
		candidate := latestAgentVersionFromAssets(assets, true)
		if candidate != "" && (latest == "" || semver.Compare(candidate, latest) > 0) {
			latest = candidate
		}
	}
	if latest == "" {
		return "", fmt.Errorf("Alpha release 中未找到带 SHA256 的 Agent 产物")
	}
	return latest, nil
}

func latestAgentVersionFromAssets(assets []githubrelease.Asset, alpha bool) string {
	names := make(map[string]bool, len(assets))
	for _, asset := range assets {
		names[asset.Name] = true
	}
	latest := ""
	for _, asset := range assets {
		candidate := agentVersionFromAssetName(asset.Name)
		if candidate == "" || !names[asset.Name+".sha256"] {
			continue
		}
		prerelease := strings.ToLower(semver.Prerelease(candidate))
		if (alpha && !strings.Contains(prerelease, "alpha")) || (!alpha && prerelease != "") {
			continue
		}
		if latest == "" || semver.Compare(candidate, latest) > 0 {
			latest = candidate
		}
	}
	return latest
}

func agentVersionFromAssetName(name string) string {
	if strings.HasSuffix(name, ".sha256") {
		return ""
	}
	matches := agentStartupAssetPattern.FindStringSubmatch(name)
	if len(matches) != 3 || !semver.IsValid(matches[2]) {
		return ""
	}
	return matches[2]
}

func agentVersionNeedsUpdate(current, target string) bool {
	current = normalizeSemver(current)
	target = normalizeSemver(target)
	return semver.IsValid(current) && semver.IsValid(target) && semver.Compare(current, target) < 0
}

func normalizeSemver(v string) string {
	s := strings.TrimSpace(v)
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "v") {
		s = "v" + s
	}
	return s
}

func isStartupSilentAgentUpdateEnabled() (bool, error) {
	var cfg database.SysConfig
	if err := database.DB.Select("value").Where("key = ?", "agent_startup_silent_update_enabled").First(&cfg).Error; err != nil {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(cfg.Value), "true"), nil
}
