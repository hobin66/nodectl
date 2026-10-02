package server

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"nodectl/internal/database"
	"nodectl/internal/logger"
	"nodectl/internal/service"
)

// nodeDeleteMu 节点删除操作的互斥锁，防止对同一节点的并发删除请求
// 导致数据库行锁争抢、死锁或连接池耗尽。
var nodeDeleteMu sync.Map // key: node UUID (string) → value: *sync.Mutex

// ------------------- [节点管理 API] -------------------

// apiUpdateNode 更新节点信息
// 功能：接收前端请求，更新节点的基础信息和协议链接，包含演示模式下对内置节点核心资产的保护与专业提示
func apiUpdateNode(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	var req struct {
		database.NodePool
		TrafficLimitRaw        string `json:"traffic_limit_raw"`
		TrafficResetAnchorDate string `json:"traffic_reset_anchor_date"`
		// Add backwards compatibility explicitly if needed, but since we embedded NodePool, it has IPMode and LinkIPModes
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, "error", "无效的数据格式")
		return
	}

	// 2. 先从数据库查出真实存在的节点 (确保拿到主键 ID 和 原有数据)
	var targetNode database.NodePool
	if err := database.DB.First(&targetNode, "uuid = ?", req.UUID).Error; err != nil {
		logger.Log.Warn("更新失败: 节点不存在", "uuid", req.UUID, "ip", clientIP)
		sendJSON(w, "error", "节点不存在")
		return
	}

	// 记录更新前快照，用于输出精确变更日志
	oldNode := targetNode
	oldNode.Links = make(map[string]string, len(targetNode.Links))
	for k, v := range targetNode.Links {
		oldNode.Links[k] = v
	}
	oldNode.LinkIPModes = make(map[string]int, len(targetNode.LinkIPModes))
	for k, v := range targetNode.LinkIPModes {
		oldNode.LinkIPModes[k] = v
	}
	oldNode.LinkPorts = make(map[string]int, len(targetNode.LinkPorts))
	for k, v := range targetNode.LinkPorts {
		oldNode.LinkPorts[k] = v
	}
	oldNode.DisabledLinks = append([]string(nil), targetNode.DisabledLinks...)

	demoPath := filepath.Join("data", "debug", "demo")
	if _, err := os.Stat(demoPath); err == nil {
		// 判定条件：节点创建时间早于程序启动时间 (内置节点)
		if targetNode.CreatedAt.Before(AppStartTime) {

			// 保护 1: 禁止修改或删除 IPv4 和 IPv6
			if targetNode.IPV4 != req.IPV4 || targetNode.IPV6 != req.IPV6 {
				logger.Log.Warn("演示模式拦截: 尝试修改或删除内置节点 IP", "uuid", req.UUID, "ip", clientIP)
				sendJSON(w, "error", "【演示模式保护】禁止修改或清空。如需体验修改功能，请新建节点。")
				return
			}

			// 保护 2: 禁止删除、清空或修改已有的协议链接
			for protoKey, oldVal := range targetNode.Links {
				newVal, exists := req.Links[protoKey]

				// 如果前端传来的数据中没有这个协议，或者内容为空，说明尝试删除
				if !exists || newVal == "" {
					logger.Log.Warn("演示模式拦截: 尝试删除内置节点协议", "uuid", req.UUID, "protocol", protoKey, "ip", clientIP)
					// [优化] 更专业的提示语
					sendJSON(w, "error", "【演示模式保护】内置预设节点的基础协议配置已被锁定，禁止删除。")
					return
				}

				// 如果前端传来的值与数据库原有值不同 (并且排除了系统占位符 "__KEEP_EXISTING__")，说明尝试修改
				if newVal != oldVal && newVal != "__KEEP_EXISTING__" {
					logger.Log.Warn("演示模式拦截: 尝试修改内置节点协议内容", "uuid", req.UUID, "protocol", protoKey, "ip", clientIP)
					sendJSON(w, "error", "【演示模式保护】禁止修改内容。如需体验完整编辑功能，请新建节点。")
					return
				}
			}
		}
	}
	// ---------------- [新增：演示模式严密保护逻辑 结束] ----------------

	// 3. 将前端请求的基础字段更新到数据库对象上
	targetNode.Name = req.Name
	targetNode.RoutingType = req.RoutingType
	targetNode.IsBlocked = req.IsBlocked
	targetNode.DisabledLinks = req.DisabledLinks
	targetNode.IPV4 = req.IPV4
	targetNode.IPV6 = req.IPV6
	targetNode.IPMode = req.IPMode
	rawResetDay := service.NormalizeNodeTrafficResetDay(req.ResetDay)
	targetNode.TrafficResetMode = service.NormalizeTrafficResetMode(req.TrafficResetMode, rawResetDay)
	targetNode.ResetDay = rawResetDay
	if targetNode.TrafficResetMode != service.TrafficResetModeFixedDay {
		targetNode.ResetDay = 0
	}
	targetNode.TrafficResetIntervalDays = service.NormalizeTrafficResetIntervalDays(req.TrafficResetIntervalDays)
	if anchor, err := service.ParseTrafficResetAnchorDate(req.TrafficResetAnchorDate); err != nil {
		sendJSON(w, "error", err.Error())
		return
	} else if anchor != nil {
		targetNode.TrafficResetAnchorAt = anchor
	} else if targetNode.TrafficResetMode == service.TrafficResetModeIntervalDays &&
		(targetNode.TrafficResetAnchorAt == nil || targetNode.TrafficResetAnchorAt.IsZero()) {
		a := service.ResolveTrafficResetAnchor(nil, targetNode.CreatedAt, time.Now())
		targetNode.TrafficResetAnchorAt = &a
	}
	targetNode.TrafficLimitType = service.NormalizeTrafficLimitType(req.TrafficLimitType)
	targetNode.TrafficLimit = req.TrafficLimit
	if raw := strings.TrimSpace(req.TrafficLimitRaw); raw != "" {
		targetNode.TrafficLimit = service.ParseTrafficLimitInputToBytes(raw)
	}
	oldResetMode := service.NormalizeTrafficResetMode(oldNode.TrafficResetMode, oldNode.ResetDay)
	oldEffectiveResetDay := 0
	if oldResetMode == service.TrafficResetModeFixedDay {
		oldEffectiveResetDay = service.NormalizeNodeTrafficResetDay(oldNode.ResetDay)
	}
	newEffectiveResetDay := 0
	if targetNode.TrafficResetMode == service.TrafficResetModeFixedDay {
		newEffectiveResetDay = service.NormalizeNodeTrafficResetDay(targetNode.ResetDay)
	}
	oldResetIntervalDays := 0
	if oldResetMode == service.TrafficResetModeIntervalDays {
		oldResetIntervalDays = service.NormalizeTrafficResetIntervalDays(oldNode.TrafficResetIntervalDays)
	}
	newResetIntervalDays := 0
	if targetNode.TrafficResetMode == service.TrafficResetModeIntervalDays {
		newResetIntervalDays = service.NormalizeTrafficResetIntervalDays(targetNode.TrafficResetIntervalDays)
	}
	oldAnchorDate := ""
	newAnchorDate := ""
	if oldNode.TrafficResetAnchorAt != nil && !oldNode.TrafficResetAnchorAt.IsZero() {
		oldAnchorDate = oldNode.TrafficResetAnchorAt.In(time.Local).Format("2006-01-02")
	}
	if targetNode.TrafficResetAnchorAt != nil && !targetNode.TrafficResetAnchorAt.IsZero() {
		newAnchorDate = targetNode.TrafficResetAnchorAt.In(time.Local).Format("2006-01-02")
	}
	resetRuleChanged := oldResetMode != targetNode.TrafficResetMode ||
		oldEffectiveResetDay != newEffectiveResetDay ||
		oldResetIntervalDays != newResetIntervalDays
	if (oldResetMode == service.TrafficResetModeIntervalDays || targetNode.TrafficResetMode == service.TrafficResetModeIntervalDays) &&
		oldAnchorDate != newAnchorDate {
		resetRuleChanged = true
	}
	if resetRuleChanged {
		targetNode.TrafficResetAt = service.ResolveTrafficResetAtOnRuleChange(
			targetNode.TrafficResetMode,
			targetNode.ResetDay,
			time.Now(),
		)
	}

	// 4. 安全检查：判断是否处于安全环境
	isSecure := service.CheckRequestSecure(r)
	if !isSecure {
		var config database.SysConfig
		database.DB.Where("key = ?", "sys_force_http").First(&config)
		if config.Value == "true" {
			isSecure = true // 用户选择忽略风险，允许在 HTTP 下编辑
		}
	}

	// 5. 根据安全状态执行智能合并或全量覆盖
	if !isSecure {
		safeLinks := make(map[string]string)
		safeLinkIPModes := make(map[string]int)
		safeLinkPorts := make(map[string]int)

		for k, v := range req.Links {
			if v == "__KEEP_EXISTING__" {
				if oldVal, ok := targetNode.Links[k]; ok {
					safeLinks[k] = oldVal
					// also keep existing ip mode
					if oldMode, mok := targetNode.LinkIPModes[k]; mok {
						safeLinkIPModes[k] = oldMode
					}
					// keep existing port
					if oldPort, pok := targetNode.LinkPorts[k]; pok {
						safeLinkPorts[k] = oldPort
					}
				}
			} else {
				if _, exists := targetNode.Links[k]; exists {
					safeLinks[k] = targetNode.Links[k]
				} else {
					safeLinks[k] = v
				}
				// allow updating link ip mode even if link value is safe-guarded
				if newMode, ok := req.LinkIPModes[k]; ok {
					safeLinkIPModes[k] = newMode
				}
				// allow updating link port
				if newPort, ok := req.LinkPorts[k]; ok {
					safeLinkPorts[k] = newPort
				} else if oldPort, pok := targetNode.LinkPorts[k]; pok {
					safeLinkPorts[k] = oldPort
				}
			}
		}

		// 保留不在 req.Links 中但存在于 req.LinkPorts 或数据库 LinkPorts 中的端口配置。
		// link_ports 中的端口信息永远不会被前端删除，只会被修改。
		for proto, port := range req.LinkPorts {
			if _, exists := safeLinkPorts[proto]; !exists && port > 0 {
				safeLinkPorts[proto] = port
			}
		}
		for proto, port := range targetNode.LinkPorts {
			if _, exists := safeLinkPorts[proto]; !exists && port > 0 {
				safeLinkPorts[proto] = port
			}
		}

		targetNode.Links = safeLinks
		targetNode.LinkIPModes = safeLinkIPModes
		targetNode.LinkPorts = safeLinkPorts
	} else {
		safeLinks := make(map[string]string)
		safeLinkIPModes := make(map[string]int)
		safeLinkPorts := make(map[string]int)

		for k, v := range req.Links {
			if v == "__KEEP_EXISTING__" {
				if oldVal, ok := targetNode.Links[k]; ok {
					safeLinks[k] = oldVal
					if oldMode, mok := targetNode.LinkIPModes[k]; mok {
						safeLinkIPModes[k] = oldMode
					}
					if oldPort, pok := targetNode.LinkPorts[k]; pok {
						safeLinkPorts[k] = oldPort
					}
				}
			} else {
				safeLinks[k] = v
				if newMode, ok := req.LinkIPModes[k]; ok {
					safeLinkIPModes[k] = newMode
				}
				if newPort, ok := req.LinkPorts[k]; ok {
					safeLinkPorts[k] = newPort
				}
			}
		}

		// 保留不在 req.Links 中但存在于 req.LinkPorts 或数据库 LinkPorts 中的端口配置。
		// link_ports 中的端口信息永远不会被前端删除，只会被修改。
		for proto, port := range req.LinkPorts {
			if _, exists := safeLinkPorts[proto]; !exists && port > 0 {
				safeLinkPorts[proto] = port
			}
		}
		for proto, port := range targetNode.LinkPorts {
			if _, exists := safeLinkPorts[proto]; !exists && port > 0 {
				safeLinkPorts[proto] = port
			}
		}

		targetNode.Links = safeLinks
		targetNode.LinkIPModes = safeLinkIPModes
		targetNode.LinkPorts = safeLinkPorts
	}

	// 6. 保存更新
	if err := database.DB.Save(&targetNode).Error; err != nil {
		logger.Log.Error("更新节点数据库失败", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "数据库更新失败")
		return
	}
	service.UpdateNodeTrafficThresholdConfigFromNode(targetNode)

	changedDetails := make([]string, 0)
	if oldNode.Name != targetNode.Name {
		changedDetails = append(changedDetails, fmt.Sprintf("name: %s -> %s", oldNode.Name, targetNode.Name))
	}
	if oldNode.RoutingType != targetNode.RoutingType {
		changedDetails = append(changedDetails, fmt.Sprintf("routing_type: %d -> %d", oldNode.RoutingType, targetNode.RoutingType))
	}
	if oldNode.IsBlocked != targetNode.IsBlocked {
		changedDetails = append(changedDetails, fmt.Sprintf("is_blocked: %t -> %t", oldNode.IsBlocked, targetNode.IsBlocked))
	}
	if oldNode.IPV4 != targetNode.IPV4 {
		changedDetails = append(changedDetails, fmt.Sprintf("ipv4: %s -> %s", oldNode.IPV4, targetNode.IPV4))
	}
	if oldNode.IPV6 != targetNode.IPV6 {
		changedDetails = append(changedDetails, fmt.Sprintf("ipv6: %s -> %s", oldNode.IPV6, targetNode.IPV6))
	}
	if oldNode.IPMode != targetNode.IPMode {
		changedDetails = append(changedDetails, fmt.Sprintf("ip_mode: %d -> %d", oldNode.IPMode, targetNode.IPMode))
	}
	if oldEffectiveResetDay != newEffectiveResetDay {
		changedDetails = append(changedDetails, fmt.Sprintf("reset_day: %d -> %d", oldEffectiveResetDay, newEffectiveResetDay))
	}
	if oldResetMode != targetNode.TrafficResetMode {
		changedDetails = append(changedDetails, fmt.Sprintf("traffic_reset_mode: %s -> %s", oldResetMode, targetNode.TrafficResetMode))
	}
	if oldResetIntervalDays != newResetIntervalDays {
		changedDetails = append(changedDetails, fmt.Sprintf("traffic_reset_interval_days: %d -> %d", oldResetIntervalDays, newResetIntervalDays))
	}
	if (oldResetMode == service.TrafficResetModeIntervalDays || targetNode.TrafficResetMode == service.TrafficResetModeIntervalDays) && oldAnchorDate != newAnchorDate {
		changedDetails = append(changedDetails, fmt.Sprintf("traffic_reset_anchor_date: %s -> %s", oldAnchorDate, newAnchorDate))
	}
	if oldNode.TrafficLimit != targetNode.TrafficLimit {
		changedDetails = append(changedDetails, fmt.Sprintf("traffic_limit: %d -> %d", oldNode.TrafficLimit, targetNode.TrafficLimit))
	}
	if service.NormalizeTrafficLimitType(oldNode.TrafficLimitType) != service.NormalizeTrafficLimitType(targetNode.TrafficLimitType) {
		changedDetails = append(changedDetails, fmt.Sprintf("traffic_limit_type: %s -> %s", service.NormalizeTrafficLimitType(oldNode.TrafficLimitType), service.NormalizeTrafficLimitType(targetNode.TrafficLimitType)))
	}

	oldDisabledSet := make(map[string]struct{}, len(oldNode.DisabledLinks))
	for _, p := range oldNode.DisabledLinks {
		oldDisabledSet[p] = struct{}{}
	}
	newDisabledSet := make(map[string]struct{}, len(targetNode.DisabledLinks))
	for _, p := range targetNode.DisabledLinks {
		newDisabledSet[p] = struct{}{}
	}
	addedDisabled := make([]string, 0)
	removedDisabled := make([]string, 0)
	for p := range newDisabledSet {
		if _, ok := oldDisabledSet[p]; !ok {
			addedDisabled = append(addedDisabled, p)
		}
	}
	for p := range oldDisabledSet {
		if _, ok := newDisabledSet[p]; !ok {
			removedDisabled = append(removedDisabled, p)
		}
	}
	if len(addedDisabled) > 0 {
		sort.Strings(addedDisabled)
		changedDetails = append(changedDetails, fmt.Sprintf("disabled_links_add: %s", strings.Join(addedDisabled, ",")))
	}
	if len(removedDisabled) > 0 {
		sort.Strings(removedDisabled)
		changedDetails = append(changedDetails, fmt.Sprintf("disabled_links_remove: %s", strings.Join(removedDisabled, ",")))
	}

	protoSet := make(map[string]struct{})
	for p := range oldNode.Links {
		protoSet[p] = struct{}{}
	}
	for p := range targetNode.Links {
		protoSet[p] = struct{}{}
	}
	protos := make([]string, 0, len(protoSet))
	for p := range protoSet {
		protos = append(protos, p)
	}
	sort.Strings(protos)
	for _, p := range protos {
		oldVal, oldOK := oldNode.Links[p]
		newVal, newOK := targetNode.Links[p]
		switch {
		case !oldOK && newOK:
			changedDetails = append(changedDetails, fmt.Sprintf("links[%s]: added", p))
		case oldOK && !newOK:
			changedDetails = append(changedDetails, fmt.Sprintf("links[%s]: removed", p))
		case oldOK && newOK && oldVal != newVal:
			changedDetails = append(changedDetails, fmt.Sprintf("links[%s]: updated", p))
		}
	}

	ipModeProtoSet := make(map[string]struct{})
	for p := range oldNode.LinkIPModes {
		ipModeProtoSet[p] = struct{}{}
	}
	for p := range targetNode.LinkIPModes {
		ipModeProtoSet[p] = struct{}{}
	}
	ipModeProtos := make([]string, 0, len(ipModeProtoSet))
	for p := range ipModeProtoSet {
		ipModeProtos = append(ipModeProtos, p)
	}
	sort.Strings(ipModeProtos)
	for _, p := range ipModeProtos {
		oldMode, oldOK := oldNode.LinkIPModes[p]
		newMode, newOK := targetNode.LinkIPModes[p]
		switch {
		case !oldOK && newOK:
			changedDetails = append(changedDetails, fmt.Sprintf("link_ip_modes[%s]: add %d", p, newMode))
		case oldOK && !newOK:
			changedDetails = append(changedDetails, fmt.Sprintf("link_ip_modes[%s]: removed", p))
		case oldOK && newOK && oldMode != newMode:
			changedDetails = append(changedDetails, fmt.Sprintf("link_ip_modes[%s]: %d -> %d", p, oldMode, newMode))
		}
	}

	// 追踪 link_ports 变更
	portProtoSet := make(map[string]struct{})
	for p := range oldNode.LinkPorts {
		portProtoSet[p] = struct{}{}
	}
	for p := range targetNode.LinkPorts {
		portProtoSet[p] = struct{}{}
	}
	portProtos := make([]string, 0, len(portProtoSet))
	for p := range portProtoSet {
		portProtos = append(portProtos, p)
	}
	sort.Strings(portProtos)
	for _, p := range portProtos {
		oldPort, oldOK := oldNode.LinkPorts[p]
		newPort, newOK := targetNode.LinkPorts[p]
		switch {
		case !oldOK && newOK:
			changedDetails = append(changedDetails, fmt.Sprintf("link_ports[%s]: add %d", p, newPort))
		case oldOK && !newOK:
			changedDetails = append(changedDetails, fmt.Sprintf("link_ports[%s]: removed", p))
		case oldOK && newOK && oldPort != newPort:
			changedDetails = append(changedDetails, fmt.Sprintf("link_ports[%s]: %d -> %d", p, oldPort, newPort))
		}
	}

	if len(changedDetails) == 0 {
		// 前端存在自动保存场景，无字段变化时不写日志，避免产生重复噪声
	} else {
		logger.Log.Info("节点更新成功",
			"name", targetNode.Name,
			"uuid", targetNode.UUID,
			"changed_count", len(changedDetails),
			"changes", strings.Join(changedDetails, " | "),
			"ip", clientIP,
			"path", reqPath,
		)
	}
	sendJSON(w, "success", "更新成功")
}

