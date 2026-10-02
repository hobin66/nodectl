// 路径: internal/service/cf_tunnel.go
// Cloudflare Tunnel 集成服务层：API 交互 + cloudflared 进程编排
package service

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"nodectl/internal/database"
	"nodectl/internal/logger"
)

// ===================== 常量与路径约定 =====================

const (
	cfTunnelBaseDir   = "data/cf/tunnel"
	cfTunnelBinDir    = "data/cf/tunnel/bin"
	cfTunnelLogDir    = "data/cf/tunnel/logs"
	cfTunnelConfigYml = "data/cf/tunnel/config.yml"
	cfTunnelPIDFile   = "data/cf/tunnel/cloudflared.pid"
)

// getAutoOriginURL 自动获取本地服务回源地址（端口跟随 dbconfig.json 中的 web_port 配置）
func getAutoOriginURL() string {
	port := database.GetWebPort()
	certEnabled := strings.ToLower(strings.TrimSpace(getCFConfig("cf_cert_enabled")))
	if (certEnabled == "true" || certEnabled == "1") && HasValidLocalCertificate() {
		return fmt.Sprintf("https://127.0.0.1:%d", port)
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// cfTunnelBinaryName 返回当前平台的 cloudflared 二进制名
func cfTunnelBinaryName() string {
	if runtime.GOOS == "windows" {
		return "cloudflared.exe"
	}
	return "cloudflared"
}

// cfTunnelBinaryPath 返回 cloudflared 二进制的完整路径
func cfTunnelBinaryPath() string {
	return filepath.Join(cfTunnelBinDir, cfTunnelBinaryName())
}

// cfTunnelCredentialsPath 返回凭据文件路径（按 Tunnel ID 约定）
func cfTunnelCredentialsPath(tunnelID string) string {
	return filepath.Join(cfTunnelBaseDir, tunnelID+".json")
}

// ===================== 进程管理 =====================

var (
	cfProcessMu   sync.Mutex
	cfProcess     *os.Process
	cfProcessCmd  *exec.Cmd
	cfStartedAt   time.Time
	cfProcessPID  int
	cfRunning     bool
	cfStopRequest bool // 标记是否为用户主动停止
)

// CFTunnelStatus Tunnel 运行状态信息
type CFTunnelStatus struct {
	Running          bool   `json:"running"`
	PID              int    `json:"pid"`
	Uptime           string `json:"uptime"`
	UptimeSeconds    int64  `json:"uptime_seconds"`
	CloudflaredVer   string `json:"cloudflared_version"`
	TunnelID         string `json:"tunnel_id"`
	TunnelName       string `json:"tunnel_name"`
	Subdomain        string `json:"subdomain"`
	AccessURL        string `json:"access_url"` // 完整访问地址，如 https://panel.example.com
	Enabled          bool   `json:"enabled"`
	BindMainProcess  bool   `json:"bind_main_process"`
	BinaryExists     bool   `json:"binary_exists"`
	ConfigExists     bool   `json:"config_exists"`
	CredentialsExist bool   `json:"credentials_exist"`
	Platform         string `json:"platform"`
	InDocker         bool   `json:"in_docker"`
}

// isRunningInDocker 检测是否运行在 Docker/容器环境中
func isRunningInDocker() bool {
	// 检查 /.dockerenv 文件（Docker 标准标识）
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	// 检查 /proc/1/cgroup 中是否包含 docker/containerd/lxc
	if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		content := strings.ToLower(string(data))
		if strings.Contains(content, "docker") || strings.Contains(content, "containerd") ||
			strings.Contains(content, "lxc") || strings.Contains(content, "kubepods") {
			return true
		}
	}
	return false
}

// ===================== 配置读写 =====================

// CFTunnelSettings 前端需要的 Tunnel 配置信息
type CFTunnelSettings struct {
	HasCFKey       bool   `json:"has_cf_key"`        // Token 是否已配置（不返回 Token 内容）
	CFAPIKeyMasked string `json:"cf_api_key_masked"` // Token 脱敏占位符（仅用于前端显示）
	CFEmail        string `json:"cf_email"`
	CFAccountID    string `json:"cf_account_id"`
	CFDomain       string `json:"cf_domain"`

	TunnelName      string `json:"cf_tunnel_name"`
	TunnelID        string `json:"cf_tunnel_id"`
	TunnelSubdomain string `json:"cf_tunnel_subdomain"`
	TunnelOriginURL string `json:"cf_tunnel_origin_url"`
	BindMainProcess string `json:"cf_tunnel_bind_main_process"`
	TunnelEnabled   string `json:"cf_tunnel_enabled"`
}

// CFTunnelListItem Cloudflare Tunnel 列表项（用于前端展示）
type CFTunnelListItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// GetCFTunnelSettings 读取 Tunnel 相关配置（token 脱敏）
func GetCFTunnelSettings() CFTunnelSettings {
	keys := []string{
		"cf_api_key", "cf_email", "cf_account_id", "cf_domain",
		"cf_tunnel_name", "cf_tunnel_id", "cf_tunnel_subdomain",
		"cf_tunnel_origin_url", "cf_tunnel_bind_main_process", "cf_tunnel_enabled",
	}
	var configs []database.SysConfig
	database.DB.Where("key IN ?", keys).Find(&configs)

	m := make(map[string]string)
	for _, c := range configs {
		m[c.Key] = c.Value
	}

	// 安全策略：完全不向前端返回 Token 内容，只暴露是否已配置
	hasCFKey := strings.TrimSpace(m["cf_api_key"]) != ""
	maskedToken := ""
	if hasCFKey {
		maskedToken = "*********"
	}

	return CFTunnelSettings{
		HasCFKey:        hasCFKey,
		CFAPIKeyMasked:  maskedToken,
		CFEmail:         m["cf_email"],
		CFAccountID:     m["cf_account_id"],
		CFDomain:        m["cf_domain"],
		TunnelName:      m["cf_tunnel_name"],
		TunnelID:        m["cf_tunnel_id"],
		TunnelSubdomain: m["cf_tunnel_subdomain"],
		TunnelOriginURL: m["cf_tunnel_origin_url"],
		BindMainProcess: m["cf_tunnel_bind_main_process"],
		TunnelEnabled:   m["cf_tunnel_enabled"],
	}
}

// SaveCFTunnelSettings 保存 Tunnel 配置
func SaveCFTunnelSettings(data map[string]string) error {
	validKeys := map[string]bool{
		"cf_api_key": true, "cf_email": true, "cf_account_id": true, "cf_domain": true,
		"cf_tunnel_name": true, "cf_tunnel_subdomain": true, "cf_tunnel_token": true,
		"cf_tunnel_origin_url": true, "cf_tunnel_bind_main_process": true, "cf_tunnel_enabled": true,
	}

	for key, value := range data {
		if !validKeys[key] {
			continue
		}
		// 跳过脱敏占位符（仅星号）与空值
		trimmed := strings.TrimSpace(value)
		if key == "cf_api_key" && (trimmed == "" || (strings.Trim(trimmed, "*") == "" && len(trimmed) > 0)) {
			continue
		}
		if err := database.DB.Model(&database.SysConfig{}).Where("key = ?", key).
			Update("value", value).Error; err != nil {
			return fmt.Errorf("保存配置 %s 失败: %w", key, err)
		}
	}
	return nil
}

// getCFConfig 内部读取原始配置值（不脱敏）
func getCFConfig(key string) string {
	var cfg database.SysConfig
	database.DB.Where("key = ?", key).First(&cfg)
	return strings.TrimSpace(cfg.Value)
}

// setCFConfig 内部写入配置
func setCFConfig(key, value string) {
	database.DB.Model(&database.SysConfig{}).Where("key = ?", key).Update("value", value)
}

// ===================== Cloudflare API 交互 =====================

// cfAPIRequest 通用 Cloudflare API 请求
func cfAPIRequest(method, url string, body interface{}) (map[string]interface{}, error) {
	token := getCFConfig("cf_api_key")
	if token == "" {
		return nil, fmt.Errorf("CF API Token 未配置")
	}

	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("序列化请求体失败: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w, body: %s", err, string(respBody))
	}

	if success, ok := result["success"].(bool); ok && !success {
		errMsg := string(respBody)
		if errors, ok := result["errors"].([]interface{}); ok && len(errors) > 0 {
			if errObj, ok := errors[0].(map[string]interface{}); ok {
				if msg, ok := errObj["message"].(string); ok {
					errMsg = msg
				}
			}
		}
		return result, fmt.Errorf("Cloudflare API 错误: %s", errMsg)
	}

	return result, nil
}

