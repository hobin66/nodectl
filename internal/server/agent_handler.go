// 路径: internal/server/agent_handler.go
// Agent 相关后端 API：初始化配置、二进制下载、新版极简安装脚本、协议重置
// 对应改造任务 task-05-backend-api.md
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"nodectl/internal/database"
	"nodectl/internal/githubrelease"
	"nodectl/internal/logger"
	"nodectl/internal/service"
	"nodectl/internal/version"
)

// ============================================================
//  5.1 Agent 初始化配置 API
// ============================================================

// apiAgentInitConfig 获取 Agent 初始化配置
// GET /api/agent/init-config?install_id={INSTALL_ID}
// Agent 首次启动时从后端拉取协议配置（启用列表 + 端口映射）
// 密钥/密码/UUID 等凭据由 Agent 本地生成，面板不负责下发
func apiAgentInitConfig(w http.ResponseWriter, r *http.Request) {
	installID := strings.TrimSpace(r.URL.Query().Get("install_id"))
	if installID == "" {
		sendJSON(w, "error", "install_id is required")
		return
	}

	// 验证 install_id 是否存在
	var node database.NodePool
	if err := database.DB.Where("install_id = ?", installID).First(&node).Error; err != nil {
		logger.Log.Warn("Agent init-config: 无效的 install_id", "install_id", installID, "ip", getClientIP(r))
		sendJSON(w, "error", "node not found")
		return
	}

	// 读取面板 URL
	panelURL := loadSysConfigValue("panel_url")

	// 构造协议配置（从节点当前链接推导启用的协议列表）
	// 面板与 Agent 统一使用下划线格式（如 vmess_tcp, vless_wst），无需转换
	enabledProtocols := make([]string, 0)
	if node.Links != nil {
		disabledSet := make(map[string]bool)
		for _, d := range node.DisabledLinks {
			disabledSet[d] = true
		}
		for proto := range node.Links {
			if !disabledSet[proto] {
				enabledProtocols = append(enabledProtocols, proto)
			}
		}
	}

	// 构造 ws_url
	wsURL := ""
	if panelURL != "" {
		wsScheme := "wss"
		if strings.HasPrefix(panelURL, "http://") {
			wsScheme = "ws"
		}
		host := strings.TrimPrefix(strings.TrimPrefix(panelURL, "https://"), "http://")
		host = strings.TrimRight(host, "/")
		wsURL = fmt.Sprintf("%s://%s/api/callback/traffic/ws", wsScheme, host)
	}

	// 🆕 构造端口映射（从节点的 LinkPorts 获取每个协议的端口配置）
	// Agent 使用这些端口配置来启动 sing-box 对应的 inbound
	// 面板与 Agent 统一使用下划线格式，无需转换
	ports := make(map[string]int)
	if node.LinkPorts != nil {
		for proto, port := range node.LinkPorts {
			if port > 0 {
				ports[proto] = port
			}
		}
	}

	// 🆕 从 SysConfig 加载自定义 SNI 配置，下发给 Agent
	sniConfig := loadSNIConfig()

	sendJSON(w, "success", map[string]interface{}{
		"data": map[string]interface{}{
			"protocols": map[string]interface{}{
				"enabled": enabledProtocols,
			},
			"ports":     ports,
			"sni":       sniConfig,
			"panel_url": panelURL,
			"ws_url":    wsURL,
		},
	})
}

// ============================================================
//  5.2 Agent 下载接口
// ============================================================

