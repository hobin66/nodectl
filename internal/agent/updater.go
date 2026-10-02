// 路径: internal/agent/updater.go
//go:build linux

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"nodectl/internal/githubrelease"

	"golang.org/x/mod/semver"
)

// GitHub 仓库坐标（编译期可通过 ldflags 覆盖）
var (
	UpdateRepoOwner = "illria"
	UpdateRepoName  = "nodectl"
)

const (
	// 检查更新周期
	updateCheckInterval = 6 * time.Hour
	// 启动后随机抖动上限，避免大量 agent 同时请求 GitHub
	updateInitialJitter = 10 * time.Minute
	// 下载缓冲大小（低内存）
	downloadBufSize = 64 * 1024 // 64 KiB
	// 健康检查超时（新版启动后在此时间内完成 WS 握手即为健康）
	// 设置为 5 分钟：Connect() 本身有 15s 超时，一次失败+退避+重试轻松超过 30s
	healthCheckTimeout = 5 * time.Minute
	// 崩溃循环阈值：连续 N 次在 healthCheckTimeout 内退出则锁定
	maxCrashCount = 3
	// HTTP 请求超时
	httpTimeout = 60 * time.Second
	// 产物文件名正则（匹配 nodectl-agent-linux-amd64-v1.4.2 或 nodectl-agent-linux-amd64-v1.4.2-alpha）
	assetPattern = `^nodectl-agent-linux-%s-(v\d+\.\d+\.\d+.*)$`
)

// updateState 更新状态持久化结构体
type updateState struct {
	CrashCount    int    `json:"crash_count"`              // 连续崩溃次数
	Locked        bool   `json:"locked"`                   // 是否锁定更新
	LockedVersion string `json:"locked_version,omitempty"` // 锁定时的版本号（出现新版后解锁）
	LastCheck     int64  `json:"last_check,omitempty"`     // 上次检查时间戳
}

// Updater Agent 自动更新器（仅 Linux）
type Updater struct {
	mu       sync.Mutex
	selfPath string       // 当前二进制绝对路径
	stateDir string       // 状态文件目录
	client   *http.Client // 复用 HTTP client
	state    updateState  // 更新状态
}

// NewUpdater 创建更新器实例
func NewUpdater() (*Updater, error) {
	selfPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("获取自身路径失败: %w", err)
	}
	selfPath, err = filepath.EvalSymlinks(selfPath)
	if err != nil {
		return nil, fmt.Errorf("解析符号链接失败: %w", err)
	}

	stateDir := filepath.Dir(selfPath)

	u := &Updater{
		selfPath: selfPath,
		stateDir: stateDir,
		client: &http.Client{
			Timeout: httpTimeout,
		},
	}

	// 加载持久化更新状态
	u.loadState()

	return u, nil
}

// statePath 返回状态文件路径
func (u *Updater) statePath() string {
	return filepath.Join(u.stateDir, ".update_state.json")
}

// loadState 加载更新状态
func (u *Updater) loadState() {
	data, err := os.ReadFile(u.statePath())
	if err != nil {
		return // 文件不存在时使用零值
	}
	json.Unmarshal(data, &u.state)
}

// saveState 持久化更新状态
func (u *Updater) saveState() {
	data, _ := json.Marshal(&u.state)
	tmp := u.statePath() + ".tmp"
	if f, err := os.Create(tmp); err == nil {
		f.Write(data)
		f.Sync()
		f.Close()
		os.Rename(tmp, u.statePath())
	}
}

