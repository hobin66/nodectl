package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"nodectl/internal/database"
	"nodectl/internal/logger"
	"nodectl/internal/service"

	"github.com/shirou/gopsutil/v4/process"
)

// ------------------- [系统运维 API] -------------------

// apiUpdateGeoIP 触发更新 GeoIP 数据库
func apiUpdateGeoIP(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	if r.Method != http.MethodPost {
		logger.Log.Warn("非法请求方法", "method", r.Method, "ip", clientIP, "path", reqPath)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	logger.Log.Info("接收到触发更新 GeoIP 数据库的请求", "ip", clientIP, "path", reqPath)
	go func() {
		logger.Log.Info("后台线程开始更新 GeoIP 数据库...")
		if err := service.GlobalGeoIP.ForceUpdate(); err != nil {
			logger.Log.Error("GeoIP 数据库更新失败", "error", err)
		} else {
			logger.Log.Info("GeoIP 数据库更新流程圆满完成")
		}
	}()

	sendJSON(w, "success", "更新任务已在后台启动，请留意日志或稍后刷新")
}

func apiGetGeoStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	localVersion := service.GlobalGeoIP.GetLocalVersion()
	remoteVersion, errRemote := service.GlobalGeoIP.GetRemoteVersion()

	status := "unknown"
	msg := ""

	// 字符串精确比对逻辑
	if localVersion == "" {
		status = "not_found" // 数据库没有记录，视为未下载
	} else if errRemote == nil && remoteVersion != "" && remoteVersion != localVersion {
		status = "update_available" // 版本字符串不一致，提示更新
	} else if errRemote == nil && remoteVersion == localVersion {
		status = "latest" // 完全一致，已是最新
	} else {
		status = "check_failed"
	}

	resp := map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"local_version":  localVersion,
			"remote_version": remoteVersion,
			"state":          status,
			"error":          msg,
		},
	}

	if errRemote != nil {
		resp["data"].(map[string]interface{})["remote_error"] = errRemote.Error()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// apiGetSystemMonitor 获取系统运行状态与硬件监控数据
func apiGetSystemMonitor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// 读取 Go 底层内存状态
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	// 计算运行时长
	uptime := time.Since(AppStartTime)
	days := int(uptime.Hours() / 24)
	hours := int(uptime.Hours()) % 24
	minutes := int(uptime.Minutes()) % 60
	seconds := int(uptime.Seconds()) % 60

	uptimeStr := ""
	if days > 0 {
		uptimeStr += fmt.Sprintf("%d天 ", days)
	}
	uptimeStr += fmt.Sprintf("%d时%d分%d秒", hours, minutes, seconds)

	// 组装监控数据
	procStats := collectNodectlProcessStats()
	data := map[string]interface{}{
		"os_arch":       fmt.Sprintf("%s / %s", runtime.GOOS, runtime.GOARCH), // 系统和架构
		"go_version":    runtime.Version(),                                    // Go版本
		"num_cpu":       runtime.NumCPU(),                                     // 逻辑CPU核心数
		"go_max_procs":  runtime.GOMAXPROCS(0),                                // 使用的线程数
		"num_goroutine": runtime.NumGoroutine(),                               // 当前协程数量
		"num_cgo_call":  runtime.NumCgoCall(),                                 // CGO调用次数
		"start_time":    AppStartTime.Format("2006/01/02 15:04:05"),           // 启动时间
		"uptime":        uptimeStr,                                            // 运行时长
		// 内存相关 (单位均为 Bytes，前端拿到后再转换为 MB/GB)
		"heap_alloc":  m.HeapAlloc,  // 当前分配的堆内存
		"heap_sys":    m.HeapSys,    // 向系统申请的堆内存
		"heap_inuse":  m.HeapInuse,  // 正在使用的堆内存
		"sys_mem":     m.Sys,        // 向系统申请的总内存
		"total_alloc": m.TotalAlloc, // 累计分配的内存(包含已释放的)
		"stack_inuse": m.StackInuse, // 栈内存使用量
		// GC 垃圾回收状态
		"num_gc":          m.NumGC,                       // 垃圾回收次数
		"pause_total_ms":  float64(m.PauseTotalNs) / 1e6, // GC总暂停时间(毫秒)
		"gc_cpu_fraction": m.GCCPUFraction,               // GC占用CPU的时间比例
		// NodeCTL 进程树（含子进程）监控
		"process_tree_pid":               procStats.RootPID,
		"process_tree_count":             procStats.ProcessCount,
		"process_tree_children_count":    procStats.ChildrenCount,
		"process_tree_total_rss":         procStats.TotalRSS,
		"process_tree_total_vms":         procStats.TotalVMS,
		"process_tree_total_cpu_percent": procStats.TotalCPUPercent,
		"process_tree_collect_error":     procStats.Error,
		"process_tree_items":             procStats.Items,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data":   data,
	})
}