// apiDownloadAgent 代理 Agent 二进制下载
// GET /api/public/download/agent?arch={amd64|arm64}&channel={stable|alpha}
// 安装脚本通过此接口下载与面板版本匹配的 Agent 二进制
//
// 核心逻辑：读取 GitHub Release 页面并匹配 agent 文件名，避免依赖 GitHub API
// 配额，也避免面板版本号与 agent 版本号不一致导致 404 错误。
func apiDownloadAgent(w http.ResponseWriter, r *http.Request) {
	arch := strings.TrimSpace(r.URL.Query().Get("arch"))
	if arch == "" {
		arch = "amd64"
	}
	channel := strings.TrimSpace(r.URL.Query().Get("channel"))
	if channel == "" {
		channel = string(version.GetChannel())
	}

	// 验证架构参数
	validArchs := map[string]bool{"amd64": true, "arm64": true, "armv7": true, "386": true}
	if !validArchs[arch] {
		http.Error(w, "invalid arch, must be one of: amd64, arm64, armv7, 386", http.StatusBadRequest)
		return
	}

	// 验证渠道参数
	if channel != "stable" && channel != "alpha" && channel != "dev" {
		channel = string(version.GetChannel())
	}

	// 获取 GitHub 仓库信息
	githubRepo := loadSysConfigValue("github_repo")
	if githubRepo == "" {
		githubRepo = "illria/nodectl"
	}

	// 构造 Release Tag（使用面板版本号，因为 Agent 二进制附在面板的 Release 中）
	releaseTag := getPanelReleaseTag(channel)

	// 从常规 GitHub Release 页面动态查找 agent 文件，版本号可独立于面板版本。
	agentPrefix := fmt.Sprintf("nodectl-agent-linux-%s-", arch)
	downloadURL, err := findAgentAssetURL(githubRepo, releaseTag, agentPrefix)
	if err != nil {
		logger.Log.Error("查找 Agent 下载地址失败",
			"repo", githubRepo,
			"tag", releaseTag,
			"prefix", agentPrefix,
			"error", err,
			"ip", getClientIP(r),
		)
		http.Error(w, fmt.Sprintf("unable to find agent binary: %v", err), http.StatusInternalServerError)
		return
	}

	// 重定向到 GitHub Release
	http.Redirect(w, r, downloadURL, http.StatusFound)
}

// ============================================================
//  5.3 新版极简安装脚本 API
// ============================================================

// apiNewInstallScript 新版极简安装脚本生成接口
// GET /api/public/new-install-script?id={INSTALL_ID}
// 生成约 50 行的 bash 脚本，仅负责：下载 agent → 写配置 → 注册系统服务
func apiNewInstallScript(w http.ResponseWriter, r *http.Request) {
	installID := strings.TrimSpace(r.URL.Query().Get("id"))
	if installID == "" {
		logger.Log.Warn("新版安装脚本请求被拦截", "reason", "缺少安装ID", "ip", getClientIP(r))
		http.Error(w, "Missing InstallID", http.StatusBadRequest)
		return
	}

	// 验证 ID 是否有效节点
	var node database.NodePool
	if err := database.DB.Where("install_id = ?", installID).First(&node).Error; err != nil {
		logger.Log.Warn("新版安装脚本请求被拦截", "reason", "无效的安装ID", "id", installID, "ip", getClientIP(r))
		http.Error(w, "Invalid InstallID", http.StatusForbidden)
		return
	}

	// 获取面板 URL
	panelURL := loadSysConfigValue("panel_url")
	isSecure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	panelURL = getPanelURLForScript(panelURL, r.Host, isSecure)

	// 读取 BBR 优化开关
	enableBBR := loadSysConfigValue("proxy_enable_bbr") == "true"

	// 生成新版极简安装脚本
	script := generateMinimalInstallScript(installID, panelURL, enableBBR)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(script))
}

// ============================================================
//  5.5 协议重置接口扩展
// ============================================================