func apiAddNode(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	if r.Method != http.MethodPost {
		logger.Log.Warn("非法请求方法", "method", r.Method, "ip", clientIP, "path", reqPath)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name                     string `json:"name"`
		RoutingType              int    `json:"routing_type"`
		ResetDay                 int    `json:"reset_day"`
		TrafficResetMode         string `json:"traffic_reset_mode"`
		TrafficResetIntervalDays int    `json:"traffic_reset_interval_days"`
		TrafficResetAnchorDate   string `json:"traffic_reset_anchor_date"`
		TrafficLimit             int64  `json:"traffic_limit"`
		TrafficLimitRaw          string `json:"traffic_limit_raw"`
		TrafficLimitType         string `json:"traffic_limit_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Log.Warn("解析 JSON 失败", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "请求格式错误")
		return
	}

	parsedAnchor, err := service.ParseTrafficResetAnchorDate(req.TrafficResetAnchorDate)
	if err != nil {
		sendJSON(w, "error", err.Error())
		return
	}

	node, err := service.AddNode(req.Name, req.RoutingType)
	if err != nil {
		logger.Log.Error("添加节点入库失败", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "数据库写入失败")
		return
	}

	// 补充更新 reset_day 和 traffic_limit
	updates := map[string]interface{}{}
	rawResetDay := service.NormalizeNodeTrafficResetDay(req.ResetDay)
	resetMode := service.NormalizeTrafficResetMode(req.TrafficResetMode, rawResetDay)
	resetDay := rawResetDay
	if resetMode != service.TrafficResetModeFixedDay {
		resetDay = 0
	}
	resetIntervalDays := service.NormalizeTrafficResetIntervalDays(req.TrafficResetIntervalDays)
	updates["reset_day"] = resetDay
	updates["traffic_reset_mode"] = resetMode
	updates["traffic_reset_interval_days"] = resetIntervalDays
	if resetMode == service.TrafficResetModeIntervalDays {
		if parsedAnchor != nil {
			updates["traffic_reset_anchor_at"] = *parsedAnchor
		} else {
			a := service.ResolveTrafficResetAnchor(nil, node.CreatedAt, time.Now())
			updates["traffic_reset_anchor_at"] = a
		}
	} else {
		updates["traffic_reset_anchor_at"] = nil
	}
	updates["traffic_reset_at"] = service.ResolveTrafficResetAtOnRuleChange(resetMode, resetDay, time.Now())
	trafficLimit := req.TrafficLimit
	if raw := strings.TrimSpace(req.TrafficLimitRaw); raw != "" {
		trafficLimit = service.ParseTrafficLimitInputToBytes(raw)
	}
	if trafficLimit > 0 {
		updates["traffic_limit"] = trafficLimit
	}
	updates["traffic_limit_type"] = service.NormalizeTrafficLimitType(req.TrafficLimitType)

	// 读取系统默认安装协议，自动为新节点添加空白协议（与编辑弹窗中手动添加空白协议效果一致）
	var defaultProtoConfig database.SysConfig
	if err := database.DB.Where("key = ?", "pref_default_install_protocols").First(&defaultProtoConfig).Error; err == nil {
		rawVal := strings.TrimSpace(defaultProtoConfig.Value)
		if rawVal != "" {
			var defaultProtos []string
			if jsonErr := json.Unmarshal([]byte(rawVal), &defaultProtos); jsonErr == nil && len(defaultProtos) > 0 {
				if node.Links == nil {
					node.Links = make(map[string]string)
				}
				for _, proto := range defaultProtos {
					proto = strings.TrimSpace(proto)
					if proto != "" {
						node.Links[proto] = "" // 空内容，等待 Agent 部署后自动上报填充
					}
				}
				if err := database.DB.Save(node).Error; err != nil {
					logger.Log.Error("写入默认协议失败", "uuid", node.UUID, "error", err)
				} else {
					logger.Log.Info("已为新节点添加默认协议", "uuid", node.UUID, "protocols", defaultProtos)
				}
			}
		}
	}

	if len(updates) > 0 {
		if err := database.DB.Model(&database.NodePool{}).Where("uuid = ?", node.UUID).Updates(updates).Error; err != nil {
			logger.Log.Error("补充更新节点属性失败", "uuid", node.UUID, "error", err)
		}
	}
	if err := database.DB.Where("uuid = ?", node.UUID).First(&node).Error; err == nil {
		service.UpdateNodeTrafficThresholdConfigFromNode(*node)
	}

	logger.Log.Info("节点添加成功", "uuid", node.UUID, "name", node.Name, "routing_type", node.RoutingType, "ip", clientIP, "path", reqPath)
	sendJSON(w, "success", "节点添加成功")
}

// apiGetNodes 获取所有节点列表
func apiGetNodes(w http.ResponseWriter, r *http.Request) {
	// 1. 从数据库查询所有节点，按 SortIndex 排序
	var nodes []database.NodePool
	if err := database.DB.Order("sort_index ASC").Find(&nodes).Error; err != nil {
		logger.Log.Error("获取节点列表失败", "error", err)
		sendJSON(w, "error", "获取节点数据失败")
		return
	}

	// 2. 安全检查：判断是否处于安全环境 (HTTPS 协议 或 用户开启了强制 HTTP)
	isSecure := service.CheckRequestSecure(r)
	if !isSecure {
		var config database.SysConfig
		database.DB.Where("key = ?", "sys_force_http").First(&config)
		if config.Value == "true" {
			isSecure = true // 用户选择忽略风险，视为安全
		}
	}

	// 如果依然不安全，则脱敏(隐藏)真实链接
	if !isSecure {
		for i := range nodes {
			maskedLinks := make(map[string]string)
			for k := range nodes[i].Links {
				maskedLinks[k] = "🔒 安全保护: 仅 HTTPS 可见"
			}
			nodes[i].Links = maskedLinks
		}
	}

	// 3. 将节点分类为中转和落地，适配前端展示需求。
	var relayNodes []database.NodePool
	var landNodes []database.NodePool

	for _, node := range nodes {
		if node.RoutingType == 1 {
			relayNodes = append(relayNodes, node)
		} else if node.RoutingType == 2 {
			landNodes = append(landNodes, node)
		}
	}

	// 确保空切片序列化为 [] 而不是 null
	if relayNodes == nil {
		relayNodes = []database.NodePool{}
	}
	if landNodes == nil {
		landNodes = []database.NodePool{}
	}

	// 获取面板地址 (前端复制链接时需要)
	var config database.SysConfig
	database.DB.Where("key = ?", "panel_url").First(&config)

	// 获取 tunnel 子域名（用于前端生成安装命令时选择回调地址模式）
	var tunnelSubdomainConfig database.SysConfig
	database.DB.Where("key = ?", "cf_tunnel_subdomain").First(&tunnelSubdomainConfig)

	// 4. 返回结构化数据
	sendJSON(w, "success", map[string]interface{}{
		"data": map[string]interface{}{
			"direct_nodes":     relayNodes, // 保留现有 API 字段名，内容是中转节点。
			"land_nodes":       landNodes,
			"panel_url":        config.Value,
			"tunnel_subdomain": tunnelSubdomainConfig.Value,
		},
	})
}

// apiGetOfflineNotifySettings 获取节点离线通知设置列表
func apiGetOfflineNotifySettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var nodes []database.NodePool
	if err := database.DB.Select("uuid", "name", "install_id", "region", "routing_type", "offline_notify_enabled", "offline_notify_grace_sec", "offline_last_notify_at", "traffic_threshold_enabled", "traffic_threshold_percent", "traffic_threshold_reached", "traffic_history_count", "sort_index", "updated_at").
		Order("routing_type ASC, sort_index ASC, updated_at DESC").
		Find(&nodes).Error; err != nil {
		sendJSON(w, "error", "读取离线通知配置失败")
		return
	}

	items := make([]map[string]interface{}, 0, len(nodes))
	for _, n := range nodes {
		grace := service.NormalizeNodeOfflineGraceSec(n.OfflineNotifyGraceSec)
		item := map[string]interface{}{
			"uuid":                      n.UUID,
			"install_id":                n.InstallID,
			"name":                      n.Name,
			"region":                    n.Region,
			"routing_type":              n.RoutingType,
			"offline_notify_grace_sec":  grace,
			"traffic_threshold_percent": service.NormalizeTrafficThresholdPercent(n.TrafficThresholdPercent),
			"traffic_threshold_reached": n.TrafficThresholdReached,
			"online":                    service.IsNodeOnlineForStatus(n.InstallID),
		}
		// 历史流量记录条数：如果为 NULL 则返回 -1 表示需要前端懒加载
		if n.TrafficHistoryCount != nil {
			item["traffic_history_count"] = *n.TrafficHistoryCount
		} else {
			item["traffic_history_count"] = -1 // NULL，前端需要异步加载
		}
		if n.OfflineLastNotifyAt != nil && !n.OfflineLastNotifyAt.IsZero() {
			item["offline_last_notify_at"] = n.OfflineLastNotifyAt.Format("2006-01-02 15:04:05")
		} else {
			item["offline_last_notify_at"] = "-"
		}
		items = append(items, item)
	}

	sendJSON(w, "success", map[string]interface{}{"nodes": items})
}

// apiUpdateOfflineNotifySetting 更新单个节点离线通知设置
func apiUpdateOfflineNotifySetting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		UUID                    string `json:"uuid"`
		OfflineNotifyGraceSec   *int   `json:"offline_notify_grace_sec"`
		TrafficThresholdPercent *int   `json:"traffic_threshold_percent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, "error", "请求格式错误")
		return
	}
	req.UUID = strings.TrimSpace(req.UUID)
	if req.UUID == "" {
		sendJSON(w, "error", "缺少节点 UUID")
		return
	}

	updates := map[string]interface{}{}

	// 宽限期：大于0自动启用离线通知，等于0自动禁用
	if req.OfflineNotifyGraceSec != nil {
		grace := service.NormalizeNodeOfflineGraceSec(*req.OfflineNotifyGraceSec)
		updates["offline_notify_grace_sec"] = grace
		updates["offline_notify_enabled"] = grace > 0
	}

	// 阈值：大于0自动启用阈值停机，等于0自动禁用
	if req.TrafficThresholdPercent != nil {
		v := service.NormalizeTrafficThresholdPercent(*req.TrafficThresholdPercent)
		updates["traffic_threshold_percent"] = v
		updates["traffic_threshold_enabled"] = v > 0
		if v <= 0 {
			updates["traffic_threshold_reached"] = false
		}
	}

	if len(updates) == 0 {
		sendJSON(w, "error", "没有可更新的字段")
		return
	}

	res := database.DB.Model(&database.NodePool{}).Where("uuid = ?", req.UUID).Updates(updates)
	if res.Error != nil {
		sendJSON(w, "error", "更新失败")
		return
	}
	if res.RowsAffected == 0 {
		sendJSON(w, "error", "节点不存在")
		return
	}

	// ----------------------------------------------------
	// 内存立刻拉平最新的状态，保证推送不再查表
	// ----------------------------------------------------
	var node database.NodePool
	queryErr := database.DB.Select("uuid", "install_id", "name", "traffic_limit", "traffic_limit_type", "traffic_threshold_enabled", "traffic_threshold_percent", "traffic_threshold_reached", "offline_notify_enabled", "offline_notify_grace_sec").Where("uuid = ?", req.UUID).First(&node).Error
	if queryErr == nil {
		service.UpdateNodeNotifyConfigFromDB(node.InstallID, node.OfflineNotifyEnabled, node.OfflineNotifyGraceSec, node.Name)
		service.UpdateNodeTrafficThresholdConfigFromNode(node)
	}

	if req.OfflineNotifyGraceSec != nil {
		if queryErr == nil {
			service.OnNodeConnectionStatusChanged(node.InstallID, service.IsNodeOnline(node.InstallID))
		}
	}

	sendJSON(w, "success", "设置已更新")
}