// RecordStartup 记录启动事件（供 main.go 调用，用于崩溃循环检测）
// 返回 true 表示需要回滚（崩溃次数已达上限）
func (u *Updater) RecordStartup() bool {
	u.mu.Lock()
	defer u.mu.Unlock()

	bakPath := u.selfPath + ".bak"

	// 检查是否存在 .bak 文件（说明刚刚经历了更新）
	if _, err := os.Stat(bakPath); err != nil {
		// 没有 .bak 文件，不是更新后启动，重置计数器
		if u.state.CrashCount > 0 {
			u.state.CrashCount = 0
			u.saveState()
		}
		return false
	}

	// 存在 .bak 文件 → 更新后启动，递增崩溃计数
	u.state.CrashCount++
	u.saveState()

	if u.state.CrashCount >= maxCrashCount {
		log.Printf("[Updater] 连续崩溃 %d 次，执行回滚...", u.state.CrashCount)
		if err := os.Rename(bakPath, u.selfPath); err != nil {
			log.Printf("[Updater] 回滚失败: %v", err)
			return false
		}
		u.state.Locked = true
		u.state.LockedVersion = AgentVersion
		u.state.CrashCount = 0
		u.saveState()
		log.Printf("[Updater] 已回滚并锁定版本 %s，将不再自动更新此版本", AgentVersion)
		return true // 通知 main.go 需要重启（使用回滚后的二进制）
	}

	return false
}

// MarkHealthy 标记启动成功（WS 首次握手后调用）
// 清除崩溃计数并删除 .bak 文件
func (u *Updater) MarkHealthy() {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.state.CrashCount > 0 {
		u.state.CrashCount = 0
		u.saveState()
	}

	// 删除 .bak 文件（更新确认成功）
	bakPath := u.selfPath + ".bak"
	os.Remove(bakPath)
}

// Run 启动更新检查循环（阻塞，应在独立 goroutine 中运行）
func (u *Updater) Run(ctx context.Context) {
	// dev 版本不检查更新
	if AgentVersion == "dev" || AgentVersion == "" {
		log.Printf("[Updater] dev 版本，跳过自动更新")
		return
	}

	// 启动后随机抖动 0~10min
	jitter := time.Duration(time.Now().UnixNano()%int64(updateInitialJitter/time.Millisecond)) * time.Millisecond
	log.Printf("[Updater] 首次检查将在 %v 后开始 (渠道: %s)", jitter.Round(time.Second), GetChannel())

	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	// 首次立即检查一次
	u.checkAndUpdate(ctx)

	ticker := time.NewTicker(updateCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			u.checkAndUpdate(ctx)
		}
	}
}

// TriggerCheck 手动触发一次更新检查（供远程命令调用）
func (u *Updater) TriggerCheck(ctx context.Context) error {
	if AgentVersion == "dev" || AgentVersion == "" {
		return fmt.Errorf("dev 版本不支持更新检查")
	}
	u.checkAndUpdate(ctx)
	return nil
}

// IsPostUpdatePending 返回当前是否处于"更新后待健康确认"阶段（存在 .bak）
func (u *Updater) IsPostUpdatePending() bool {
	bakPath := u.selfPath + ".bak"
	_, err := os.Stat(bakPath)
	return err == nil
}

// HealthTimeout 返回更新后首个 WS 握手健康检查超时时间
func (u *Updater) HealthTimeout() time.Duration {
	return healthCheckTimeout
}