// TestCFCredentials 测试 CF Token + Account ID + Zone 有效性
func TestCFCredentials() (string, error) {
	token := getCFConfig("cf_api_key")
	accountID := getCFConfig("cf_account_id")
	domain := getCFConfig("cf_domain")

	if token == "" {
		return "", fmt.Errorf("CF API Token 未配置")
	}
	if accountID == "" {
		return "", fmt.Errorf("CF Account ID 未配置")
	}
	if domain == "" {
		return "", fmt.Errorf("CF 域名未配置")
	}

	// 1. 验证 Token（获取用户信息）
	result, err := cfAPIRequest("GET", "https://api.cloudflare.com/client/v4/user/tokens/verify", nil)
	if err != nil {
		return "", fmt.Errorf("Token 验证失败: %w", err)
	}

	tokenStatus := ""
	if r, ok := result["result"].(map[string]interface{}); ok {
		if s, ok := r["status"].(string); ok {
			tokenStatus = s
		}
	}

	// 2. 验证 Account ID
	_, err = cfAPIRequest("GET",
		fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s", accountID), nil)
	if err != nil {
		return "", fmt.Errorf("Account ID 验证失败: %w", err)
	}

	// 3. 验证 Zone
	zoneResult, err := cfAPIRequest("GET",
		fmt.Sprintf("https://api.cloudflare.com/client/v4/zones?name=%s", domain), nil)
	if err != nil {
		return "", fmt.Errorf("Zone 验证失败: %w", err)
	}

	zoneName := domain
	if r, ok := zoneResult["result"].([]interface{}); ok && len(r) > 0 {
		if z, ok := r[0].(map[string]interface{}); ok {
			if n, ok := z["name"].(string); ok {
				zoneName = n
			}
		}
	} else {
		return "", fmt.Errorf("未找到域名 %s 对应的 Zone", domain)
	}

	return fmt.Sprintf("验证通过！Token 状态: %s，Zone: %s", tokenStatus, zoneName), nil
}

// ===================== cloudflared 二进制管理 =====================

// cloudflaredDownloadURL 返回 cloudflared 下载地址
func cloudflaredDownloadURL() (string, error) {
	goos := runtime.GOOS
	goarch := runtime.GOARCH

	var filename string
	switch goos {
	case "linux":
		switch goarch {
		case "amd64":
			filename = "cloudflared-linux-amd64"
		case "arm64":
			filename = "cloudflared-linux-arm64"
		case "arm":
			filename = "cloudflared-linux-arm"
		case "386":
			filename = "cloudflared-linux-386"
		default:
			return "", fmt.Errorf("不支持的 Linux 架构: %s", goarch)
		}
	case "darwin":
		switch goarch {
		case "amd64":
			filename = "cloudflared-darwin-amd64.tgz"
		case "arm64":
			filename = "cloudflared-darwin-amd64.tgz" // darwin arm64 uses same binary via Rosetta or universal
		default:
			return "", fmt.Errorf("不支持的 macOS 架构: %s", goarch)
		}
	case "windows":
		switch goarch {
		case "amd64":
			filename = "cloudflared-windows-amd64.exe"
		case "386":
			filename = "cloudflared-windows-386.exe"
		default:
			return "", fmt.Errorf("不支持的 Windows 架构: %s", goarch)
		}
	default:
		return "", fmt.Errorf("不支持的操作系统: %s", goos)
	}

	return "https://github.com/cloudflare/cloudflared/releases/latest/download/" + filename, nil
}

// CheckCloudflaredBinary 检查 cloudflared 是否已存在
func CheckCloudflaredBinary() (bool, string) {
	binPath := cfTunnelBinaryPath()
	if _, err := os.Stat(binPath); err != nil {
		return false, ""
	}
	// 尝试获取版本号
	ver := getCloudflaredVersion()
	return true, ver
}

// getCloudflaredVersion 获取 cloudflared 版本号
func getCloudflaredVersion() string {
	binPath := cfTunnelBinaryPath()
	cmd := exec.Command(binPath, "version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "未知"
	}
	// 输出格式类似: cloudflared version 2024.1.2 (built 2024-01-15-1200 UTC)
	line := strings.TrimSpace(string(out))
	if idx := strings.Index(line, "version "); idx != -1 {
		rest := line[idx+8:]
		if sp := strings.Index(rest, " "); sp != -1 {
			return rest[:sp]
		}
		return rest
	}
	return strings.Split(line, "\n")[0]
}

// DownloadCloudflared 下载 cloudflared 二进制，通过 progressFn 回调进度
// progressFn(downloaded, total, percent) - total 为 -1 表示未知大小
func DownloadCloudflared(progressFn func(downloaded, total int64, percent int)) error {
	downloadURL, err := cloudflaredDownloadURL()
	if err != nil {
		return err
	}

	// 确保目录存在
	if err := os.MkdirAll(cfTunnelBinDir, 0755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}

	logger.Log.Info("开始下载 cloudflared", "url", downloadURL)

	resp, err := http.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("下载请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("下载失败，HTTP 状态码: %d", resp.StatusCode)
	}

	destPath := cfTunnelBinaryPath()
	tmpPath := destPath + ".tmp"
	outFile, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}

	total := resp.ContentLength
	var downloaded int64
	buf := make([]byte, 32*1024)
	lastReportTime := time.Now()

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := outFile.Write(buf[:n]); writeErr != nil {
				outFile.Close()
				os.Remove(tmpPath)
				return fmt.Errorf("写入文件失败: %w", writeErr)
			}
			downloaded += int64(n)

			// 限制回调频率（最多每 200ms 一次）
			if progressFn != nil && time.Since(lastReportTime) > 200*time.Millisecond {
				pct := 0
				if total > 0 {
					pct = int(downloaded * 100 / total)
				}
				progressFn(downloaded, total, pct)
				lastReportTime = time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			outFile.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("读取下载流失败: %w", readErr)
		}
	}
	outFile.Close()

	// 最终进度
	if progressFn != nil {
		progressFn(downloaded, total, 100)
	}

	// 重命名临时文件
	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("重命名文件失败: %w", err)
	}

	// [FIX-12] Linux/macOS 设置可执行权限
	if runtime.GOOS != "windows" {
		if err := os.Chmod(destPath, 0755); err != nil {
			return fmt.Errorf("设置可执行权限失败: %w", err)
		}
	}

	logger.Log.Info("cloudflared 下载完成", "path", destPath, "size", downloaded)
	return nil
}

// ===================== Tunnel CRUD =====================

// CreateCFTunnel 创建 Cloudflare Tunnel（幂等：已存在同名则复用）
func CreateCFTunnel() (string, error) {
	accountID := getCFConfig("cf_account_id")
	tunnelName := getCFConfig("cf_tunnel_name")
	if tunnelName == "" {
		tunnelName = "nodectl"
	}

	if accountID == "" {
		return "", fmt.Errorf("CF Account ID 未配置")
	}

	// [FIX-03] 先查询是否已存在同名 Tunnel
	listURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel?name=%s&is_deleted=false",
		accountID, tunnelName)
	listResult, err := cfAPIRequest("GET", listURL, nil)
	if err != nil {
		return "", fmt.Errorf("查询已有 Tunnel 失败: %w", err)
	}

	if tunnels, ok := listResult["result"].([]interface{}); ok && len(tunnels) > 0 {
		// 已存在同名 Tunnel，复用
		tunnel := tunnels[0].(map[string]interface{})
		tunnelID := tunnel["id"].(string)
		logger.Log.Info("发现已存在的同名 Tunnel，复用", "name", tunnelName, "id", tunnelID)

		// 回填 tunnel_id
		setCFConfig("cf_tunnel_id", tunnelID)

		// 获取 Tunnel 运行 Token（用于 --token 模式）
		if err := fetchAndSaveTunnelToken(accountID, tunnelID); err != nil {
			logger.Log.Warn("获取 Tunnel Token 失败，尝试删除并重新创建", "error", err)
			// 无法获取 Token，删除旧 Tunnel 重建
			cleanURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s/connections",
				accountID, tunnelID)
			_, _ = cfAPIRequest("DELETE", cleanURL, nil)
			delURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s",
				accountID, tunnelID)
			_, _ = cfAPIRequest("DELETE", delURL, nil)
			// 清理后走下面的创建逻辑
		} else {
			return tunnelID, nil
		}
	}

	// 创建新 Tunnel
	createURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel", accountID)
	createBody := map[string]interface{}{
		"name":          tunnelName,
		"config_src":    "cloudflare",
		"tunnel_secret": generateTunnelSecret(),
	}

	createResult, err := cfAPIRequest("POST", createURL, createBody)
	if err != nil {
		return "", fmt.Errorf("创建 Tunnel 失败: %w", err)
	}

	var tunnelID string
	if r, ok := createResult["result"].(map[string]interface{}); ok {
		if id, ok := r["id"].(string); ok {
			tunnelID = id
		}
		// 保存凭据
		if err := saveTunnelCredentials(tunnelID, createResult); err != nil {
			logger.Log.Warn("保存 Tunnel 凭据失败", "error", err)
		}
	}

	if tunnelID == "" {
		return "", fmt.Errorf("创建 Tunnel 返回结果缺少 ID")
	}

	// 回填配置
	setCFConfig("cf_tunnel_id", tunnelID)

	// 获取 Tunnel 运行 Token
	if err := fetchAndSaveTunnelToken(accountID, tunnelID); err != nil {
		logger.Log.Warn("获取新建 Tunnel Token 失败（将使用凭据文件模式）", "error", err)
	}

	logger.Log.Info("Tunnel 创建成功", "name", tunnelName, "id", tunnelID)

	return tunnelID, nil
}

// fetchAndSaveTunnelToken 从 API 获取 Tunnel 运行 Token 并保存到数据库
func fetchAndSaveTunnelToken(accountID, tunnelID string) error {
	tokenURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s/token",
		accountID, tunnelID)
	tokenResult, err := cfAPIRequest("GET", tokenURL, nil)
	if err != nil {
		return fmt.Errorf("请求 Tunnel Token 失败: %w", err)
	}

	// API 返回 {"result": "<token_string>"}
	tokenStr, ok := tokenResult["result"].(string)
	if !ok || tokenStr == "" {
		return fmt.Errorf("API 返回的 Token 为空或格式异常")
	}

	setCFConfig("cf_tunnel_token", tokenStr)
	logger.Log.Info("Tunnel 运行 Token 已保存", "tunnel_id", tunnelID)

	// 同时尝试从 Token 解码凭据并保存本地凭据文件（作为备用）
	if err := decodeTunnelTokenToCredentials(tunnelID, tokenStr); err != nil {
		logger.Log.Warn("从 Token 解码凭据失败（不影响 Token 模式运行）", "error", err)
	}

	return nil
}