// apiGetTunnelNodeSettings 获取节点 tunnel 设置列表
func apiGetTunnelNodeSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var nodes []database.NodePool
	if err := database.DB.Select("uuid", "name", "install_id", "links", "disabled_links", "tunnel_enabled", "tunnel_id", "tunnel_name", "tunnel_domain", "region", "routing_type", "sort_index", "updated_at").
		Order("routing_type ASC, sort_index ASC, updated_at DESC").
		Find(&nodes).Error; err != nil {
		sendJSON(w, "error", "读取 tunnel 节点配置失败")
		return
	}

	items := make([]map[string]interface{}, 0, len(nodes))
	for _, n := range nodes {
		supported := getSupportedTunnelProtocolsForNode(n)
		tunnelDomain := strings.TrimSpace(n.TunnelDomain)
		tunnelID := strings.TrimSpace(n.TunnelID)
		tunnelName := strings.TrimSpace(n.TunnelName)

		hosts := getNodeTunnelHosts(n)
		item := map[string]interface{}{
			"uuid":                       n.UUID,
			"install_id":                 n.InstallID,
			"name":                       n.Name,
			"region":                     n.Region,
			"routing_type":               n.RoutingType,
			"tunnel_enabled":             n.TunnelEnabled,
			"tunnel_id":                  tunnelID,
			"tunnel_name":                tunnelName,
			"tunnel_domain":              tunnelDomain,
			"tunnel_hosts":               hosts,
			"online":                     service.IsNodeOnlineForStatus(n.InstallID),
			"supported_tunnel_protocols": supported,
		}
		items = append(items, item)
	}

	sendJSON(w, "success", map[string]interface{}{"nodes": items})
}

