// 路径: internal/service/cf_ipopt.go
// Cloudflare IP 优选服务层：CloudflareST 二进制调用 + 任务编排
package service

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"encoding/csv"
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
	cfIPOptBaseDir       = "data/cf/ipopt"
	cfIPOptBinDir        = "data/cf/ipopt/bin"
	cfIPOptInputDir      = "data/cf/ipopt/input"
	cfIPOptOutputDir     = "data/cf/ipopt/output"
	cfIPOptLogDir        = "data/cf/ipopt/logs"
	cfIPOptTmpDir        = "data/cf/ipopt/tmp"
	cfIPOptResultFile    = "data/cf/ipopt/output/latest_result.json"
	cfIPOptResultCSVFile = "data/cf/ipopt/tmp/result.csv"

	// CloudflareST 默认版本（从 GitHub Releases 下载）
	cfIPOptDefaultVersion = "2.3.4"

	// 内置默认测速地址
	cfIPOptDefaultSpeedTestURL = "https://nodectl-ipopt.hobin.net/200MB.zip"

	// 内置测速地址ID（固定值，不可删除）
	cfIPOptBuiltInID = "builtin"

	// 任务执行超时时间（固定 20 分钟）
	cfIPOptTaskTimeout = 20 * time.Minute

	// 日志环形缓冲大小
	cfIPOptLogBufferSize = 200

	// 日志保留天数
	cfIPOptLogRetentionDays = 7
)

// ===================== 数据结构 =====================

// CFIPOptTaskState 任务运行状态
type CFIPOptTaskState struct {
	TaskID    string    `json:"task_id"`    // 任务唯一ID
	Status    string    `json:"status"`     // idle/running/completed/failed/stopped
	Progress  int       `json:"progress"`   // 0-100 百分比
	StartedAt time.Time `json:"started_at"` // 开始时间
	EndedAt   time.Time `json:"ended_at"`   // 结束时间
	Error     string    `json:"error"`      // 错误信息
	LogLines  []string  `json:"log_lines"`  // 环形缓冲最近 N 行日志
	LogVer    int64     `json:"log_ver"`    // 日志版本号（每次变更递增，供 SSE 检测变化）
}

// CFIPOptResult 优选结果
type CFIPOptResult struct {
	Top10    []CFIPOptEntry `json:"top10"`     // 优选出的 10 个最优 IP（IPv4+IPv6 双栈）
	Duration time.Duration  `json:"duration"`  // 执行总耗时
	FinishAt time.Time      `json:"finish_at"` // 完成时间
}

// CFIPOptEntry 单个优选IP条目
type CFIPOptEntry struct {
	IP      string  `json:"ip"`      // IP 地址
	Latency float64 `json:"latency"` // 延迟 (ms)
	Speed   float64 `json:"speed"`   // 下载速度 (MB/s)
	Loss    float64 `json:"loss"`    // 丢包率 (%)
}

// CFIPOptSettings 优选设置
type CFIPOptSettings struct {
	ScheduleInterval     int    `json:"schedule_interval"`       // 定时优选间隔（小时），0 表示关闭
	ApplyToTunnelNodes   bool   `json:"apply_to_tunnel_nodes"`   // 是否应用到 Tunnel 节点
	BinaryExists         bool   `json:"binary_exists"`           // 二进制是否存在
	BinaryVersion        string `json:"binary_version"`          // 二进制版本号
	Platform             string `json:"platform"`                // 当前平台（如 linux_amd64）
	HasValidResult       bool   `json:"has_valid_result"`        // 是否有可用的优选结果
	SpeedTestURL         string `json:"speed_test_url"`          // 自定义下载测速地址（为空则使用内置默认）
	ScheduleSpeedTestURL string `json:"schedule_speed_test_url"` // 定时优选默认使用的测速地址（为空则使用 speed_test_url 或内置默认）
	DebugMode            bool   `json:"debug_mode"`              // 是否开启调试模式（-debug 参数）
}

// CFIPOptBinaryStatus 二进制状态
type CFIPOptBinaryStatus struct {
	Exists   bool   `json:"exists"`   // 是否存在
	Version  string `json:"version"`  // 版本号
	Platform string `json:"platform"` // 平台架构
	OS       string `json:"os"`       // 操作系统
	Arch     string `json:"arch"`     // 架构
}

// SpeedTestURLItem 测速地址条目
type SpeedTestURLItem struct {
	ID        string `json:"id"`         // 唯一ID
	Name      string `json:"name"`       // 备注名
	URL       string `json:"url"`        // 测速地址
	IsBuiltin bool   `json:"is_builtin"` // 是否为内置地址（内置地址不可删除）
}

// ===================== 全局状态 =====================

var (
	cfIPOptMu       sync.Mutex
	cfIPOptCmd      *exec.Cmd
	cfIPOptState    = CFIPOptTaskState{Status: "idle"}
	cfIPOptLogIndex int
	cfIPOptStopChan chan struct{}

	// 定时任务相关
	cfIPOptSchedulerMu      sync.Mutex
	cfIPOptSchedulerStop    chan struct{}
	cfIPOptSchedulerRunning bool
)

// ===================== 初始化 =====================

// InitCFIPOpt 初始化优选目录结构
// 注意：此函数必须在 logger 初始化之后调用
func InitCFIPOpt() {
	dirs := []string{
		cfIPOptBaseDir,
		cfIPOptBinDir,
		cfIPOptInputDir,
		cfIPOptOutputDir,
		cfIPOptLogDir,
		cfIPOptTmpDir,
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			logger.Log.Error("创建 CF 优选目录失败", "dir", dir, "error", err)
		}
	}

	// 清理过期日志文件
	cleanOldLogs()

	logger.Log.Debug("CF 优选IP模块初始化完成")
}

// cleanOldLogs 清理超过 7 天的日志文件
func cleanOldLogs() {
	files, err := os.ReadDir(cfIPOptLogDir)
	if err != nil {
		return
	}

	cutoff := time.Now().AddDate(0, 0, -cfIPOptLogRetentionDays)
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		info, err := file.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(cfIPOptLogDir, file.Name()))
		}
	}
}

// ===================== 二进制管理 =====================

// cfIPOptBinaryName 返回当前平台的 CloudflareST 二进制名
func cfIPOptBinaryName() string {
	if runtime.GOOS == "windows" {
		return "cfst.exe"
	}
	return "cfst"
}

// cfIPOptBinaryPath 返回 CloudflareST 二进制的完整路径
func cfIPOptBinaryPath() string {
	return filepath.Join(cfIPOptBinDir, cfIPOptBinaryName())
}

// IsCFIPOptBinaryExists 检查二进制是否存在
func IsCFIPOptBinaryExists() (exists bool, version string, err error) {
	binaryPath := cfIPOptBinaryPath()
	_, err = os.Stat(binaryPath)
	if os.IsNotExist(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}

	// 从数据库读取版本号
	version = getCFIPOptConfig("cf_ipopt_bin_version")
	return true, version, nil
}

// GetCFIPOptBinaryStatus 获取二进制状态
func GetCFIPOptBinaryStatus() CFIPOptBinaryStatus {
	exists, version, _ := IsCFIPOptBinaryExists()
	return CFIPOptBinaryStatus{
		Exists:   exists,
		Version:  version,
		Platform: runtime.GOOS + "_" + runtime.GOARCH,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
	}
}