// decodeTunnelTokenToCredentials 将 JWT-like Token 解码为本地凭据文件
// Token 格式: base64({"a":"<account_tag>","t":"<tunnel_id>","s":"<secret_base64>"})
func decodeTunnelTokenToCredentials(tunnelID, tokenStr string) error {
	if err := os.MkdirAll(cfTunnelBaseDir, 0755); err != nil {
		return err
	}

	// Token 可能有 padding 问题，尝试多种解码
	var decoded []byte
	var decodeErr error
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		decoded, decodeErr = encoding.DecodeString(tokenStr)
		if decodeErr == nil {
			break
		}
	}
	if decodeErr != nil {
		return fmt.Errorf("Token base64 解码失败: %w", decodeErr)
	}

	// 尝试解析 JSON
	var tokenData map[string]interface{}
	if err := json.Unmarshal(decoded, &tokenData); err != nil {
		return fmt.Errorf("Token JSON 解析失败: %w", err)
	}

	// 提取字段: a=AccountTag, t=TunnelID, s=TunnelSecret
	creds := map[string]interface{}{
		"AccountTag":   tokenData["a"],
		"TunnelID":     tokenData["t"],
		"TunnelSecret": tokenData["s"],
	}

	credPath := cfTunnelCredentialsPath(tunnelID)
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}

	logger.Log.Info("从 Token 解码凭据成功", "path", credPath)
	return os.WriteFile(credPath, data, 0600)
}

// generateTunnelSecret 生成 Tunnel Secret（32 bytes base64）
func generateTunnelSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// fallback: use time-based seed
		for i := range b {
			b[i] = byte(time.Now().UnixNano() % 256)
			time.Sleep(1 * time.Nanosecond)
		}
	}
	return base64.StdEncoding.EncodeToString(b)
}

// ===================== 节点专属 Tunnel 管理 =====================

// generateNodeTunnelSuffix 生成 6 位随机字母数字后缀，用于节点 Tunnel 命名防碰撞
func generateNodeTunnelSuffix() (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b), nil
}

// sanitizeNodeTunnelName 生成节点专属 Tunnel 名称：nodetunnel-{random6}
// 使用随机后缀代替节点名，避免特殊字符和命名碰撞
func sanitizeNodeTunnelName(name string) string {
	suffix, err := generateNodeTunnelSuffix()
	if err != nil {
		return "nodetunnel-node"
	}
	return "nodetunnel-" + suffix
}

// fetchTunnelTokenString 从 CF API 获取指定 Tunnel 的运行 Token（不保存到全局配置）
func fetchTunnelTokenString(accountID, tunnelID string) (string, error) {
	tokenURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s/token",
		accountID, tunnelID)
	tokenResult, err := cfAPIRequest("GET", tokenURL, nil)
	if err != nil {
		return "", fmt.Errorf("请求 Tunnel Token 失败: %w", err)
	}
	tokenStr, ok := tokenResult["result"].(string)
	if !ok || tokenStr == "" {
		return "", fmt.Errorf("API 返回的 Token 为空或格式异常")
	}
	return tokenStr, nil
}

// CreateNodeDedicatedTunnel 为节点创建专属 Tunnel（不影响面板全局配置）
// 返回 tunnelID、运行 token、tunnel 名称
func CreateNodeDedicatedTunnel(nodeName string) (tunnelID, token, tunnelName string, err error) {
	accountID := getCFConfig("cf_account_id")
	if accountID == "" {
		return "", "", "", fmt.Errorf("CF Account ID 未配置，请先在 Cloudflare 设置页面完成配置")
	}

	tunnelName = sanitizeNodeTunnelName(nodeName)

	// 创建新 Tunnel
	createURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel", accountID)
	createBody := map[string]interface{}{
		"name":          tunnelName,
		"config_src":    "cloudflare",
		"tunnel_secret": generateTunnelSecret(),
	}

	createResult, err := cfAPIRequest("POST", createURL, createBody)
	if err != nil {
		return "", "", "", fmt.Errorf("创建节点 Tunnel 失败: %w", err)
	}

	if r, ok := createResult["result"].(map[string]interface{}); ok {
		if id, ok := r["id"].(string); ok {
			tunnelID = id
		}
	}
	if tunnelID == "" {
		return "", "", "", fmt.Errorf("创建 Tunnel 返回结果缺少 ID")
	}

	// 获取该 Tunnel 的运行 Token
	token, err = fetchTunnelTokenString(accountID, tunnelID)
	if err != nil {
		// 创建成功但获取 Token 失败，清理已创建的 Tunnel
		cleanURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s/connections", accountID, tunnelID)
		_, _ = cfAPIRequest("DELETE", cleanURL, nil)
		delURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s", accountID, tunnelID)
		_, _ = cfAPIRequest("DELETE", delURL, nil)
		return "", "", "", fmt.Errorf("获取节点 Tunnel Token 失败: %w", err)
	}

	logger.Log.Info("节点专属 Tunnel 创建成功", "name", tunnelName, "id", tunnelID)
	return tunnelID, token, tunnelName, nil
}

// DeleteNodeDedicatedTunnel 删除节点专属 Tunnel（不影响面板全局配置）
func DeleteNodeDedicatedTunnel(tunnelID string) error {
	tunnelID = strings.TrimSpace(tunnelID)
	if tunnelID == "" {
		return nil
	}

	accountID := getCFConfig("cf_account_id")
	if accountID == "" {
		return fmt.Errorf("CF Account ID 未配置")
	}

	// 先清理连接
	cleanURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s/connections", accountID, tunnelID)
	_, _ = cfAPIRequest("DELETE", cleanURL, nil)

	// 删除 Tunnel
	deleteURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s", accountID, tunnelID)
	if _, err := cfAPIRequest("DELETE", deleteURL, nil); err != nil {
		return fmt.Errorf("删除节点 Tunnel 失败: %w", err)
	}

	logger.Log.Info("节点专属 Tunnel 已删除", "id", tunnelID)
	return nil
}

// ListCFTunnelsByPrefix 列出指定前缀的 Cloudflare Tunnel（不含已删除）
func ListCFTunnelsByPrefix(prefix string) ([]CFTunnelListItem, error) {
	accountID := getCFConfig("cf_account_id")
	if accountID == "" {
		return nil, fmt.Errorf("CF Account ID 未配置")
	}

	listURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel?is_deleted=false&per_page=100", accountID)
	result, err := cfAPIRequest("GET", listURL, nil)
	if err != nil {
		return nil, fmt.Errorf("查询 Tunnel 列表失败: %w", err)
	}

	prefix = strings.ToLower(strings.TrimSpace(prefix))
	items := make([]CFTunnelListItem, 0)

	raw, ok := result["result"].([]interface{})
	if !ok {
		return items, nil
	}

	for _, one := range raw {
		m, ok := one.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if prefix != "" && !strings.HasPrefix(strings.ToLower(name), prefix) {
			continue
		}
		id, _ := m["id"].(string)
		status, _ := m["status"].(string)
		createdAt, _ := m["created_at"].(string)

		items = append(items, CFTunnelListItem{
			ID:        id,
			Name:      name,
			Status:    status,
			CreatedAt: createdAt,
		})
	}

	return items, nil
}

// DeleteCFTunnelByID 删除指定 Tunnel（仅删除远端；若为当前 Tunnel 则同步清理本地状态）
func DeleteCFTunnelByID(tunnelID string) error {
	tunnelID = strings.TrimSpace(tunnelID)
	if tunnelID == "" {
		return fmt.Errorf("Tunnel ID 不能为空")
	}

	accountID := getCFConfig("cf_account_id")
	if accountID == "" {
		return fmt.Errorf("CF Account ID 未配置")
	}

	cleanURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s/connections", accountID, tunnelID)
	_, _ = cfAPIRequest("DELETE", cleanURL, nil)

	deleteURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s", accountID, tunnelID)
	if _, err := cfAPIRequest("DELETE", deleteURL, nil); err != nil {
		return fmt.Errorf("删除 Tunnel 失败: %w", err)
	}

	currentTunnelID := strings.TrimSpace(getCFConfig("cf_tunnel_id"))
	if currentTunnelID == tunnelID {
		StopCFTunnel()
		os.Remove(cfTunnelCredentialsPath(tunnelID))
		os.Remove(cfTunnelConfigYml)
		os.Remove(cfTunnelPIDFile)

		setCFConfig("cf_tunnel_id", "")
		setCFConfig("cf_tunnel_token", "")
		setCFConfig("cf_tunnel_subdomain", "")
		setCFConfig("cf_tunnel_enabled", "false")
	}

	logger.Log.Info("指定 Tunnel 已删除", "id", tunnelID)
	return nil
}

// DeleteCFTunnel 删除 Tunnel + 清理 DNS + 删除本地文件
func DeleteCFTunnel() error {
	accountID := getCFConfig("cf_account_id")
	tunnelID := getCFConfig("cf_tunnel_id")

	if accountID == "" || tunnelID == "" {
		return fmt.Errorf("Account ID 或 Tunnel ID 未配置")
	}

	// 先停止运行中的 Tunnel
	StopCFTunnel()

	// 清理 DNS CNAME（忽略错误）
	subdomain := getCFConfig("cf_tunnel_subdomain")
	domain := getCFConfig("cf_domain")
	if subdomain != "" && domain != "" {
		_ = deleteTunnelDNS(accountID, domain, subdomain)
	}

	// 删除 Tunnel（需要先清理连接）
	cleanURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s/connections",
		accountID, tunnelID)
	_, _ = cfAPIRequest("DELETE", cleanURL, nil)

	deleteURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s",
		accountID, tunnelID)
	_, err := cfAPIRequest("DELETE", deleteURL, nil)
	if err != nil {
		return fmt.Errorf("删除 Tunnel 失败: %w", err)
	}

	// 清理本地文件
	credPath := cfTunnelCredentialsPath(tunnelID)
	os.Remove(credPath)
	os.Remove(cfTunnelConfigYml)
	os.Remove(cfTunnelPIDFile)

	// 清空配置
	setCFConfig("cf_tunnel_id", "")
	setCFConfig("cf_tunnel_token", "")
	setCFConfig("cf_tunnel_subdomain", "")
	setCFConfig("cf_tunnel_enabled", "false")

	logger.Log.Info("Tunnel 已删除", "id", tunnelID)
	return nil
}