// apiUpdateTunnelNodeSetting 更新单个节点 tunnel 设置
func apiUpdateTunnelNodeSetting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		UUID          string `json:"uuid"`
		TunnelEnabled *bool  `json:"tunnel_enabled"`
		TunnelID      string `json:"tunnel_id"`
		TunnelDomain  string `json:"tunnel_domain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, "error", "请求格式错误")
		return
	}
	req.UUID = strings.TrimSpace(req.UUID)
	if req.UUID == "" {
		sendJSON(w, "error", "缺少节点 UUID")
		return
	}

	var node database.NodePool
	if err := database.DB.Select("uuid", "name", "install_id", "links", "disabled_links", "tunnel_enabled", "tunnel_id", "tunnel_token", "tunnel_name", "tunnel_domain").Where("uuid = ?", req.UUID).First(&node).Error; err != nil {
		sendJSON(w, "error", "节点不存在")
		return
	}

	updates := map[string]interface{}{}
	if strings.TrimSpace(req.TunnelID) != "" {
		updates["tunnel_id"] = strings.TrimSpace(req.TunnelID)
	}
	if req.TunnelEnabled != nil {
		updates["tunnel_enabled"] = *req.TunnelEnabled
		if *req.TunnelEnabled {
			supported := getSupportedTunnelProtocolsForNode(node)
			if len(supported) == 0 {
				sendJSON(w, "error", "该节点暂无可通过 Tunnel 加速的协议。")
				return
			}
			domain := strings.TrimSpace(req.TunnelDomain)
			if domain == "" {
				domain = strings.TrimSpace(node.TunnelDomain)
			}
			if domain == "" {
				generated, err := generateNodeTunnelDomain(req.UUID)
				if err != nil {
					sendJSON(w, "error", err.Error())
					return
				}
				domain = generated
			}
			updates["tunnel_domain"] = domain
			if _, ok := updates["tunnel_id"]; !ok {
				// 每节点独立 Tunnel 设计：检查节点是否已有专属 Tunnel
				existingID := strings.TrimSpace(node.TunnelID)
				existingToken := strings.TrimSpace(node.TunnelToken)
				if existingID != "" && existingToken != "" {
					// 节点已有专属 Tunnel，直接复用
				} else {
					// 节点没有专属 Tunnel —— 自动创建
					newTunnelID, newToken, newName, createErr := service.CreateNodeDedicatedTunnel(node.Name)
					if createErr != nil {
						sendJSON(w, "error", "创建节点专属 Tunnel 失败: "+createErr.Error())
						return
					}
					updates["tunnel_id"] = newTunnelID
					updates["tunnel_token"] = newToken
					updates["tunnel_name"] = newName
				}
			}
		} else if strings.TrimSpace(req.TunnelDomain) != "" {
			updates["tunnel_domain"] = strings.TrimSpace(req.TunnelDomain)
		}
	} else if strings.TrimSpace(req.TunnelDomain) != "" {
		updates["tunnel_domain"] = strings.TrimSpace(req.TunnelDomain)
	}

	if len(updates) == 0 {
		sendJSON(w, "error", "没有可更新的字段")
		return
	}

	res := database.DB.Model(&database.NodePool{}).Where("uuid = ?", req.UUID).Updates(updates)
	if res.Error != nil {
		sendJSON(w, "error", "更新失败")
		return
	}
	if res.RowsAffected == 0 {
		sendJSON(w, "error", "节点不存在")
		return
	}

	if err := database.DB.Where("uuid = ?", req.UUID).First(&node).Error; err != nil {
		sendJSON(w, "success", "设置已更新")
		return
	}

	resp := map[string]interface{}{
		"message": "设置已更新",
	}

	if req.TunnelEnabled != nil {
		if service.IsNodeOnline(node.InstallID) {
			if *req.TunnelEnabled {
				payload, err := buildNodeTunnelCommandPayload(node)
				if err != nil {
					sendJSON(w, "error", err.Error())
					return
				}
				commandID, err := service.FireCommandToNode(node.InstallID, "tunnel-start", payload)
				if err != nil {
					sendJSON(w, "error", fmt.Sprintf("隧道启动命令下发失败: %v", err))
					return
				}
				resp["command_id"] = commandID
				resp["message"] = "设置已更新，已下发 tunnel 启动命令"
			} else {
				commandID, err := service.FireCommandToNode(node.InstallID, "tunnel-stop", map[string]interface{}{})
				if err != nil {
					sendJSON(w, "error", fmt.Sprintf("隧道停止命令下发失败: %v", err))
					return
				}
				resp["command_id"] = commandID
				resp["message"] = "设置已更新，已下发 tunnel 停止命令"
			}
		} else {
			if *req.TunnelEnabled {
				resp["message"] = "设置已更新（节点离线，未下发启动命令）"
			} else {
				resp["message"] = "设置已更新（节点离线，未下发停止命令）"
			}
		}
	}

	sendJSON(w, "success", resp)
}

// apiNodeControlTunnelStart 远程启动节点 tunnel
func apiNodeControlTunnelStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, "error", "仅支持 POST 请求")
		return
	}

	var req struct {
		UUID string `json:"uuid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.UUID) == "" {
		sendJSON(w, "error", "参数错误: uuid 不能为空")
		return
	}

	var node database.NodePool
	if err := database.DB.Where("uuid = ?", strings.TrimSpace(req.UUID)).First(&node).Error; err != nil {
		sendJSON(w, "error", "节点不存在")
		return
	}
	if !service.IsNodeOnline(node.InstallID) {
		sendJSON(w, "error", "节点不在线，无法执行命令")
		return
	}
	if !node.TunnelEnabled {
		sendJSON(w, "error", "当前节点未启用 tunnel")
		return
	}

	payload, err := buildNodeTunnelCommandPayload(node)
	if err != nil {
		sendJSON(w, "error", err.Error())
		return
	}

	commandID, err := service.FireCommandToNode(node.InstallID, "tunnel-start", payload)
	if err != nil {
		sendJSON(w, "error", fmt.Sprintf("命令下发失败: %v", err))
		return
	}

	sendJSON(w, "success", map[string]interface{}{
		"command_id": commandID,
		"message":    "启动命令已下发",
	})
}