// checkAndUpdate 执行一次完整的检查和更新流程
func (u *Updater) checkAndUpdate(ctx context.Context) {
	if !u.mu.TryLock() {
		return // 另一个更新流程正在进行
	}
	defer u.mu.Unlock()

	channel := GetChannel()
	log.Printf("[Updater] 开始检查更新 (当前版本: %s, 渠道: %s)", AgentVersion, channel)

	// 1. 根据渠道查询最新 release
	targetVersion, downloadURL, sha256URL, err := u.findLatestAgentRelease(ctx)
	if err != nil {
		log.Printf("[Updater] 检查更新失败: %v", err)
		return
	}

	// 2. 版本比较
	localVer := AgentVersion
	if !strings.HasPrefix(localVer, "v") {
		localVer = "v" + localVer
	}
	if !semver.IsValid(localVer) {
		log.Printf("[Updater] 本地版本号无效: %s，跳过更新", AgentVersion)
		return
	}
	if !semver.IsValid(targetVersion) {
		log.Printf("[Updater] 远端版本号无效: %s，跳过更新", targetVersion)
		return
	}

	// 渠道匹配检查：确保目标版本与当前渠道匹配
	targetPre := semver.Prerelease(targetVersion)
	switch channel {
	case ChannelAlpha:
		// Alpha 渠道：目标必须是 alpha 版本
		if !strings.Contains(strings.ToLower(targetPre), "alpha") {
			log.Printf("[Updater] Alpha 渠道但目标版本 %s 不是 alpha 版本，跳过", targetVersion)
			return
		}
	case ChannelStable:
		// 正式渠道：目标必须是正式版本（无预发布后缀）
		if targetPre != "" {
			log.Printf("[Updater] 正式渠道但目标版本 %s 是预发布版本，跳过", targetVersion)
			return
		}
	default:
		log.Printf("[Updater] 未知渠道 %s，跳过更新", channel)
		return
	}

	if semver.Compare(targetVersion, localVer) <= 0 {
		log.Printf("[Updater] 已是最新版本 (%s >= %s)", localVer, targetVersion)
		u.state.LastCheck = time.Now().Unix()
		u.saveState()
		return
	}

	// 3. 锁定状态检查（必须在获取 targetVersion 之后才能判断是否有更新的新版可以解锁）
	// Bug 修复：旧代码在函数开头 if locked { return } 导致此处解锁代码永远不会执行，
	// 锁定状态一旦设置就永久有效。正确做法是先查询远端版本，再决定是否跳过或解锁。
	if u.state.Locked {
		if u.state.LockedVersion == targetVersion {
			// 最新版本与锁定版本相同，仍在锁定范围内，跳过
			log.Printf("[Updater] 版本 %s 更新已锁定（曾导致崩溃），跳过本次更新", targetVersion)
			return
		}
		// 远端已发布更新的新版本，解除旧锁定，尝试更新
		log.Printf("[Updater] 检测到新版本 %s（锁定版本为 %s），解除锁定并尝试更新",
			targetVersion, u.state.LockedVersion)
		u.state.Locked = false
		u.state.LockedVersion = ""
		u.saveState()
	}

	log.Printf("[Updater] 发现新版本: %s → %s，开始更新", localVer, targetVersion)

	// 4. 下载 sha256 校验文件
	expectedHash, err := u.downloadSHA256(ctx, sha256URL)
	if err != nil {
		log.Printf("[Updater] 下载 SHA256 文件失败: %v", err)
		return
	}

	// 5. 流式下载二进制 + 边下载边计算哈希
	tmpPath := u.selfPath + ".new.tmp"
	actualHash, err := u.downloadBinary(ctx, downloadURL, tmpPath)
	if err != nil {
		log.Printf("[Updater] 下载二进制失败: %v", err)
		os.Remove(tmpPath)
		return
	}

	// 6. 校验哈希
	if actualHash != expectedHash {
		log.Printf("[Updater] SHA256 校验失败: 预期 %s, 实际 %s", expectedHash, actualHash)
		os.Remove(tmpPath)
		return
	}
	log.Printf("[Updater] SHA256 校验通过")

	// 7. staging: 设置可执行权限
	if err := os.Chmod(tmpPath, 0755); err != nil {
		log.Printf("[Updater] chmod 失败: %v", err)
		os.Remove(tmpPath)
		return
	}

	// 8. staging → switching: 原子切换
	newPath := u.selfPath + ".new"
	if err := os.Rename(tmpPath, newPath); err != nil {
		log.Printf("[Updater] staging 重命名失败: %v", err)
		os.Remove(tmpPath)
		return
	}

	bakPath := u.selfPath + ".bak"
	// 清理可能存在的旧 .bak
	os.Remove(bakPath)

	// agent → agent.bak
	if err := os.Rename(u.selfPath, bakPath); err != nil {
		log.Printf("[Updater] 备份当前版本失败: %v", err)
		// 恢复 .new 文件
		os.Rename(newPath, tmpPath)
		os.Remove(tmpPath)
		return
	}

	// agent.new → agent
	if err := os.Rename(newPath, u.selfPath); err != nil {
		log.Printf("[Updater] 切换新版本失败: %v", err)
		// 回滚
		os.Rename(bakPath, u.selfPath)
		return
	}

	// 9. 重置崩溃计数器（新的更新周期开始）
	u.state.CrashCount = 0
	u.state.LastCheck = time.Now().Unix()
	u.saveState()

	log.Printf("[Updater] 更新完成 %s → %s，准备加载新进程", localVer, targetVersion)

	// 10. 就地替换进程镜像（execve），成功后不会返回
	// 优点：PID 不变，systemd 无需感知；新二进制从 main() 重新初始化。
	u.reexec(u.selfPath)
}