// BindTunnelDNS 绑定子域名 CNAME
func BindTunnelDNS() error {
	domain := getCFConfig("cf_domain")
	subdomain := getCFConfig("cf_tunnel_subdomain")
	tunnelID := getCFConfig("cf_tunnel_id")

	if domain == "" || subdomain == "" || tunnelID == "" {
		return fmt.Errorf("域名/子域名/Tunnel ID 未配置完整")
	}

	// 获取 Zone ID
	zoneID, err := getZoneID(domain)
	if err != nil {
		return fmt.Errorf("获取 Zone ID 失败: %w", err)
	}

	// 目标 CNAME: <tunnel_id>.cfargotunnel.com
	cnameTarget := tunnelID + ".cfargotunnel.com"

	// 先检查是否已存在该 DNS 记录
	checkURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?type=CNAME&name=%s",
		zoneID, subdomain)
	checkResult, err := cfAPIRequest("GET", checkURL, nil)
	if err != nil {
		return fmt.Errorf("查询 DNS 记录失败: %w", err)
	}

	if records, ok := checkResult["result"].([]interface{}); ok && len(records) > 0 {
		// 更新已有记录
		record := records[0].(map[string]interface{})
		recordID := record["id"].(string)
		updateURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s",
			zoneID, recordID)
		updateBody := map[string]interface{}{
			"type":    "CNAME",
			"name":    subdomain,
			"content": cnameTarget,
			"proxied": true,
		}
		_, err = cfAPIRequest("PUT", updateURL, updateBody)
		if err != nil {
			if strings.Contains(err.Error(), "Authentication") || strings.Contains(err.Error(), "authorization") {
				return fmt.Errorf("更新 DNS 记录失败（Token 缺少 Zone:DNS:Edit 权限，请在 Cloudflare 控制台编辑 Token 添加该权限）: %w", err)
			}
			return fmt.Errorf("更新 DNS 记录失败: %w", err)
		}
		logger.Log.Info("DNS CNAME 记录已更新", "name", subdomain, "target", cnameTarget)
	} else {
		// 创建新记录
		createURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", zoneID)
		createBody := map[string]interface{}{
			"type":    "CNAME",
			"name":    subdomain,
			"content": cnameTarget,
			"proxied": true,
		}
		_, err = cfAPIRequest("POST", createURL, createBody)
		if err != nil {
			if strings.Contains(err.Error(), "Authentication") || strings.Contains(err.Error(), "authorization") {
				return fmt.Errorf("创建 DNS 记录失败（Token 缺少 Zone:DNS:Edit 权限，请在 Cloudflare 控制台编辑 Token 添加该权限）: %w", err)
			}
			return fmt.Errorf("创建 DNS 记录失败: %w", err)
		}
		logger.Log.Info("DNS CNAME 记录已创建", "name", subdomain, "target", cnameTarget)
	}

	return nil
}

// getZoneID 获取域名的 Zone ID
func getZoneID(domain string) (string, error) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones?name=%s", domain)
	result, err := cfAPIRequest("GET", url, nil)
	if err != nil {
		return "", err
	}

	if zones, ok := result["result"].([]interface{}); ok && len(zones) > 0 {
		zone := zones[0].(map[string]interface{})
		if id, ok := zone["id"].(string); ok {
			return id, nil
		}
	}

	return "", fmt.Errorf("未找到域名 %s 的 Zone", domain)
}

// deleteTunnelDNS 删除 Tunnel 对应的 DNS 记录
func deleteTunnelDNS(accountID, domain, subdomain string) error {
	zoneID, err := getZoneID(domain)
	if err != nil {
		return err
	}

	checkURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?type=CNAME&name=%s",
		zoneID, subdomain)
	checkResult, err := cfAPIRequest("GET", checkURL, nil)
	if err != nil {
		return err
	}

	if records, ok := checkResult["result"].([]interface{}); ok {
		for _, r := range records {
			record := r.(map[string]interface{})
			recordID := record["id"].(string)
			deleteURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s",
				zoneID, recordID)
			_, _ = cfAPIRequest("DELETE", deleteURL, nil)
			logger.Log.Info("已删除 DNS 记录", "id", recordID, "name", subdomain)
		}
	}

	return nil
}

// DeleteCFTunnelDNSHost 删除指定完整主机名的 Tunnel DNS 记录。
// host 必须是完整域名，例如 vmess-ws.ab12cd.example.com
func DeleteCFTunnelDNSHost(host string) error {
	host = strings.TrimSpace(strings.TrimSuffix(host, "."))
	if host == "" {
		return nil
	}

	domain := strings.TrimSpace(getCFConfig("cf_domain"))
	if domain == "" {
		return fmt.Errorf("cf_domain 未配置")
	}

	zoneID, err := getZoneID(domain)
	if err != nil {
		// 容错：当 Zone 不存在（例如用户已在 CF 删除域名）时，按“记录已不存在”处理，不阻断节点删除流程
		if strings.Contains(err.Error(), "未找到域名") && strings.Contains(err.Error(), "Zone") {
			logger.Log.Warn("删除 Tunnel DNS 跳过：Zone 不存在，按已删除处理", "host", host, "domain", domain, "error", err)
			return nil
		}
		return fmt.Errorf("获取 Zone ID 失败: %w", err)
	}

	checkURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?type=CNAME&name=%s", zoneID, host)
	checkResult, err := cfAPIRequest("GET", checkURL, nil)
	if err != nil {
		return fmt.Errorf("查询 DNS 记录失败: %w", err)
	}

	records, ok := checkResult["result"].([]interface{})
	if !ok || len(records) == 0 {
		return nil
	}

	for _, raw := range records {
		record, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		recordID, _ := record["id"].(string)
		if recordID == "" {
			continue
		}
		deleteURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", zoneID, recordID)
		if _, err := cfAPIRequest("DELETE", deleteURL, nil); err != nil {
			return fmt.Errorf("删除 DNS 记录失败: %w", err)
		}
		logger.Log.Info("Tunnel DNS CNAME 已删除", "host", host, "record_id", recordID)
	}

	return nil
}

// EnsureCFTunnelDNSHost 确保指定主机名存在指向指定 Tunnel 的 CNAME 记录。
// host 必须是完整域名，例如 vmess-ws.ab12cd.example.com
// nodeTunnelID 为节点专属 Tunnel ID；若为空则回退读取全局 cf_tunnel_id。
func EnsureCFTunnelDNSHost(host string, nodeTunnelID ...string) error {
	host = strings.TrimSpace(strings.TrimSuffix(host, "."))
	if host == "" {
		return fmt.Errorf("host 不能为空")
	}

	domain := strings.TrimSpace(getCFConfig("cf_domain"))
	// 优先使用调用方传入的节点专属 Tunnel ID，回退到全局配置
	tunnelID := ""
	if len(nodeTunnelID) > 0 {
		tunnelID = strings.TrimSpace(nodeTunnelID[0])
	}
	if tunnelID == "" {
		tunnelID = strings.TrimSpace(getCFConfig("cf_tunnel_id"))
	}
	if domain == "" {
		return fmt.Errorf("cf_domain 未配置")
	}
	if tunnelID == "" {
		return fmt.Errorf("cf_tunnel_id 未配置")
	}

	zoneID, err := getZoneID(domain)
	if err != nil {
		return fmt.Errorf("获取 Zone ID 失败: %w", err)
	}

	cnameTarget := tunnelID + ".cfargotunnel.com"

	checkURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?type=CNAME&name=%s", zoneID, host)
	checkResult, err := cfAPIRequest("GET", checkURL, nil)
	if err != nil {
		return fmt.Errorf("查询 DNS 记录失败: %w", err)
	}

	if records, ok := checkResult["result"].([]interface{}); ok && len(records) > 0 {
		record := records[0].(map[string]interface{})
		recordID := record["id"].(string)
		updateURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", zoneID, recordID)
		updateBody := map[string]interface{}{
			"type":    "CNAME",
			"name":    host,
			"content": cnameTarget,
			"proxied": true,
		}
		if _, err := cfAPIRequest("PUT", updateURL, updateBody); err != nil {
			return fmt.Errorf("更新 DNS 记录失败: %w", err)
		}
		logger.Log.Info("Tunnel DNS CNAME 已更新", "host", host, "target", cnameTarget)
		return nil
	}

	createURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", zoneID)
	createBody := map[string]interface{}{
		"type":    "CNAME",
		"name":    host,
		"content": cnameTarget,
		"proxied": true,
	}
	if _, err := cfAPIRequest("POST", createURL, createBody); err != nil {
		return fmt.Errorf("创建 DNS 记录失败: %w", err)
	}
	logger.Log.Info("Tunnel DNS CNAME 已创建", "host", host, "target", cnameTarget)
	return nil
}

// saveTunnelCredentials 保存 Tunnel 凭据到本地文件
func saveTunnelCredentials(tunnelID string, result map[string]interface{}) error {
	if err := os.MkdirAll(cfTunnelBaseDir, 0755); err != nil {
		return err
	}

	credPath := cfTunnelCredentialsPath(tunnelID)

	// 构造凭据 JSON
	creds := map[string]interface{}{
		"AccountTag":   getCFConfig("cf_account_id"),
		"TunnelID":     tunnelID,
		"TunnelName":   getCFConfig("cf_tunnel_name"),
		"TunnelSecret": "",
	}

	// 从创建结果中提取 Secret
	if r, ok := result["result"].(map[string]interface{}); ok {
		if s, ok := r["credentials_file"].(map[string]interface{}); ok {
			for k, v := range s {
				creds[k] = v
			}
		}
		if s, ok := r["tunnel_secret"].(string); ok {
			creds["TunnelSecret"] = s
		}
	}

	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(credPath, data, 0600)
}

// ===================== 配置文件生成 =====================