// apiResetProtocol 协议重置接口（面板调用 → 通过 WS 下发命令到 Agent）
// POST /api/callback/reset-protocol
// 支持单协议/多协议重置，以及 disable/enable 操作
func apiResetProtocol(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		InstallID string   `json:"install_id"`
		Protocols []string `json:"protocols"`
		Action    string   `json:"action"` // reset / disable / enable
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, "error", "invalid request body")
		return
	}

	installID := strings.TrimSpace(req.InstallID)
	if installID == "" {
		sendJSON(w, "error", "install_id is required")
		return
	}

	if len(req.Protocols) == 0 {
		sendJSON(w, "error", "protocols list is required")
		return
	}

	action := strings.TrimSpace(req.Action)
	if action == "" {
		action = "reset"
	}
	if action != "reset" && action != "disable" && action != "enable" {
		sendJSON(w, "error", "invalid action, must be reset/disable/enable")
		return
	}

	// 验证节点存在
	var node database.NodePool
	if err := database.DB.Where("install_id = ?", installID).First(&node).Error; err != nil {
		sendJSON(w, "error", "node not found")
		return
	}

	// 根据 action 分发处理
	switch action {
	case "reset":
		// 通过 WebSocket 通道下发 reset-links 命令到 Agent
		payload := map[string]interface{}{
			"protocols": req.Protocols,
		}
		commandID, err := service.FireCommandToNode(installID, "reset-links", payload)
		if err != nil {
			sendJSON(w, "error", fmt.Sprintf("命令下发失败: %v", err))
			return
		}
		sendJSON(w, "success", map[string]interface{}{
			"message":    "reset command sent",
			"command_id": commandID,
		})

	case "disable":
		// 更新数据库中节点的 disabled_links
		disabledSet := make(map[string]bool)
		for _, d := range node.DisabledLinks {
			disabledSet[d] = true
		}
		for _, proto := range req.Protocols {
			disabledSet[proto] = true
		}
		newDisabled := make([]string, 0, len(disabledSet))
		for k := range disabledSet {
			newDisabled = append(newDisabled, k)
		}
		if err := database.DB.Model(&database.NodePool{}).Where("install_id = ?", installID).
			Update("disabled_links", newDisabled).Error; err != nil {
			sendJSON(w, "error", fmt.Sprintf("更新失败: %v", err))
			return
		}
		sendJSON(w, "success", "protocols disabled")

	case "enable":
		// 从 disabled_links 中移除指定协议
		enableSet := make(map[string]bool)
		for _, p := range req.Protocols {
			enableSet[p] = true
		}
		newDisabled := make([]string, 0)
		for _, d := range node.DisabledLinks {
			if !enableSet[d] {
				newDisabled = append(newDisabled, d)
			}
		}
		if err := database.DB.Model(&database.NodePool{}).Where("install_id = ?", installID).
			Update("disabled_links", newDisabled).Error; err != nil {
			sendJSON(w, "error", fmt.Sprintf("更新失败: %v", err))
			return
		}
		sendJSON(w, "success", "protocols enabled")
	}
}

// ============================================================
//  辅助函数
// ============================================================

// loadSysConfigValue 从 sys_config 表读取单个配置值
func loadSysConfigValue(key string) string {
	var cfg database.SysConfig
	if err := database.DB.Where("key = ?", key).Take(&cfg).Error; err != nil {
		return ""
	}
	return strings.TrimSpace(cfg.Value)
}

// loadSNIConfig 从 SysConfig 表加载所有协议的自定义 SNI 配置
// 返回 map[string]string，key 为协议标识（与 Agent 端 ProtocolConfig 字段对应），value 为 SNI 域名
// 仅返回非空的自定义 SNI，Agent 端对未包含的协议继续使用内置默认值
func loadSNIConfig() map[string]string {
	// SysConfig Key -> SNI 配置标识的映射
	keyToProto := map[string]string{
		"proxy_hy2_sni":        "hy2",
		"proxy_tuic_sni":       "tuic",
		"proxy_trojan_sni":     "trojan",
		"proxy_anytls_sni":     "anytls",
		"proxy_vmess_tls_sni":  "vmess_tls",
		"proxy_vless_tls_sni":  "vless_tls",
		"proxy_trojan_tls_sni": "trojan_tls",
		// reality 不再单独下发 SNI，由 Agent 端复用 vless_tls 的 SNI
	}

	// 收集所有需要查询的 Key
	keys := make([]string, 0, len(keyToProto))
	for k := range keyToProto {
		keys = append(keys, k)
	}

	var cfgs []database.SysConfig
	database.DB.Where("key IN ?", keys).Find(&cfgs)

	result := make(map[string]string)
	valueByKey := make(map[string]string, len(cfgs))
	for _, c := range cfgs {
		valueByKey[c.Key] = strings.TrimSpace(c.Value)
	}

	for sysKey, protoKey := range keyToProto {
		if val, ok := valueByKey[sysKey]; ok && val != "" {
			result[protoKey] = val
		}
	}

	return result
}