// reexec 通过 execve 就地替换进程镜像为 binPath。
// 成功时永不返回；失败时以 exit(1) 退出，由 systemd/supervise 重新拉起服务。
// 注意：不能发送 SIGINT/SIGTERM 作为回退——那会导致 rt.Run() 返回 nil，
// 进程以 exit(0) 退出，而 Restart=on-failure 不会重启 exit(0) 的进程。
func (u *Updater) reexec(binPath string) {
	execArgs := append([]string{binPath}, os.Args[1:]...)
	if err := syscall.Exec(binPath, execArgs, os.Environ()); err != nil {
		log.Printf("[Updater] 就地重启失败: %v，以 exit(1) 退出等待 systemd 重新拉起", err)
		os.Exit(1)
	}
	// 成功：永不到达此处
}

// RollbackAndReexec 紧急回滚：将 .bak 还原为正式版本，然后就地加载旧版本二进制。
// 用于健康检查超时时不经 systemd 重启直接原地恢复，减少 StartLimitBurst 消耗。
// 该函数不会正常返回（execve 成功 或 os.Exit(1)）。
func (u *Updater) RollbackAndReexec() {
	u.mu.Lock()
	bakPath := u.selfPath + ".bak"
	if _, err := os.Stat(bakPath); err != nil {
		u.mu.Unlock()
		log.Printf("[Updater] 紧急回滚失败：.bak 文件不存在，以 exit(1) 退出")
		os.Exit(1)
	}
	if err := os.Rename(bakPath, u.selfPath); err != nil {
		u.mu.Unlock()
		log.Printf("[Updater] 紧急回滚失败：%v，以 exit(1) 退出", err)
		os.Exit(1)
	}
	u.state.Locked = true
	u.state.LockedVersion = AgentVersion // 锁定刚才失败的新版本
	u.state.CrashCount = 0
	u.saveState()
	u.mu.Unlock()

	log.Printf("[Updater] 已回滚到旧版本，就地加载旧二进制...")
	u.reexec(u.selfPath)
}

// ReexecSelf 就地加载当前 selfPath 指向的二进制（回滚已完成后由 main.go 调用）。
// 该函数不会正常返回（execve 成功 或 os.Exit(1)）。
func (u *Updater) ReexecSelf() {
	log.Printf("[Updater] 就地加载当前版本二进制: %s", u.selfPath)
	u.reexec(u.selfPath)
}

// ============================================================
//  GitHub API 交互
// ============================================================

// ghRelease GitHub Release API 响应（仅需要的字段）
type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