// RenderTunnelConfig 生成 data/cf/tunnel/config.yml
func RenderTunnelConfig() error {
	tunnelID := getCFConfig("cf_tunnel_id")
	originURL := getAutoOriginURL()

	if tunnelID == "" {
		return fmt.Errorf("Tunnel ID 为空，请先创建 Tunnel")
	}

	credPath := cfTunnelCredentialsPath(tunnelID)

	if err := os.MkdirAll(cfTunnelBaseDir, 0755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	if err := os.MkdirAll(cfTunnelLogDir, 0755); err != nil {
		return fmt.Errorf("创建日志目录失败: %w", err)
	}

	config := fmt.Sprintf(`# Cloudflare Tunnel 配置文件 (由 NodeCTL 自动生成)
# 生成时间: %s
tunnel: %s
credentials-file: %s

ingress:
  - service: %s

# WebSocket 支持已内置，无需额外配置
`, time.Now().Format("2006-01-02 15:04:05"), tunnelID, credPath, originURL)

	if err := os.WriteFile(cfTunnelConfigYml, []byte(config), 0644); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}

	logger.Log.Info("Tunnel 配置文件已生成", "path", cfTunnelConfigYml)
	return nil
}

// ConfigureTunnelRemoteIngress 通过 Cloudflare API 设置 Tunnel 的远程 Ingress 规则
// Token 模式下 cloudflared 的路由规则来自 Cloudflare API 远程配置，必须调用此接口
func ConfigureTunnelRemoteIngress() error {
	accountID := getCFConfig("cf_account_id")
	tunnelID := getCFConfig("cf_tunnel_id")
	subdomain := getCFConfig("cf_tunnel_subdomain")
	originURL := getAutoOriginURL()

	if accountID == "" || tunnelID == "" {
		return fmt.Errorf("Account ID 或 Tunnel ID 为空")
	}

	// 构建 ingress 规则
	ingress := []map[string]interface{}{
		{
			"service": "http_status:404",
		},
	}

	// 如果有子域名，添加 hostname 匹配规则
	if subdomain != "" {
		ingress = []map[string]interface{}{
			{
				"hostname": subdomain,
				"service":  originURL,
				"originRequest": map[string]interface{}{
					"noTLSVerify": true,
				},
			},
			{
				"service": "http_status:404",
			},
		}
	}

	configURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s/configurations",
		accountID, tunnelID)

	body := map[string]interface{}{
		"config": map[string]interface{}{
			"ingress": ingress,
		},
	}

	_, err := cfAPIRequest("PUT", configURL, body)
	if err != nil {
		return fmt.Errorf("配置远程 Ingress 规则失败: %w", err)
	}

	logger.Log.Info("Tunnel 远程 Ingress 规则已配置", "subdomain", subdomain, "origin", originURL)
	return nil
}

// ===================== 进程生命周期 =====================

// StartCFTunnel 启动 cloudflared 子进程
func StartCFTunnel() error {
	cfProcessMu.Lock()
	defer cfProcessMu.Unlock()

	if cfRunning {
		return fmt.Errorf("Tunnel 已在运行中 (PID: %d)", cfProcessPID)
	}

	binPath := cfTunnelBinaryPath()
	if _, err := os.Stat(binPath); err != nil {
		return fmt.Errorf("cloudflared 二进制不存在: %s", binPath)
	}

	// 确保日志目录存在
	os.MkdirAll(cfTunnelLogDir, 0755)

	logFile := filepath.Join(cfTunnelLogDir, "cloudflared.log")

	// 优先使用 Token 模式（更可靠），回退到 config 模式
	tunnelToken := getCFConfig("cf_tunnel_token")

	var cmd *exec.Cmd
	if tunnelToken != "" {
		// 确保远程 Ingress 规则已配置（Token 模式依赖远程配置路由流量）
		if err := ConfigureTunnelRemoteIngress(); err != nil {
			logger.Log.Warn("配置远程 Ingress 规则失败（将尝试继续启动）", "error", err)
		}
		// Token 模式: cloudflared tunnel run --token <TOKEN>
		logger.Log.Info("使用 Token 模式启动 cloudflared")
		cmd = exec.Command(binPath, "tunnel", "--no-autoupdate", "--loglevel", "info",
			"run", "--token", tunnelToken)
	} else {
		// 回退: Config 文件模式
		configPath := cfTunnelConfigYml
		if _, err := os.Stat(configPath); err != nil {
			return fmt.Errorf("Tunnel Token 和配置文件均不存在，无法启动")
		}
		logger.Log.Info("使用 Config 文件模式启动 cloudflared")
		cmd = exec.Command(binPath, "tunnel", "--config", configPath, "--loglevel", "info", "run")
	}

	// 重定向输出到日志文件
	lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	cmd.Stdout = lf
	cmd.Stderr = lf

	if err := cmd.Start(); err != nil {
		lf.Close()
		return fmt.Errorf("启动 cloudflared 失败: %w", err)
	}

	cfProcess = cmd.Process
	cfProcessCmd = cmd
	cfProcessPID = cmd.Process.Pid
	cfStartedAt = time.Now()
	cfRunning = true
	cfStopRequest = false

	// 写 PID 文件
	os.WriteFile(cfTunnelPIDFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0644)

	logger.Log.Info("cloudflared 已启动", "pid", cmd.Process.Pid)

	// 后台监控进程状态 + 崩溃自动重试
	go func() {
		retryCount := 0
		maxRetries := 5
		baseBackoff := 1 * time.Second
		maxBackoff := 60 * time.Second

		for {
			err := cmd.Wait()
			lf.Close()

			cfProcessMu.Lock()
			cfRunning = false
			cfProcess = nil
			cfProcessCmd = nil
			cfProcessPID = 0
			os.Remove(cfTunnelPIDFile)

			// 如果是用户主动停止，不重试
			if cfStopRequest {
				cfProcessMu.Unlock()
				logger.Log.Info("cloudflared 已停止（用户主动）")
				return
			}
			cfProcessMu.Unlock()

			if err != nil {
				logger.Log.Warn("cloudflared 异常退出", "error", err, "retry", retryCount+1)
			}

			retryCount++
			if retryCount > maxRetries {
				logger.Log.Error("cloudflared 连续崩溃超过限制，停止重试", "max_retries", maxRetries)
				return
			}

			// 指数退避
			backoff := baseBackoff * time.Duration(1<<uint(retryCount-1))
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			logger.Log.Info("等待后重新启动 cloudflared...", "backoff", backoff.String())
			time.Sleep(backoff)

			// 重新启动
			cfProcessMu.Lock()
			if cfStopRequest {
				cfProcessMu.Unlock()
				return
			}

			newLf, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				cfProcessMu.Unlock()
				logger.Log.Error("重试打开日志文件失败", "error", err)
				return
			}

			if tunnelToken != "" {
				cmd = exec.Command(binPath, "tunnel", "--no-autoupdate", "--loglevel", "info",
					"run", "--token", tunnelToken)
			} else {
				cmd = exec.Command(binPath, "tunnel", "--config", cfTunnelConfigYml, "--loglevel", "info", "run")
			}
			cmd.Stdout = newLf
			cmd.Stderr = newLf
			lf = newLf

			if err := cmd.Start(); err != nil {
				cfProcessMu.Unlock()
				newLf.Close()
				logger.Log.Error("重试启动 cloudflared 失败", "error", err)
				continue
			}

			cfProcess = cmd.Process
			cfProcessCmd = cmd
			cfProcessPID = cmd.Process.Pid
			cfStartedAt = time.Now()
			cfRunning = true
			os.WriteFile(cfTunnelPIDFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0644)
			cfProcessMu.Unlock()

			logger.Log.Info("cloudflared 已重新启动", "pid", cmd.Process.Pid, "retry", retryCount)
		}
	}()

	// [FIX-01] 启用 Tunnel 后自动设置 sys_force_http=true
	setCFConfig("sys_force_http", "true")

	return nil
}

// StopCFTunnel 停止 cloudflared 子进程
func StopCFTunnel() {
	cfProcessMu.Lock()
	defer cfProcessMu.Unlock()

	cfStopRequest = true

	if cfProcess == nil {
		return
	}

	logger.Log.Info("正在停止 cloudflared...", "pid", cfProcessPID)

	// 优雅停止：发送 SIGTERM (Windows 用 Kill)
	if runtime.GOOS == "windows" {
		cfProcess.Kill()
	} else {
		cfProcess.Signal(os.Interrupt)
		// 等待 5 秒，超时则强杀
		done := make(chan struct{})
		go func() {
			if cfProcessCmd != nil {
				cfProcessCmd.Wait()
			}
			close(done)
		}()

		select {
		case <-done:
			// 正常退出
		case <-time.After(5 * time.Second):
			logger.Log.Warn("cloudflared 优雅停止超时，强制终止")
			cfProcess.Kill()
		}
	}

	cfRunning = false
	cfProcess = nil
	cfProcessCmd = nil
	cfProcessPID = 0
	os.Remove(cfTunnelPIDFile)

	logger.Log.Info("cloudflared 已停止")
}

// GetCFTunnelStatus 获取 Tunnel 运行状态
func GetCFTunnelStatus() CFTunnelStatus {
	cfProcessMu.Lock()
	running := cfRunning
	pid := cfProcessPID
	startedAt := cfStartedAt
	cfProcessMu.Unlock()

	status := CFTunnelStatus{
		Running:         running,
		PID:             pid,
		TunnelID:        getCFConfig("cf_tunnel_id"),
		TunnelName:      getCFConfig("cf_tunnel_name"),
		Subdomain:       getCFConfig("cf_tunnel_subdomain"),
		Enabled:         getCFConfig("cf_tunnel_enabled") == "true",
		BindMainProcess: getCFConfig("cf_tunnel_bind_main_process") == "true",
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
		InDocker:        isRunningInDocker(),
	}

	// 计算访问地址
	if status.Subdomain != "" {
		status.AccessURL = "https://" + status.Subdomain
	}

	// 二进制存在性检查
	exists, ver := CheckCloudflaredBinary()
	status.BinaryExists = exists
	status.CloudflaredVer = ver

	// 配置文件存在性检查
	if _, err := os.Stat(cfTunnelConfigYml); err == nil {
		status.ConfigExists = true
	}

	// 凭据文件存在性检查
	if status.TunnelID != "" {
		if _, err := os.Stat(cfTunnelCredentialsPath(status.TunnelID)); err == nil {
			status.CredentialsExist = true
		}
	}

	if running {
		uptime := time.Since(startedAt)
		status.UptimeSeconds = int64(uptime.Seconds())
		hours := int(uptime.Hours())
		mins := int(uptime.Minutes()) % 60
		secs := int(uptime.Seconds()) % 60
		if hours > 0 {
			status.Uptime = fmt.Sprintf("%dh%dm%ds", hours, mins, secs)
		} else if mins > 0 {
			status.Uptime = fmt.Sprintf("%dm%ds", mins, secs)
		} else {
			status.Uptime = fmt.Sprintf("%ds", secs)
		}
	}

	return status
}