// getCFIPOptDownloadURL 获取下载地址
func getCFIPOptDownloadURL(version string) (string, error) {
	goos := runtime.GOOS
	arch := runtime.GOARCH

	// 仅支持 linux 和 windows
	if goos != "linux" && goos != "windows" {
		return "", fmt.Errorf("不支持的平台: %s", goos)
	}
	// 仅支持 amd64 和 arm64
	if arch != "amd64" && arch != "arm64" {
		return "", fmt.Errorf("不支持的架构: %s", arch)
	}

	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}

	return fmt.Sprintf(
		"https://github.com/XIU2/CloudflareSpeedTest/releases/download/v%s/cfst_%s_%s.%s",
		version, goos, arch, ext,
	), nil
}

// DownloadCFIPOptBinary 下载 CloudflareST 二进制
// 返回下载进度通道，用于 SSE 推送
func DownloadCFIPOptBinary() (<-chan map[string]interface{}, error) {
	cfIPOptMu.Lock()
	defer cfIPOptMu.Unlock()

	// 检查平台支持
	goos := runtime.GOOS
	arch := runtime.GOARCH
	if goos != "linux" && goos != "windows" {
		return nil, fmt.Errorf("不支持的平台: %s，仅支持 Linux 和 Windows", goos)
	}
	if arch != "amd64" && arch != "arm64" {
		return nil, fmt.Errorf("不支持的架构: %s，仅支持 amd64 和 arm64", arch)
	}

	// 获取下载地址
	downloadURL, err := getCFIPOptDownloadURL(cfIPOptDefaultVersion)
	if err != nil {
		return nil, err
	}

	// 创建进度通道
	progressChan := make(chan map[string]interface{}, 100)

	go func() {
		defer close(progressChan)

		// 重试逻辑
		maxRetries := 3
		retryInterval := 5 * time.Second
		var lastErr error

		for attempt := 1; attempt <= maxRetries; attempt++ {
			if attempt > 1 {
				progressChan <- map[string]interface{}{
					"status":  "retrying",
					"attempt": attempt,
					"max":     maxRetries,
					"message": fmt.Sprintf("下载失败，%d 秒后重试...", retryInterval/time.Second),
				}
				time.Sleep(retryInterval)
			}

			progressChan <- map[string]interface{}{
				"status":  "downloading",
				"message": fmt.Sprintf("正在从 GitHub 下载 CloudflareST v%s...", cfIPOptDefaultVersion),
				"attempt": attempt,
			}

			err := downloadAndExtract(downloadURL, progressChan)
			if err == nil {
				// 下载成功，保存版本号
				setCFIPOptConfig("cf_ipopt_bin_version", cfIPOptDefaultVersion)
				progressChan <- map[string]interface{}{
					"status":  "completed",
					"message": "CloudflareST 下载完成",
					"version": cfIPOptDefaultVersion,
				}
				return
			}
			lastErr = err
			logger.Log.Warn("CloudflareST 下载失败", "attempt", attempt, "error", err)
		}

		// 所有重试失败
		progressChan <- map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("下载失败（已重试 %d 次）: %v", maxRetries, lastErr),
		}
	}()

	return progressChan, nil
}

// downloadAndExtract 下载并解压
func downloadAndExtract(url string, progressChan chan<- map[string]interface{}) error {
	// 创建临时文件
	tmpFile := filepath.Join(cfIPOptTmpDir, "download.tmp")
	defer os.Remove(tmpFile)

	// 发送 HTTP 请求
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("HTTP 请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP 状态码: %d", resp.StatusCode)
	}

	// 获取文件大小
	totalSize := resp.ContentLength

	// 创建目标文件
	out, err := os.Create(tmpFile)
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %v", err)
	}

	// 带进度的下载
	progressReader := &progressReader{
		reader: resp.Body,
		total:  totalSize,
		onProgress: func(downloaded, total int64) {
			if total > 0 {
				percent := int(float64(downloaded) / float64(total) * 100)
				progressChan <- map[string]interface{}{
					"status":     "downloading",
					"progress":   percent,
					"downloaded": downloaded,
					"total":      total,
				}
			}
		},
	}

	_, err = io.Copy(out, progressReader)
	out.Close()
	if err != nil {
		return fmt.Errorf("下载失败: %v", err)
	}

	progressChan <- map[string]interface{}{
		"status":   "extracting",
		"message":  "正在解压...",
		"progress": 100,
	}

	// 解压文件
	if strings.HasSuffix(url, ".zip") {
		return extractZip(tmpFile, progressChan)
	}
	return extractTarGz(tmpFile, progressChan)
}

// progressReader 带进度回调的 Reader
type progressReader struct {
	reader     io.Reader
	total      int64
	downloaded int64
	onProgress func(downloaded, total int64)
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.downloaded += int64(n)
	if r.onProgress != nil {
		r.onProgress(r.downloaded, r.total)
	}
	return n, err
}

// extractTarGz 解压 tar.gz 文件到 bin 目录（保留所有文件）
func extractTarGz(src string, progressChan chan<- map[string]interface{}) error {
	file, err := os.Open(src)
	if err != nil {
		return err
	}
	defer file.Close()

	gzr, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	binaryFound := false

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// 跳过目录
		if header.Typeflag == tar.TypeDir {
			continue
		}

		// 提取文件名（可能包含子目录）
		name := filepath.Base(header.Name)
		if name == "." || name == ".." {
			continue
		}

		// 构建目标路径（直接放在 bin 目录）
		targetPath := filepath.Join(cfIPOptBinDir, name)

		// 创建目标文件
		out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
		if err != nil {
			return err
		}

		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		out.Close()

		// 检查是否找到二进制文件
		if name == "cfst" || name == "cfst.exe" {
			binaryFound = true
			// Linux 下设置可执行权限
			if runtime.GOOS != "windows" {
				os.Chmod(targetPath, 0755)
			}
		}

		logger.Log.Debug("解压文件", "file", name, "target", targetPath)
	}

	if !binaryFound {
		return fmt.Errorf("压缩包中未找到 CloudflareST 二进制文件")
	}

	logger.Log.Info("CloudflareST 解压完成", "dir", cfIPOptBinDir)
	return nil
}

// extractZip 解压 zip 文件到 bin 目录（保留所有文件）
func extractZip(src string, progressChan chan<- map[string]interface{}) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	binaryFound := false

	for _, f := range r.File {
		// 跳过目录
		if f.FileInfo().IsDir() {
			continue
		}

		// 提取文件名
		name := filepath.Base(f.Name)
		if name == "." || name == ".." {
			continue
		}

		// 构建目标路径（直接放在 bin 目录）
		targetPath := filepath.Join(cfIPOptBinDir, name)

		// 创建目标文件
		out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			out.Close()
			return err
		}

		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()

		if err != nil {
			return err
		}

		// 检查是否找到二进制文件
		if name == "cfst" || name == "cfst.exe" {
			binaryFound = true
			// Linux 下设置可执行权限
			if runtime.GOOS != "windows" {
				os.Chmod(targetPath, 0755)
			}
		}

		logger.Log.Debug("解压文件", "file", name, "target", targetPath)
	}

	if !binaryFound {
		return fmt.Errorf("压缩包中未找到 CloudflareST 二进制文件")
	}

	logger.Log.Info("CloudflareST 解压完成", "dir", cfIPOptBinDir)
	return nil
}

// ===================== 任务控制 =====================

// StartCFIPOptTask 启动优选任务（手动触发）
func StartCFIPOptTask() (taskID string, err error) {
	return startCFIPOptTaskInternal(false)
}

// StartCFIPOptTaskScheduled 启动优选任务（定时任务触发）
func StartCFIPOptTaskScheduled() (taskID string, err error) {
	return startCFIPOptTaskInternal(true)
}