// apiNodeControlTunnelStop 远程停止节点 tunnel
func apiNodeControlTunnelStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, "error", "仅支持 POST 请求")
		return
	}

	var req struct {
		UUID string `json:"uuid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.UUID) == "" {
		sendJSON(w, "error", "参数错误: uuid 不能为空")
		return
	}

	var node database.NodePool
	if err := database.DB.Where("uuid = ?", strings.TrimSpace(req.UUID)).First(&node).Error; err != nil {
		sendJSON(w, "error", "节点不存在")
		return
	}
	if !service.IsNodeOnline(node.InstallID) {
		sendJSON(w, "error", "节点不在线，无法执行命令")
		return
	}

	commandID, err := service.FireCommandToNode(node.InstallID, "tunnel-stop", map[string]interface{}{})
	if err != nil {
		sendJSON(w, "error", fmt.Sprintf("命令下发失败: %v", err))
		return
	}

	sendJSON(w, "success", map[string]interface{}{
		"command_id": commandID,
		"message":    "停止命令已下发",
	})
}

// apiDeleteTunnelNode 删除（或解绑）节点 tunnel
func apiDeleteTunnelNode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		UUID string `json:"uuid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, "error", "请求格式错误")
		return
	}
	req.UUID = strings.TrimSpace(req.UUID)
	if req.UUID == "" {
		sendJSON(w, "error", "缺少节点 UUID")
		return
	}

	var node database.NodePool
	if err := database.DB.Select("uuid", "install_id", "links", "disabled_links", "tunnel_enabled", "tunnel_id", "tunnel_domain").Where("uuid = ?", req.UUID).First(&node).Error; err != nil {
		sendJSON(w, "error", "节点不存在")
		return
	}

	cleanupResult, err := cleanupNodeTunnelResources(node)
	if err != nil {
		sendJSON(w, "error", err.Error())
		return
	}

	updates := map[string]interface{}{
		"tunnel_enabled": false,
		"tunnel_id":      "",
		"tunnel_domain":  "",
	}
	if err := database.DB.Model(&database.NodePool{}).Where("uuid = ?", node.UUID).Updates(updates).Error; err != nil {
		sendJSON(w, "error", "清理节点 tunnel 设置失败")
		return
	}

	resp := map[string]interface{}{}
	if cleanupResult.StopCommandID != "" {
		resp["command_id"] = cleanupResult.StopCommandID
	}

	tunnelID := strings.TrimSpace(node.TunnelID)
	if tunnelID == "" {
		if cleanupResult.RemovedDNSCount > 0 {
			resp["message"] = fmt.Sprintf("节点未绑定 Tunnel，已清理 %d 条 DNS 记录并完成本地清理", cleanupResult.RemovedDNSCount)
		} else {
			resp["message"] = "节点未绑定 Tunnel，已清理本地配置"
		}
	} else if cleanupResult.SharedTunnel {
		resp["message"] = fmt.Sprintf("已解绑当前节点并清理 %d 条 DNS 记录（该 Tunnel 仍被其他节点使用，未删除远端）", cleanupResult.RemovedDNSCount)
	} else {
		resp["message"] = fmt.Sprintf("已清理 %d 条 DNS 记录，删除远端 Tunnel 并完成节点配置清理", cleanupResult.RemovedDNSCount)
	}

	sendJSON(w, "success", resp)
}

func generateNodeTunnelDomain(nodeUUID string) (string, error) {
	rootDomain := ""
	var cfg database.SysConfig
	if err := database.DB.Where("key = ?", "cf_domain").First(&cfg).Error; err == nil {
		rootDomain = strings.TrimSpace(cfg.Value)
	}
	if rootDomain == "" {
		return "", fmt.Errorf("未配置 Cloudflare 根域名，请先在 Token 管理中保存域名")
	}

	for i := 0; i < 8; i++ {
		prefix, err := generateRandomAlphaNum(6)
		if err != nil {
			return "", err
		}
		domain := strings.ToLower(prefix) + "." + rootDomain

		var count int64
		database.DB.Model(&database.NodePool{}).
			Where("tunnel_domain = ? AND uuid <> ?", domain, nodeUUID).
			Count(&count)
		if count == 0 {
			return domain, nil
		}
	}

	return "", fmt.Errorf("生成 tunnel 域名前缀失败，请重试")
}