// AutoStartCFTunnel 自动启动 Tunnel（NodeCTL 启动时调用）
func AutoStartCFTunnel() {
	// 延迟 2 秒，确保 Web 服务完全就绪
	time.Sleep(2 * time.Second)

	enabled := getCFConfig("cf_tunnel_enabled")
	bindMain := getCFConfig("cf_tunnel_bind_main_process")
	tunnelID := getCFConfig("cf_tunnel_id")

	if enabled != "true" || bindMain != "true" || tunnelID == "" {
		return
	}

	// 检查二进制是否就绪
	binExists, _ := CheckCloudflaredBinary()
	if !binExists {
		logger.Log.Warn("Tunnel 已启用但 cloudflared 二进制不存在，跳过自动启动")
		return
	}

	// Token 模式或 Config 模式至少需要一个
	tunnelToken := getCFConfig("cf_tunnel_token")
	_, configErr := os.Stat(cfTunnelConfigYml)
	if tunnelToken == "" && configErr != nil {
		logger.Log.Warn("Tunnel 已启用但 Token 和配置文件均不存在，跳过自动启动")
		return
	}

	logger.Log.Info("自动拉起 Cloudflare Tunnel...")
	if err := StartCFTunnel(); err != nil {
		logger.Log.Error("自动启动 Tunnel 失败", "error", err)
	}
}

// GetCFConfigPublic 暴露给 handler 层读取配置的接口
func GetCFConfigPublic(key string) string {
	return getCFConfig(key)
}

// SetCFConfigPublic 暴露给 handler 层写入配置的接口
func SetCFConfigPublic(key, value string) {
	setCFConfig(key, value)
}

// upsertCFConfig 插入或更新配置（适用于动态新建 Key 场景）
func upsertCFConfig(key, value, desc string) {
	cfg := database.SysConfig{Key: key, Value: value, Description: desc}
	result := database.DB.Where("key = ?", key).FirstOrCreate(&cfg)
	if result.RowsAffected == 0 {
		database.DB.Model(&database.SysConfig{}).Where("key = ?", key).Update("value", value)
	}
}

// ===================== Token 校验记录持久化 =====================

// CFTokenVerifyRecord 最近一次 Token 校验记录
type CFTokenVerifyRecord struct {
	VerifyAt string               `json:"verify_at"` // RFC3339 时间字符串
	Result   *CFTokenVerifyResult `json:"result"`
}

// SaveTokenVerifyRecord 持久化保存最近一次 Token 校验结果（仅保留最新一条）
func SaveTokenVerifyRecord(result *CFTokenVerifyResult) {
	if result == nil {
		return
	}
	data, err := json.Marshal(result)
	if err != nil {
		logger.Log.Warn("序列化 Token 校验记录失败", "error", err)
		return
	}
	now := time.Now().Format(time.RFC3339)
	upsertCFConfig("cf_token_verify_at", now, "最近一次 Token 校验时间")
	upsertCFConfig("cf_token_verify_result", string(data), "最近一次 Token 校验结果 JSON")
}

// GetLastTokenVerifyRecord 读取最近一次 Token 校验记录
func GetLastTokenVerifyRecord() *CFTokenVerifyRecord {
	at := getCFConfig("cf_token_verify_at")
	resultJSON := getCFConfig("cf_token_verify_result")
	if at == "" || resultJSON == "" {
		return nil
	}
	var result CFTokenVerifyResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		logger.Log.Warn("解析 Token 校验记录失败", "error", err)
		return nil
	}
	return &CFTokenVerifyRecord{
		VerifyAt: at,
		Result:   &result,
	}
}

// ===================== Token 权限管理 =====================

// CFTokenPermission 单项权限检查结果
type CFTokenPermission struct {
	Name        string `json:"name"`        // 权限名称
	Resource    string `json:"resource"`    // 资源范围 (Account / Zone)
	Permission  string `json:"permission"`  // 权限类型 (Read / Edit)
	Required    bool   `json:"required"`    // 是否必需
	HasAccess   bool   `json:"has_access"`  // 是否拥有
	Description string `json:"description"` // 说明
}

// CFTokenVerifyResult Token 验证结果
type CFTokenVerifyResult struct {
	Valid       bool                `json:"valid"`        // Token 是否有效
	Status      string              `json:"status"`       // Token 状态 (active / expired / disabled)
	TokenName   string              `json:"token_name"`   // Token 名称
	ExpiresOn   string              `json:"expires_on"`   // 过期时间
	Email       string              `json:"email"`        // 账户邮箱
	AccountID   string              `json:"account_id"`   // Account ID
	AccountName string              `json:"account_name"` // 账户名
	Permissions []CFTokenPermission `json:"permissions"`  // 权限列表
	AllRequired bool                `json:"all_required"` // 所有必需权限是否满足
	Zones       []string            `json:"zones"`        // 可用域名
	Summary     string              `json:"summary"`      // 总结文字
}

// PickBestCFEmail 选择可用的 Cloudflare 邮箱：优先显式邮箱，其次从账户名中推断。
func PickBestCFEmail(email, accountName string) string {
	email = strings.TrimSpace(email)
	if email != "" {
		return email
	}

	name := strings.TrimSpace(accountName)
	if name == "" {
		return ""
	}

	// 常见格式：foo@example.com's Account
	if strings.HasSuffix(name, "'s Account") {
		name = strings.TrimSpace(strings.TrimSuffix(name, "'s Account"))
	}

	name = strings.Trim(name, `"'`)
	if strings.Contains(name, "@") && strings.Contains(name, ".") {
		return name
	}

	return ""
}

