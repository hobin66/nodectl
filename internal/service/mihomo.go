package service

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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

	"gopkg.in/yaml.v3"
)

const (
	MihomoLatestReleaseURL = "https://github.com/MetaCubeX/mihomo/releases/latest"
	MihomoDBConfigKey      = "mihomo_core_version"
)

var GlobalMihomo *MihomoService

type MihomoService struct {
	mu      sync.RWMutex
	dirPath string // 存放核心的目录: data/mihomo
	binPath string // 二进制文件的最终路径
}

// InitMihomo 初始化核心管理器
func InitMihomo() {
	dir := filepath.Join("data", "mihomo")
	if err := os.MkdirAll(dir, 0755); err != nil {
		logger.Log.Error("创建 Mihomo 核心目录失败", "err", err)
		return
	}

	binName := "mihomo"
	if runtime.GOOS == "windows" {
		binName = "mihomo.exe"
	}

	GlobalMihomo = &MihomoService{
		dirPath: dir,
		binPath: filepath.Join(dir, binName),
	}

	// 检查核心是否就绪，仅输出日志提示
	if !GlobalMihomo.IsCoreReady() {
		logger.Log.Warn("本地暂无 Mihomo 核心，请在系统设置中手动下载。测速功能需要 Mihomo 核心支持。")
	} else {
		logger.Log.Debug("Mihomo 测试核心已就绪", "version", GlobalMihomo.GetLocalVersion())
	}
}

// IsCoreReady 检查物理文件是否存在
func (s *MihomoService) IsCoreReady() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, err := os.Stat(s.binPath)
	return err == nil
}

// GetLocalVersion 读取本地数据库记录的版本号
func (s *MihomoService) GetLocalVersion() string {
	if !s.IsCoreReady() {
		return "" // 文件不在，直接视为无版本
	}
	var config database.SysConfig
	if err := database.DB.Where("key = ?", MihomoDBConfigKey).First(&config).Error; err != nil {
		return ""
	}
	return config.Value
}

// GetRemoteVersion 通过 Release 重定向获取最新版本，并构造当前平台的下载链接。
func (s *MihomoService) GetRemoteVersion() (version string, downloadURL string, isZip bool, err error) {
	client := &http.Client{Timeout: 15 * time.Second}
	version, _, err = fetchLatestGitHubRelease(client, MihomoLatestReleaseURL, "NodeCTL-Core-Manager")
	if err != nil {
		return "", "", false, err
	}
	filename, isZip, err := mihomoAssetFilename(runtime.GOOS, runtime.GOARCH, version)
	if err != nil {
		return "", "", false, err
	}
	downloadURL = fmt.Sprintf("https://github.com/MetaCubeX/mihomo/releases/download/%s/%s", url.PathEscape(version), filename)
	return version, downloadURL, isZip, nil
}

func mihomoAssetFilename(goos, goarch, version string) (string, bool, error) {
	var ext string
	switch goos {
	case "windows":
		ext = "zip"
	case "linux", "darwin":
		ext = "gz"
	default:
		return "", false, fmt.Errorf("不支持的 Mihomo 操作系统: %s", goos)
	}
	if strings.TrimSpace(goarch) == "" || strings.TrimSpace(version) == "" {
		return "", false, errors.New("Mihomo 下载平台或版本为空")
	}
	filename := fmt.Sprintf("mihomo-%s-%s-%s.%s", goos, goarch, version, ext)
	return filename, ext == "zip", nil
}

// ForceUpdate 强制下载并更新核心
func (s *MihomoService) ForceUpdate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tempArchive := filepath.Join(s.dirPath, "temp_archive")
	tempBin := s.binPath + ".tmp"
	defer os.Remove(tempArchive)
	defer os.Remove(tempBin)

	version, dlURL, isZip, err := s.GetRemoteVersion()
	if err != nil {
		return fmt.Errorf("获取远程版本失败: %w", err)
	}

	// 复用与 geo 一致的通用下载策略
	if err := downloadFile(tempArchive, dlURL); err != nil {
		return fmt.Errorf("文件下载失败: %w", err)
	}

	if isZip {
		if err := s.extractZip(tempArchive, tempBin); err != nil {
			return err
		}
	} else {
		if err := s.extractGz(tempArchive, tempBin); err != nil {
			return err
		}
	}

	// Linux/Mac 赋予执行权限
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tempBin, 0755); err != nil {
			return fmt.Errorf("设置 Mihomo 核心权限失败: %w", err)
		}
	}

	if err := os.Rename(tempBin, s.binPath); err != nil {
		return fmt.Errorf("替换文件失败: %w", err)
	}

	// 写入数据库 (使用 FirstOrCreate 确保记录存在)
	config := database.SysConfig{Key: MihomoDBConfigKey}
	database.DB.Where(database.SysConfig{Key: MihomoDBConfigKey}).
		Assign(database.SysConfig{Value: version, Description: "Mihomo 核心版本号"}).
		FirstOrCreate(&config)

	logger.Log.Info("Mihomo 核心更新成功", "version", version)
	return nil
}