type monitorProcessTreeStats struct {
	RootPID         int
	ProcessCount    int
	ChildrenCount   int
	TotalRSS        uint64
	TotalVMS        uint64
	TotalCPUPercent float64
	Items           []map[string]interface{}
	Error           string
}

func collectNodectlProcessStats() monitorProcessTreeStats {
	stats := monitorProcessTreeStats{Items: make([]map[string]interface{}, 0)}
	rootPID := int32(os.Getpid())
	stats.RootPID = int(rootPID)

	rootProc, err := process.NewProcess(rootPID)
	if err != nil {
		stats.Error = err.Error()
		return stats
	}

	procs := flattenProcessTree(rootProc)
	if len(procs) == 0 {
		procs = []*process.Process{rootProc}
	}

	now := time.Now()
	for _, p := range procs {
		pid := p.Pid
		name, _ := p.Name()
		exe, _ := p.Exe()
		cmdline, _ := p.Cmdline()
		ppid, _ := p.Ppid()
		threads, _ := p.NumThreads()
		cpuPercent, _ := p.CPUPercent()
		if cpuPercent < 0 {
			cpuPercent = 0
		}

		memInfo, _ := p.MemoryInfo()
		var rss uint64
		var vms uint64
		if memInfo != nil {
			rss = memInfo.RSS
			vms = memInfo.VMS
		}

		createMS, _ := p.CreateTime()
		startTime := ""
		uptimeSeconds := int64(0)
		if createMS > 0 {
			startedAt := time.UnixMilli(createMS)
			startTime = startedAt.Format("2006/01/02 15:04:05")
			uptimeSeconds = int64(now.Sub(startedAt).Seconds())
			if uptimeSeconds < 0 {
				uptimeSeconds = 0
			}
		}

		role := classifyChildProcessRole(pid, rootPID, name, exe, cmdline)

		stats.TotalRSS += rss
		stats.TotalVMS += vms
		stats.TotalCPUPercent += cpuPercent

		item := map[string]interface{}{
			"pid":            pid,
			"ppid":           ppid,
			"name":           name,
			"exe":            exe,
			"cmdline":        cmdline,
			"threads":        threads,
			"cpu_percent":    cpuPercent,
			"rss":            rss,
			"vms":            vms,
			"start_time":     startTime,
			"uptime_seconds": uptimeSeconds,
			"is_root":        pid == rootPID,
			"role":           role,
		}
		stats.Items = append(stats.Items, item)
	}

	stats.ProcessCount = len(stats.Items)
	if stats.ProcessCount > 0 {
		stats.ChildrenCount = stats.ProcessCount - 1
	}

	sort.Slice(stats.Items, func(i, j int) bool {
		rssi, _ := stats.Items[i]["rss"].(uint64)
		rssj, _ := stats.Items[j]["rss"].(uint64)
		if rssi == rssj {
			pi, _ := stats.Items[i]["pid"].(int32)
			pj, _ := stats.Items[j]["pid"].(int32)
			return pi < pj
		}
		return rssi > rssj
	})

	return stats
}

func flattenProcessTree(root *process.Process) []*process.Process {
	out := make([]*process.Process, 0, 8)
	if root == nil {
		return out
	}

	queue := []*process.Process{root}
	visited := map[int32]bool{}

	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if p == nil {
			continue
		}
		if visited[p.Pid] {
			continue
		}
		visited[p.Pid] = true
		out = append(out, p)

		children, err := p.Children()
		if err != nil {
			continue
		}
		for _, child := range children {
			if child == nil || visited[child.Pid] {
				continue
			}
			queue = append(queue, child)
		}
	}

	return out
}