func buildNodeTunnelCommandPayload(node database.NodePool) (map[string]interface{}, error) {
	baseDomain := strings.TrimSpace(node.TunnelDomain)
	if baseDomain == "" {
		return nil, fmt.Errorf("当前节点未配置 tunnel 域名")
	}

	tunnelToken := strings.TrimSpace(node.TunnelToken)
	if tunnelToken == "" {
		return nil, fmt.Errorf("该节点未配置 Tunnel Token，请重新启用 Tunnel 以创建专属隧道")
	}

	portMap := loadProxyPortMap()
	routes := make([]map[string]string, 0)

	for proto := range node.Links {
		if sliceContains(node.DisabledLinks, proto) {
			continue
		}
		if !isTunnelCompatibleProtocol(proto) {
			continue
		}
		port, ok := portMap[proto]
		if !ok || port <= 0 {
			continue
		}

		host := buildTunnelHostForProtocol(baseDomain, proto)
		// 传入节点专属的 TunnelID，确保 DNS CNAME 指向节点自己的 Tunnel 而非面板全局 Tunnel
		nodeTunnelID := strings.TrimSpace(node.TunnelID)
		if err := service.EnsureCFTunnelDNSHost(host, nodeTunnelID); err != nil {
			return nil, fmt.Errorf("绑定 tunnel DNS 失败(%s): %w", host, err)
		}

		// TLS 协议（_wst/_hut）的 sing-box 端口启用了 TLS（自签证书），
		// cloudflared 必须使用 https:// 连接，并配合 noTLSVerify 跳过证书验证。
		// 非 TLS 协议（vmess_ws）的 sing-box 端口是纯 HTTP。
		scheme := "http"
		if isTLSBackendProtocol(proto) {
			scheme = "https"
		}

		routes = append(routes, map[string]string{
			"protocol": proto,
			"hostname": host,
			"service":  scheme + "://127.0.0.1:" + strconv.Itoa(port),
		})
	}

	if len(routes) == 0 {
		return nil, fmt.Errorf("该节点暂无可通过 Tunnel 加速的协议。")
	}

	tunnelID := strings.TrimSpace(node.TunnelID)
	if tunnelID == "" {
		return nil, fmt.Errorf("该节点未配置 Tunnel ID，请重新启用 Tunnel 以创建专属隧道")
	}

	payload := map[string]interface{}{
		"base_domain":  baseDomain,
		"tunnel_token": tunnelToken,
		"tunnel_id":    tunnelID,
		"routes":       routes,
	}
	return payload, nil
}

func loadProxyPortMap() map[string]int {
	keyByProto := map[string]string{
		"vmess_ws":   "proxy_port_vmess_ws",
		"vmess_http": "proxy_port_vmess_http",
		"vmess_wst":  "proxy_port_vmess_wst",
		"vmess_hut":  "proxy_port_vmess_hut",
		"vless_wst":  "proxy_port_vless_wst",
		"vless_hut":  "proxy_port_vless_hut",
		"trojan_wst": "proxy_port_trojan_wst",
		"trojan_hut": "proxy_port_trojan_hut",
	}

	keys := make([]string, 0, len(keyByProto))
	for _, key := range keyByProto {
		keys = append(keys, key)
	}

	var cfgs []database.SysConfig
	database.DB.Where("key IN ?", keys).Find(&cfgs)
	valueByKey := make(map[string]string, len(cfgs))
	for _, c := range cfgs {
		valueByKey[c.Key] = strings.TrimSpace(c.Value)
	}

	defaultPort := map[string]int{
		"proxy_port_vmess_ws":   20009,
		"proxy_port_vmess_http": 20010,
		"proxy_port_vmess_wst":  20012,
		"proxy_port_vmess_hut":  20014,
		"proxy_port_vless_wst":  20015,
		"proxy_port_vless_hut":  20017,
		"proxy_port_trojan_wst": 20018,
		"proxy_port_trojan_hut": 20020,
	}

	ports := make(map[string]int, len(keyByProto))
	for proto, key := range keyByProto {
		if p, err := strconv.Atoi(valueByKey[key]); err == nil && p > 0 {
			ports[proto] = p
			continue
		}
		ports[proto] = defaultPort[key]
	}

	return ports
}

// buildTunnelHostForProtocol 生成 Tunnel 协议专属域名。
// baseDomain 格式为 "{随机前缀}.{CF根域名}"，例如 "abc123.example.com"。
// 将协议名拼接到随机前缀后面而非前面，确保结果是 CF 根域名的一级子域名：
//
//	abc123-vmess-wst.example.com  ✅ 被 *.example.com 通配证书覆盖
//
// 而非旧格式的二级子域名：
//
//	vmess-wst.abc123.example.com  ❌ 不被 *.example.com 覆盖，443 端口 TLS 握手失败
func buildTunnelHostForProtocol(baseDomain, protocol string) string {
	base := strings.TrimSpace(strings.TrimSuffix(baseDomain, "."))
	if base == "" {
		return ""
	}
	prefix := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(protocol), "_", "-"))
	if prefix == "" {
		return base
	}
	// 将 "abc123.example.com" 拆分为 subdomain="abc123" + parentDomain="example.com"
	dotIndex := strings.Index(base, ".")
	if dotIndex < 0 {
		// 无法拆分（不含 dot），回退为旧格式
		return prefix + "." + base
	}
	subdomain := base[:dotIndex]
	parentDomain := base[dotIndex+1:]
	// 生成: "abc123-vmess-wst.example.com"
	return subdomain + "-" + prefix + "." + parentDomain
}

// isTunnelCompatibleProtocol 判断协议是否兼容 Cloudflare Tunnel。
// 注意：vmess_http（sing-box "http" transport）不兼容 CF Tunnel，因为 cloudflared
// 作为 HTTP 反向代理会解析 HTTP body，而 VMess 的二进制流会导致
// "unexpected EOF" / "chunked line ends with bare LF" 等解析错误。
// 只有 WebSocket 和 HTTPUpgrade 系列协议能通过 Upgrade 机制让 cloudflared 跳过 HTTP 解析。
func isTunnelCompatibleProtocol(protocol string) bool {
	switch strings.TrimSpace(protocol) {
	case "vmess_ws", "vmess_wst", "vmess_hut", "vless_wst", "vless_hut", "trojan_wst", "trojan_hut":
		return true
	default:
		return false
	}
}

// isTLSBackendProtocol 判断该协议在 sing-box 后端是否启用了 TLS（自签证书）。
// _wst（WebSocket+TLS）和 _hut（HTTP Upgrade+TLS）后端均使用 TLS，
// cloudflared 的 ingress 需要用 https:// 并设置 noTLSVerify 连接。
func isTLSBackendProtocol(protocol string) bool {
	switch strings.TrimSpace(protocol) {
	case "vmess_wst", "vmess_hut", "vless_wst", "vless_hut", "trojan_wst", "trojan_hut":
		return true
	default:
		return false
	}
}

func getSupportedTunnelProtocolsForNode(node database.NodePool) []string {
	if len(node.Links) == 0 {
		return []string{}
	}
	protos := make([]string, 0)
	for proto := range node.Links {
		if sliceContains(node.DisabledLinks, proto) {
			continue
		}
		if isTunnelCompatibleProtocol(proto) {
			protos = append(protos, proto)
		}
	}
	sort.Strings(protos)
	return protos
}