// extractGz 解压 .gz (Linux/Mac)
func (s *MihomoService) extractGz(gzFile, destFile string) error {
	f, err := os.Open(gzFile)
	if err != nil {
		return err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gr.Close()

	out, err := os.Create(destFile)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, gr)
	return err
}

// extractZip 解压 .zip (Windows)
func (s *MihomoService) extractZip(zipFile, destFile string) error {
	r, err := zip.OpenReader(zipFile)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		if strings.Contains(f.Name, "mihomo") {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			out, err := os.Create(destFile)
			if err != nil {
				rc.Close()
				return err
			}
			_, err = io.Copy(out, rc)
			out.Close()
			rc.Close()
			if err == nil {
				return nil
			}
		}
	}
	return errors.New("zip 压缩包中未找到核心文件")
}

// TestNodeInfo 用于向 Mihomo 传递需要测试的节点信息
type TestNodeInfo struct {
	Name string
	Link string
}

// MinimalConfig Mihomo 极简测试配置结构
type MinimalConfig struct {
	MixedPort          int           `yaml:"mixed-port"`
	ExternalController string        `yaml:"external-controller"`
	AllowLan           bool          `yaml:"allow-lan"`
	Mode               string        `yaml:"mode"`
	LogLevel           string        `yaml:"log-level"`
	Proxies            []interface{} `yaml:"proxies"`
}

type batchTestTarget struct {
	NodeID    string
	NodeName  string
	ProxyName string
	Link      string
}

func buildTestProxyPayload(name, link string) (map[string]interface{}, error) {
	clashNode := ParseLinkToClashNode(link, "")
	if clashNode == nil {
		return nil, errors.New("parse_failed")
	}

	if clashNode.Server == "" || clashNode.Port <= 0 || clashNode.Port > 65535 {
		return nil, errors.New("invalid_server_or_port")
	}

	clashNode.Name = name
	nodeMap := make(map[string]interface{})
	nodeBytes, _ := yaml.Marshal(clashNode)
	yaml.Unmarshal(nodeBytes, &nodeMap)

	if clashNode.Type == "vmess" && clashNode.Cipher == "" {
		nodeMap["cipher"] = "auto"
	}

	return nodeMap, nil
}