func classifyChildProcessRole(pid int32, rootPID int32, name, exe, cmdline string) string {
	if pid == rootPID {
		return "nodectl"
	}
	merged := strings.ToLower(name + " " + exe + " " + cmdline)
	if strings.Contains(merged, "cloudflared") {
		return "cloudflare"
	}
	if strings.Contains(merged, "mihomo") {
		return "mihomo"
	}
	return "child"
}

// apiApplyCert 处理 Cloudflare 自动申请证书请求
func apiApplyCert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email  string `json:"email"`
		ApiKey string `json:"api_key"`
		Domain string `json:"domain"`
	}
	// 解析 JSON body
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, "error", "参数解析失败")
		return
	}

	if req.ApiKey == "********" {
		var keyConf database.SysConfig
		database.DB.Where("key = ?", "cf_api_key").First(&keyConf)
		req.ApiKey = keyConf.Value
	}

	if req.Email == "" || req.ApiKey == "" || req.Domain == "" {
		sendJSON(w, "error", "请填写完整的 Cloudflare 信息")
		return
	}

	// 调用 service 层的申请逻辑
	if err := service.ApplyCloudflareCert(req.Email, req.ApiKey, req.Domain); err != nil {
		logger.Log.Error("证书申请失败", "error", err)
		sendJSON(w, "error", "申请失败: "+err.Error())
		return
	}

	sendJSON(w, "success", "证书申请任务已提交")
}

// apiRestartCore 处理前端下发的重启核心请求
// 功能：返回成功响应后，异步触发系统的热重启逻辑
func apiRestartCore(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	if r.Method != http.MethodPost {
		logger.Log.Warn("非法请求方法", "method", r.Method, "ip", clientIP, "path", reqPath)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	logger.Log.Info("接收到面板核心重启请求", "ip", clientIP)

	// 必须先给前端返回成功的 JSON，否则直接重启会导致前端请求 Pending 并报错
	sendJSON(w, "success", "系统核心即将重启，面板可能会短暂断开连接...")

	// 延迟 1 秒后触发重启，确保 HTTP 响应已经发送给前端
	go func() {
		time.Sleep(1 * time.Second)
		TriggerRestart() // 调用 server.go 中的重启触发器
	}()
}

// apiGetCertLogs 获取证书申请的实时日志供前端黑框展示
func apiGetCertLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	sendJSON(w, "success", service.GetCertLogs())
}

// apiGetRecentLogs 获取最近系统日志（含中文解读）
func apiGetRecentLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	limit := 120
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}

	logs, err := service.GetRecentLogs(limit)
	if err != nil {
		logger.Log.Error("读取最近日志失败", "error", err)
		sendJSON(w, "error", "读取日志失败，请检查日志文件是否存在")
		return
	}

	sendJSON(w, "success", map[string]interface{}{
		"logs": logs,
	})
}

// apiStreamRecentLogs 通过 SSE 持续推送最近日志（含中文解读）
func apiStreamRecentLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming Unsupported", http.StatusInternalServerError)
		return
	}

	limit := 120
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx := r.Context()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	lastFingerprint := ""
	sendLogs := func(force bool) {
		logs, err := service.GetRecentLogs(limit)
		if err != nil {
			payload, _ := json.Marshal(map[string]interface{}{
				"status":  "error",
				"message": "读取日志失败，请检查日志文件是否存在",
			})
			fmt.Fprintf(w, "event: logs\ndata: %s\n\n", payload)
			flusher.Flush()
			return
		}

		fingerprint := buildRecentLogFingerprint(logs)
		if !force && fingerprint == lastFingerprint {
			return
		}

		payload, _ := json.Marshal(map[string]interface{}{
			"status": "success",
			"logs":   logs,
		})
		fmt.Fprintf(w, "event: logs\ndata: %s\n\n", payload)
		flusher.Flush()
		lastFingerprint = fingerprint
	}

	sendLogs(true)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendLogs(false)
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func buildRecentLogFingerprint(logs []service.RecentLogEntry) string {
	if len(logs) == 0 {
		return "empty"
	}
	head := logs[0]
	return fmt.Sprintf("%d|%s|%s|%s", len(logs), head.Time, head.Level, head.Raw)
}

// apiCheckUpdate 检查程序是否有新版本可用（带后端缓存，避免频繁请求 GitHub）
func apiCheckUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// force=true 时强制刷新缓存
	forceRefresh := strings.TrimSpace(r.URL.Query().Get("force")) == "true"

	result := service.CheckForUpdate(forceRefresh)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data":   result,
	})
}