// startCFIPOptTaskInternal 内部启动优选任务
func startCFIPOptTaskInternal(isScheduled bool) (taskID string, err error) {
	cfIPOptMu.Lock()
	defer cfIPOptMu.Unlock()

	// 检查是否已有任务运行
	if cfIPOptState.Status == "running" {
		return "", fmt.Errorf("任务正在运行中")
	}

	// 检查二进制是否存在
	exists, _, _ := IsCFIPOptBinaryExists()
	if !exists {
		return "", fmt.Errorf("CloudflareST 二进制文件不存在，请先下载或手动上传")
	}

	// 生成任务 ID
	taskID = fmt.Sprintf("%d", time.Now().Unix())

	// 初始化状态
	cfIPOptState = CFIPOptTaskState{
		TaskID:    taskID,
		Status:    "running",
		Progress:  0,
		StartedAt: time.Now(),
		LogLines:  make([]string, 0, cfIPOptLogBufferSize),
	}
	cfIPOptLogIndex = 0

	// 创建停止通道
	cfIPOptStopChan = make(chan struct{})

	// 记录上次运行时间
	setCFIPOptConfig("cf_ipopt_last_run_at", time.Now().Format(time.RFC3339))

	// 启动后台任务
	go runCFIPOptTask(taskID, isScheduled)

	taskType := "手动"
	if isScheduled {
		taskType = "定时"
	}
	logger.Log.Info("CF 优选任务已启动", "task_id", taskID, "type", taskType)
	return taskID, nil
}

// StopCFIPOptTask 停止当前任务
func StopCFIPOptTask() error {
	cfIPOptMu.Lock()
	defer cfIPOptMu.Unlock()

	if cfIPOptState.Status != "running" {
		return nil
	}

	// 发送停止信号
	if cfIPOptStopChan != nil {
		close(cfIPOptStopChan)
	}

	// 终止进程
	if cfIPOptCmd != nil && cfIPOptCmd.Process != nil {
		cfIPOptCmd.Process.Kill()
	}

	cfIPOptState.Status = "stopped"
	cfIPOptState.EndedAt = time.Now()

	logger.Log.Info("CF 优选任务已停止", "task_id", cfIPOptState.TaskID)
	return nil
}

// GetCFIPOptProgress 获取任务进度
func GetCFIPOptProgress() CFIPOptTaskState {
	cfIPOptMu.Lock()
	defer cfIPOptMu.Unlock()
	return cfIPOptState
}

// runCFIPOptTask 执行优选任务
func runCFIPOptTask(taskID string, isScheduled bool) {
	// 创建日志文件
	logFilePath := filepath.Join(cfIPOptLogDir, fmt.Sprintf("task_%s.log", taskID))
	logFile, err := os.Create(logFilePath)
	if err != nil {
		logger.Log.Error("创建任务日志文件失败", "error", err)
	} else {
		defer logFile.Close()
	}

	// 多重写入器（同时写文件和内存缓冲）
	var logWriter io.Writer = os.Stdout
	if logFile != nil {
		logWriter = io.MultiWriter(os.Stdout, logFile)
	}

	// 执行 CloudflareST
	binaryPath := cfIPOptBinaryPath()

	// 获取绝对路径
	absBinaryPath, err := filepath.Abs(binaryPath)
	if err != nil {
		setTaskFailed(fmt.Sprintf("获取二进制绝对路径失败: %v", err))
		return
	}

	// 获取工作目录的绝对路径（bin 目录，因为 ip.txt 等配置文件在 bin 目录下）
	absWorkDir, err := filepath.Abs(cfIPOptBinDir)
	if err != nil {
		setTaskFailed(fmt.Sprintf("获取工作目录绝对路径失败: %v", err))
		return
	}

	// 构建命令参数
	absResultCSV, _ := filepath.Abs(cfIPOptResultCSVFile)
	args := []string{
		"-o", absResultCSV, // 指定输出文件路径到 tmp 目录
		"-p", "0", // 禁用结果打印（Windows 上 -p 非0 会在末尾调用 fmt.Scanln 等待回车，导致 pipe 模式永远阻塞）
	}

	// 获取实际使用的测速地址
	// isScheduled 为 true 时，优先使用定时优选专用地址
	speedTestURL := GetEffectiveSpeedTestURL(isScheduled)
	taskType := "手动"
	if isScheduled {
		taskType = "定时"
	}
	logger.Log.Info("CF 优选使用测速地址", "url", speedTestURL, "type", taskType)
	args = append(args, "-url", speedTestURL)

	// 读取调试模式配置
	debugMode := getCFIPOptConfig("cf_ipopt_debug_mode")
	if debugMode == "true" || debugMode == "1" {
		args = append(args, "-debug")
		logger.Log.Info("CF 优选已开启调试模式")
	}

	cfIPOptCmd = exec.Command(absBinaryPath, args...)
	cfIPOptCmd.Dir = absWorkDir

	// 关闭 stdin，防止子进程等待输入
	cfIPOptCmd.Stdin = nil

	// 创建管道获取输出
	stdout, err := cfIPOptCmd.StdoutPipe()
	if err != nil {
		setTaskFailed(fmt.Sprintf("创建 stdout 管道失败: %v", err))
		return
	}
	stderr, err := cfIPOptCmd.StderrPipe()
	if err != nil {
		setTaskFailed(fmt.Sprintf("创建 stderr 管道失败: %v", err))
		return
	}

	// 启动进程
	startTime := time.Now()
	if err := cfIPOptCmd.Start(); err != nil {
		setTaskFailed(fmt.Sprintf("启动 CloudflareST 失败: %v", err))
		return
	}

	// 实时读取输出并更新进度
	// 【关键说明】：
	// 1. stdout 输出标题和阶段信息（如 "# XIU2/CloudflareSpeedTest", "开始延迟测速"，结果表格等）
	// 2. stderr 输出进度条（使用 pb/v3 库）
	// 3. pb/v3 在 TTY 模式下使用 \r 回车覆盖进度条，但在非 TTY（pipe）模式下
	//    既不使用 \r 也不使用 \n，进度条行之间只有空格分隔（无分隔符）
	// 4. 因此必须用不同的读取策略分别处理 stdout 和 stderr
	outputChan := make(chan string, 200)

	// 读取 stdout：按行（\r 或 \n）分割
	readStdout := func(pipe io.ReadCloser) {
		reader := bufio.NewReader(pipe)
		var lineBuffer strings.Builder

		for {
			b, err := reader.ReadByte()
			if err != nil {
				if lineBuffer.Len() > 0 {
					outputChan <- lineBuffer.String()
				}
				break
			}

			switch b {
			case '\r':
				if lineBuffer.Len() > 0 {
					outputChan <- lineBuffer.String()
					lineBuffer.Reset()
				}
			case '\n':
				if lineBuffer.Len() > 0 {
					outputChan <- lineBuffer.String()
					lineBuffer.Reset()
				}
			default:
				lineBuffer.WriteByte(b)
			}
		}
	}

	// 读取 stderr（进度条流 + debug 信息）：
	// pb/v3 非 TTY 模式输出格式：
	//   "50 / 5955 [ ___ ... ] 可用: 50  85 / 5955 [↗___ ... ] 可用: 75  ..."
	// 每个进度条片段结构："{current} / {total} [{bar}] {text}  " (末尾两个空格)
	// 分割策略：以 "]" + 后续文本 + "  " + 数字 为边界分割进度条行
	//
	// 【重要】开启 -debug 参数后，CloudflareST 的调试信息（如 TLS 报错等）也通过 stderr 输出，
	// 这些行使用标准 \n 换行。因此 readStderr 必须同时处理两种模式：
	//   1. 遇到 \n 或 \r 时按行输出（debug 信息、普通文本）
	//   2. 无换行符时按进度条分割逻辑处理
	readStderr := func(pipe io.ReadCloser) {
		reader := bufio.NewReader(pipe)
		buf := make([]byte, 0, 4096)

		for {
			b, err := reader.ReadByte()
			if err != nil {
				// 处理剩余内容
				if len(buf) > 0 {
					line := strings.TrimSpace(string(buf))
					if line != "" {
						outputChan <- line
					}
				}
				break
			}

			// 遇到换行符时，按行输出（支持 debug 信息）
			if b == '\n' || b == '\r' {
				if len(buf) > 0 {
					line := strings.TrimSpace(string(buf))
					if line != "" {
						outputChan <- line
					}
					buf = buf[:0]
				}
				continue
			}

			buf = append(buf, b)

			// 检查是否遇到进度条行的分割点
			// 特征：`]` 后面跟着非`[`字符，然后是两个空格 `  ` 紧接数字
			// 实际格式如 "可用: 50  85 / 5955" 中的 "  85" 就是分割点
			bufLen := len(buf)
			if bufLen >= 4 && b >= '0' && b <= '9' {
				// 往前查看是否有 "  " (两个空格) 紧接当前数字
				if buf[bufLen-2] == ' ' && buf[bufLen-3] == ' ' {
					// 提取分割点之前的内容（不含末尾的两个空格和当前数字）
					line := strings.TrimSpace(string(buf[:bufLen-3]))
					if line != "" {
						outputChan <- line
					}
					// 保留当前数字作为下一行的开头
					buf = buf[:1]
					buf[0] = b
				}
			}

			// 防止缓冲区无限增长（安全阀）
			if bufLen > 1024 {
				// 强制截断输出
				line := strings.TrimSpace(string(buf))
				if line != "" {
					outputChan <- line
				}
				buf = buf[:0]
			}
		}
	}

	// 用 WaitGroup 等待两个 pipe reader 都退出后再关闭 outputChan
	var pipeWg sync.WaitGroup
	pipeWg.Add(2)
	go func() {
		defer pipeWg.Done()
		readStdout(stdout)
	}()
	go func() {
		defer pipeWg.Done()
		readStderr(stderr)
	}()
	go func() {
		pipeWg.Wait()
		close(outputChan)
	}()

	// 处理输出
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		for line := range outputChan {
			// 写入日志（进度条行不写入日志文件，避免刷屏）
			if !isProgressBarLine(line) {
				fmt.Fprintln(logWriter, line)
			}

			// 添加到内存缓冲（进度条行替换最后一行）
			appendLogLine(line)

			// 解析进度（CloudflareST 输出格式）
			updateProgressFromOutput(line)
		}
	}()

	// 等待完成或超时
	done := make(chan error, 1)
	go func() {
		done <- cfIPOptCmd.Wait()
	}()

	select {
	case <-cfIPOptStopChan:
		// 用户停止
		cfIPOptMu.Lock()
		cfIPOptState.Status = "stopped"
		cfIPOptState.EndedAt = time.Now()
		cfIPOptMu.Unlock()

	case err := <-done:
		// 等待输出处理完毕，确保所有日志都被记录
		<-outputDone

		cfIPOptMu.Lock()
		defer cfIPOptMu.Unlock()

		if err != nil {
			cfIPOptState.Status = "failed"
			cfIPOptState.Error = err.Error()
			cfIPOptState.EndedAt = time.Now()
			logger.Log.Error("CloudflareST 执行失败", "error", err)
		} else {
			// 解析结果
			duration := time.Since(startTime)
			if err := parseAndSaveResult(duration); err != nil {
				cfIPOptState.Status = "failed"
				cfIPOptState.Error = fmt.Sprintf("解析结果失败: %v", err)
				cfIPOptState.EndedAt = time.Now()
			} else {
				cfIPOptState.Status = "completed"
				cfIPOptState.Progress = 100
				cfIPOptState.EndedAt = time.Now()
				logger.Log.Info("CF 优选任务完成", "task_id", taskID, "duration", duration)
			}
		}

	case <-time.After(cfIPOptTaskTimeout):
		// 超时
		cfIPOptCmd.Process.Kill()
		cfIPOptMu.Lock()
		cfIPOptState.Status = "failed"
		cfIPOptState.Error = "任务执行超时（20分钟）"
		cfIPOptState.EndedAt = time.Now()
		cfIPOptMu.Unlock()
	}
}