// GenerateTestConfig 动态生成 Mihomo 测速所需的临时 yaml 文件
// 接收一个节点数组，支持单节点或多节点批量测试
func (s *MihomoService) GenerateTestConfig(nodes []TestNodeInfo) (yamlPath string, apiPort int, mixedPort int, accepted map[string]struct{}, err error) {
	if len(nodes) == 0 {
		return "", 0, 0, nil, errors.New("没有需要测试的节点")
	}

	// 1. 获取两个系统的随机空闲端口，避免与系统其他服务冲突
	apiPort, err = getFreePort()
	if err != nil {
		return "", 0, 0, nil, fmt.Errorf("分配 API 端口失败: %w", err)
	}
	mixedPort, err = getFreePort()
	if err != nil {
		return "", 0, 0, nil, fmt.Errorf("分配代理端口失败: %w", err)
	}

	// 2. 组装极简基础配置
	config := MinimalConfig{
		MixedPort:          mixedPort,
		ExternalController: fmt.Sprintf("127.0.0.1:%d", apiPort),
		AllowLan:           false,
		Mode:               "global",
		LogLevel:           "error",
		Proxies:            make([]interface{}, 0), // 初始化为空接口数组
	}

	// 3. 复用 links.go 解析逻辑，将数据库 Link 转为 Mihomo 节点
	accepted = make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		nodeMap, parseErr := buildTestProxyPayload(n.Name, n.Link)
		if parseErr != nil {
			logger.Log.Warn("测试节点解析失败跳过", "name", n.Name, "link", n.Link)
			continue
		}

		config.Proxies = append(config.Proxies, nodeMap)
		accepted[n.Name] = struct{}{}
	}

	if len(config.Proxies) == 0 {
		return "", 0, 0, nil, errors.New("所有节点解析均失败，无法生成测试配置")
	}

	// 4. 创建随机的临时配置文件 (支持高并发，不同用户同时测速不会冲突)
	tempFile, err := os.CreateTemp(s.dirPath, "test_*.yaml")
	if err != nil {
		return "", 0, 0, nil, fmt.Errorf("创建临时配置文件失败: %w", err)
	}
	defer tempFile.Close()

	// 5. 序列化为 YAML 并写入
	encoder := yaml.NewEncoder(tempFile)
	encoder.SetIndent(2)
	if err := encoder.Encode(&config); err != nil {
		os.Remove(tempFile.Name()) // 出错时清理残骸
		return "", 0, 0, nil, fmt.Errorf("写入 YAML 失败: %w", err)
	}
	encoder.Close()

	return tempFile.Name(), apiPort, mixedPort, accepted, nil
}

// getFreePort 向操作系统申请一个可用的随机 TCP 端口
func getFreePort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// SpeedTestResult 定义流式返回的数据结构
type SpeedTestResult struct {
	NodeID string `json:"node_id"`
	Type   string `json:"type"` // "ping" | "tcp" | "speed" | "error"
	Text   string `json:"text"` // 界面显示的文字，如 "45ms", "42.0 MBps"
}

func NormalizeSpeedTestMode(mode string) string {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case "ping_only", "all", "ping_speed":
		return strings.TrimSpace(strings.ToLower(mode))
	default:
		return "ping_speed"
	}
}

func switchBatchProxy(client *http.Client, apiPort int, proxyName string) error {
	body, _ := json.Marshal(map[string]string{"name": proxyName})
	req, err := http.NewRequest(http.MethodPut, fmt.Sprintf("http://127.0.0.1:%d/proxies/GLOBAL", apiPort), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("switch proxy failed: %s", resp.Status)
	}
	return nil
}