// ghAsset GitHub Release Asset
type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// findLatestAgentRelease 查询最新 release 并匹配当前架构的 agent 产物
// Alpha 单独列出预发布版本；Stable 通过 latest 重定向读取 Release 页面。
// 返回: 目标版本号、二进制下载 URL、sha256 下载 URL
func (u *Updater) findLatestAgentRelease(ctx context.Context) (version, binaryURL, sha256URL string, err error) {
	channel := GetChannel()

	switch channel {
	case ChannelAlpha:
		// Alpha 版本：查询所有 releases，筛选最新的 alpha 版本
		return u.findLatestAlphaRelease(ctx)
	case ChannelStable:
		// 稳定版路径不访问 GitHub API。
		return u.findLatestStableRelease(ctx)
	default:
		return "", "", "", fmt.Errorf("不支持更新检查的渠道: %s", channel)
	}
}

// findLatestStableRelease 从 releases/latest 的重定向页面寻找当前架构的 Agent 产物。
func (u *Updater) findLatestStableRelease(ctx context.Context) (version, binaryURL, sha256URL string, err error) {
	latestURL := fmt.Sprintf("https://github.com/%s/%s/releases/latest", UpdateRepoOwner, UpdateRepoName)
	_, releasePageURL, err := githubrelease.FetchLatestRelease(ctx, u.client, latestURL, fmt.Sprintf("nodectl-agent/%s", AgentVersion))
	if err != nil {
		return "", "", "", fmt.Errorf("获取稳定版 release tag 失败: %w", err)
	}
	assetsURL, err := githubrelease.ExpandedAssetsURL(releasePageURL)
	if err != nil {
		return "", "", "", err
	}
	body, finalAssetsURL, err := githubrelease.FetchReleasePage(ctx, u.client, assetsURL, fmt.Sprintf("nodectl-agent/%s", AgentVersion))
	if err != nil {
		return "", "", "", fmt.Errorf("获取稳定版 Agent 产物页面失败: %w", err)
	}
	assets := githubrelease.Assets(body, finalAssetsURL)
	arch := runtime.GOARCH
	pattern := regexp.MustCompile(fmt.Sprintf(assetPattern, regexp.QuoteMeta(arch)))
	var matchedAsset *githubrelease.Asset
	var matchedVersion string
	for i := range assets {
		if strings.HasSuffix(assets[i].Name, ".sha256") {
			continue
		}
		matches := pattern.FindStringSubmatch(assets[i].Name)
		if matches != nil && len(matches) >= 2 {
			matchedAsset = &assets[i]
			matchedVersion = matches[1]
			break
		}
	}
	if matchedAsset == nil {
		return "", "", "", fmt.Errorf("稳定版 release 中未找到 %s 架构的 agent 产物", arch)
	}

	sha256Name := matchedAsset.Name + ".sha256"
	for _, asset := range assets {
		if asset.Name == sha256Name {
			return matchedVersion, matchedAsset.URL, asset.URL, nil
		}
	}
	return "", "", "", fmt.Errorf("稳定版 release 中未找到 SHA256 校验文件: %s", sha256Name)
}