// isProgressBarLine 检查是否为进度条行（包含进度条特征）
// CloudflareST 进度条格式：123 / 5955 [---...] 可用: 50
// 需要更精确的匹配，避免将 debug 信息（如 TLS 错误）误判为进度条
func isProgressBarLine(line string) bool {
	// 进度条行的核心特征：以 "数字 / 数字" 开头，并包含 "[" 和 "]"
	trimmed := strings.TrimSpace(line)
	// 快速排除：debug 信息通常包含这些关键词
	if strings.Contains(trimmed, "tls:") || strings.Contains(trimmed, "http:") ||
		strings.Contains(trimmed, "error") || strings.Contains(trimmed, "Error") ||
		strings.Contains(trimmed, "timeout") || strings.Contains(trimmed, "reset") ||
		strings.Contains(trimmed, "certificate") || strings.Contains(trimmed, "handshake") ||
		strings.Contains(trimmed, "connection") || strings.Contains(trimmed, "HTTP") {
		return false
	}
	// 基本进度条特征：包含 "数字 / 数字" 和 "[" 和 "]"
	if !strings.Contains(trimmed, "/") || !strings.Contains(trimmed, "[") || !strings.Contains(trimmed, "]") {
		return false
	}
	// 进一步检查：以数字开头（进度条的 current 值）
	if len(trimmed) > 0 && trimmed[0] >= '0' && trimmed[0] <= '9' {
		return true
	}
	return false
}

// appendLogLine 添加日志行到环形缓冲
// 进度条行会替换最后一行，避免刷屏
func appendLogLine(line string) {
	cfIPOptMu.Lock()
	defer cfIPOptMu.Unlock()

	// 每次变更都递增版本号，供 SSE 检测变化
	cfIPOptState.LogVer++

	// 如果是进度条行，替换最后一行
	if isProgressBarLine(line) {
		if len(cfIPOptState.LogLines) > 0 {
			// 检查最后一行是否也是进度条行，是则替换，否则新增
			lastIdx := len(cfIPOptState.LogLines) - 1
			if isProgressBarLine(cfIPOptState.LogLines[lastIdx]) {
				cfIPOptState.LogLines[lastIdx] = line
			} else {
				cfIPOptState.LogLines = append(cfIPOptState.LogLines, line)
			}
		} else {
			cfIPOptState.LogLines = append(cfIPOptState.LogLines, line)
		}
		return
	}

	// 普通行，正常添加
	if len(cfIPOptState.LogLines) < cfIPOptLogBufferSize {
		cfIPOptState.LogLines = append(cfIPOptState.LogLines, line)
	} else {
		cfIPOptState.LogLines[cfIPOptLogIndex] = line
		cfIPOptLogIndex = (cfIPOptLogIndex + 1) % cfIPOptLogBufferSize
	}
}