// RunBatchTestWithMode 流水线逐个启动核心并进行测速 (支持按请求指定模式)
func (s *MihomoService) RunBatchTestWithMode(ctx context.Context, nodes []database.AirportNode, resultChan chan<- SpeedTestResult, mode string) {
	defer close(resultChan) // 退出时自动关闭通道，前端 SSE 连接结束

	testMode := NormalizeSpeedTestMode(mode)

	batchTargets := make([]batchTestTarget, 0, len(nodes))
	configNodes := make([]TestNodeInfo, 0, len(nodes))
	for i, n := range nodes {
		proxyName := fmt.Sprintf("__nodectl_%d_%d", time.Now().UnixNano(), i)
		batchTargets = append(batchTargets, batchTestTarget{
			NodeID:    n.ID,
			NodeName:  n.Name,
			ProxyName: proxyName,
			Link:      n.Link,
		})
		configNodes = append(configNodes, TestNodeInfo{Name: proxyName, Link: n.Link})
	}

	yamlPath, apiPort, mixedPort, accepted, err := s.GenerateTestConfig(configNodes)
	if err != nil {
		for _, t := range batchTargets {
			resultChan <- SpeedTestResult{NodeID: t.NodeID, Type: "error", Text: "配置残缺"}
		}
		return
	}
	defer os.Remove(yamlPath)

	testable := make([]batchTestTarget, 0, len(batchTargets))
	for _, t := range batchTargets {
		if _, ok := accepted[t.ProxyName]; !ok {
			resultChan <- SpeedTestResult{NodeID: t.NodeID, Type: "error", Text: "配置残缺"}
			continue
		}
		testable = append(testable, t)
	}
	if len(testable) == 0 {
		return
	}

	cmd := exec.CommandContext(ctx, s.binPath, "-d", s.dirPath, "-f", yamlPath)
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf

	if err := cmd.Start(); err != nil {
		for _, t := range testable {
			resultChan <- SpeedTestResult{NodeID: t.NodeID, Type: "error", Text: "引擎拉起失败"}
		}
		return
	}
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}()

	processExit := make(chan error, 1)
	go func() { processExit <- cmd.Wait() }()

	client := &http.Client{Timeout: 2 * time.Second}
	ready := false
	for i := 0; i < 75; i++ {
		select {
		case <-processExit:
			errMsg := "引擎异常"
			if strings.Contains(outBuf.String(), "Parse config error") || strings.Contains(outBuf.String(), "unmarshal errors") {
				errMsg = "协议不兼容"
			}
			logger.Log.Warn("批量引擎拦截", "error", outBuf.String())
			for _, t := range testable {
				resultChan <- SpeedTestResult{NodeID: t.NodeID, Type: "error", Text: errMsg}
			}
			return
		default:
		}

		resp, reqErr := client.Get(fmt.Sprintf("http://127.0.0.1:%d/version", apiPort))
		if reqErr == nil && resp.StatusCode == http.StatusOK {
			ready = true
			resp.Body.Close()
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}

	if !ready {
		for _, t := range testable {
			resultChan <- SpeedTestResult{NodeID: t.NodeID, Type: "error", Text: "启动超时"}
		}
		return
	}

	proxyURL, _ := url.Parse(fmt.Sprintf("socks5://127.0.0.1:%d", mixedPort))
	proxyTransport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}

	var fileSizeConfig database.SysConfig
	database.DB.Where("key = ?", "pref_speed_test_file_size").First(&fileSizeConfig)
	fileSizeMB := "50"
	if fileSizeConfig.Value != "" {
		fileSizeMB = fileSizeConfig.Value
	}

	bytesSize := "50000000"
	if mb, convErr := strconv.Atoi(fileSizeMB); convErr == nil && mb > 0 {
		bytesSize = fmt.Sprintf("%d", mb*1000000)
	}

	speedURLs := []string{
		fmt.Sprintf("https://speed.cloudflare.com/__down?bytes=%s", bytesSize),
		fmt.Sprintf("http://speedtest.tele2.net/%sMB.zip", fileSizeMB),
		fmt.Sprintf("https://proof.ovh.net/files/%sMb.dat", fileSizeMB),
	}

	for _, t := range testable {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := switchBatchProxy(client, apiPort, t.ProxyName); err != nil {
			resultChan <- SpeedTestResult{NodeID: t.NodeID, Type: "error", Text: "切换失败"}
			continue
		}

		delayURL := fmt.Sprintf("http://127.0.0.1:%d/proxies/%s/delay?timeout=5000&url=http://cp.cloudflare.com/generate_204", apiPort, url.PathEscape(t.ProxyName))

		var totalPing int
		var pingSuccess int
		for i := 0; i < 3; i++ {
			resp, reqErr := client.Get(delayURL)
			if reqErr == nil {
				var delayRes struct {
					Delay int `json:"delay"`
				}
				if decodeErr := json.NewDecoder(resp.Body).Decode(&delayRes); decodeErr == nil && delayRes.Delay > 0 {
					totalPing += delayRes.Delay
					pingSuccess++
				}
				resp.Body.Close()
			}
			if i < 2 {
				time.Sleep(100 * time.Millisecond)
			}
		}

		var totalTCPDelay int64
		var tcpSuccess int
		if testMode == "all" || pingSuccess == 0 {
			tcpClient := &http.Client{Transport: proxyTransport, Timeout: 3 * time.Second}
			for i := 0; i < 3; i++ {
				reqTCP, _ := http.NewRequest("HEAD", "http://1.1.1.1", nil)
				reqTCP.Close = true

				startTCP := time.Now()
				tcpResp, reqErr := tcpClient.Do(reqTCP)
				if reqErr == nil {
					tcpResp.Body.Close()
					totalTCPDelay += time.Since(startTCP).Milliseconds()
					tcpSuccess++
				}
				if i < 2 {
					time.Sleep(100 * time.Millisecond)
				}
			}
		}

		var avgTCPDelay int64
		if tcpSuccess > 0 {
			avgTCPDelay = totalTCPDelay / int64(tcpSuccess)
		}

		if pingSuccess > 0 {
			avgPing := totalPing / pingSuccess
			resultChan <- SpeedTestResult{NodeID: t.NodeID, Type: "ping", Text: fmt.Sprintf("%d ms", avgPing)}
		} else if tcpSuccess > 0 {
			resultChan <- SpeedTestResult{NodeID: t.NodeID, Type: "ping", Text: fmt.Sprintf("TCP %dms", avgTCPDelay)}
		} else {
			resultChan <- SpeedTestResult{NodeID: t.NodeID, Type: "error", Text: "无效节点"}
			continue
		}

		if testMode == "ping_only" {
			continue
		}

		if testMode == "all" {
			if tcpSuccess > 0 {
				resultChan <- SpeedTestResult{NodeID: t.NodeID, Type: "tcp", Text: fmt.Sprintf("%d ms", avgTCPDelay)}
			} else {
				resultChan <- SpeedTestResult{NodeID: t.NodeID, Type: "error", Text: "TCP异常"}
				continue
			}
		}

		speedClient := &http.Client{Transport: proxyTransport, Timeout: 8 * time.Second}
		var finalSpeedMBps float64
		var speedSuccess bool
		var interceptCode int
		var speedMu sync.Mutex

		ctxTraffic, cancelTraffic := context.WithCancel(context.Background())
		go func() {
			defer cancelTraffic()
			trafficReq, reqErr := http.NewRequestWithContext(ctxTraffic, "GET", fmt.Sprintf("http://127.0.0.1:%d/traffic", apiPort), nil)
			if reqErr != nil {
				return
			}
			trafficClient := &http.Client{Timeout: 0}
			trafficResp, reqErr := trafficClient.Do(trafficReq)
			if reqErr != nil {
				return
			}
			defer trafficResp.Body.Close()

			decoder := json.NewDecoder(trafficResp.Body)
			for {
				var traffic struct {
					Up   int64 `json:"up"`
					Down int64 `json:"down"`
				}
				if err := decoder.Decode(&traffic); err != nil {
					break
				}
				currentMBps := float64(traffic.Down) / 1024 / 1024

				speedMu.Lock()
				if currentMBps > finalSpeedMBps {
					finalSpeedMBps = currentMBps
				}
				speedMu.Unlock()
			}
		}()

		for _, targetURL := range speedURLs {
			reqSpeed, _ := http.NewRequest("GET", targetURL, nil)
			reqSpeed.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
			reqSpeed.Header.Set("Cache-Control", "no-cache")

			speedResp, reqErr := speedClient.Do(reqSpeed)
			if reqErr != nil {
				continue
			}

			statusCode := speedResp.StatusCode
			if statusCode == http.StatusOK {
				startSpeed := time.Now()
				maxTestTimer := time.AfterFunc(4*time.Second, func() {
					speedResp.Body.Close()
				})

				buf := make([]byte, 256*1024)
				written, _ := io.CopyBuffer(io.Discard, speedResp.Body, buf)

				maxTestTimer.Stop()
				duration := time.Since(startSpeed).Seconds()
				speedResp.Body.Close()
				time.Sleep(200 * time.Millisecond)

				speedMu.Lock()
				currentMax := finalSpeedMBps
				speedMu.Unlock()

				if currentMax == 0 && duration > 0 && written > 1024 {
					speedMu.Lock()
					finalSpeedMBps = (float64(written) / 1024 / 1024) / duration
					speedMu.Unlock()
					currentMax = finalSpeedMBps
				}

				if currentMax > 0 || written > 1024 {
					speedSuccess = true
					break
				}
			} else {
				speedResp.Body.Close()
				if statusCode == 429 || statusCode == 403 {
					interceptCode = statusCode
				}
			}
		}

		cancelTraffic()

		if speedSuccess {
			finalSpeedMbps := finalSpeedMBps * 8
			resultChan <- SpeedTestResult{NodeID: t.NodeID, Type: "speed", Text: fmt.Sprintf("%.2f MBps", finalSpeedMbps)}
		} else if interceptCode > 0 {
			resultChan <- SpeedTestResult{NodeID: t.NodeID, Type: "error", Text: fmt.Sprintf("节点拦截(%d)", interceptCode)}
		} else {
			resultChan <- SpeedTestResult{NodeID: t.NodeID, Type: "error", Text: "测速超时"}
		}

		time.Sleep(300 * time.Millisecond)
	}
}
