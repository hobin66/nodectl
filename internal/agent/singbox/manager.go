// 路径: internal/agent/singbox/manager.go
// sing-box 子进程生命周期管理器
// 负责启动、停止、重启 sing-box，以及崩溃自动重启和日志捕获
package singbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ProcessStatus sing-box 进程状态
type ProcessStatus struct {
	Running    bool      `json:"running"`
	PID        int       `json:"pid,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	RestartCnt int       `json:"restart_count"`
	LastError  string    `json:"last_error,omitempty"`
}

// Manager sing-box 进程管理器
type Manager struct {
	mu sync.RWMutex

	// 配置
	binaryPath string
	configPath string
	logPath    string
	pidPath    string

	// 子进程
	cmd    *exec.Cmd
	cancel context.CancelFunc

	// 状态
	running    bool
	pid        int
	startedAt  time.Time
	restartCnt int
	lastError  string

	// 配置管理器
	config    *ConfigManager
	installer *Installer

	// 自动重启控制
	maxRestarts    int           // 最大连续重启次数
	restartDelay   time.Duration // 重启间隔
	restartCounter int           // 连续崩溃计数器（非正常退出时+1，正常运行一段时间后清零）
}

// NewManager 创建 sing-box 管理器
func NewManager() *Manager {
	return &Manager{
		binaryPath:   DefaultBinaryPath,
		configPath:   DefaultConfigPath,
		logPath:      "/var/log/nodectl-agent/singbox.log",
		pidPath:      filepath.Join(DefaultWorkDir, "singbox.pid"),
		config:       NewConfigManager(),
		installer:    NewInstaller(""),
		maxRestarts:  5,
		restartDelay: 3 * time.Second,
	}
}

// NewManagerWithConfig 使用指定配置创建管理器
func NewManagerWithConfig(config *ConfigManager, installer *Installer) *Manager {
	m := NewManager()
	if config != nil {
		m.config = config
		m.configPath = config.GetConfigPath()
	}
	if installer != nil {
		m.installer = installer
		m.binaryPath = installer.GetBinaryPath()
	}
	return m
}

// GetConfigManager 返回配置管理器
func (m *Manager) GetConfigManager() *ConfigManager {
	return m.config
}

// GetInstaller 返回安装器
func (m *Manager) GetInstaller() *Installer {
	return m.installer
}

// Start 启动 sing-box 子进程
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running {
		return fmt.Errorf("sing-box 已在运行 (PID=%d)", m.pid)
	}

	// 确保 sing-box 已安装
	if !m.installer.IsInstalled() {
		return fmt.Errorf("sing-box 二进制不存在: %s，请先安装", m.binaryPath)
	}

	// 确保配置文件存在
	if _, err := os.Stat(m.configPath); os.IsNotExist(err) {
		return fmt.Errorf("sing-box 配置文件不存在: %s，请先生成配置", m.configPath)
	}

	return m.startProcess(ctx)
}

// startProcess 内部启动逻辑（调用者须持有锁）
func (m *Manager) startProcess(ctx context.Context) error {
	// 创建子 context 用于管理进程生命周期
	procCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	// 构造命令: sing-box run -c config.json
	cmd := exec.CommandContext(procCtx, m.binaryPath, "run", "-c", m.configPath)

	// 设置日志输出
	logWriter, err := m.openLogWriter()
	if err != nil {
		cancel()
		return fmt.Errorf("打开日志文件失败: %w", err)
	}

	cmd.Stdout = logWriter

	// 🆕 同时将 stderr 捕获到管道，便于回写 Agent 日志诊断启动失败原因
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		if closer, ok := logWriter.(io.Closer); ok {
			closer.Close()
		}
		return fmt.Errorf("创建 stderr 管道失败: %w", err)
	}

	// 启动子进程
	if err := cmd.Start(); err != nil {
		cancel()
		if closer, ok := logWriter.(io.Closer); ok {
			closer.Close()
		}
		m.lastError = err.Error()
		return fmt.Errorf("启动 sing-box 失败: %w", err)
	}

	// 🆕 启动 stderr 读取协程：将 sing-box 的错误输出写入 singbox 专用日志文件
	// 注意：不再写入 Agent 主日志（log.Printf），因为 sing-box 在高流量时会产生
	// 大量连接日志（inbound/outbound connection），会导致 agent 日志文件快速膨胀
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		scanner.Buffer(make([]byte, 64*1024), 256*1024)
		for scanner.Scan() {
			line := scanner.Text()
			// 仅写入 singbox 专用日志文件，不写入 agent 主日志
			if logWriter != nil {
				fmt.Fprintf(logWriter, "[stderr] %s\n", line)
			}
		}
	}()

	m.cmd = cmd
	m.running = true
	m.pid = cmd.Process.Pid
	m.startedAt = time.Now()
	m.lastError = ""

	// 保存 PID 文件
	m.savePID()

	log.Printf("[SingBox] sing-box 已启动 (PID=%d, config=%s)", m.pid, m.configPath)

	// 启动监控协程
	go m.watchProcess(ctx, cmd, logWriter)

	return nil
}

// Stop 停止 sing-box 子进程
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 主动停止时重置崩溃计数器，避免之前的崩溃记录影响后续启动
	m.restartCounter = 0

	return m.stopProcess()
}

// stopProcess 内部停止逻辑（调用者须持有锁）
func (m *Manager) stopProcess() error {
	if !m.running || m.cmd == nil {
		return nil
	}

	log.Printf("[SingBox] 正在停止 sing-box (PID=%d)...", m.pid)

	// 取消 context 触发进程终止
	if m.cancel != nil {
		m.cancel()
	}

	// 等待进程退出（最多 10 秒）
	done := make(chan struct{})
	go func() {
		if m.cmd.Process != nil {
			m.cmd.Wait()
		}
		close(done)
	}()

	select {
	case <-done:
		// 正常退出
	case <-time.After(10 * time.Second):
		// 超时强杀
		if m.cmd.Process != nil {
			log.Printf("[SingBox] sing-box 停止超时，强制终止 (PID=%d)", m.pid)
			m.cmd.Process.Kill()
		}
	}

	m.running = false
	m.cmd = nil
	m.removePID()

	log.Printf("[SingBox] sing-box 已停止")
	return nil
}

// Restart 重启 sing-box 子进程
func (m *Manager) Restart(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 主动重启时重置崩溃计数器，避免之前的崩溃记录影响本次启动
	m.restartCounter = 0

	// 先停止
	if err := m.stopProcess(); err != nil {
		log.Printf("[SingBox] 停止 sing-box 出错: %v", err)
	}

	// 等待端口释放（SIGKILL 后内核回收 socket，800ms 提供安全冗余）
	time.Sleep(800 * time.Millisecond)

	// 重新启动
	m.restartCnt++
	return m.startProcess(ctx)
}

// ReloadConfig 重新生成配置并重启
func (m *Manager) ReloadConfig(ctx context.Context) error {
	// 重新生成 sing-box 配置
	if err := m.config.GenerateAndSave(); err != nil {
		return fmt.Errorf("重新生成配置失败: %w", err)
	}

	// 保存协议缓存
	if err := m.config.SaveToCache(); err != nil {
		log.Printf("[SingBox] 保存协议缓存失败: %v", err)
	}

	// 重启 sing-box
	return m.Restart(ctx)
}

// Status 获取 sing-box 运行状态
func (m *Manager) Status() ProcessStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return ProcessStatus{
		Running:    m.running,
		PID:        m.pid,
		StartedAt:  m.startedAt,
		RestartCnt: m.restartCnt,
		LastError:  m.lastError,
	}
}

// IsRunning 检查 sing-box 是否在运行
func (m *Manager) IsRunning() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.running
}

// --- 内部方法 ---

// watchProcess 监控 sing-box 子进程，异常退出时自动重启
func (m *Manager) watchProcess(ctx context.Context, cmd *exec.Cmd, logWriter io.Writer) {
	// 等待进程退出
	err := cmd.Wait()

	// 关闭日志文件
	if closer, ok := logWriter.(io.Closer); ok {
		closer.Close()
	}

	m.mu.Lock()
	m.running = false
	m.removePID()

	if err != nil {
		m.lastError = err.Error()
		log.Printf("[SingBox] sing-box 异常退出 (PID=%d): %v", m.pid, err)
	} else {
		log.Printf("[SingBox] sing-box 正常退出 (PID=%d)", m.pid)
	}

	// 检查是否需要自动重启
	if ctx.Err() != nil {
		// 上下文已取消（用户主动停止），不重启
		m.mu.Unlock()
		return
	}

	// 检查连续崩溃次数
	m.restartCounter++
	if m.restartCounter > m.maxRestarts {
		log.Printf("[SingBox] 连续崩溃次数已达上限 (%d)，停止自动重启", m.maxRestarts)
		m.mu.Unlock()
		return
	}

	log.Printf("[SingBox] %v 后自动重启 (第 %d 次)...", m.restartDelay, m.restartCounter)
	m.mu.Unlock()

	// 等待一段时间后重启
	select {
	case <-ctx.Done():
		return
	case <-time.After(m.restartDelay):
	}

	m.mu.Lock()
	if err := m.startProcess(ctx); err != nil {
		log.Printf("[SingBox] 自动重启失败: %v", err)
		m.lastError = fmt.Sprintf("自动重启失败: %v", err)
	} else {
		m.restartCnt++
		// 启动成功，10 秒后若仍在运行则清零崩溃计数
		go func() {
			time.Sleep(10 * time.Second)
			m.mu.Lock()
			if m.running {
				m.restartCounter = 0
			}
			m.mu.Unlock()
		}()
	}
	m.mu.Unlock()
}

// openLogWriter 打开日志文件
func (m *Manager) openLogWriter() (io.Writer, error) {
	if dir := filepath.Dir(m.logPath); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}

	f, err := os.OpenFile(m.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}

	return f, nil
}

// savePID 保存子进程 PID 到文件
func (m *Manager) savePID() {
	if m.pid <= 0 {
		return
	}
	if dir := filepath.Dir(m.pidPath); dir != "" {
		os.MkdirAll(dir, 0755)
	}
	os.WriteFile(m.pidPath, []byte(strconv.Itoa(m.pid)), 0644)
}

// removePID 删除 PID 文件
func (m *Manager) removePID() {
	os.Remove(m.pidPath)
}

// Shutdown 优雅关闭（供 Runtime.shutdown() 调用）
func (m *Manager) Shutdown() {
	if err := m.Stop(); err != nil {
		log.Printf("[SingBox] 关闭 sing-box 失败: %v", err)
	}
}

// ForceKill 立即强制杀死 sing-box 进程（SIGKILL）
// 不走优雅退出流程，用于用户执行协议变更操作时第一时间释放端口。
// 同时扫描 agent 子进程树，确保不存在残留的 sing-box 进程。
// kill 死 singbox 不会有任何副作用（无状态进程）。
func (m *Manager) ForceKill() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 重置崩溃计数器，避免 watchProcess 触发自动重启
	m.restartCounter = m.maxRestarts + 1

	// 1. 如果 Manager 正在管理一个进程，直接 SIGKILL
	if m.running && m.cmd != nil && m.cmd.Process != nil {
		pid := m.pid
		log.Printf("[SingBox] ForceKill: 正在强制终止 sing-box (PID=%d)...", pid)

		// 取消 context，防止 watchProcess 尝试自动重启
		if m.cancel != nil {
			m.cancel()
		}

		// 直接 SIGKILL，不等待优雅退出
		m.cmd.Process.Kill()

		// 短暂等待进程退出，回收资源
		done := make(chan struct{})
		go func() {
			m.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			// 2 秒内未退出也无妨，SIGKILL 一定会生效
		}

		m.running = false
		m.cmd = nil
		m.removePID()
		log.Printf("[SingBox] ForceKill: sing-box (PID=%d) 已被强制终止", pid)
	}

	// 2. 扫描 agent 子进程树，确保没有残留的 sing-box 进程
	// （防御场景：Manager 状态与实际进程不一致、遗留进程等）
	killed := killSingboxChildren()
	if killed > 0 {
		log.Printf("[SingBox] ForceKill: 额外清理了 %d 个残留 sing-box 子进程", killed)
	}

	// 重置崩溃计数器（ForceKill 后会重新启动，不应受之前崩溃影响）
	m.restartCounter = 0

	// 等待端口释放（内核回收 socket fd、UDP 端口等，800ms 提供安全冗余）
	time.Sleep(800 * time.Millisecond)
}

// killSingboxChildren 扫描当前 agent 进程的子进程树，找到所有名为 sing-box 的子进程并 SIGKILL。
// 确保 agent 子进程中 sing-box 进程永远只有 0 个（在 ForceKill 后）或 1 个（运行中）。
// 不会误杀 cloudflared 等其他子进程，因为严格匹配进程名。
// 返回实际杀死的进程数量。
func killSingboxChildren() int {
	myPID := os.Getpid()
	killed := 0

	// 读取 /proc 目录，遍历所有进程
	entries, err := os.ReadDir("/proc")
	if err != nil {
		log.Printf("[SingBox] killSingboxChildren: 无法读取 /proc: %v", err)
		return 0
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}

		// 检查是否是当前 agent 的子进程（通过 /proc/<pid>/stat 中的 ppid）
		statData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}

		// /proc/<pid>/stat 格式: <pid> (<comm>) <state> <ppid> ...
		// 找到最后一个 ')' 的位置，其后的字段以空格分隔
		statStr := string(statData)
		closeParen := strings.LastIndex(statStr, ")")
		if closeParen < 0 || closeParen+2 >= len(statStr) {
			continue
		}
		fields := strings.Fields(statStr[closeParen+2:])
		if len(fields) < 2 {
			continue
		}
		// fields[0] = state, fields[1] = ppid
		ppid, err := strconv.Atoi(fields[1])
		if err != nil || ppid != myPID {
			continue // 不是 agent 的子进程
		}

		// 提取进程名（在括号之间的部分）
		openParen := strings.Index(statStr, "(")
		if openParen < 0 || openParen >= closeParen {
			continue
		}
		comm := statStr[openParen+1 : closeParen]

		// 严格匹配进程名为 "sing-box"
		if comm != "sing-box" {
			continue
		}

		// 找到了 agent 子进程中的 sing-box，直接 SIGKILL
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		log.Printf("[SingBox] killSingboxChildren: 发现残留 sing-box 子进程 (PID=%d)，正在 SIGKILL...", pid)
		proc.Signal(syscall.SIGKILL)
		killed++
	}

	return killed
}

// KillOrphanProcess 检测并终止遗留的 sing-box 子进程
// 场景：Agent 通过 execve 就地更新后，旧 sing-box 子进程仍在运行（PID 不变但 Agent 内存状态全部重置）。
// 新 Agent 不知道旧 sing-box 的存在，直接启动新的 sing-box 会导致端口冲突，进而断网。
//
// 检测策略（双重保险）：
//  1. 通过 PID 文件查找遗留进程 → 验证 cmdline 包含 "sing-box" → SIGKILL
//  2. 通过 killSingboxChildren() 扫描 agent 子进程树做兜底（防止 PID 文件丢失/损坏）
//  3. 等待端口释放
//
// 使用 SIGKILL 而非 SIGTERM，因为：
// - sing-box 是无状态进程，SIGKILL 没有任何副作用
// - 旧版使用 SIGTERM + 10 秒超时太慢，延迟了 Agent 启动
//
// 返回 true 表示确实终止了一个或多个遗留进程
func (m *Manager) KillOrphanProcess() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 如果当前 Manager 已经在管理一个进程，无需处理
	if m.running && m.cmd != nil {
		return false
	}

	killedAny := false

	// 策略 1：通过 PID 文件查找遗留进程
	pidData, err := os.ReadFile(m.pidPath)
	if err == nil {
		pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
		if err == nil && pid > 0 {
			// 检查进程是否存活
			proc, err := os.FindProcess(pid)
			if err == nil {
				if err := proc.Signal(syscall.Signal(0)); err == nil {
					// 进程存活，验证是否确实是 sing-box
					cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
					if err == nil && strings.Contains(string(cmdline), "sing-box") {
						// 确认是遗留 sing-box 进程，直接 SIGKILL（不走优雅退出）
						log.Printf("[SingBox] 检测到遗留 sing-box 进程 (PID=%d)，直接 SIGKILL...", pid)
						proc.Signal(syscall.SIGKILL)
						killedAny = true
					} else if err == nil {
						log.Printf("[SingBox] PID %d 不是 sing-box 进程 (cmdline: %q)，清理 PID 文件", pid, string(cmdline))
					}
				}
			}
		}
	}

	// 清理 PID 文件
	m.removePID()

	// 策略 2：扫描 agent 子进程树做兜底（防止 PID 文件丢失/损坏的场景）
	extraKilled := killSingboxChildren()
	if extraKilled > 0 {
		log.Printf("[SingBox] 通过子进程扫描额外清理了 %d 个遗留 sing-box 进程", extraKilled)
		killedAny = true
	}

	if killedAny {
		// 等待端口释放（SIGKILL 后内核回收 socket，800ms 提供安全冗余）
		time.Sleep(800 * time.Millisecond)
		log.Printf("[SingBox] 遗留 sing-box 进程已全部终止")
	}

	return killedAny
}

// PortConflict 端口冲突信息
type PortConflict struct {
	Protocol string // 协议名称（如 "ss", "hy2"）
	Port     int    // 冲突端口
	Network  string // "tcp" / "udp"
	Reason   string // 冲突原因描述
}

// CheckPortConflicts 检测协议配置中的端口冲突问题
// 返回两类冲突：
// 1. 协议之间的端口重复（同一端口被多个协议使用）
// 2. 端口被系统其他进程占用（排除当前 sing-box 自身占用的端口）
//
// excludePorts: 需要排除的端口集合（当前 sing-box 已在使用的端口），
// 这些端口虽然"被占用"，但属于 sing-box 自身进程，不应视为冲突。
// 传 nil 表示不排除任何端口（首次启动时使用）。
func CheckPortConflicts(pc *ProtocolConfig, excludePorts map[string]bool) []PortConflict {
	var conflicts []PortConflict

	// 1. 收集所有启用协议的端口
	type protoPort struct {
		protocol string
		port     int
		networks []string // tcp, udp, or both
	}

	var protoPorts []protoPort
	enabledList := pc.EnabledProtocolList()

	for _, proto := range enabledList {
		port := getProtoPort(pc, proto)
		if port <= 0 {
			continue
		}
		nets := getProtoNetworks(proto)
		protoPorts = append(protoPorts, protoPort{protocol: proto, port: port, networks: nets})
	}

	// 2. 检测协议之间的端口重复
	portUsageMap := make(map[string][]string) // "tcp:8388" -> ["ss", "socks5"]
	for _, pp := range protoPorts {
		for _, n := range pp.networks {
			key := fmt.Sprintf("%s:%d", n, pp.port)
			portUsageMap[key] = append(portUsageMap[key], pp.protocol)
		}
	}
	for key, protos := range portUsageMap {
		if len(protos) > 1 {
			parts := strings.SplitN(key, ":", 2)
			network := parts[0]
			port, _ := strconv.Atoi(parts[1])
			conflicts = append(conflicts, PortConflict{
				Protocol: strings.Join(protos, ", "),
				Port:     port,
				Network:  network,
				Reason:   fmt.Sprintf("端口 %d/%s 被多个协议共用: [%s]", port, strings.ToUpper(network), strings.Join(protos, ", ")),
			})
		}
	}

	// 3. 检测端口是否被系统其他进程占用
	// 排除当前 sing-box 自身已占用的端口（推送新配置时，sing-box 仍在运行）
	checkedPorts := make(map[string]bool)
	for _, pp := range protoPorts {
		for _, n := range pp.networks {
			key := fmt.Sprintf("%s:%d", n, pp.port)
			if checkedPorts[key] {
				continue
			}
			checkedPorts[key] = true

			// 如果该端口在排除列表中（即当前 sing-box 正在使用的端口），跳过检测
			if excludePorts != nil && excludePorts[key] {
				continue
			}

			if isPortInUse(n, pp.port) {
				conflicts = append(conflicts, PortConflict{
					Protocol: pp.protocol,
					Port:     pp.port,
					Network:  n,
					Reason:   fmt.Sprintf("端口 %d/%s (协议 %s) 已被系统其他进程占用", pp.port, strings.ToUpper(n), pp.protocol),
				})
			}
		}
	}

	return conflicts
}

// CollectCurrentPorts 收集当前协议配置中所有已启用协议的端口集合
// 返回 map[string]bool，key 格式为 "tcp:20021" 或 "udp:8443"
// 用于在推送新配置时，排除当前 sing-box 自身占用的端口
func CollectCurrentPorts(pc *ProtocolConfig) map[string]bool {
	if pc == nil {
		return nil
	}
	ports := make(map[string]bool)
	for _, proto := range pc.EnabledProtocolList() {
		port := getProtoPort(pc, proto)
		if port <= 0 {
			continue
		}
		for _, n := range getProtoNetworks(proto) {
			key := fmt.Sprintf("%s:%d", n, port)
			ports[key] = true
		}
	}
	if len(ports) == 0 {
		return nil
	}
	return ports
}

// getProtoPort 获取协议的端口号
func getProtoPort(pc *ProtocolConfig, proto string) int {
	switch proto {
	case ProtoSS:
		return pc.SS.Port
	case ProtoHY2:
		return pc.HY2.Port
	case ProtoTUIC:
		return pc.TUIC.Port
	case ProtoReality:
		return pc.Reality.Port
	case ProtoSocks5:
		return pc.Socks5.Port
	case ProtoTrojan:
		return pc.Trojan.Port
	case ProtoAnyTLS:
		return pc.AnyTLS.Port
	case ProtoVmessTCP:
		return pc.VMess.TCPPort
	case ProtoVmessWS:
		return pc.VMess.WSPort
	case ProtoVmessHTTP:
		return pc.VMess.HTTPPort
	case ProtoVmessQUIC:
		return pc.VMess.QUICPort
	case ProtoVmessWST:
		return pc.VMess.WSTPort
	case ProtoVmessHUT:
		return pc.VMess.HUTPort
	case ProtoVlessWST:
		return pc.VlessTLS.WSTPort
	case ProtoVlessHUT:
		return pc.VlessTLS.HUTPort
	case ProtoTrojanWST:
		return pc.TrojanTLS.WSTPort
	case ProtoTrojanHUT:
		return pc.TrojanTLS.HUTPort
	default:
		return 0
	}
}

// getProtoNetworks 返回协议使用的网络类型
func getProtoNetworks(proto string) []string {
	switch proto {
	case ProtoHY2, ProtoTUIC, ProtoVmessQUIC:
		return []string{"udp"}
	default:
		return []string{"tcp"}
	}
}

// isPortInUse 检测指定端口是否被占用
func isPortInUse(network string, port int) bool {
	addr := fmt.Sprintf(":%d", port)
	switch network {
	case "tcp":
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return true // 端口被占用
		}
		ln.Close()
		return false
	case "udp":
		conn, err := net.ListenPacket("udp", addr)
		if err != nil {
			return true // 端口被占用
		}
		conn.Close()
		return false
	default:
		return false
	}
}

// StartAndVerify 启动 sing-box 并等待一段时间验证是否存活
// 返回：启动错误 或 验证失败错误
// healthCheckDuration: 启动后等待多久来验证进程存活（推荐 2-3 秒）
func (m *Manager) StartAndVerify(ctx context.Context, healthCheckDuration time.Duration) error {
	if err := m.Start(ctx); err != nil {
		return err
	}

	// 等待一段时间，检测 sing-box 是否快速退出（如端口冲突、配置错误等）
	time.Sleep(healthCheckDuration)

	m.mu.RLock()
	running := m.running
	lastErr := m.lastError
	m.mu.RUnlock()

	if !running {
		errMsg := "sing-box 启动后立即退出"
		if lastErr != "" {
			errMsg = fmt.Sprintf("sing-box 启动后立即退出: %s", lastErr)
		}
		return errors.New(errMsg)
	}

	return nil
}

// FormatPortConflictsMessage 将端口冲突列表格式化为人类可读的消息
func FormatPortConflictsMessage(conflicts []PortConflict) string {
	if len(conflicts) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("⚠️ 检测到 %d 个端口冲突问题:\n", len(conflicts)))
	for i, c := range conflicts {
		sb.WriteString(fmt.Sprintf("  %d. %s\n", i+1, c.Reason))
	}
	return sb.String()
}