// updateProgressFromOutput 从输出解析进度
func updateProgressFromOutput(line string) {
	cfIPOptMu.Lock()
	defer cfIPOptMu.Unlock()

	// CloudflareST 进度条格式示例：
	//   123 / 5955 [------>                    ]  可用: 10
	// 解析进度条中的 current / total 来计算百分比
	if isProgressBarLineUnlocked(line) {
		// 尝试从 "current / total" 解析
		line = strings.TrimSpace(line)
		parts := strings.SplitN(line, "/", 2)
		if len(parts) == 2 {
			currentStr := strings.TrimSpace(parts[0])
			// total 在 '/' 后面、'[' 前面
			rest := parts[1]
			if idx := strings.Index(rest, "["); idx > 0 {
				rest = rest[:idx]
			}
			totalStr := strings.TrimSpace(rest)

			current, err1 := strconv.Atoi(currentStr)
			total, err2 := strconv.Atoi(totalStr)
			if err1 == nil && err2 == nil && total > 0 {
				// 延迟测速阶段占 0-50%，下载测速阶段占 50-90%
				rawPercent := float64(current) / float64(total) * 100
				if cfIPOptState.Progress < 50 {
					// 延迟测速阶段
					cfIPOptState.Progress = int(rawPercent * 0.5)
				} else {
					// 下载测速阶段
					cfIPOptState.Progress = 50 + int(rawPercent*0.4)
				}
			}
		}
		return
	}

	// 根据关键词判断阶段
	lineLower := strings.ToLower(line)

	if strings.Contains(lineLower, "延迟测速") || strings.Contains(lineLower, "latency") {
		if cfIPOptState.Progress < 5 {
			cfIPOptState.Progress = 5
		}
	} else if strings.Contains(lineLower, "下载测速") || strings.Contains(lineLower, "download") {
		cfIPOptState.Progress = 50
	} else if strings.Contains(lineLower, "result") || strings.Contains(lineLower, "结果") || strings.Contains(lineLower, "完整测速") {
		cfIPOptState.Progress = 95
	}
}

// isProgressBarLineUnlocked 不加锁版本的进度条检测（供内部已加锁的函数调用）
// 复用 isProgressBarLine 的逻辑（该函数本身不涉及锁操作）
func isProgressBarLineUnlocked(line string) bool {
	return isProgressBarLine(line)
}

// setTaskFailed 设置任务失败
func setTaskFailed(errMsg string) {
	cfIPOptMu.Lock()
	defer cfIPOptMu.Unlock()
	cfIPOptState.Status = "failed"
	cfIPOptState.Error = errMsg
	cfIPOptState.EndedAt = time.Now()
	logger.Log.Error("CF 优选任务失败", "error", errMsg)
}

// parseAndSaveResult 解析并保存结果
func parseAndSaveResult(duration time.Duration) error {
	// 通过 -o 参数指定了 CSV 输出路径
	csvPath := cfIPOptResultCSVFile

	// 也检查 bin 目录下的 result.csv（兼容旧版本或未使用 -o 的情况）
	if _, err := os.Stat(csvPath); os.IsNotExist(err) {
		altPath := filepath.Join(cfIPOptBinDir, "result.csv")
		if _, err2 := os.Stat(altPath); err2 == nil {
			csvPath = altPath
		} else {
			return fmt.Errorf("结果文件不存在: %s", csvPath)
		}
	}

	// 读取 CSV 文件
	file, err := os.Open(csvPath)
	if err != nil {
		return fmt.Errorf("打开结果文件失败: %v", err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		return fmt.Errorf("读取 CSV 失败: %v", err)
	}

	if len(records) < 2 {
		return fmt.Errorf("结果文件格式错误或无数据")
	}

	// CloudflareST v2.x CSV 表头: IP地址, 已发送, 已接收, 丢包率, 平均延迟, 下载速度 (MB/s)
	// 找到各列的索引
	header := records[0]
	colIP := 0
	colLatency := -1
	colSpeed := -1
	colLoss := -1
	for i, h := range header {
		h = strings.TrimSpace(h)
		switch {
		case strings.Contains(h, "IP"):
			colIP = i
		case strings.Contains(h, "延迟") || strings.Contains(strings.ToLower(h), "latency"):
			colLatency = i
		case strings.Contains(h, "速度") || strings.Contains(strings.ToLower(h), "speed"):
			colSpeed = i
		case strings.Contains(h, "丢包") || strings.Contains(strings.ToLower(h), "loss"):
			colLoss = i
		}
	}

	// 解析结果（跳过表头）
	var entries []CFIPOptEntry
	for i, record := range records[1:] {
		if i >= 10 {
			break // 只取前 10 个
		}
		if len(record) <= colIP {
			continue
		}

		entry := CFIPOptEntry{
			IP: strings.TrimSpace(record[colIP]),
		}

		// 解析数值字段
		if colLatency >= 0 && colLatency < len(record) {
			if val, err := strconv.ParseFloat(strings.TrimSpace(record[colLatency]), 64); err == nil {
				entry.Latency = val
			}
		}
		if colSpeed >= 0 && colSpeed < len(record) {
			if val, err := strconv.ParseFloat(strings.TrimSpace(record[colSpeed]), 64); err == nil {
				entry.Speed = val
			}
		}
		if colLoss >= 0 && colLoss < len(record) {
			lossStr := strings.TrimSpace(record[colLoss])
			lossStr = strings.TrimSuffix(lossStr, "%")
			if val, err := strconv.ParseFloat(lossStr, 64); err == nil {
				entry.Loss = val
			}
		}

		entries = append(entries, entry)
	}

	if len(entries) == 0 {
		return fmt.Errorf("未找到有效的优选结果")
	}

	// 构建结果对象
	result := CFIPOptResult{
		Top10:    entries,
		Duration: duration,
		FinishAt: time.Now(),
	}

	// 保存到 JSON 文件
	resultJSON, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化结果失败: %v", err)
	}

	if err := os.WriteFile(cfIPOptResultFile, resultJSON, 0644); err != nil {
		return fmt.Errorf("保存结果文件失败: %v", err)
	}

	logger.Log.Info("CF 优选结果已保存", "file", cfIPOptResultFile, "count", len(entries))
	return nil
}

// GetCFIPOptResult 读取 latest_result.json
func GetCFIPOptResult() (*CFIPOptResult, error) {
	data, err := os.ReadFile(cfIPOptResultFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取结果文件失败: %v", err)
	}

	var result CFIPOptResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("解析结果文件失败: %v", err)
	}

	return &result, nil
}

// HasValidIPOptResult 检查是否有可用的优选结果
func HasValidIPOptResult() bool {
	result, err := GetCFIPOptResult()
	if err != nil || result == nil {
		return false
	}
	return len(result.Top10) > 0
}

// GetTop1IPOptIP 供订阅生成链路读取 Top1 IP
func GetTop1IPOptIP() (string, error) {
	result, err := GetCFIPOptResult()
	if err != nil {
		return "", err
	}
	if result == nil || len(result.Top10) == 0 {
		return "", fmt.Errorf("无可用的优选结果")
	}
	return result.Top10[0].IP, nil
}

// ===================== 设置管理 =====================

// getCFIPOptConfig 获取配置值
func getCFIPOptConfig(key string) string {
	var config database.SysConfig
	if err := database.DB.Where("key = ?", key).First(&config).Error; err != nil {
		return ""
	}
	return config.Value
}

// setCFIPOptConfig 设置配置值
func setCFIPOptConfig(key, value string) {
	config := database.SysConfig{
		Key:   key,
		Value: value,
	}
	database.DB.Where("key = ?", key).Assign(config).FirstOrCreate(&config)
}

