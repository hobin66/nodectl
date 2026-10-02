package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"nodectl/internal/database"
	"nodectl/internal/logger"
	"nodectl/internal/service"
)

// ------------------- [机场订阅相关 API] -------------------

type airportSpeedJob struct {
	HistoryID string
	TaskKey   string
	SubID     string
	Mode      string
	Cancel    context.CancelFunc
	Total     int
	mu        sync.RWMutex
	Results   map[string]map[string]service.SpeedTestResult
	Done      int
	Errors    int
}

var (
	airportSpeedJobsMu    sync.Mutex
	airportSpeedJobsBySub = make(map[string]*airportSpeedJob)
	airportSpeedJobsByID  = make(map[string]*airportSpeedJob)
)

// apiAirportList 获取订阅源列表
func apiAirportList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var subs []database.AirportSub
	if err := database.DB.Order("updated_at DESC").Find(&subs).Error; err != nil {
		logger.Log.Error("获取机场订阅列表失败", "error", err)
		sendJSON(w, "error", "获取列表失败")
		return
	}

	sendJSON(w, "success", subs)
}

// apiAirportAdd 添加新订阅
func apiAirportAdd(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	if r.Method != http.MethodPost {
		logger.Log.Warn("非法请求方法", "method", r.Method, "ip", clientIP, "path", reqPath)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Log.Warn("添加机场订阅失败: JSON 解析异常", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "格式错误")
		return
	}

	if req.URL == "" {
		logger.Log.Warn("添加机场订阅失败: 缺少订阅链接", "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "订阅链接不能为空")
		return
	}

	if req.Name == "" {
		fetchedName := service.FetchSubscriptionName(req.URL)
		if fetchedName != "" {
			req.Name = fetchedName
		} else {
			req.Name = "未命名订阅 " + time.Now().Format("01-02 15:04")
		}
	}

	sub := database.AirportSub{
		Name: req.Name,
		URL:  req.URL,
	}

	if err := database.DB.Create(&sub).Error; err != nil {
		logger.Log.Error("添加机场订阅失败", "error", err, "name", req.Name, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "数据库写入失败")
		return
	}

	// 自动触发一次同步
	syncStatus := "成功"
	if err := service.SyncAirportSubscription(sub.ID); err != nil {
		syncStatus = "失败"
		logger.Log.Error("添加后自动同步机场订阅失败", "id", sub.ID, "name", sub.Name, "error", err, "ip", clientIP, "path", reqPath)
	}

	logger.Log.Info("机场订阅已添加",
		"id", sub.ID,
		"name", sub.Name,
		"changes", fmt.Sprintf("新增订阅 %s | 自动同步 %s", sub.Name, syncStatus),
		"ip", clientIP,
		"path", reqPath,
	)

	sendJSON(w, "success", map[string]interface{}{
		"message": "添加成功",
		"id":      sub.ID,
	})
}

// apiAirportUpdate 手动更新订阅
func apiAirportUpdate(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	if r.Method != http.MethodPost {
		logger.Log.Warn("非法请求方法", "method", r.Method, "ip", clientIP, "path", reqPath)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Log.Warn("更新机场订阅失败: JSON 解析异常", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "格式错误")
		return
	}
	if req.ID == "" {
		logger.Log.Warn("更新机场订阅失败: 缺少订阅ID", "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "缺少订阅ID")
		return
	}

	var sub database.AirportSub
	if err := database.DB.Where("id = ?", req.ID).First(&sub).Error; err != nil {
		logger.Log.Warn("更新机场订阅失败: 订阅不存在", "id", req.ID, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "订阅不存在")
		return
	}

	logger.Log.Info("手动触发机场订阅同步", "id", sub.ID, "name", sub.Name, "changes", "手动同步 "+sub.Name, "ip", clientIP, "path", reqPath)

	// 调用 service 层逻辑进行同步 (包含保留状态逻辑)
	if err := service.SyncAirportSubscription(req.ID); err != nil {
		logger.Log.Error("更新订阅失败", "id", sub.ID, "name", sub.Name, "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "更新失败: "+err.Error())
		return
	}

	logger.Log.Info("机场订阅同步成功", "id", sub.ID, "name", sub.Name, "changes", "同步订阅 "+sub.Name, "ip", clientIP, "path", reqPath)

	sendJSON(w, "success", "订阅已更新")
}

// apiAirportDelete 删除订阅
func apiAirportDelete(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	if r.Method != http.MethodPost {
		logger.Log.Warn("非法请求方法", "method", r.Method, "ip", clientIP, "path", reqPath)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Log.Warn("删除机场订阅失败: JSON 解析异常", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "格式错误")
		return
	}

	var sub database.AirportSub
	if err := database.DB.Where("id = ?", req.ID).First(&sub).Error; err != nil {
		logger.Log.Warn("删除机场订阅失败: 订阅不存在", "id", req.ID, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "订阅不存在")
		return
	}
	var nodeCount int64
	database.DB.Model(&database.AirportNode{}).Where("sub_id = ?", req.ID).Count(&nodeCount)

	if err := service.DeleteAirportSubscription(req.ID); err != nil {
		logger.Log.Error("删除机场订阅失败", "id", req.ID, "name", sub.Name, "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "删除失败: "+err.Error())
		return
	}

	logger.Log.Info("机场订阅已删除",
		"id", sub.ID,
		"name", sub.Name,
		"changes", fmt.Sprintf("删除订阅 %s | 清理节点 %d 个", sub.Name, nodeCount),
		"ip", clientIP,
		"path", reqPath,
	)

	sendJSON(w, "success", "删除成功")
}

// apiAirportNodes 获取指定订阅的节点列表
func apiAirportNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	subID := r.URL.Query().Get("id")
	if subID == "" {
		sendJSON(w, "error", "缺少订阅ID")
		return
	}

	var nodes []database.AirportNode
	// 按启用状态(倒序, 启用在前) -> 原始索引(正序) 排序
	if err := database.DB.Where("sub_id = ?", subID).
		Order("routing_type DESC, original_index ASC").
		Find(&nodes).Error; err != nil {
		sendJSON(w, "error", "获取节点失败")
		return
	}

	sendJSON(w, "success", map[string]interface{}{
		"nodes": nodes,
	})
}

// apiAirportNodeRouting 修改单个节点的路由策略 (三态切换)
func apiAirportNodeRouting(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	if r.Method != http.MethodPost {
		logger.Log.Warn("非法请求方法", "method", r.Method, "ip", clientIP, "path", reqPath)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID          string `json:"id"`
		RoutingType int    `json:"routing_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Log.Warn("修改机场节点状态失败: JSON 解析异常", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "格式错误")
		return
	}

	var oldNode database.AirportNode
	if err := database.DB.Where("id = ?", req.ID).First(&oldNode).Error; err != nil {
		logger.Log.Warn("修改机场节点状态失败: 节点不存在", "id", req.ID, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "节点不存在")
		return
	}

	// 0=禁用，1=中转，2=落地
	if err := database.DB.Model(&database.AirportNode{}).
		Where("id = ?", req.ID).
		Update("routing_type", req.RoutingType).Error; err != nil {

		logger.Log.Error("修改机场节点状态失败", "id", req.ID, "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "数据库更新失败")
		return
	}

	if oldNode.RoutingType != req.RoutingType {
		logger.Log.Info("机场订阅节点状态已更新",
			"id", oldNode.ID,
			"sub_id", oldNode.SubID,
			"name", oldNode.Name,
			"changes", fmt.Sprintf("节点 %s 状态 %s -> %s", oldNode.Name, airportRoutingTypeLabel(oldNode.RoutingType), airportRoutingTypeLabel(req.RoutingType)),
			"ip", clientIP,
			"path", reqPath,
		)
	}

	sendJSON(w, "success", "状态已更新")
}

// apiAirportEdit 编辑订阅信息 (名称和URL)
func apiAirportEdit(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	if r.Method != http.MethodPost {
		logger.Log.Warn("非法请求方法", "method", r.Method, "ip", clientIP, "path", reqPath)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Log.Warn("编辑机场订阅失败: JSON 解析异常", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "格式错误")
		return
	}

	if req.ID == "" {
		logger.Log.Warn("编辑机场订阅失败: 缺少订阅ID", "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "订阅ID不能为空")
		return
	}

	var oldSub database.AirportSub
	if err := database.DB.Where("id = ?", req.ID).First(&oldSub).Error; err != nil {
		logger.Log.Warn("编辑机场订阅失败: 订阅不存在", "id", req.ID, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "订阅不存在")
		return
	}

	// 准备更新的数据
	updates := make(map[string]interface{})
	if req.Name != "" {
		updates["name"] = req.Name
	}
	if req.URL != "" {
		updates["url"] = req.URL
	}

	if len(updates) == 0 {
		sendJSON(w, "error", "没有检测到变更内容")
		return
	}

	// 名称和链接编辑不是成功同步，不改变 UpdatedAt。
	if err := database.DB.Model(&database.AirportSub{}).Where("id = ?", req.ID).UpdateColumns(updates).Error; err != nil {
		logger.Log.Error("编辑机场订阅失败", "id", req.ID, "name", oldSub.Name, "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "数据库更新失败")
		return
	}

	changed := make([]string, 0, 2)
	if req.Name != "" && req.Name != oldSub.Name {
		changed = append(changed, fmt.Sprintf("订阅名称 %s -> %s", oldSub.Name, req.Name))
	}
	if req.URL != "" && req.URL != oldSub.URL {
		changed = append(changed, "订阅链接已更新")
	}
	if len(changed) == 0 {
		changed = append(changed, "提交编辑但无有效变化")
	}

	logger.Log.Info("机场订阅信息已修改",
		"id", req.ID,
		"name", oldSub.Name,
		"changes", strings.Join(changed, " | "),
		"ip", clientIP,
		"path", reqPath,
	)
	sendJSON(w, "success", "修改成功")
}

// ------------------- [Mihomo 核心管理] -------------------

// apiUpdateMihomo 触发 Mihomo 核心更新/下载
func apiUpdateMihomo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	logger.Log.Info("后台线程开始更新 Mihomo 核心...")
	go func() {
		if err := service.GlobalMihomo.ForceUpdate(); err != nil {
			logger.Log.Error("Mihomo 核心更新失败", "error", err)
		}
	}()

	sendJSON(w, "success", "更新任务已在后台启动，请稍后刷新查看状态")
}

// apiGetMihomoStatus 获取 Mihomo 核心状态
func apiGetMihomoStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	isReady := service.GlobalMihomo.IsCoreReady()
	localVersion := service.GlobalMihomo.GetLocalVersion()
	remoteVersion, _, _, errRemote := service.GlobalMihomo.GetRemoteVersion()

	status := "unknown"
	if !isReady {
		status = "not_found"
	} else if localVersion == "" {
		// 文件存在但版本号未记录，视为已安装但版本未知
		status = "installed"
	} else if errRemote == nil && remoteVersion != "" && remoteVersion != localVersion {
		status = "update_available"
	} else if errRemote == nil && remoteVersion == localVersion {
		status = "latest"
	} else {
		status = "check_failed"
	}

	resp := map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"local_version":  localVersion,
			"remote_version": remoteVersion,
			"state":          status,
		},
	}

	if errRemote != nil {
		resp["data"].(map[string]interface{})["remote_error"] = errRemote.Error()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ------------------- [机场节点测速 API] -------------------

// apiTestAirportNodes 流式处理节点测速请求 (Server-Sent Events)
func apiTestAirportNodes(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	// 1. 设置 SSE 必需的响应头
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	subID := r.URL.Query().Get("sub_id")
	nodeID := r.URL.Query().Get("node_id")
	mode := service.NormalizeSpeedTestMode(r.URL.Query().Get("mode"))
	subName := "全部订阅"

	var nodes []database.AirportNode
	if subID != "" {
		var sub database.AirportSub
		if err := database.DB.Where("id = ?", subID).First(&sub).Error; err == nil {
			subName = sub.Name
		} else {
			subName = subID
		}
		database.DB.Where("sub_id = ?", subID).Find(&nodes)
	} else if nodeID != "" {
		database.DB.Where("id = ?", nodeID).Find(&nodes)
		if len(nodes) > 0 {
			var sub database.AirportSub
			if err := database.DB.Where("id = ?", nodes[0].SubID).First(&sub).Error; err == nil {
				subName = sub.Name
			} else {
				subName = nodes[0].SubID
			}
		}
	}

	if len(nodes) == 0 {
		logger.Log.Warn("机场节点测速失败: 未找到可测速节点", "sub_id", subID, "node_id", nodeID, "ip", clientIP, "path", reqPath)
		fmt.Fprintf(w, "data: %s\n\n", `{"node_id": "all", "type": "error", "text": "未找到需要测试的节点"}`)
		flusher.Flush()
		return
	}

	if !service.GlobalMihomo.IsCoreReady() {
		logger.Log.Warn("机场节点测速失败: Mihomo 核心未就绪", "sub_id", subID, "node_id", nodeID, "ip", clientIP, "path", reqPath)
		fmt.Fprintf(w, "data: %s\n\n", `{"node_id": "all", "type": "error", "text": "请先在设置中下载 Mihomo 核心"}`)
		flusher.Flush()
		return
	}

	scope := "全部节点"
	if nodeID != "" {
		scope = "单节点"
	} else if subID != "" {
		scope = "订阅全部节点"
	}
	logger.Log.Info("机场节点测速开始",
		"sub_id", subID,
		"sub_name", subName,
		"node_id", nodeID,
		"mode", mode,
		"node_count", len(nodes),
		"changes", fmt.Sprintf("订阅名称 %s | 测速范围 %s | 节点数 %d", subName, scope, len(nodes)),
		"ip", clientIP,
		"path", reqPath,
	)

	// 2. 利用 r.Context() 感知客户端断开连接
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	resultChan := make(chan service.SpeedTestResult)

	// 3. 异步启动测速任务
	go service.GlobalMihomo.RunBatchTestWithMode(ctx, nodes, resultChan, mode)

	// 4. 死循环监听管道，来一个结果发一个给前端 (流式推送)
	resultCount := 0
	errorCount := 0
	for res := range resultChan {
		resultCount++
		if strings.EqualFold(res.Type, "error") {
			errorCount++
		}
		data, _ := json.Marshal(res)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush() // 立即把缓冲区数据推给前端
	}

	logger.Log.Info("机场节点测速结束",
		"sub_id", subID,
		"sub_name", subName,
		"node_id", nodeID,
		"mode", mode,
		"result_count", resultCount,
		"error_count", errorCount,
		"changes", fmt.Sprintf("订阅名称 %s | 测速结束 | 返回结果 %d 条 | 错误 %d 条", subName, resultCount, errorCount),
		"ip", clientIP,
		"path", reqPath,
	)
}

func runAirportSpeedTestJob(ctx context.Context, job *airportSpeedJob, history database.AirportSpeedTestHistory, nodes []database.AirportNode, mode string) {
	resultChan := make(chan service.SpeedTestResult)
	go service.GlobalMihomo.RunBatchTestWithMode(ctx, nodes, resultChan, mode)
	nodeNameByID := make(map[string]string, len(nodes))
	for _, n := range nodes {
		nodeNameByID[n.ID] = n.Name
	}

	resultCount := 0
	errorCount := 0
	for res := range resultChan {
		resultCount++
		if strings.EqualFold(res.Type, "error") {
			errorCount++
		}

		if err := database.DB.Create(&database.AirportSpeedTestResult{
			HistoryID:  history.ID,
			TaskKey:    history.TaskKey,
			SubID:      history.SubID,
			NodeID:     res.NodeID,
			NodeName:   nodeNameByID[res.NodeID],
			ResultType: res.Type,
			ResultText: res.Text,
		}).Error; err != nil {
			logger.Log.Warn("写入机场测速详细结果失败", "history_id", history.ID, "node_id", res.NodeID, "error", err)
		}

		if err := database.DB.Model(&database.AirportSpeedTestHistory{}).Where("id = ?", history.ID).Updates(map[string]interface{}{
			"result_count": resultCount,
			"error_count":  errorCount,
		}).Error; err != nil {
			logger.Log.Warn("更新机场测速历史统计失败", "history_id", history.ID, "error", err)
		}

		if job != nil {
			job.mu.Lock()
			if job.Results == nil {
				job.Results = make(map[string]map[string]service.SpeedTestResult)
			}
			if _, ok := job.Results[res.NodeID]; !ok {
				job.Results[res.NodeID] = make(map[string]service.SpeedTestResult)
			}
			resultType := strings.TrimSpace(strings.ToLower(res.Type))
			if resultType == "" {
				resultType = "unknown"
			}
			job.Results[res.NodeID][resultType] = res
			job.Done = resultCount
			job.Errors = errorCount
			job.mu.Unlock()
		}
	}

	now := time.Now()
	finalStatus := "completed"
	if ctx.Err() != nil {
		finalStatus = "stopped"
	}

	database.DB.Model(&database.AirportSpeedTestHistory{}).Where("id = ?", history.ID).Updates(map[string]interface{}{
		"status":       finalStatus,
		"result_count": resultCount,
		"error_count":  errorCount,
		"finished_at":  &now,
	})

	if finalStatus == "completed" {
		go service.SendBatchSpeedTestNotification(history.SubName, history.TaskKey, finalStatus, len(nodes), resultCount, errorCount, history.StartedAt, now)
	}

	airportSpeedJobsMu.Lock()
	if job, ok := airportSpeedJobsByID[history.ID]; ok {
		delete(airportSpeedJobsByID, history.ID)
		if job.SubID != "" {
			delete(airportSpeedJobsBySub, job.SubID)
		}
	}
	airportSpeedJobsMu.Unlock()
}

// apiStartAirportSpeedTest 启动后台测速任务并保存历史记录
func apiStartAirportSpeedTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		SubID  string `json:"sub_id"`
		NodeID string `json:"node_id"`
		Mode   string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, "error", "请求格式错误")
		return
	}

	req.SubID = strings.TrimSpace(req.SubID)
	req.NodeID = strings.TrimSpace(req.NodeID)
	req.Mode = service.NormalizeSpeedTestMode(req.Mode)

	if req.SubID == "" {
		sendJSON(w, "error", "仅支持整组测速记录历史，请选择订阅后执行一键测速")
		return
	}
	if req.NodeID != "" {
		sendJSON(w, "error", "单节点测速不写入历史记录，请使用节点测速按钮")
		return
	}

	if !service.GlobalMihomo.IsCoreReady() {
		sendJSON(w, "error", "请先在设置中下载 Mihomo 核心")
		return
	}

	var nodes []database.AirportNode
	subName := ""
	database.DB.Where("sub_id = ?", req.SubID).Find(&nodes)
	var sub database.AirportSub
	if err := database.DB.Where("id = ?", req.SubID).First(&sub).Error; err == nil {
		subName = sub.Name
	}

	if len(nodes) == 0 {
		sendJSON(w, "error", "未找到可测速节点")
		return
	}

	airportSpeedJobsMu.Lock()
	if req.SubID != "" {
		if running, ok := airportSpeedJobsBySub[req.SubID]; ok {
			airportSpeedJobsMu.Unlock()
			sendJSON(w, "success", map[string]interface{}{
				"message":   "测速任务已在运行",
				"record_id": running.HistoryID,
				"task_key":  running.TaskKey,
				"mode":      running.Mode,
				"running":   true,
			})
			return
		}
	}
	airportSpeedJobsMu.Unlock()

	history := database.AirportSpeedTestHistory{
		SubID:      req.SubID,
		SubName:    subName,
		Status:     "running",
		TotalCount: len(nodes),
		StartedAt:  time.Now(),
	}
	if err := database.DB.Create(&history).Error; err != nil {
		sendJSON(w, "error", "保存测速记录失败")
		return
	}

	jobCtx, cancel := context.WithCancel(context.Background())
	job := &airportSpeedJob{HistoryID: history.ID, TaskKey: history.TaskKey, SubID: req.SubID, Mode: req.Mode, Cancel: cancel, Total: len(nodes), Results: make(map[string]map[string]service.SpeedTestResult)}

	airportSpeedJobsMu.Lock()
	airportSpeedJobsByID[history.ID] = job
	if req.SubID != "" {
		airportSpeedJobsBySub[req.SubID] = job
	}
	airportSpeedJobsMu.Unlock()

	go runAirportSpeedTestJob(jobCtx, job, history, nodes, req.Mode)

	sendJSON(w, "success", map[string]interface{}{
		"message":   "测速任务已启动",
		"record_id": history.ID,
		"task_key":  history.TaskKey,
		"mode":      req.Mode,
		"running":   true,
	})
}

func apiStopAirportSpeedTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		RecordID string `json:"record_id"`
		SubID    string `json:"sub_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, "error", "请求格式错误")
		return
	}

	req.RecordID = strings.TrimSpace(req.RecordID)
	req.SubID = strings.TrimSpace(req.SubID)

	airportSpeedJobsMu.Lock()
	defer airportSpeedJobsMu.Unlock()

	var job *airportSpeedJob
	if req.SubID != "" {
		job = airportSpeedJobsBySub[req.SubID]
	}
	if job == nil && req.RecordID != "" {
		job = airportSpeedJobsByID[req.RecordID]
	}

	if job == nil {
		sendJSON(w, "success", "当前无运行中的测速任务")
		return
	}

	job.Cancel()
	sendJSON(w, "success", "测速任务已停止")
}

func apiAirportSpeedRunning(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	airportSpeedJobsMu.Lock()
	runningDetail := make([]map[string]interface{}, 0, len(airportSpeedJobsByID))
	for _, job := range airportSpeedJobsByID {
		job.mu.RLock()
		results := make(map[string]map[string]service.SpeedTestResult, len(job.Results))
		for nodeID, byType := range job.Results {
			copiedByType := make(map[string]service.SpeedTestResult, len(byType))
			for resultType, result := range byType {
				copiedByType[resultType] = result
			}
			results[nodeID] = copiedByType
		}
		runningDetail = append(runningDetail, map[string]interface{}{
			"record_id":    job.HistoryID,
			"task_key":     job.TaskKey,
			"sub_id":       job.SubID,
			"mode":         job.Mode,
			"total":        job.Total,
			"done":         job.Done,
			"error_count":  job.Errors,
			"result_count": job.Done,
			"results":      results,
		})
		job.mu.RUnlock()
	}
	airportSpeedJobsMu.Unlock()

	sendJSON(w, "success", map[string]interface{}{"running": runningDetail})
}

func apiAirportSpeedHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	subID := strings.TrimSpace(r.URL.Query().Get("sub_id"))
	q := database.DB.Model(&database.AirportSpeedTestHistory{})
	if subID != "" {
		q = q.Where("sub_id = ?", subID)
	}

	var records []database.AirportSpeedTestHistory
	if err := q.Order("created_at desc").Limit(200).Find(&records).Error; err != nil {
		sendJSON(w, "error", "读取测速历史失败")
		return
	}

	sendJSON(w, "success", map[string]interface{}{"records": records})
}

func apiAirportSpeedHistoryResults(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	historyID := strings.TrimSpace(r.URL.Query().Get("id"))
	if historyID == "" {
		sendJSON(w, "error", "缺少历史记录ID")
		return
	}

	var history database.AirportSpeedTestHistory
	if err := database.DB.Where("id = ?", historyID).First(&history).Error; err != nil {
		sendJSON(w, "error", "历史记录不存在")
		return
	}

	var rows []database.AirportSpeedTestResult
	if err := database.DB.Where("history_id = ?", historyID).Order("created_at desc").Find(&rows).Error; err != nil {
		sendJSON(w, "error", "读取详细结果失败")
		return
	}

	latestByNodeType := make(map[string]database.AirportSpeedTestResult)
	for _, row := range rows {
		typeKey := strings.TrimSpace(strings.ToLower(row.ResultType))
		if typeKey == "" {
			typeKey = "unknown"
		}
		key := row.NodeID + "|" + typeKey
		if _, ok := latestByNodeType[key]; !ok {
			latestByNodeType[key] = row
		}
	}

	typeRank := func(t string) int {
		switch strings.TrimSpace(strings.ToLower(t)) {
		case "ping":
			return 1
		case "tcp":
			return 2
		case "speed":
			return 3
		case "error":
			return 4
		default:
			return 99
		}
	}

	results := make([]database.AirportSpeedTestResult, 0, len(latestByNodeType))
	for _, row := range latestByNodeType {
		results = append(results, row)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].NodeName == results[j].NodeName {
			ri := typeRank(results[i].ResultType)
			rj := typeRank(results[j].ResultType)
			if ri != rj {
				return ri < rj
			}
			if results[i].NodeID == results[j].NodeID {
				return results[i].ResultType < results[j].ResultType
			}
			return results[i].NodeID < results[j].NodeID
		}
		return results[i].NodeName < results[j].NodeName
	})

	sendJSON(w, "success", map[string]interface{}{
		"history": history,
		"results": results,
	})
}

func apiAirportSpeedHistoryDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, "error", "请求格式错误")
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		sendJSON(w, "error", "缺少记录ID")
		return
	}

	airportSpeedJobsMu.Lock()
	_, running := airportSpeedJobsByID[req.ID]
	airportSpeedJobsMu.Unlock()
	if running {
		sendJSON(w, "error", "运行中的记录不能删除")
		return
	}

	if err := database.DB.Where("id = ?", req.ID).Delete(&database.AirportSpeedTestHistory{}).Error; err != nil {
		sendJSON(w, "error", "删除历史记录失败")
		return
	}
	sendJSON(w, "success", "已删除")
}