func getNodeTunnelHosts(node database.NodePool) []string {
	baseDomain := strings.TrimSpace(node.TunnelDomain)
	if baseDomain == "" {
		return []string{}
	}

	supported := getSupportedTunnelProtocolsForNode(node)
	if len(supported) == 0 {
		return []string{}
	}

	hostSet := make(map[string]struct{}, len(supported))
	for _, proto := range supported {
		host := buildTunnelHostForProtocol(baseDomain, proto)
		if host == "" {
			continue
		}
		hostSet[host] = struct{}{}
	}

	hosts := make([]string, 0, len(hostSet))
	for host := range hostSet {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

// getAllTunnelCompatibleHostsForNode 获取节点所有 Tunnel 兼容协议的域名（含已禁用的）。
// 用于 DNS 清理场景：确保被禁用协议的 DNS 记录也能被正确删除。
func getAllTunnelCompatibleHostsForNode(node database.NodePool) []string {
	baseDomain := strings.TrimSpace(node.TunnelDomain)
	if baseDomain == "" {
		return []string{}
	}

	hostSet := make(map[string]struct{})
	for proto, link := range node.Links {
		if strings.TrimSpace(link) == "" {
			continue
		}
		if !isTunnelCompatibleProtocol(proto) {
			continue
		}
		host := buildTunnelHostForProtocol(baseDomain, proto)
		if host != "" {
			hostSet[host] = struct{}{}
		}
	}

	hosts := make([]string, 0, len(hostSet))
	for host := range hostSet {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

func cleanupNodeTunnelDNSRecords(node database.NodePool) (int, error) {
	// 使用包含已禁用协议的完整列表，确保不会遗漏之前创建的 DNS 记录
	hosts := getAllTunnelCompatibleHostsForNode(node)
	if len(hosts) == 0 {
		return 0, nil
	}

	removed := 0
	var firstErr error
	for _, host := range hosts {
		if err := service.DeleteCFTunnelDNSHost(host); err != nil {
			logger.Log.Warn("删除 Tunnel DNS 记录失败，继续清理其余记录", "host", host, "error", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("删除 DNS 记录失败(%s): %w", host, err)
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

type nodeTunnelCleanupResult struct {
	RemovedDNSCount int
	SharedTunnel    bool
	DeletedTunnel   bool
	StopCommandID   string
}

func cleanupNodeTunnelResources(node database.NodePool) (nodeTunnelCleanupResult, error) {
	result := nodeTunnelCleanupResult{}
	if service.IsNodeOnline(node.InstallID) && (node.TunnelEnabled || strings.TrimSpace(node.TunnelID) != "" || strings.TrimSpace(node.TunnelDomain) != "") {
		if commandID, err := service.FireCommandToNode(node.InstallID, "tunnel-stop", map[string]interface{}{}); err == nil {
			result.StopCommandID = commandID
		} else {
			logger.Log.Warn("删除前停止节点 Tunnel 失败", "uuid", node.UUID, "install_id", node.InstallID, "error", err)
		}
	}

	removedDNSCount, err := cleanupNodeTunnelDNSRecords(node)
	if err != nil {
		return result, err
	}
	result.RemovedDNSCount = removedDNSCount

	tunnelID := strings.TrimSpace(node.TunnelID)
	if tunnelID == "" {
		return result, nil
	}

	result.SharedTunnel = isTunnelUsedByOtherNodes(node.UUID, tunnelID)
	if result.SharedTunnel {
		return result, nil
	}

	// 使用节点专属 Tunnel 删除函数（不影响面板全局配置）
	if err := service.DeleteNodeDedicatedTunnel(tunnelID); err != nil {
		logger.Log.Warn("删除节点专属 Tunnel 失败", "tunnel_id", tunnelID, "error", err)
		// 不阻塞节点删除流程
	} else {
		result.DeletedTunnel = true
	}
	return result, nil
}

func isTunnelUsedByOtherNodes(currentUUID, tunnelID string) bool {
	tunnelID = strings.TrimSpace(tunnelID)
	if tunnelID == "" {
		return false
	}
	var usedByOthers int64
	database.DB.Model(&database.NodePool{}).
		Where("uuid <> ? AND tunnel_id = ?", currentUUID, tunnelID).
		Count(&usedByOthers)
	return usedByOthers > 0
}

func sliceContains(items []string, target string) bool {
	t := strings.TrimSpace(target)
	for _, item := range items {
		if strings.TrimSpace(item) == t {
			return true
		}
	}
	return false
}

func generateRandomAlphaNum(length int) (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	if length <= 0 {
		return "", fmt.Errorf("invalid random length")
	}
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b), nil
}

func apiReorderNodes(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	if r.Method != http.MethodPost {
		logger.Log.Warn("非法请求方法", "method", r.Method, "ip", clientIP, "path", reqPath)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TargetRoutingType int      `json:"target_routing_type"`
		NodeUUIDs         []string `json:"node_uuids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Log.Warn("解析 JSON 失败", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "请求格式错误")
		return
	}

	if len(req.NodeUUIDs) == 0 {
		sendJSON(w, "success", "排序已更新")
		return
	}

	var oldNodes []database.NodePool
	if err := database.DB.Select("uuid", "name", "routing_type", "sort_index").Where("uuid IN ?", req.NodeUUIDs).Find(&oldNodes).Error; err != nil {
		logger.Log.Error("读取重排前节点状态失败", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "保存排序失败")
		return
	}

	oldByUUID := make(map[string]database.NodePool, len(oldNodes))
	for _, n := range oldNodes {
		oldByUUID[n.UUID] = n
	}

	movedDetails := make([]string, 0)
	for _, uuid := range req.NodeUUIDs {
		if old, ok := oldByUUID[uuid]; ok && old.RoutingType != req.TargetRoutingType {
			name := strings.TrimSpace(old.Name)
			if name == "" {
				name = old.UUID
			}
			movedDetails = append(movedDetails, fmt.Sprintf("节点 %s；节点类型 由 %s 改为 %s", name, nodeRoutingTypeLabel(old.RoutingType), nodeRoutingTypeLabel(req.TargetRoutingType)))
		}
	}

	oldOrder := append([]string(nil), req.NodeUUIDs...)
	sort.Slice(oldOrder, func(i, j int) bool {
		li, lok := oldByUUID[oldOrder[i]]
		lj, rok := oldByUUID[oldOrder[j]]
		if !lok && !rok {
			return oldOrder[i] < oldOrder[j]
		}
		if !lok {
			return false
		}
		if !rok {
			return true
		}
		if li.SortIndex == lj.SortIndex {
			return li.UUID < lj.UUID
		}
		return li.SortIndex < lj.SortIndex
	})

	orderChanged := false
	if len(oldOrder) == len(req.NodeUUIDs) {
		for i := range req.NodeUUIDs {
			if req.NodeUUIDs[i] != oldOrder[i] {
				orderChanged = true
				break
			}
		}
	}

	err := service.ReorderNodes(req.TargetRoutingType, req.NodeUUIDs)
	if err != nil {
		logger.Log.Error("更新排序入库失败", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "保存排序失败")
		return
	}

	if len(movedDetails) > 0 {
		logger.Log.Info("节点更新成功",
			"changed_count", len(movedDetails),
			"changes", strings.Join(movedDetails, " | "),
			"target_group", req.TargetRoutingType,
			"ip", clientIP,
			"path", reqPath,
		)
	} else if orderChanged {
		logger.Log.Info("节点排序更新成功", "target_group", req.TargetRoutingType, "ip", clientIP, "path", reqPath)
	}

	sendJSON(w, "success", "排序已更新")
}

// getNodeDeleteLock 获取指定节点 UUID 的删除互斥锁（懒初始化）。
// 保证同一节点在同一时刻只有一个删除操作能执行，
// 避免用户多次点击导致并发 DELETE 争抢行锁、死锁或连接池耗尽。
func getNodeDeleteLock(uuid string) *sync.Mutex {
	val, _ := nodeDeleteMu.LoadOrStore(uuid, &sync.Mutex{})
	return val.(*sync.Mutex)
}

// apiDeleteNode 处理节点删除请求
// 功能：验证请求参数并从数据库中删除指定 UUID 的节点，支持演示模式下对内置节点的保护与专业拦截提示
func apiDeleteNode(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	if r.Method != http.MethodPost {
		logger.Log.Warn("非法请求方法", "method", r.Method, "ip", clientIP, "path", reqPath)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		UUID string `json:"uuid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Log.Warn("解析 JSON 失败", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "请求格式错误")
		return
	}

	if req.UUID == "" {
		logger.Log.Warn("请求缺少节点 UUID", "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "缺少节点 ID")
		return
	}

	// ── 防并发：同一节点同时只允许一个删除操作 ──
	mu := getNodeDeleteLock(req.UUID)
	if !mu.TryLock() {
		// 已有一个删除请求正在处理，直接返回，避免行锁争抢
		logger.Log.Warn("删除请求被防重复拦截（已有进行中的删除操作）", "uuid", req.UUID, "ip", clientIP)
		sendJSON(w, "error", "该节点正在删除中，请勿重复操作")
		return
	}
	defer func() {
		mu.Unlock()
		// 删除完成后清理 map 条目，避免长期内存泄漏
		nodeDeleteMu.Delete(req.UUID)
	}()

	// 先查询目标节点，便于后续日志记录 name
	var targetNode database.NodePool
	if err := database.DB.Where("uuid = ?", req.UUID).First(&targetNode).Error; err != nil {
		logger.Log.Warn("删除拦截: 节点不存在", "uuid", req.UUID, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "节点不存在")
		return
	}

	// 检查是否处于演示模式 (存在 data/debug/demo 文件)
	demoPath := filepath.Join("data", "debug", "demo")
	if _, err := os.Stat(demoPath); err == nil {
		if targetNode.CreatedAt.Before(AppStartTime) {
			logger.Log.Warn("尝试在演示模式下删除初始节点，已被拦截", "uuid", req.UUID, "name", targetNode.Name, "ip", clientIP)
			sendJSON(w, "error", "【演示模式保护】禁止删除。您可自由测试并删除您自行创建的节点。")
			return
		}
	}

	cleanupResult, err := cleanupNodeTunnelResources(targetNode)
	if err != nil {
		logger.Log.Error("删除节点前清理 Tunnel 资源失败", "error", err, "uuid", req.UUID, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "删除节点前清理 Tunnel 资源失败: "+err.Error())
		return
	}

	// 1. 分批删除关联的流量统计数据，避免一次性删除大量行导致长事务锁表
	//    每批删除 1000 条，每批独立事务，大幅降低锁持有时间和 WAL 压力。
	deleted, batchErr := database.DeleteNodeTrafficStatsBatched(req.UUID, 1000)
	if batchErr != nil {
		logger.Log.Error("分批删除节点流量数据失败", "error", batchErr, "uuid", req.UUID, "deleted_so_far", deleted)
		// 不阻断，继续删除节点本身
	} else if deleted > 0 {
		logger.Log.Info("已分批删除节点流量历史数据", "uuid", req.UUID, "total_deleted", deleted)
	}

	// 2. 删除节点记录
	result := database.DB.Where("uuid = ?", req.UUID).Delete(&database.NodePool{})
	if result.Error != nil {
		logger.Log.Error("删除节点失败", "error", result.Error, "uuid", req.UUID, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "数据库删除失败")
		return
	}

	// 3. 清理内存中的实时状态（Agent 连接、流量缓存、前端订阅）
	service.CleanupNodeState(targetNode.InstallID, targetNode.UUID)
	// 清理内存中可能残存的离线通知配置数据
	service.DeleteNodeNotifyConfig(targetNode.InstallID)
	service.DeleteNodeTrafficThresholdConfig(targetNode.InstallID)

	logger.Log.Info("节点已删除", "uuid", req.UUID, "name", targetNode.Name, "ip", clientIP, "path", reqPath)
	if cleanupResult.RemovedDNSCount > 0 || cleanupResult.DeletedTunnel || cleanupResult.SharedTunnel {
		msg := fmt.Sprintf("节点已删除；已清理 %d 条 Tunnel DNS 记录", cleanupResult.RemovedDNSCount)
		if cleanupResult.SharedTunnel {
			msg += "；共享 Tunnel 已保留"
		} else if cleanupResult.DeletedTunnel {
			msg += "；远端 Tunnel 已删除"
		}
		sendJSON(w, "success", msg)
		return
	}
	sendJSON(w, "success", "节点已删除")
}

// ------------------- [节点控制接口] -------------------

// apiNodeControlCheckAgentUpdate 远程触发 Agent 主动检查更新（异步下发 + 返回 command_id）
// 路由: POST /api/node/control/check-agent-update
// 请求体: { "uuid": "节点UUID" }
func apiNodeControlCheckAgentUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, "error", "仅支持 POST 请求")
		return
	}

	var req struct {
		UUID string `json:"uuid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UUID == "" {
		sendJSON(w, "error", "参数错误: uuid 不能为空")
		return
	}

	var node database.NodePool
	if err := database.DB.Where("uuid = ?", req.UUID).First(&node).Error; err != nil {
		sendJSON(w, "error", "节点不存在")
		return
	}

	if !service.IsNodeOnline(node.InstallID) {
		sendJSON(w, "error", "节点不在线，无法执行命令")
		return
	}

	commandID, err := service.FireCommandToNode(node.InstallID, "check-agent-update", map[string]interface{}{})
	if err != nil {
		logger.Log.Error("Agent 检查更新命令下发失败", "uuid", req.UUID, "error", err)
		sendJSON(w, "error", fmt.Sprintf("命令下发失败: %v", err))
		return
	}

	// 仅在面板手动下发 Agent 更新请求后，抑制该节点 30 秒上下线通知，避免更新重连造成刷屏。
	service.SuppressNodeStatusNotifyForDuration(node.InstallID, 30*time.Second)

	logger.Log.Info("Agent 检查更新命令已下发", "uuid", req.UUID, "command_id", commandID)
	sendJSON(w, "success", map[string]interface{}{
		"command_id": commandID,
		"message":    "命令已下发，Agent 正在检查更新",
	})
}

// apiNodeOnlineStatus 查询节点在线状态
// 路由: GET /api/node/online-status?uuid=...
func apiNodeOnlineStatus(w http.ResponseWriter, r *http.Request) {
	nodeUUID := strings.TrimSpace(r.URL.Query().Get("uuid"))

	if nodeUUID == "" {
		// 返回所有节点状态
		var nodes []database.NodePool
		database.DB.Find(&nodes)

		statusList := make([]map[string]interface{}, 0, len(nodes))
		for _, node := range nodes {
			online := service.IsNodeOnlineForStatus(node.InstallID)
			item := map[string]interface{}{
				"uuid":   node.UUID,
				"name":   node.Name,
				"online": online,
			}
			if live := service.GetNodeLiveState(node.InstallID); live != nil {
				item["rx_rate_bps"] = live.RXRateBps
				item["tx_rate_bps"] = live.TXRateBps
				item["last_live_at"] = live.LastLiveAt.Unix()
				if live.AgentVersion != "" {
					item["agent_version"] = live.AgentVersion
				}
			}
			statusList = append(statusList, item)
		}
		sendJSON(w, "success", map[string]interface{}{
			"nodes": statusList,
		})
		return
	}

	// 查询单个节点
	var node database.NodePool
	if err := database.DB.Where("uuid = ?", nodeUUID).First(&node).Error; err != nil {
		sendJSON(w, "error", "节点不存在")
		return
	}

	online := service.IsNodeOnlineForStatus(node.InstallID)
	result := map[string]interface{}{
		"uuid":   node.UUID,
		"name":   node.Name,
		"online": online,
	}
	if live := service.GetNodeLiveState(node.InstallID); live != nil {
		result["rx_rate_bps"] = live.RXRateBps
		result["tx_rate_bps"] = live.TXRateBps
		result["last_live_at"] = live.LastLiveAt.Unix()
		if live.AgentVersion != "" {
			result["agent_version"] = live.AgentVersion
		}
	}
	sendJSON(w, "success", result)
}

// apiNodeControlStream 命令执行日志 SSE 流式推送
// 路由: GET /api/node/control/stream?command_id=xxx
func apiNodeControlStream(w http.ResponseWriter, r *http.Request) {
	commandID := strings.TrimSpace(r.URL.Query().Get("command_id"))
	if commandID == "" {
		http.Error(w, "缺少 command_id 参数", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "不支持 SSE", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	history, ch, done, unsub := service.SubscribeCommandLog(commandID)
	defer unsub()

	// 如果命令不存在
	if history == nil && ch == nil {
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]string{
			"type": "error", "message": "命令不存在或已过期",
		}))
		flusher.Flush()
		return
	}

	// 先发送历史条目
	for _, entry := range history {
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(entry))
		flusher.Flush()
	}

	// 如果命令已完成，直接结束
	if done {
		return
	}

	// 实时订阅
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(entry))
			flusher.Flush()
			if entry.Type == "result" {
				return
			}
		}
	}
}

// apiNodeControlPushConfig 推送协议配置到 Agent
// 路由: POST /api/node/control/push-config
// 请求体: { "uuid": "节点UUID", "protocols": ["ss", "hy2"], "ports": {"ss": 8388, "hy2": 8443} }
// 功能：将前端编辑的启用协议列表和端口配置推送给在线的 Agent，
// Agent 收到后更新本地协议配置、重新生成 sing-box 配置并重启。
func apiNodeControlPushConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, "error", "仅支持 POST 请求")
		return
	}

	var req struct {
		UUID      string         `json:"uuid"`
		Protocols []string       `json:"protocols"` // 启用的协议列表
		Ports     map[string]int `json:"ports"`     // 协议端口映射
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.UUID) == "" {
		sendJSON(w, "error", "参数错误: uuid 不能为空")
		return
	}

	if len(req.Protocols) == 0 {
		sendJSON(w, "error", "启用的协议列表不能为空")
		return
	}

	// 查找节点
	var node database.NodePool
	if err := database.DB.Where("uuid = ?", strings.TrimSpace(req.UUID)).First(&node).Error; err != nil {
		sendJSON(w, "error", "节点不存在")
		return
	}

	// 检查节点是否在线
	if !service.IsNodeOnline(node.InstallID) {
		sendJSON(w, "error", "节点不在线，无法推送配置")
		return
	}

	// 构造推送配置 payload
	// 面板与 Agent 统一使用下划线格式，无需转换

	// 端口优先使用前端传入的值，其次使用节点数据库中的 link_ports
	mergedPorts := make(map[string]int)
	if node.LinkPorts != nil {
		for k, v := range node.LinkPorts {
			if v > 0 {
				mergedPorts[k] = v
			}
		}
	}
	if req.Ports != nil {
		for k, v := range req.Ports {
			if v > 0 {
				mergedPorts[k] = v
			}
		}
	}

	// 🆕 从 SysConfig 加载自定义 SNI 配置，下发给 Agent
	sniConfig := loadSNIConfig()

	payload := map[string]interface{}{
		"protocols": req.Protocols,
		"ports":     mergedPorts,
		"sni":       sniConfig,
	}

	// 如果节点启用了 Tunnel，附带 Tunnel 配置
	if node.TunnelEnabled {
		if tunnelPayload, e := buildNodeTunnelCommandPayload(node); e == nil {
			payload["tunnel"] = tunnelPayload
		} else {
			logger.Log.Warn("推送配置时跳过 Tunnel 刷新", "uuid", req.UUID, "error", e)
		}
	}

	commandID, err := service.FireCommandToNode(node.InstallID, "push-config", payload)
	if err != nil {
		logger.Log.Error("推送配置命令下发失败", "uuid", req.UUID, "error", err)
		sendJSON(w, "error", fmt.Sprintf("命令下发失败: %v", err))
		return
	}

	logger.Log.Info("推送配置命令已下发",
		"uuid", req.UUID,
		"name", node.Name,
		"command_id", commandID,
		"protocols", req.Protocols,
	)
	sendJSON(w, "success", map[string]interface{}{
		"command_id": commandID,
		"message":    "推送配置命令已下发，Agent 正在应用配置...",
	})
}