// GetCFIPOptSettings 获取优选设置
func GetCFIPOptSettings() CFIPOptSettings {
	exists, version, _ := IsCFIPOptBinaryExists()

	// 解析定时间隔
	intervalStr := getCFIPOptConfig("cf_ipopt_schedule_interval")
	interval, _ := strconv.Atoi(intervalStr)

	// 解析应用开关
	applyStr := getCFIPOptConfig("cf_ipopt_apply_tunnel_nodes")
	applyToTunnel := applyStr == "true" || applyStr == "1"

	// 解析测速地址
	speedTestURL := getCFIPOptConfig("cf_ipopt_speed_test_url")

	// 解析定时优选测速地址
	scheduleSpeedTestURL := getCFIPOptConfig("cf_ipopt_schedule_speed_test_url")

	// 解析调试模式
	debugStr := getCFIPOptConfig("cf_ipopt_debug_mode")
	debugMode := debugStr == "true" || debugStr == "1"

	return CFIPOptSettings{
		ScheduleInterval:     interval,
		ApplyToTunnelNodes:   applyToTunnel,
		BinaryExists:         exists,
		BinaryVersion:        version,
		Platform:             runtime.GOOS + "_" + runtime.GOARCH,
		HasValidResult:       HasValidIPOptResult(),
		SpeedTestURL:         speedTestURL,
		ScheduleSpeedTestURL: scheduleSpeedTestURL,
		DebugMode:            debugMode,
	}
}

// SetCFIPOptSettings 保存优选设置
func SetCFIPOptSettings(interval int, applyToTunnel bool) {
	setCFIPOptConfig("cf_ipopt_schedule_interval", strconv.Itoa(interval))

	applyValue := "false"
	if applyToTunnel {
		applyValue = "true"
	}
	setCFIPOptConfig("cf_ipopt_apply_tunnel_nodes", applyValue)

	logger.Log.Info("CF 优选设置已保存", "interval", interval, "apply_to_tunnel", applyToTunnel)
}

// SetCFIPOptSpeedTestURL 保存自定义测速地址
func SetCFIPOptSpeedTestURL(url string) {
	setCFIPOptConfig("cf_ipopt_speed_test_url", url)
	logger.Log.Info("CF 优选测速地址已保存", "url", url)
}

// SetCFIPOptScheduleSpeedTestURL 保存定时优选测速地址
func SetCFIPOptScheduleSpeedTestURL(url string) {
	setCFIPOptConfig("cf_ipopt_schedule_speed_test_url", url)
	logger.Log.Info("CF 定时优选测速地址已保存", "url", url)
}

// GetEffectiveSpeedTestURL 获取实际使用的测速地址
// isScheduled: 是否为定时优选任务
// 定时优选任务：使用默认测速地址ID对应的URL，若未设置则使用内置默认
// 手动优选任务：直接使用内置默认地址
func GetEffectiveSpeedTestURL(isScheduled bool) string {
	if isScheduled {
		// 定时优选任务：使用设置的默认测速地址
		defaultID := GetDefaultSpeedTestURLID()
		if defaultID != "" {
			return GetSpeedTestURLByID(defaultID)
		}
	}
	// 手动优选任务或未设置默认：使用内置默认地址
	return cfIPOptDefaultSpeedTestURL
}

// SetCFIPOptDebugMode 保存调试模式开关
func SetCFIPOptDebugMode(enabled bool) {
	value := "false"
	if enabled {
		value = "true"
	}
	setCFIPOptConfig("cf_ipopt_debug_mode", value)
	logger.Log.Info("CF 优选调试模式已保存", "enabled", enabled)
}

// IsApplyToTunnelNodesEnabled 检查是否启用应用到 Tunnel 节点
func IsApplyToTunnelNodesEnabled() bool {
	applyStr := getCFIPOptConfig("cf_ipopt_apply_tunnel_nodes")
	return applyStr == "true" || applyStr == "1"
}

// ===================== 定时任务 =====================

// StartCFIPOptScheduler 启动定时优选调度器
func StartCFIPOptScheduler() {
	cfIPOptSchedulerMu.Lock()
	defer cfIPOptSchedulerMu.Unlock()

	if cfIPOptSchedulerRunning {
		return
	}

	// 检查是否配置了定时
	intervalStr := getCFIPOptConfig("cf_ipopt_schedule_interval")
	interval, _ := strconv.Atoi(intervalStr)
	if interval <= 0 {
		return
	}

	cfIPOptSchedulerStop = make(chan struct{})
	cfIPOptSchedulerRunning = true

	go runCFIPOptScheduler(interval)

	logger.Log.Info("CF 定时优选调度器已启动", "interval_hours", interval)
}

// StopCFIPOptScheduler 停止定时优选调度器
func StopCFIPOptScheduler() {
	cfIPOptSchedulerMu.Lock()
	defer cfIPOptSchedulerMu.Unlock()

	if !cfIPOptSchedulerRunning {
		return
	}

	if cfIPOptSchedulerStop != nil {
		close(cfIPOptSchedulerStop)
	}
	cfIPOptSchedulerRunning = false

	logger.Log.Info("CF 定时优选调度器已停止")
}

// runCFIPOptScheduler 运行定时调度器
func runCFIPOptScheduler(intervalHours int) {
	// 等待 60 秒延迟
	select {
	case <-time.After(60 * time.Second):
	case <-cfIPOptSchedulerStop:
		return
	}

	// 检查是否需要立即执行（启动时超过间隔）
	lastRunStr := getCFIPOptConfig("cf_ipopt_last_run_at")
	if lastRunStr != "" {
		lastRun, err := time.Parse(time.RFC3339, lastRunStr)
		if err == nil && time.Since(lastRun) > time.Duration(intervalHours)*time.Hour {
			// 超过间隔，立即执行
			triggerScheduledIPOpt()
		}
	} else {
		// 从未执行过，立即执行
		triggerScheduledIPOpt()
	}

	// 计算下次整点执行时间
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-cfIPOptSchedulerStop:
			return
		case now := <-ticker.C:
			// 检查是否是整点
			if now.Minute() != 0 || now.Second() != 0 {
				continue
			}

			// 检查是否达到间隔
			lastRunStr := getCFIPOptConfig("cf_ipopt_last_run_at")
			if lastRunStr == "" {
				triggerScheduledIPOpt()
				continue
			}

			lastRun, err := time.Parse(time.RFC3339, lastRunStr)
			if err != nil {
				triggerScheduledIPOpt()
				continue
			}

			if time.Since(lastRun) >= time.Duration(intervalHours)*time.Hour {
				triggerScheduledIPOpt()
			}
		}
	}
}

// triggerScheduledIPOpt 触发定时优选
func triggerScheduledIPOpt() {
	cfIPOptMu.Lock()
	// 检查是否有任务正在运行
	if cfIPOptState.Status == "running" {
		logger.Log.Info("定时优选跳过：已有任务运行中")
		cfIPOptMu.Unlock()
		return
	}
	cfIPOptMu.Unlock()

	// 启动优选任务（使用定时任务专用接口）
	taskID, err := StartCFIPOptTaskScheduled()
	if err != nil {
		logger.Log.Error("定时优选启动失败", "error", err)
		return
	}
	logger.Log.Info("定时优选任务已启动", "task_id", taskID)
}

// ===================== 测速地址管理 =====================

// GetSpeedTestURLs 获取所有测速地址列表
func GetSpeedTestURLs() []SpeedTestURLItem {
	// 从数据库读取用户自定义地址列表
	urlsJSON := getCFIPOptConfig("cf_ipopt_speed_urls")

	var urls []SpeedTestURLItem
	if urlsJSON != "" && urlsJSON != "[]" {
		json.Unmarshal([]byte(urlsJSON), &urls)
	}

	// 在列表开头插入内置地址
	result := make([]SpeedTestURLItem, 0, len(urls)+1)
	result = append(result, SpeedTestURLItem{
		ID:        cfIPOptBuiltInID,
		Name:      "内置测速地址",
		URL:       cfIPOptDefaultSpeedTestURL,
		IsBuiltin: true,
	})
	result = append(result, urls...)

	return result
}