// ============================================================
//  GitHub Release Asset 动态查找（带缓存）
// ============================================================

// agentAssetCache 缓存 GitHub Release 中 agent 文件的下载 URL
// key = "{repo}|{tag}|{prefix}"，value = browser_download_url
var agentAssetCache = struct {
	sync.RWMutex
	data   map[string]string
	expiry map[string]time.Time
	ttl    time.Duration
}{
	data:   make(map[string]string),
	expiry: make(map[string]time.Time),
	ttl:    5 * time.Minute, // 缓存 5 分钟
}

// findAgentAssetURL 从指定 Release 页面查找匹配前缀的 agent 二进制下载 URL。
// 参数：
//   - repo: GitHub 仓库（如 "illria/nodectl"）
//   - tag:  Release tag（如 "v0.4.32-alpha"）
//   - prefix: 文件名前缀（如 "nodectl-agent-linux-amd64-"）
//
// 返回匹配的第一个（非 .sha256）asset 的 browser_download_url
func findAgentAssetURL(repo, tag, prefix string) (string, error) {
	cacheKey := repo + "|" + tag + "|" + prefix

	// 1. 检查缓存
	agentAssetCache.RLock()
	if url, ok := agentAssetCache.data[cacheKey]; ok {
		if time.Now().Before(agentAssetCache.expiry[cacheKey]) {
			agentAssetCache.RUnlock()
			return url, nil
		}
	}
	agentAssetCache.RUnlock()

	releaseURL := fmt.Sprintf("https://github.com/%s/releases/tag/%s", strings.Trim(repo, "/"), url.PathEscape(tag))
	assetsURL, err := githubrelease.ExpandedAssetsURL(releaseURL)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	body, finalURL, err := githubrelease.FetchReleasePage(context.Background(), client, assetsURL, "nodectl-panel/"+version.Version)
	if err != nil {
		return "", fmt.Errorf("读取 GitHub release 页面失败: %w", err)
	}
	assets := githubrelease.Assets(body, finalURL)

	// 在页面中查找匹配前缀且非 .sha256 的文件。
	for _, asset := range assets {
		if strings.HasPrefix(asset.Name, prefix) && !strings.HasSuffix(asset.Name, ".sha256") {
			// 写入缓存
			agentAssetCache.Lock()
			agentAssetCache.data[cacheKey] = asset.URL
			agentAssetCache.expiry[cacheKey] = time.Now().Add(agentAssetCache.ttl)
			agentAssetCache.Unlock()

			return asset.URL, nil
		}
	}

	return "", fmt.Errorf("Release %s 中未找到匹配 %s* 的 agent 文件", tag, prefix)
}

// getPanelReleaseTag 根据渠道使用面板版本号生成 GitHub Release Tag
// 因为 Agent 二进制文件附在面板的 Release 中发布，所以 tag 必须是面板版本号
// 工作流规则：
//   - release-main.yml:  tag = version.Version（如 "v0.4.0"）
//   - release-alpha.yml: tag = version.Version（如 "v0.4.0-alpha"，编译时已注入含 -alpha 的版本号）
func getPanelReleaseTag(channel string) string {
	panelVer := version.Version

	if channel == "alpha" {
		// Alpha 渠道：面板版本号应已包含 -alpha 后缀（编译时通过 ldflags 注入）
		// 但为安全起见做兜底处理
		if !strings.HasSuffix(panelVer, "-alpha") {
			return panelVer + "-alpha"
		}
		return panelVer
	}
	// Stable 渠道：面板版本号无后缀
	return strings.TrimSuffix(panelVer, "-alpha")
}