// VerifyCFTokenPermissions 详细验证 CF Token 的所有权限
// 参数 token: 如果为空则使用已保存的 cf_api_key
func VerifyCFTokenPermissions(token string) (*CFTokenVerifyResult, error) {
	if token == "" {
		token = getCFConfig("cf_api_key")
	}
	if token == "" {
		return nil, fmt.Errorf("Token 不能为空")
	}

	result := &CFTokenVerifyResult{
		Permissions: []CFTokenPermission{},
	}

	// 带 Token 的请求函数
	apiCall := func(method, url string, body interface{}) (map[string]interface{}, error) {
		var bodyReader io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			bodyReader = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, url, bodyReader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		var data map[string]interface{}
		if err := json.Unmarshal(respBody, &data); err != nil {
			return nil, fmt.Errorf("解析响应失败: %w", err)
		}
		return data, nil
	}

	// 检查 API 响应是否成功
	isSuccess := func(data map[string]interface{}) bool {
		if s, ok := data["success"].(bool); ok {
			return s
		}
		return false
	}

	// ── 1. 验证 Token 基本有效性 ──
	verifyData, err := apiCall("GET", "https://api.cloudflare.com/client/v4/user/tokens/verify", nil)
	if err != nil {
		return nil, fmt.Errorf("Token 验证失败（网络错误）: %w", err)
	}
	if !isSuccess(verifyData) {
		result.Valid = false
		result.Status = "invalid"
		result.Summary = "❌ Token 无效或已过期"
		return result, nil
	}

	result.Valid = true
	if r, ok := verifyData["result"].(map[string]interface{}); ok {
		if s, ok := r["status"].(string); ok {
			result.Status = s
		}
		if exp, ok := r["expires_on"].(string); ok && exp != "" {
			result.ExpiresOn = exp
		}
	}

	// ── 2. 获取 Token 详情 (名称等) ──
	// 通过 /user/tokens 获取当前 token ID 然后获取详情
	tokensData, err := apiCall("GET", "https://api.cloudflare.com/client/v4/user/tokens", nil)
	if err == nil && isSuccess(tokensData) {
		if tokens, ok := tokensData["result"].([]interface{}); ok {
			for _, t := range tokens {
				if tk, ok := t.(map[string]interface{}); ok {
					if status, ok := tk["status"].(string); ok && status == "active" {
						if name, ok := tk["name"].(string); ok {
							result.TokenName = name
						}
						if exp, ok := tk["expires_on"].(string); ok && exp != "" {
							result.ExpiresOn = exp
						}
						break
					}
				}
			}
		}
	}

	// ── 3. 获取用户信息（邮箱） ──
	userData, err := apiCall("GET", "https://api.cloudflare.com/client/v4/user", nil)
	if err == nil && isSuccess(userData) {
		if u, ok := userData["result"].(map[string]interface{}); ok {
			if email, ok := u["email"].(string); ok {
				result.Email = email
			}
		}
	}

	// ── 4. 获取账户信息 ──
	accountsData, err := apiCall("GET", "https://api.cloudflare.com/client/v4/accounts?page=1&per_page=5", nil)
	if err == nil && isSuccess(accountsData) {
		if accounts, ok := accountsData["result"].([]interface{}); ok && len(accounts) > 0 {
			if acc, ok := accounts[0].(map[string]interface{}); ok {
				if id, ok := acc["id"].(string); ok {
					result.AccountID = id
				}
				if name, ok := acc["name"].(string); ok {
					result.AccountName = name
				}
				if result.Email == "" {
					result.Email = PickBestCFEmail("", result.AccountName)
				}
			}
		}
	}
	// 添加 Account:Read 权限结果
	result.Permissions = append(result.Permissions, CFTokenPermission{
		Name:        "Account Settings",
		Resource:    "Account",
		Permission:  "Read",
		Required:    true,
		HasAccess:   result.AccountID != "",
		Description: "读取账户信息",
	})

	// ── 5. 检测 Zone:Read (列出域名) ──
	zonesData, err := apiCall("GET", "https://api.cloudflare.com/client/v4/zones?status=active&per_page=50", nil)
	zoneReadOK := false
	if err == nil && isSuccess(zonesData) {
		zoneReadOK = true
		if zones, ok := zonesData["result"].([]interface{}); ok {
			for _, z := range zones {
				if zone, ok := z.(map[string]interface{}); ok {
					if name, ok := zone["name"].(string); ok {
						result.Zones = append(result.Zones, name)
					}
				}
			}
		}
	}
	result.Permissions = append(result.Permissions, CFTokenPermission{
		Name:        "DNS Settings",
		Resource:    "Zone",
		Permission:  "Read",
		Required:    true,
		HasAccess:   zoneReadOK,
		Description: "读取域名列表",
	})

	// ── 6. 检测 DNS:Read (读取 DNS 记录) ──
	var testZoneID string
	dnsReadOK := false
	dnsEditOK := false
	if len(result.Zones) > 0 {
		zoneListData, _ := apiCall("GET",
			fmt.Sprintf("https://api.cloudflare.com/client/v4/zones?name=%s", result.Zones[0]), nil)
		if zoneListData != nil && isSuccess(zoneListData) {
			if zoneArr, ok := zoneListData["result"].([]interface{}); ok && len(zoneArr) > 0 {
				if zObj, ok := zoneArr[0].(map[string]interface{}); ok {
					if zid, ok := zObj["id"].(string); ok {
						testZoneID = zid
					}
				}
			}
		}
		if testZoneID != "" {
			// DNS Read: 尝试列出记录
			dnsListData, dnsErr := apiCall("GET",
				fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?per_page=1", testZoneID), nil)
			if dnsErr == nil && isSuccess(dnsListData) {
				dnsReadOK = true
			}

			// DNS Edit: 尝试创建一个临时 TXT 记录然后删除
			testRecordName := "_nodectl-perm-test." + result.Zones[0]
			createBody := map[string]interface{}{
				"type":    "TXT",
				"name":    testRecordName,
				"content": "nodectl-permission-test",
				"ttl":     120,
			}
			createData, createErr := apiCall("POST",
				fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", testZoneID), createBody)
			if createErr == nil && isSuccess(createData) {
				dnsEditOK = true
				// 立即删除测试记录
				if cr, ok := createData["result"].(map[string]interface{}); ok {
					if rid, ok := cr["id"].(string); ok {
						apiCall("DELETE",
							fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", testZoneID, rid), nil)
					}
				}
			}
		}
	}

	result.Permissions = append(result.Permissions, CFTokenPermission{
		Name:        "DNS",
		Resource:    "Zone",
		Permission:  "Read",
		Required:    false,
		HasAccess:   dnsReadOK,
		Description: "读取 DNS 记录",
	})
	result.Permissions = append(result.Permissions, CFTokenPermission{
		Name:        "DNS",
		Resource:    "Zone",
		Permission:  "Edit",
		Required:    true,
		HasAccess:   dnsEditOK,
		Description: "创建/修改 DNS 记录 (绑定域名)",
	})

	// ── 7. 检测 Tunnel:Read (列出 Tunnel) ──
	tunnelReadOK := false
	tunnelEditOK := false
	if result.AccountID != "" {
		tunnelListData, tErr := apiCall("GET",
			fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel?per_page=1&is_deleted=false", result.AccountID), nil)
		if tErr == nil && isSuccess(tunnelListData) {
			tunnelReadOK = true
		}

		// Tunnel:Edit 检测 — 创建并立即删除一个测试 Tunnel
		testTunnelBody := map[string]interface{}{
			"name":          "_nodectl_perm_test_" + strconv.FormatInt(time.Now().Unix(), 10),
			"config_src":    "cloudflare",
			"tunnel_secret": generateTunnelSecret(),
		}
		createTunnelData, ctErr := apiCall("POST",
			fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel", result.AccountID), testTunnelBody)
		if ctErr == nil && isSuccess(createTunnelData) {
			tunnelEditOK = true
			// 立即删除测试 Tunnel
			if tr, ok := createTunnelData["result"].(map[string]interface{}); ok {
				if tid, ok := tr["id"].(string); ok {
					apiCall("DELETE",
						fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s", result.AccountID, tid), nil)
				}
			}
		}
	}

	result.Permissions = append(result.Permissions, CFTokenPermission{
		Name:        "Cloudflare Tunnel",
		Resource:    "Account",
		Permission:  "Read",
		Required:    false,
		HasAccess:   tunnelReadOK,
		Description: "列出 Tunnel",
	})
	result.Permissions = append(result.Permissions, CFTokenPermission{
		Name:        "Cloudflare Tunnel",
		Resource:    "Account",
		Permission:  "Edit",
		Required:    true,
		HasAccess:   tunnelEditOK,
		Description: "创建/管理 Tunnel",
	})

	// ── 汇总 ──
	result.AllRequired = true
	missingCount := 0
	for _, p := range result.Permissions {
		if p.Required && !p.HasAccess {
			result.AllRequired = false
			missingCount++
		}
	}

	if result.AllRequired {
		result.Summary = "✅ Token 权限完整，可正常使用所有功能"
	} else {
		result.Summary = fmt.Sprintf("⚠️ 缺少 %d 项必需权限，部分功能将不可用", missingCount)
	}

	logger.Log.Info("Token 权限验证完成",
		"valid", result.Valid,
		"all_required", result.AllRequired,
		"permissions_count", len(result.Permissions),
	)

	return result, nil
}

// ===================== 懒人模式：自动发现 + 一键部署 =====================

// CFAutoDiscoverResult 自动发现的 Cloudflare 账户信息
type CFAutoDiscoverResult struct {
	AccountID           string   `json:"account_id"`
	AccountName         string   `json:"account_name"`
	Email               string   `json:"email"`
	Zones               []string `json:"zones"`                 // 可用域名列表
	FirstZone           string   `json:"first_zone"`            // 默认推荐域名
	HasDNSPermission    bool     `json:"has_dns_permission"`    // 是否有 DNS 编辑权限
	HasTunnelPermission bool     `json:"has_tunnel_permission"` // 是否有 Tunnel 编辑权限
	PermissionWarnings  []string `json:"permission_warnings"`   // 权限警告信息
}

// AutoDiscoverCFAccount 通过 Token 自动发现账户信息（Account ID、Email、Zones）
// 只需一个 Token 即可自动探测所有必要信息
func AutoDiscoverCFAccount(token string) (*CFAutoDiscoverResult, error) {
	if token == "" {
		return nil, fmt.Errorf("Token 不能为空")
	}

	result := &CFAutoDiscoverResult{}

	// 构建带临时 Token 的请求函数
	apiCall := func(method, url string) (map[string]interface{}, error) {
		req, err := http.NewRequest(method, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		var data map[string]interface{}
		if err := json.Unmarshal(body, &data); err != nil {
			return nil, fmt.Errorf("解析响应失败: %w", err)
		}

		if success, ok := data["success"].(bool); ok && !success {
			errMsg := "API 错误"
			if errors, ok := data["errors"].([]interface{}); ok && len(errors) > 0 {
				if errObj, ok := errors[0].(map[string]interface{}); ok {
					if msg, ok := errObj["message"].(string); ok {
						errMsg = msg
					}
				}
			}
			return nil, fmt.Errorf("%s", errMsg)
		}

		return data, nil
	}

	// Step 1: 验证 Token
	verifyData, err := apiCall("GET", "https://api.cloudflare.com/client/v4/user/tokens/verify")
	if err != nil {
		return nil, fmt.Errorf("Token 验证失败: %w", err)
	}
	if r, ok := verifyData["result"].(map[string]interface{}); ok {
		if status, ok := r["status"].(string); ok && status != "active" {
			return nil, fmt.Errorf("Token 状态异常: %s", status)
		}
	}

	// Step 2: 获取账户列表（取第一个）
	accountsData, err := apiCall("GET", "https://api.cloudflare.com/client/v4/accounts?page=1&per_page=5")
	if err != nil {
		return nil, fmt.Errorf("获取账户信息失败: %w", err)
	}
	if accounts, ok := accountsData["result"].([]interface{}); ok && len(accounts) > 0 {
		if acc, ok := accounts[0].(map[string]interface{}); ok {
			if id, ok := acc["id"].(string); ok {
				result.AccountID = id
			}
			if name, ok := acc["name"].(string); ok {
				result.AccountName = name
			}
		}
	}
	if result.AccountID == "" {
		return nil, fmt.Errorf("未找到任何 Cloudflare 账户，请检查 Token 权限")
	}

	// Step 3: 获取 Zone 列表
	zonesData, err := apiCall("GET", "https://api.cloudflare.com/client/v4/zones?status=active&per_page=50")
	if err != nil {
		logger.Log.Warn("获取 Zone 列表失败，将继续（域名需手动填写）", "error", err)
	} else {
		if zones, ok := zonesData["result"].([]interface{}); ok {
			for _, z := range zones {
				if zone, ok := z.(map[string]interface{}); ok {
					if name, ok := zone["name"].(string); ok {
						result.Zones = append(result.Zones, name)
					}
				}
			}
		}
		if len(result.Zones) > 0 {
			result.FirstZone = result.Zones[0]
		}
	}

	// Step 4: 检测 DNS 权限（尝试读取第一个 Zone 的 DNS 记录）
	if len(result.Zones) > 0 {
		// 先获取 Zone ID
		zoneListData, zErr := apiCall("GET",
			fmt.Sprintf("https://api.cloudflare.com/client/v4/zones?name=%s", result.Zones[0]))
		if zErr == nil {
			var testZoneID string
			if zoneArr, ok := zoneListData["result"].([]interface{}); ok && len(zoneArr) > 0 {
				if zObj, ok := zoneArr[0].(map[string]interface{}); ok {
					if zid, ok := zObj["id"].(string); ok {
						testZoneID = zid
					}
				}
			}
			if testZoneID != "" {
				// 尝试读取 DNS 记录来检测权限
				_, dnsErr := apiCall("GET",
					fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?per_page=1", testZoneID))
				if dnsErr == nil {
					result.HasDNSPermission = true
				} else {
					result.HasDNSPermission = false
					result.PermissionWarnings = append(result.PermissionWarnings,
						"Token 缺少 DNS 编辑权限 (Zone → DNS → Edit)，无法自动绑定域名")
					logger.Log.Warn("Token 缺少 DNS 权限", "zone", result.Zones[0], "error", dnsErr)
				}
			}
		}
	}

	// Step 5: 检测 Tunnel 权限
	_, tunnelErr := apiCall("GET",
		fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel?per_page=1", result.AccountID))
	if tunnelErr == nil {
		result.HasTunnelPermission = true
	} else {
		result.HasTunnelPermission = false
		result.PermissionWarnings = append(result.PermissionWarnings,
			"Token 缺少 Tunnel 管理权限 (Account → Cloudflare Tunnel → Edit)")
		logger.Log.Warn("Token 缺少 Tunnel 权限", "error", tunnelErr)
	}

	// Step 6: 尝试获取邮箱（可能需要 User 读取权限）
	userData, err := apiCall("GET", "https://api.cloudflare.com/client/v4/user")
	if err == nil {
		if u, ok := userData["result"].(map[string]interface{}); ok {
			if email, ok := u["email"].(string); ok {
				result.Email = email
			}
		}
	}

	logger.Log.Info("CF 自动发现完成",
		"account_id", result.AccountID,
		"account_name", result.AccountName,
		"zones_count", len(result.Zones),
	)

	return result, nil
}

// OneClickSetupProgress 一键部署进度回调
type OneClickSetupProgress struct {
	Step    int    `json:"step"`
	Total   int    `json:"total"`
	Phase   string `json:"phase"`
	Message string `json:"message"`
	Percent int    `json:"percent"`
}

// OneClickSetupCFTunnel 一键部署 Cloudflare Tunnel
// 参数: token, subdomain (必填), domain (可选，自动发现), tunnelName (可选，默认 nodectl)
// 通过 progressFn 回调报告每个步骤的进度
func OneClickSetupCFTunnel(token, subdomain, domain, tunnelName string, progressFn func(OneClickSetupProgress)) error {
	totalSteps := 8

	report := func(step int, phase, msg string) {
		if progressFn != nil {
			progressFn(OneClickSetupProgress{
				Step:    step,
				Total:   totalSteps,
				Phase:   phase,
				Message: msg,
				Percent: step * 100 / totalSteps,
			})
		}
	}

	// ── Step 1: 验证 Token 并自动发现账户 ──
	report(1, "discover", "🔍 正在验证 Token 并自动发现账户信息...")

	discovered, err := AutoDiscoverCFAccount(token)
	if err != nil {
		return fmt.Errorf("账户自动发现失败: %w", err)
	}

	// 自动填充域名
	if domain == "" {
		if discovered.FirstZone == "" {
			return fmt.Errorf("未找到可用域名，请手动填写")
		}
		domain = discovered.FirstZone
	}

	// 自动构造子域名：如果 subdomain 不包含域名后缀，自动补全
	if subdomain != "" && !strings.Contains(subdomain, ".") {
		subdomain = subdomain + "." + domain
	}
	if subdomain == "" {
		subdomain = "panel." + domain
	}

	if tunnelName == "" {
		tunnelName = "nodectl"
	}

	report(1, "discover", fmt.Sprintf("✅ 账户: %s | 域名: %s | 子域名: %s", discovered.AccountName, domain, subdomain))

	// ── Step 2: 保存配置 ──
	report(2, "save", "💾 正在保存配置...")

	configMap := map[string]string{
		"cf_api_key":                  token,
		"cf_account_id":               discovered.AccountID,
		"cf_domain":                   domain,
		"cf_tunnel_name":              tunnelName,
		"cf_tunnel_subdomain":         subdomain,
		"cf_tunnel_origin_url":        fmt.Sprintf("http://127.0.0.1:%d", database.GetWebPort()),
		"cf_tunnel_bind_main_process": "true",
		"cf_tunnel_enabled":           "true",
	}
	if discovered.Email != "" {
		configMap["cf_email"] = discovered.Email
	}

	for key, value := range configMap {
		setCFConfig(key, value)
	}

	report(2, "save", "✅ 配置已保存")

	// ── Step 3: 下载 cloudflared (如果不存在) ──
	report(3, "download", "📦 检查 cloudflared...")

	exists, ver := CheckCloudflaredBinary()
	if exists {
		report(3, "download", fmt.Sprintf("✅ cloudflared 已就绪 (v%s)", ver))
	} else {
		report(3, "download", "📦 正在下载 cloudflared...")
		err := DownloadCloudflared(func(downloaded, total int64, percent int) {
			var msg string
			if total > 0 {
				msg = fmt.Sprintf("📦 下载中 %.1f/%.1f MB (%d%%)",
					float64(downloaded)/1024/1024, float64(total)/1024/1024, percent)
			} else {
				msg = fmt.Sprintf("📦 已下载 %.1f MB", float64(downloaded)/1024/1024)
			}
			if progressFn != nil {
				progressFn(OneClickSetupProgress{
					Step: 3, Total: totalSteps, Phase: "download",
					Message: msg, Percent: 25 + percent*12/100, // 25%~37%
				})
			}
		})
		if err != nil {
			return fmt.Errorf("下载 cloudflared 失败: %w", err)
		}
		_, ver = CheckCloudflaredBinary()
		report(3, "download", fmt.Sprintf("✅ cloudflared 下载完成 (v%s)", ver))
	}

	// ── Step 4: 创建 Tunnel (幂等) ──
	report(4, "create", "🔧 正在创建 Tunnel...")

	tunnelID, err := CreateCFTunnel()
	if err != nil {
		return fmt.Errorf("创建 Tunnel 失败: %w", err)
	}

	report(4, "create", fmt.Sprintf("✅ Tunnel 已就绪 (ID: %s)", tunnelID[:8]+"..."))

	// ── Step 5: 绑定 DNS ──
	report(5, "dns", fmt.Sprintf("🌐 正在绑定 DNS: %s → Tunnel...", subdomain))

	if err := BindTunnelDNS(); err != nil {
		return fmt.Errorf("绑定 DNS 失败: %w", err)
	}

	report(5, "dns", fmt.Sprintf("✅ DNS 已绑定: %s", subdomain))

	// ── Step 6: 生成配置文件 + 远程 Ingress ──
	report(6, "config", "📝 正在配置 Tunnel 路由规则...")

	if err := RenderTunnelConfig(); err != nil {
		logger.Log.Warn("生成本地配置文件失败（非致命）", "error", err)
	}

	// 关键: 通过 API 设置远程 Ingress 规则，Token 模式下 cloudflared 依赖此配置路由流量
	if err := ConfigureTunnelRemoteIngress(); err != nil {
		return fmt.Errorf("配置 Tunnel 路由规则失败: %w", err)
	}

	report(6, "config", "✅ Tunnel 路由规则已配置")

	// ── Step 7: 启动 Tunnel ──
	report(7, "start", "▶️ 正在启动 Tunnel...")

	if err := StartCFTunnel(); err != nil {
		return fmt.Errorf("启动 Tunnel 失败: %w", err)
	}

	report(7, "start", "✅ Tunnel 已启动")

	// ── Step 8: 设置面板地址 ──
	report(8, "finalize", "🔗 正在设置面板访问地址...")

	panelURL := "https://" + subdomain
	currentPanelURL := getCFConfig("panel_url")
	if currentPanelURL == "" {
		setCFConfig("panel_url", panelURL)
	}

	report(8, "finalize", fmt.Sprintf("🎉 一键部署完成！面板地址: %s", panelURL))

	logger.Log.Info("一键部署完成",
		"subdomain", subdomain,
		"tunnel_id", tunnelID,
		"panel_url", panelURL,
	)

	return nil
}

// ===================== cloudflared 远程版本检查与自动更新 =====================

// GetCloudflaredRemoteVersion 跟随 GitHub Releases 重定向获取 cloudflared 最新版本号。
func GetCloudflaredRemoteVersion() (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	tag, _, err := fetchLatestGitHubRelease(client, "https://github.com/cloudflare/cloudflared/releases/latest", "nodectl")
	if err != nil {
		return "", fmt.Errorf("获取 cloudflared release 失败: %w", err)
	}
	return strings.TrimSpace(tag), nil
}

// GetCloudflaredVersionStatus 获取 cloudflared 版本状态（供前端版本检测用）
// 返回: localVersion, remoteVersion, state ("latest"/"update_available"/"not_found"/"error")
func GetCloudflaredVersionStatus() (localVer, remoteVer, state string) {
	exists, local := CheckCloudflaredBinary()
	if !exists {
		localVer = ""
	} else {
		localVer = local
	}

	remote, err := GetCloudflaredRemoteVersion()
	if err != nil {
		logger.Log.Warn("获取 cloudflared 远程版本失败", "error", err)
		remoteVer = ""
		if localVer == "" {
			state = "not_found"
		} else {
			state = "error"
		}
		return
	}
	remoteVer = remote

	if localVer == "" {
		state = "not_found"
		return
	}

	// 版本比较：去掉前缀后对比
	cleanLocal := strings.TrimSpace(localVer)
	cleanRemote := strings.TrimSpace(strings.TrimPrefix(remoteVer, "v"))
	cleanRemote = strings.TrimPrefix(cleanRemote, "v")

	if cleanLocal == cleanRemote {
		state = "latest"
	} else {
		state = "update_available"
	}
	return
}

// ForceUpdateCloudflared 强制下载最新版 cloudflared（覆盖现有）
func ForceUpdateCloudflared() error {
	logger.Log.Info("开始强制更新 cloudflared...")
	err := DownloadCloudflared(nil)
	if err != nil {
		return fmt.Errorf("下载 cloudflared 失败: %w", err)
	}
	logger.Log.Info("cloudflared 更新完成")
	return nil
}