// AddSpeedTestURL 添加测速地址
func AddSpeedTestURL(name, url string) (*SpeedTestURLItem, error) {
	if name == "" {
		return nil, fmt.Errorf("备注名不能为空")
	}
	if url == "" {
		return nil, fmt.Errorf("测速地址不能为空")
	}

	// 验证URL格式
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("测速地址必须以 http:// 或 https:// 开头")
	}

	// 读取现有列表
	urlsJSON := getCFIPOptConfig("cf_ipopt_speed_urls")
	var urls []SpeedTestURLItem
	if urlsJSON != "" && urlsJSON != "[]" {
		json.Unmarshal([]byte(urlsJSON), &urls)
	}

	// 生成唯一ID
	id := fmt.Sprintf("url_%d", time.Now().UnixNano())

	// 创建新条目
	newItem := SpeedTestURLItem{
		ID:        id,
		Name:      name,
		URL:       url,
		IsBuiltin: false,
	}

	urls = append(urls, newItem)

	// 保存到数据库
	newJSON, _ := json.Marshal(urls)
	setCFIPOptConfig("cf_ipopt_speed_urls", string(newJSON))

	logger.Log.Info("添加测速地址", "id", id, "name", name, "url", url)
	return &newItem, nil
}

// UpdateSpeedTestURL 更新测速地址
func UpdateSpeedTestURL(id, name, url string) error {
	if id == "" {
		return fmt.Errorf("ID不能为空")
	}
	if id == cfIPOptBuiltInID {
		return fmt.Errorf("内置地址不可修改")
	}
	if name == "" {
		return fmt.Errorf("备注名不能为空")
	}
	if url == "" {
		return fmt.Errorf("测速地址不能为空")
	}

	// 验证URL格式
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("测速地址必须以 http:// 或 https:// 开头")
	}

	// 读取现有列表
	urlsJSON := getCFIPOptConfig("cf_ipopt_speed_urls")
	var urls []SpeedTestURLItem
	if urlsJSON != "" && urlsJSON != "[]" {
		json.Unmarshal([]byte(urlsJSON), &urls)
	}

	// 查找并更新
	found := false
	for i, item := range urls {
		if item.ID == id {
			urls[i].Name = name
			urls[i].URL = url
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("未找到指定的测速地址")
	}

	// 保存到数据库
	newJSON, _ := json.Marshal(urls)
	setCFIPOptConfig("cf_ipopt_speed_urls", string(newJSON))

	logger.Log.Info("更新测速地址", "id", id, "name", name, "url", url)
	return nil
}

// DeleteSpeedTestURL 删除测速地址
func DeleteSpeedTestURL(id string) error {
	if id == "" {
		return fmt.Errorf("ID不能为空")
	}
	if id == cfIPOptBuiltInID {
		return fmt.Errorf("内置地址不可删除")
	}

	// 读取现有列表
	urlsJSON := getCFIPOptConfig("cf_ipopt_speed_urls")
	var urls []SpeedTestURLItem
	if urlsJSON != "" && urlsJSON != "[]" {
		json.Unmarshal([]byte(urlsJSON), &urls)
	}

	// 查找并删除
	found := false
	newUrls := make([]SpeedTestURLItem, 0, len(urls))
	for _, item := range urls {
		if item.ID == id {
			found = true
			continue
		}
		newUrls = append(newUrls, item)
	}

	if !found {
		return fmt.Errorf("未找到指定的测速地址")
	}

	// 保存到数据库
	newJSON, _ := json.Marshal(newUrls)
	setCFIPOptConfig("cf_ipopt_speed_urls", string(newJSON))

	// 如果删除的是默认地址，清空默认设置
	defaultID := getCFIPOptConfig("cf_ipopt_default_speed_url_id")
	if defaultID == id {
		setCFIPOptConfig("cf_ipopt_default_speed_url_id", "")
		logger.Log.Info("删除的地址是定时优选默认地址，已清空默认设置")
	}

	logger.Log.Info("删除测速地址", "id", id)
	return nil
}

