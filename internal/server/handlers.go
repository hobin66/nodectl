package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"nodectl/internal/database"
	"nodectl/internal/service"
)

// AppStartTime 记录后端程序启动的确切时间
var AppStartTime = time.Now()

// ------------------- [通用辅助函数] -------------------

// getClientIP 从请求中提取真实客户端 IP（支持反向代理场景）
// 优先级: X-Real-IP > X-Forwarded-For 第一个 > RemoteAddr
func getClientIP(r *http.Request) string {
	// 1. 优先使用 X-Real-IP（Nginx 常用）
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	// 2. 其次取 X-Forwarded-For 的第一个 IP（多级代理场景）
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if ip := strings.TrimSpace(strings.Split(xff, ",")[0]); ip != "" {
			return ip
		}
	}
	// 3. 兜底使用 RemoteAddr，并去掉端口号
	ip := r.RemoteAddr
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		// 处理 IPv6 带方括号的情况 [::1]:port
		if bracketIdx := strings.LastIndex(ip, "]"); bracketIdx != -1 {
			return strings.Trim(ip[:bracketIdx+1], "[]")
		}
		return ip[:idx]
	}
	return ip
}

// sendJSON 辅助函数：智能返回 JSON 响应
// payload 可以是 string (作为 message) 或 map (作为数据合并)
func sendJSON(w http.ResponseWriter, status string, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")

	// 1. 初始化基础响应结构
	response := map[string]interface{}{
		"status": status,
	}

	// 2. 根据 payload 的类型智能处理
	switch v := payload.(type) {
	case string:
		// 如果传入的是字符串，自动放入 "message" 字段 (兼容旧代码)
		response["message"] = v
	case map[string]interface{}:
		// 如果传入的是 Map，将其字段合并到顶层 JSON 中
		for k, val := range v {
			response[k] = val
		}
	default:
		// 其他情况作为 data 字段返回
		response["data"] = v
	}

	json.NewEncoder(w).Encode(response)
}

func parseConfigListValue(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "<empty>" {
		return nil
	}

	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		var arr []string
		if err := json.Unmarshal([]byte(raw), &arr); err == nil {
			out := make([]string, 0, len(arr))
			for _, v := range arr {
				v = strings.TrimSpace(v)
				if v != "" {
					out = append(out, v)
				}
			}
			return out
		}
	}

	raw = strings.TrimPrefix(raw, "[")
	raw = strings.TrimSuffix(raw, "]")
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		v := strings.Trim(strings.TrimSpace(p), `"`)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func summarizeListDelta(oldList, newList []string) (added []string, removed []string, reordered bool) {
	oldSet := make(map[string]struct{}, len(oldList))
	newSet := make(map[string]struct{}, len(newList))

	for _, v := range oldList {
		if v != "" {
			oldSet[v] = struct{}{}
		}
	}
	for _, v := range newList {
		if v != "" {
			newSet[v] = struct{}{}
		}
	}

	for _, v := range newList {
		if _, ok := oldSet[v]; !ok {
			added = append(added, v)
		}
	}
	for _, v := range oldList {
		if _, ok := newSet[v]; !ok {
			removed = append(removed, v)
		}
	}

	if len(added) == 0 && len(removed) == 0 && strings.Join(oldList, ",") != strings.Join(newList, ",") {
		reordered = true
	}

	return added, removed, reordered
}

func normalizeCustomRuleLines(raw string) []string {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		if _, ok := seen[line]; ok {
			continue
		}
		seen[line] = struct{}{}
		out = append(out, line)
	}

	return out
}

func diffCustomRuleLines(oldLines, newLines []string) (added []string, removed []string) {
	oldSet := make(map[string]struct{}, len(oldLines))
	newSet := make(map[string]struct{}, len(newLines))

	for _, v := range oldLines {
		oldSet[v] = struct{}{}
	}
	for _, v := range newLines {
		newSet[v] = struct{}{}
	}

	for _, v := range newLines {
		if _, ok := oldSet[v]; !ok {
			added = append(added, v)
		}
	}
	for _, v := range oldLines {
		if _, ok := newSet[v]; !ok {
			removed = append(removed, v)
		}
	}

	return added, removed
}

func limitJoinedValues(values []string, max int) string {
	if len(values) == 0 {
		return ""
	}
	if max <= 0 || len(values) <= max {
		return strings.Join(values, ",")
	}
	return strings.Join(values[:max], ",") + fmt.Sprintf(" 等%d项", len(values))
}

func customGroupName(rule service.CustomProxyRule) string {
	name := strings.TrimSpace(rule.Name)
	if name != "" {
		return name
	}
	if strings.TrimSpace(rule.ID) != "" {
		return rule.ID
	}
	return "未命名分组"
}

func airportRoutingTypeLabel(rt int) string {
	switch rt {
	case 1:
		return "中转"
	case 2:
		return "落地"
	default:
		return "禁用"
	}
}

func nodeRoutingTypeLabel(rt int) string {
	switch rt {
	case 0:
		return "禁用"
	case 1:
		return "中转"
	case 2:
		return "落地"
	default:
		return "未知"
	}
}

func normalizeAuthCookieTTLMode(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	switch raw {
	case "1d", "3d", "7d", "never":
		return raw
	default:
		return "1d"
	}
}

func loadAuthCookieTTLMode() string {
	var cfg database.SysConfig
	if err := database.DB.Where("key = ?", "auth_cookie_ttl_mode").First(&cfg).Error; err != nil {
		return "1d"
	}
	return normalizeAuthCookieTTLMode(cfg.Value)
}

func resolveAuthCookieTTL(mode string) (expiresAt time.Time, maxAge int, persistent bool) {
	now := time.Now()
	switch normalizeAuthCookieTTLMode(mode) {
	case "3d":
		return now.Add(72 * time.Hour), 72 * 3600, true
	case "7d":
		return now.Add(7 * 24 * time.Hour), 7 * 24 * 3600, true
	case "never":
		return now.AddDate(20, 0, 0), 20 * 365 * 24 * 3600, true
	default:
		return now.Add(24 * time.Hour), 24 * 3600, true
	}
}

// mustJSON 辅助：序列化为 JSON 字符串，出错时返回空对象
func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