// findLatestAlphaRelease 获取最新的 Alpha 版本（alpha 分支）
// 从所有 releases 中筛选出最新的 alpha 版本
func (u *Updater) findLatestAlphaRelease(ctx context.Context) (version, binaryURL, sha256URL string, err error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases", UpdateRepoOwner, UpdateRepoName)

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", fmt.Sprintf("nodectl-agent/%s", AgentVersion))

	resp, err := u.client.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("请求 GitHub API 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("GitHub API 返回 %d", resp.StatusCode)
	}

	// 限制读取体积（防止异常响应撑爆内存）
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return "", "", "", fmt.Errorf("读取响应失败: %w", err)
	}

	var releases []ghRelease
	if err := json.Unmarshal(body, &releases); err != nil {
		return "", "", "", fmt.Errorf("解析 releases JSON 失败: %w", err)
	}

	// 筛选最新的 alpha 版本
	arch := runtime.GOARCH
	pattern := regexp.MustCompile(fmt.Sprintf(assetPattern, regexp.QuoteMeta(arch)))

	type matchedRelease struct {
		version   string
		binaryURL string
		sha256URL string
		tagName   string
	}

	var latestAlpha *matchedRelease

	for i := range releases {
		tagLower := strings.ToLower(releases[i].TagName)
		// 只考虑 alpha 版本
		if !strings.Contains(tagLower, "-alpha") {
			continue
		}

		// 查找匹配当前架构的 asset
		var matchedAsset *ghAsset
		var matchedVersion string

		for j := range releases[i].Assets {
			asset := &releases[i].Assets[j]
			if strings.HasSuffix(asset.Name, ".sha256") {
				continue
			}
			matches := pattern.FindStringSubmatch(asset.Name)
			if matches != nil && len(matches) >= 2 {
				matchedAsset = asset
				matchedVersion = matches[1]
				break
			}
		}

		if matchedAsset == nil {
			continue // 该 release 没有匹配当前架构的产物
		}

		// 查找对应的 .sha256 文件
		sha256Name := matchedAsset.Name + ".sha256"
		var sha256Asset *ghAsset
		for j := range releases[i].Assets {
			if releases[i].Assets[j].Name == sha256Name {
				sha256Asset = &releases[i].Assets[j]
				break
			}
		}

		if sha256Asset == nil {
			continue // 缺少校验文件，跳过
		}

		candidate := &matchedRelease{
			version:   matchedVersion,
			binaryURL: matchedAsset.BrowserDownloadURL,
			sha256URL: sha256Asset.BrowserDownloadURL,
			tagName:   releases[i].TagName,
		}

		// 使用 semver 比较版本
		candidateVer := "v" + strings.TrimPrefix(matchedVersion, "v")
		if !semver.IsValid(candidateVer) {
			continue
		}

		if latestAlpha == nil {
			latestAlpha = candidate
		} else {
			currentVer := "v" + strings.TrimPrefix(latestAlpha.version, "v")
			if semver.Compare(candidateVer, currentVer) > 0 {
				latestAlpha = candidate
			}
		}
	}

	if latestAlpha == nil {
		return "", "", "", fmt.Errorf("未找到 Alpha 版本的 agent 产物 (架构: %s)", arch)
	}

	return latestAlpha.version, latestAlpha.binaryURL, latestAlpha.sha256URL, nil
}

// downloadSHA256 下载 .sha256 文件并提取哈希值
func (u *Updater) downloadSHA256(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", fmt.Sprintf("nodectl-agent/%s", AgentVersion))

	resp, err := u.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载 SHA256 文件返回 %d", resp.StatusCode)
	}

	// sha256 文件很小，限制 1KiB
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", err
	}

	// 格式: "<hash>  <filename>\n" 或 "<hash> <filename>\n"
	line := strings.TrimSpace(string(data))
	parts := strings.Fields(line)
	if len(parts) < 1 {
		return "", fmt.Errorf("SHA256 文件格式异常: %q", line)
	}

	hash := strings.ToLower(parts[0])
	if len(hash) != 64 {
		return "", fmt.Errorf("SHA256 哈希长度异常: %d", len(hash))
	}

	return hash, nil
}

// downloadBinary 流式下载二进制文件并同时计算 SHA256
// 返回计算出的哈希（小写 hex）
func (u *Updater) downloadBinary(ctx context.Context, url string, destPath string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", fmt.Sprintf("nodectl-agent/%s", AgentVersion))

	resp, err := u.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载二进制文件返回 %d", resp.StatusCode)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf("创建临时文件失败: %w", err)
	}
	defer f.Close()

	// 使用 TeeReader 边下载边计算哈希
	hasher := sha256.New()
	reader := io.TeeReader(resp.Body, hasher)

	buf := make([]byte, downloadBufSize)
	if _, err := io.CopyBuffer(f, reader, buf); err != nil {
		return "", fmt.Errorf("下载写入失败: %w", err)
	}

	// fsync 确保落盘
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("fsync 失败: %w", err)
	}

	hash := hex.EncodeToString(hasher.Sum(nil))
	return hash, nil
}