// SetDefaultSpeedTestURL 设置定时优选默认测速地址
func SetDefaultSpeedTestURL(id string) error {
	if id == "" {
		// 清空默认设置
		setCFIPOptConfig("cf_ipopt_default_speed_url_id", "")
		logger.Log.Info("已清空定时优选默认测速地址")
		return nil
	}

	// 验证ID是否存在
	urls := GetSpeedTestURLs()
	found := false
	for _, item := range urls {
		if item.ID == id {
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("未找到指定的测速地址")
	}

	setCFIPOptConfig("cf_ipopt_default_speed_url_id", id)
	logger.Log.Info("设置定时优选默认测速地址", "id", id)
	return nil
}

// GetDefaultSpeedTestURLID 获取定时优选默认测速地址ID
func GetDefaultSpeedTestURLID() string {
	return getCFIPOptConfig("cf_ipopt_default_speed_url_id")
}

// GetSpeedTestURLByID 根据ID获取测速地址
func GetSpeedTestURLByID(id string) string {
	if id == "" || id == cfIPOptBuiltInID {
		return cfIPOptDefaultSpeedTestURL
	}

	urls := GetSpeedTestURLs()
	for _, item := range urls {
		if item.ID == id {
			return item.URL
		}
	}

	// 未找到则返回内置地址
	return cfIPOptDefaultSpeedTestURL
}

// ===================== 手动优选列表管理 =====================

// ManualIPOptItem 手动优选IP条目
type ManualIPOptItem struct {
	ID      string `json:"id"`      // 唯一ID
	Remark  string `json:"remark"`  // 备注
	IP      string `json:"ip"`      // IP地址
	Enabled bool   `json:"enabled"` // 是否启用
}

// GetManualIPOptList 获取手动优选IP列表
func GetManualIPOptList() []ManualIPOptItem {
	ipsJSON := getCFIPOptConfig("cf_ipopt_manual_ips")

	var ips []ManualIPOptItem
	if ipsJSON != "" && ipsJSON != "[]" {
		json.Unmarshal([]byte(ipsJSON), &ips)
	}

	return ips
}

// GetManualIPOptPriority 获取手动优选IP优先级
// 返回值: "disabled" (停用), "preferred" (首选)
func GetManualIPOptPriority() string {
	priority := getCFIPOptConfig("cf_ipopt_manual_priority")
	if priority == "" {
		return "disabled"
	}
	return priority
}

// SetManualIPOptPriority 设置手动优选IP优先级
func SetManualIPOptPriority(priority string) error {
	if priority != "disabled" && priority != "preferred" {
		return fmt.Errorf("无效的优先级值")
	}
	setCFIPOptConfig("cf_ipopt_manual_priority", priority)
	logger.Log.Info("手动优选IP优先级已设置", "priority", priority)
	return nil
}

// AddManualIPOpt 添加手动优选IP
func AddManualIPOpt(remark, ip string) (*ManualIPOptItem, error) {
	if ip == "" {
		return nil, fmt.Errorf("IP地址不能为空")
	}

	// 读取现有列表
	ipsJSON := getCFIPOptConfig("cf_ipopt_manual_ips")
	var ips []ManualIPOptItem
	if ipsJSON != "" && ipsJSON != "[]" {
		json.Unmarshal([]byte(ipsJSON), &ips)
	}

	// 生成唯一ID
	id := fmt.Sprintf("manual_%d", time.Now().UnixNano())

	// 创建新条目
	newItem := ManualIPOptItem{
		ID:      id,
		Remark:  remark,
		IP:      ip,
		Enabled: true,
	}

	ips = append(ips, newItem)

	// 保存到数据库
	newJSON, _ := json.Marshal(ips)
	setCFIPOptConfig("cf_ipopt_manual_ips", string(newJSON))

	logger.Log.Info("添加手动优选IP", "id", id, "remark", remark, "ip", ip)
	return &newItem, nil
}

// UpdateManualIPOpt 更新手动优选IP
func UpdateManualIPOpt(id, remark, ip string) error {
	if id == "" {
		return fmt.Errorf("ID不能为空")
	}
	if ip == "" {
		return fmt.Errorf("IP地址不能为空")
	}

	// 读取现有列表
	ipsJSON := getCFIPOptConfig("cf_ipopt_manual_ips")
	var ips []ManualIPOptItem
	if ipsJSON != "" && ipsJSON != "[]" {
		json.Unmarshal([]byte(ipsJSON), &ips)
	}

	// 查找并更新
	found := false
	for i, item := range ips {
		if item.ID == id {
			ips[i].Remark = remark
			ips[i].IP = ip
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("未找到指定的手动优选IP")
	}

	// 保存到数据库
	newJSON, _ := json.Marshal(ips)
	setCFIPOptConfig("cf_ipopt_manual_ips", string(newJSON))

	logger.Log.Info("更新手动优选IP", "id", id, "remark", remark, "ip", ip)
	return nil
}

// DeleteManualIPOpt 删除手动优选IP
func DeleteManualIPOpt(id string) error {
	if id == "" {
		return fmt.Errorf("ID不能为空")
	}

	// 读取现有列表
	ipsJSON := getCFIPOptConfig("cf_ipopt_manual_ips")
	var ips []ManualIPOptItem
	if ipsJSON != "" && ipsJSON != "[]" {
		json.Unmarshal([]byte(ipsJSON), &ips)
	}

	// 查找并删除
	found := false
	newIps := make([]ManualIPOptItem, 0, len(ips))
	for _, item := range ips {
		if item.ID == id {
			found = true
			continue
		}
		newIps = append(newIps, item)
	}

	if !found {
		return fmt.Errorf("未找到指定的手动优选IP")
	}

	// 保存到数据库
	newJSON, _ := json.Marshal(newIps)
	setCFIPOptConfig("cf_ipopt_manual_ips", string(newJSON))

	logger.Log.Info("删除手动优选IP", "id", id)
	return nil
}

// ToggleManualIPOpt 切换手动优选IP启用状态
// 【重要】实现互斥逻辑：同时只能有一个IP是启用状态
// 当 enabled=true 时，会自动将其它所有IP设置为 enabled=false
func ToggleManualIPOpt(id string, enabled bool) error {
	if id == "" {
		return fmt.Errorf("ID不能为空")
	}

	// 读取现有列表
	ipsJSON := getCFIPOptConfig("cf_ipopt_manual_ips")
	var ips []ManualIPOptItem
	if ipsJSON != "" && ipsJSON != "[]" {
		json.Unmarshal([]byte(ipsJSON), &ips)
	}

	// 查找并更新
	found := false
	for i, item := range ips {
		if item.ID == id {
			ips[i].Enabled = enabled
			found = true
		} else if enabled {
			// 【互斥逻辑】如果是开启操作，将其它所有IP都设置为关闭
			ips[i].Enabled = false
		}
	}

	if !found {
		return fmt.Errorf("未找到指定的手动优选IP")
	}

	// 保存到数据库
	newJSON, _ := json.Marshal(ips)
	setCFIPOptConfig("cf_ipopt_manual_ips", string(newJSON))

	logger.Log.Info("切换手动优选IP启用状态", "id", id, "enabled", enabled)
	return nil
}

// GetEffectiveOptIP 获取实际生效的优选IP（供订阅生成使用）
// 优先级逻辑：
// 1. 手动优选优先级为 "preferred" 且有启用的IP时，返回第一个启用的手动IP
// 2. 否则返回定时优选的 Top1 IP
// 【防报错】即使存在多个启用的IP（异常情况），也只使用第一个
func GetEffectiveOptIP() (string, error) {
	// 检查手动优选优先级
	priority := GetManualIPOptPriority()
	if priority == "preferred" {
		// 查找所有启用的手动优选IP
		ips := GetManualIPOptList()
		var enabledIPs []ManualIPOptItem
		for _, ip := range ips {
			if ip.Enabled && ip.IP != "" {
				enabledIPs = append(enabledIPs, ip)
			}
		}

		// 【防报错】如果存在多个启用的IP，记录警告日志，但只使用第一个
		if len(enabledIPs) > 1 {
			logger.Log.Warn("检测到多个启用的手动优选IP，仅使用第一个",
				"enabled_count", len(enabledIPs),
				"using_ip", enabledIPs[0].IP,
				"using_id", enabledIPs[0].ID)
		}

		if len(enabledIPs) > 0 {
			return enabledIPs[0].IP, nil
		}
	}

	// 使用定时优选的 Top1 IP
	return GetTop1IPOptIP()
}

// ===================== CloudflareST 远程版本检查与自动更新 =====================

// GetCFIPOptRemoteVersion 跟随 GitHub Releases 重定向获取 CloudflareST 最新版本号。
func GetCFIPOptRemoteVersion() (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	tag, _, err := fetchLatestGitHubRelease(client, "https://github.com/XIU2/CloudflareSpeedTest/releases/latest", "nodectl")
	if err != nil {
		return "", fmt.Errorf("获取 CloudflareST release 失败: %w", err)
	}
	return strings.TrimSpace(tag), nil
}

// GetCFIPOptVersionStatus 获取 CloudflareST 版本状态（供前端版本检测用）
// 返回: localVersion, remoteVersion, state ("latest"/"update_available"/"not_found"/"error")
func GetCFIPOptVersionStatus() (localVer, remoteVer, state string) {
	exists, local, _ := IsCFIPOptBinaryExists()
	if !exists {
		localVer = ""
	} else {
		localVer = local
	}

	remote, err := GetCFIPOptRemoteVersion()
	if err != nil {
		logger.Log.Warn("获取 CloudflareST 远程版本失败", "error", err)
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

	// 版本比较：去掉 v 前缀后对比
	cleanLocal := strings.TrimSpace(strings.TrimPrefix(localVer, "v"))
	cleanRemote := strings.TrimSpace(strings.TrimPrefix(remoteVer, "v"))

	if cleanLocal == cleanRemote {
		state = "latest"
	} else {
		state = "update_available"
	}
	return
}

// ForceUpdateCFIPOpt 强制下载最新版 CloudflareST（覆盖现有）
func ForceUpdateCFIPOpt() error {
	logger.Log.Info("开始强制更新 CloudflareST...")

	// 获取最新版本号
	remoteVer, err := GetCFIPOptRemoteVersion()
	if err != nil {
		return fmt.Errorf("获取远程版本失败: %w", err)
	}

	cleanVer := strings.TrimPrefix(strings.TrimSpace(remoteVer), "v")
	downloadURL, err := getCFIPOptDownloadURL(cleanVer)
	if err != nil {
		return fmt.Errorf("获取下载地址失败: %w", err)
	}

	// 创建一个 nil 通道（同步下载，不推送进度）
	progressChan := make(chan map[string]interface{}, 10)
	go func() {
		for range progressChan {
			// 丢弃进度消息
		}
	}()

	err = downloadAndExtract(downloadURL, progressChan)
	close(progressChan)

	if err != nil {
		return fmt.Errorf("下载 CloudflareST 失败: %w", err)
	}

	// 保存版本号
	setCFIPOptConfig("cf_ipopt_bin_version", cleanVer)
	logger.Log.Info("CloudflareST 更新完成", "version", cleanVer)
	return nil
}
