package service

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type RecentLogEntry struct {
	Time      string `json:"time"`
	Level     string `json:"level"`
	LevelCN   string `json:"level_cn"`
	Source    string `json:"source"`
	Message   string `json:"message"`
	MessageCN string `json:"message_cn"`
	Operation string `json:"operation"`
	Raw       string `json:"raw"`
}

var (
	logTimeReg   = regexp.MustCompile(`time="([^"]+)"`)
	logLevelReg  = regexp.MustCompile(`\blevel=([^\s]+)`)
	logSourceReg = regexp.MustCompile(`\bsource=([^\s]+)`)
	logMsgReg    = regexp.MustCompile(`\bmsg=("[^"]*"|[^\s]+)`)

	configKeyCNMap = map[string]string{
		"panel_url":                               "面板地址",
		"sub_token":                               "订阅令牌",
		"sub_custom_name":                         "订阅自定义名称",
		"proxy_port_ss":                           "SS 端口",
		"proxy_port_hy2":                          "Hysteria2 端口",
		"proxy_port_tuic":                         "TUIC 端口",
		"proxy_port_reality":                      "Reality 端口",
		"proxy_port_trojan":                       "Trojan 端口",
		"proxy_port_anytls":                       "AnyTLS 端口",
		"proxy_port_socks5":                       "Socks5 端口",
		"proxy_socks5_user":                       "Socks5 用户名",
		"proxy_socks5_pass":                       "Socks5 密码",
		"proxy_socks5_random_auth":                "Socks5 随机认证",
		"proxy_ss_method":                         "SS 加密方式",
		"pref_use_emoji_flag":                     "国旗 Emoji 显示",
		"pref_ip_strategy":                        "默认 IP 策略",
		"pref_default_install_protocols":          "默认安装协议",
		"pref_traffic_stats_retention_days":       "流量记录保留天数",
		"pref_traffic_point_persist_interval_sec": "实时流量点数据落库间隔",
		"pref_speed_test_file_size":               "测速文件大小",
		"sys_force_http":                          "HTTP 风险放行",
		"sys_log_level":                           "系统日志等级",
		"cf_email":                                "Cloudflare 邮箱",
		"cf_api_key":                              "Cloudflare API Key",
		"cf_domain":                               "Cloudflare 域名",
		"cf_auto_renew":                           "证书自动续签",
		"airport_filter_invalid":                  "过滤失效节点",
		"airport_auto_update_enabled":             "机场订阅自动同步",
		"airport_auto_update_interval_minutes":    "机场订阅同步间隔",
		"tg_bot_enabled":                          "Telegram Bot 开关",
		"tg_bot_token":                            "Telegram Bot Token",
		"tg_bot_whitelist":                        "Telegram 白名单",
		"tg_bot_register_commands":                "Telegram 自动注册命令",
		"tg_bot_socks5_enabled":                   "Telegram Socks5 代理开关",
		"tg_bot_socks5_addr":                      "Telegram Socks5 代理地址",
		"tg_bot_socks5_user":                      "Telegram Socks5 用户名",
		"tg_bot_socks5_pass":                      "Telegram Socks5 密码",
		"clash_proxies_update_interval":           "Clash 代理更新间隔",
		"clash_rules_update_interval":             "Clash 规则更新间隔",
		"clash_public_rules_update_interval":      "Clash 公共规则更新间隔",
		"proxy_hy2_sni":                           "Hysteria2 SNI",
		"proxy_tuic_sni":                          "TUIC SNI",
		"proxy_enable_bbr":                        "BBR 开关",
		"proxy_port_vmess_tcp":                    "VMess TCP 端口",
		"proxy_port_vmess_ws":                     "VMess WS 端口",
		"proxy_port_vmess_http":                   "VMess HTTP 端口",
		"proxy_port_vmess_quic":                   "VMess QUIC 端口",
		"proxy_port_vmess_wst":                    "VMess WSS 端口",
		"proxy_port_vmess_hut":                    "VMess H2 端口",
		"proxy_port_vless_wst":                    "VLESS WSS 端口",
		"proxy_port_vless_hut":                    "VLESS H2 端口",
		"proxy_port_trojan_wst":                   "Trojan WSS 端口",
		"proxy_port_trojan_hut":                   "Trojan H2 端口",
		"proxy_tls_transport_path":                "TLS 传输路径",
		"proxy_vmess_tls_sni":                     "VMess TLS SNI",
		"proxy_vless_tls_sni":                     "VLESS TLS SNI",
		"proxy_trojan_tls_sni":                    "Trojan TLS SNI",
		"proxy_anytls_sni":                        "AnyTLS SNI",
		"auth_cookie_ttl_mode":                    "登录会话有效期",
		"login_ip_retry_window_sec":               "登录失败计数窗口",
		"login_ip_max_retries":                    "登录最大重试次数",
		"login_ip_block_ttl_sec":                  "登录封禁时长",
		"pref_force_protocol_prefix":              "强制协议前缀",
		"tg_login_notify_mode":                    "登录通知模式",
		"tg_speedtest_notify_enabled":             "测速通知开关",
		"tg_threshold_stop_notify_enabled":        "流量阈值通知开关",
		"geo_auto_update":                         "GeoIP 自动更新",
		"mihomo_auto_update":                      "Mihomo 自动更新",
		"agent_startup_silent_update_enabled":     "Agent 静默更新",
	}
)

// GetRecentLogs 读取并解析最近日志，按时间倒序返回。
func GetRecentLogs(limit int) ([]RecentLogEntry, error) {
	if limit <= 0 {
		limit = 120
	}
	if limit > 1000 {
		limit = 1000
	}

	lines, err := readRecentLogLines(filepath.Join("data", "logs", "nodectl.log"), limit)
	if err != nil {
		return nil, err
	}

	entries := make([]RecentLogEntry, 0, len(lines))
	for i := len(lines) - 1; i >= 0; i-- {
		entries = append(entries, parseRecentLogLine(lines[i]))
	}

	return entries, nil
}

func readRecentLogLines(logPath string, limit int) ([]string, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lines := make([]string, 0, limit)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}

	return lines, nil
}

func parseRecentLogLine(line string) RecentLogEntry {
	attrs := parseLogAttrs(line)

	msg := extractLogField(logMsgReg, line)
	if msg == "" {
		msg = attrs["msg"]
	}
	msg = decodeLogValue(msg)

	level := strings.ToUpper(extractLogField(logLevelReg, line))
	if level == "" {
		level = "INFO"
	}

	messageCN := buildMessageCN(msg, attrs)
	if messageCN == "" {
		messageCN = translateLogMessage(msg)
	}
	messageCN = enrichLogMessageWithContext(messageCN, msg, attrs)

	entry := RecentLogEntry{
		Time:      extractLogField(logTimeReg, line),
		Level:     level,
		LevelCN:   levelToCN(level),
		Source:    extractLogField(logSourceReg, line),
		Message:   msg,
		MessageCN: messageCN,
		Operation: summarizeLogOperation(msg, attrs),
		Raw:       line,
	}

	if entry.MessageCN == "" {
		entry.MessageCN = msg
	}

	return entry
}

func parseLogAttrs(line string) map[string]string {
	attrs := make(map[string]string)
	n := len(line)
	i := 0

	for i < n {
		for i < n && line[i] == ' ' {
			i++
		}
		if i >= n {
			break
		}

		keyStart := i
		for i < n && line[i] != '=' && line[i] != ' ' {
			i++
		}
		if i >= n || line[i] != '=' {
			for i < n && line[i] != ' ' {
				i++
			}
			continue
		}

		key := strings.TrimSpace(line[keyStart:i])
		i++ // skip '='

		if key == "" {
			continue
		}

		val := ""
		if i < n && line[i] == '"' {
			start := i
			i++
			escaped := false
			for i < n {
				if line[i] == '\\' && !escaped {
					escaped = true
					i++
					continue
				}
				if line[i] == '"' && !escaped {
					i++
					break
				}
				escaped = false
				i++
			}
			val = decodeLogValue(line[start:i])
		} else {
			start := i
			for i < n && line[i] != ' ' {
				i++
			}
			val = decodeLogValue(line[start:i])
		}

		if _, exists := attrs[key]; !exists {
			attrs[key] = val
		}
	}

	return attrs
}

func extractLogField(reg *regexp.Regexp, line string) string {
	matches := reg.FindStringSubmatch(line)
	if len(matches) < 2 {
		return ""
	}
	return matches[1]
}

func decodeLogValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "\"") && strings.HasSuffix(v, "\"") {
		if unquoted, err := strconv.Unquote(v); err == nil {
			return unquoted
		}
	}
	return v
}

func levelToCN(level string) string {
	switch strings.ToUpper(level) {
	case "DEBUG":
		return "调试"
	case "WARN", "WARNING":
		return "警告"
	case "ERROR":
		return "错误"
	default:
		return "信息"
	}
}

func summarizeLogOperation(msg string, attrs map[string]string) string {
	path := strings.TrimSpace(attrs["path"])
	if strings.HasPrefix(path, "/api/airport/") {
		return "机场订阅"
	}
	if strings.HasPrefix(path, "/api/clash/") {
		return "Clash规则集"
	}

	changesRaw := strings.TrimSpace(attrs["changes"])
	if changesRaw == "" {
		changesRaw = strings.TrimSpace(attrs["changed"])
	}

	if changesRaw != "" {
		if strings.Contains(msg, "系统全局配置已更新") {
			if isMySubscriptionChange(changesRaw) {
				return "我的订阅"
			}
		}
		if strings.Contains(msg, "Clash") {
			return "Clash规则集"
		}
		if strings.Contains(msg, "机场订阅") || strings.Contains(msg, "订阅信息") || strings.Contains(msg, "添加订阅") || strings.Contains(msg, "删除订阅") || strings.Contains(msg, "更新订阅") || strings.Contains(msg, "同步订阅") {
			return "机场订阅"
		}
		if strings.Contains(msg, "自定义分流") || strings.Contains(msg, "自定义路由") {
			return "自定义分流"
		}
		if strings.Contains(msg, "系统全局配置已更新") {
			if isSingboxConfigChange(changesRaw) {
				return "SingBox配置"
			}
		}
		if strings.Contains(msg, "系统") || strings.Contains(msg, "配置") || strings.Contains(msg, "设置") {
			return "系统配置"
		}
		if strings.Contains(msg, "节点") {
			return "节点管理"
		}
		if strings.Contains(msg, "Clash") {
			return "Clash配置变更"
		}
	}

	if msg == "" {
		return "系统日志"
	}

	switch {
	case strings.Contains(msg, "HTTP 服务告警"):
		return "系统日志"
	case strings.Contains(msg, "链接转换"):
		return "链接转换"
	case strings.Contains(msg, "成功下发 Clash 订阅模板") || strings.Contains(msg, "成功下发 V2Ray Base64 订阅") || strings.Contains(msg, "成功下发 Raw"):
		return "我的订阅"
	case strings.Contains(msg, "Clash"):
		return "Clash规则集"
	case strings.Contains(msg, "机场订阅") || strings.Contains(msg, "订阅信息") || strings.Contains(msg, "添加订阅") || strings.Contains(msg, "删除订阅") || strings.Contains(msg, "更新订阅") || strings.Contains(msg, "同步订阅"):
		return "机场订阅"
	case strings.Contains(msg, "自定义分流") || strings.Contains(msg, "自定义路由"):
		return "自定义分流"
	case strings.Contains(msg, "登录"):
		return "用户登录"
	case strings.Contains(msg, "退出"):
		return "用户退出"
	case strings.Contains(msg, "节点"):
		return "节点管理"
	case strings.Contains(msg, "证书") || strings.Contains(msg, "ACME"):
		return "证书管理"
	case strings.Contains(msg, "GeoIP"):
		return "GeoIP"
	case strings.Contains(msg, "Mihomo"):
		return "Mihomo"
	case strings.Contains(msg, "重启"):
		return "系统重启"
	case strings.Contains(msg, "配置") || strings.Contains(msg, "设置"):
		return "配置变更"
	case strings.Contains(msg, "订阅"):
		return "机场订阅"
	default:
		return "系统日志"
	}
}

func isMySubscriptionChange(changesRaw string) bool {
	parts := strings.Split(changesRaw, " | ")
	if len(parts) == 0 {
		return false
	}

	total := 0
	subCount := 0
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		total++

		key := p
		if idx := strings.Index(p, ":"); idx >= 0 {
			key = strings.TrimSpace(p[:idx])
		}

		if key == "sub_custom_name" || key == "sub_token" {
			subCount++
		}
	}

	return total > 0 && subCount == total
}

func enrichLogMessageWithContext(messageCN, rawMsg string, attrs map[string]string) string {
	ip := strings.TrimSpace(attrs["ip"])

	if strings.Contains(rawMsg, "Agent 版本已记录") || strings.Contains(rawMsg, "Agent 版本已更新") {
		nodeName := strings.TrimSpace(attrs["node_name"])
		agentVersion := strings.TrimSpace(attrs["agent_version"])
		oldVersion := strings.TrimSpace(attrs["old_version"])
		newVersion := strings.TrimSpace(attrs["new_version"])

		if nodeName == "" {
			nodeName = "unknown"
		}

		if strings.Contains(rawMsg, "Agent 版本已记录") {
			parts := []string{fmt.Sprintf("节点 %s", nodeName), "Agent 版本已记录"}
			if agentVersion != "" {
				parts = append(parts, "版本: "+agentVersion)
			}
			return strings.Join(parts, "；")
		}

		parts := []string{fmt.Sprintf("节点 %s", nodeName), "Agent 版本已更新"}
		if oldVersion != "" && newVersion != "" {
			parts = append(parts, fmt.Sprintf("版本: %s -> %s", oldVersion, newVersion))
		} else if newVersion != "" {
			parts = append(parts, "版本: "+newVersion)
		}
		return strings.Join(parts, "；")
	}

	if strings.Contains(rawMsg, "未授权访问拦截") {
		reason := strings.TrimSpace(attrs["reason"])
		path := strings.TrimSpace(attrs["path"])
		method := strings.TrimSpace(attrs["method"])
		userAgent := strings.TrimSpace(attrs["user_agent"])

		parts := make([]string, 0, 6)
		parts = append(parts, "未授权访问拦截")
		if reason != "" {
			parts = append(parts, "原因: "+reason)
		}
		if method != "" {
			parts = append(parts, "方法: "+method)
		}
		if path != "" {
			parts = append(parts, "路径: "+path)
		}
		if userAgent != "" {
			parts = append(parts, "UA: "+userAgent)
		}
		if ip != "" {
			parts = append(parts, "IP: "+ip)
		}

		return strings.Join(parts, "；")
	}

	if strings.Contains(rawMsg, "HTTP 服务告警") {
		reason := strings.TrimSpace(attrs["reason"])
		hint := strings.TrimSpace(attrs["security_hint"])
		method := strings.TrimSpace(attrs["method"])
		path := strings.TrimSpace(attrs["path"])

		parts := make([]string, 0, 7)
		parts = append(parts, "HTTP 服务告警")
		if reason != "" {
			parts = append(parts, "原因: "+reason)
		}
		if method != "" {
			parts = append(parts, "方法: "+method)
		}
		if path != "" {
			parts = append(parts, "路径: "+path)
		}
		if hint != "" {
			parts = append(parts, "说明: "+hint)
		}
		if ip != "" {
			parts = append(parts, "IP: "+ip)
		}

		return strings.Join(parts, "；")
	}

	// 接收到节点协议上报：显示具体协议名称
	if strings.Contains(rawMsg, "接收到节点协议上报") {
		nodeName := strings.TrimSpace(attrs["name"])
		protocol := strings.TrimSpace(attrs["protocol"])
		if nodeName == "" {
			nodeName = "unknown"
		}
		if protocol != "" {
			return fmt.Sprintf("节点 %s；上报协议 %s", nodeName, protocol)
		}
		return fmt.Sprintf("节点 %s；接收到协议上报", nodeName)
	}

	// 接收到节点 IP 上报：显示 IPv4/IPv6/双栈信息
	if strings.Contains(rawMsg, "接收到节点 IP 上报") {
		nodeName := strings.TrimSpace(attrs["name"])
		ipv4 := strings.TrimSpace(attrs["ipv4"])
		ipv6 := strings.TrimSpace(attrs["ipv6"])
		if nodeName == "" {
			nodeName = "unknown"
		}
		var stackType string
		switch {
		case ipv4 != "" && ipv6 != "":
			stackType = "双栈"
		case ipv4 != "":
			stackType = "仅 IPv4"
		case ipv6 != "":
			stackType = "仅 IPv6"
		}
		if stackType != "" {
			return fmt.Sprintf("节点 %s；上报 IP（%s）", nodeName, stackType)
		}
		return fmt.Sprintf("节点 %s；接收到 IP 上报", nodeName)
	}

	// Agent 推送间隔批量下发：显示推送间隔、成功/失败数量
	if strings.Contains(rawMsg, "Agent 推送间隔已批量下发") {
		intervalSec := strings.TrimSpace(attrs["interval_sec"])
		successCount := strings.TrimSpace(attrs["success"])
		failCount := strings.TrimSpace(attrs["fail"])
		parts := []string{"Agent 推送间隔已批量下发"}
		if intervalSec != "" {
			parts = append(parts, "间隔: "+intervalSec+" 秒")
		}
		if successCount != "" {
			parts = append(parts, "成功: "+successCount+" 个节点")
		}
		if failCount != "" && failCount != "0" {
			parts = append(parts, "失败: "+failCount+" 个节点")
		}
		return strings.Join(parts, "；")
	}

	if strings.Contains(rawMsg, "节点添加成功") || strings.Contains(rawMsg, "节点已删除") {
		nodeName := strings.TrimSpace(attrs["name"])
		nodeUUID := strings.TrimSpace(attrs["uuid"])
		if nodeName == "" {
			nodeName = "unknown"
		}

		if strings.Contains(rawMsg, "节点添加成功") {
			if nodeUUID != "" {
				return fmt.Sprintf("添加节点 %s（UUID: %s）", nodeName, nodeUUID)
			}
			return fmt.Sprintf("添加节点 %s", nodeName)
		}

		if nodeUUID != "" {
			return fmt.Sprintf("删除节点 %s（UUID: %s）", nodeName, nodeUUID)
		}
		return fmt.Sprintf("删除节点 %s", nodeName)
	}

	if strings.Contains(rawMsg, "节点") {
		nodeName := strings.TrimSpace(attrs["name"])
		groupRaw := strings.TrimSpace(attrs["routing_type"])
		if groupRaw == "" {
			groupRaw = strings.TrimSpace(attrs["target_group"])
		}
		groupName := nodeGroupToCN(groupRaw)

		prefix := make([]string, 0, 2)
		if groupName != "" {
			prefix = append(prefix, "分组 "+groupName)
		}
		if nodeName != "" {
			prefix = append(prefix, "节点 "+nodeName)
		}

		if len(prefix) > 0 {
			base := strings.TrimSpace(messageCN)
			if base == "" {
				base = strings.TrimSpace(rawMsg)
			}
			return strings.Join(prefix, "；") + "；" + base
		}
	}

	if strings.Contains(rawMsg, "链接转换失败") {
		nodeName := strings.TrimSpace(attrs["node_name"])
		if nodeName == "" {
			nodeName = "unknown"
		}
		protocol := strings.TrimSpace(attrs["protocol"])
		if protocol == "" {
			protocol = "unknown"
		}
		reason := strings.TrimSpace(attrs["reason"])
		if reason == "" {
			reason = "unknown"
		}
		return fmt.Sprintf("节点 %s；失败协议 %s；原因 %s", nodeName, protocol, reason)
	}

	// 对于包含"失败"的消息，附加 error 属性以便前端展示具体失败原因
	errAttr := strings.TrimSpace(attrs["error"])
	if errAttr != "" && strings.Contains(rawMsg, "失败") {
		base := strings.TrimSpace(messageCN)
		if base == "" {
			base = rawMsg
		}
		if !strings.Contains(base, errAttr) {
			if ip != "" {
				return fmt.Sprintf("%s；错误: %s；IP: %s", base, errAttr, ip)
			}
			return fmt.Sprintf("%s；错误: %s", base, errAttr)
		}
	}

	if ip == "" {
		return messageCN
	}

	if shouldAppendIPForSecurityLog(rawMsg) {
		if strings.Contains(messageCN, "IP:") || strings.Contains(messageCN, "请求IP") {
			return messageCN
		}
		return fmt.Sprintf("%s；IP: %s", strings.TrimSpace(messageCN), ip)
	}

	if strings.Contains(rawMsg, "成功下发 Clash 订阅模板") || strings.Contains(rawMsg, "成功下发 V2Ray Base64 订阅") {
		if strings.Contains(messageCN, "请求IP") {
			return messageCN
		}
		return fmt.Sprintf("%s；请求IP: %s", strings.TrimSpace(messageCN), ip)
	}

	// Agent WS 相关日志追加 IP
	if strings.HasPrefix(rawMsg, "Agent WS ") {
		nodeName := strings.TrimSpace(attrs["node_name"])
		if nodeName != "" {
			if strings.Contains(messageCN, "节点") {
				if strings.Contains(messageCN, "IP:") {
					return messageCN
				}
				if ip != "" {
					return fmt.Sprintf("%s；IP: %s", strings.TrimSpace(messageCN), ip)
				}
				return messageCN
			}

			if ip != "" {
				return fmt.Sprintf("节点 %s；%s；IP: %s", nodeName, strings.TrimSpace(messageCN), ip)
			}
			return fmt.Sprintf("节点 %s；%s", nodeName, strings.TrimSpace(messageCN))
		}

		if strings.Contains(messageCN, "IP:") {
			return messageCN
		}
		return fmt.Sprintf("%s；IP: %s", strings.TrimSpace(messageCN), ip)
	}

	return messageCN
}

func shouldAppendIPForSecurityLog(rawMsg string) bool {
	rawMsg = strings.TrimSpace(rawMsg)
	if rawMsg == "" {
		return false
	}

	return strings.Contains(rawMsg, "未授权访问拦截") ||
		strings.Contains(rawMsg, "管理员登录成功") ||
		strings.Contains(rawMsg, "登录拦截") ||
		strings.Contains(rawMsg, "HTTP 服务告警") ||
		strings.Contains(rawMsg, "Token 校验失败") ||
		strings.Contains(rawMsg, "Token 校验警告") ||
		strings.Contains(rawMsg, "鉴权系统异常") ||
		strings.Contains(rawMsg, "安装脚本请求被拦截") ||
		strings.Contains(rawMsg, "非法请求方法")
}

func nodeGroupToCN(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}

	switch v {
	case "0":
		return "禁用"
	case "1":
		return "中转"
	case "2":
		return "落地"
	case "3":
		return "屏蔽"
	default:
		return v
	}
}

func isSingboxConfigChange(changesRaw string) bool {
	parts := strings.Split(changesRaw, " | ")
	if len(parts) == 0 {
		return false
	}

	total := 0
	singboxCount := 0

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		total++

		key := p
		if idx := strings.Index(p, ":"); idx >= 0 {
			key = strings.TrimSpace(p[:idx])
		}

		if isSingboxConfigKey(key) {
			singboxCount++
		}
	}

	return total > 0 && singboxCount == total
}

func isSingboxConfigKey(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}

	if strings.HasPrefix(key, "proxy_") {
		return true
	}

	switch key {
	case "pref_default_install_protocols", "默认安装协议新增", "默认安装协议删除", "默认安装协议", "pref_ip_strategy":
		return true
	default:
		return false
	}
}

func buildMessageCN(msg string, attrs map[string]string) string {
	changesRaw := strings.TrimSpace(attrs["changes"])
	if changesRaw == "" {
		changesRaw = strings.TrimSpace(attrs["changed"])
	}
	if changesRaw == "" {
		return ""
	}

	formatted := formatChangesCN(changesRaw)
	if formatted == "" {
		return ""
	}

	switch {
	case strings.Contains(msg, "系统全局配置已更新"):
		return formatted
	case strings.Contains(msg, "节点更新成功"):
		return formatted
	case strings.Contains(msg, "Clash 模板模块设置已更新"):
		return formatted
	case strings.Contains(msg, "自定义分流规则已更新"):
		return formatted
	case strings.Contains(msg, "机场订阅") || strings.Contains(msg, "机场节点测速"):
		return formatted
	default:
		if msg != "" {
			return msg + "：" + formatted
		}
		return formatted
	}
}

func formatChangesCN(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	parts := strings.Split(raw, " | ")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		s := formatChangePartCN(strings.TrimSpace(p))
		if s != "" {
			result = append(result, s)
		}
	}

	if len(result) == 0 {
		return ""
	}
	if len(result) > 12 {
		return strings.Join(result[:12], "；") + fmt.Sprintf("；等 %d 项", len(result))
	}

	return strings.Join(result, "；")
}

func formatChangePartCN(part string) string {
	if part == "" {
		return ""
	}

	if strings.HasPrefix(part, "新增:") || strings.HasPrefix(part, "移除:") || strings.HasPrefix(part, "仅顺序变化") || strings.HasPrefix(part, "无变化") {
		return part
	}

	idx := strings.Index(part, ":")
	if idx < 0 {
		return part
	}

	key := strings.TrimSpace(part[:idx])
	content := strings.TrimSpace(part[idx+1:])

	if key == "默认安装协议新增" || key == "默认安装协议删除" {
		if content == "" {
			return key
		}
		return key + " " + content
	}

	if key == "新增规则集" || key == "移除规则集" {
		if content == "" {
			return key
		}
		return key + " " + content
	}

	if strings.HasPrefix(key, "disabled_links_add") {
		if content == "" {
			return "禁用协议已新增"
		}
		return "新增禁用协议: " + content
	}
	if strings.HasPrefix(key, "disabled_links_remove") {
		if content == "" {
			return "禁用协议已移除"
		}
		return "移除禁用协议: " + content
	}

	if strings.HasPrefix(key, "links[") && strings.HasSuffix(key, "]") {
		proto := key[len("links[") : len(key)-1]
		switch content {
		case "added":
			return fmt.Sprintf("协议 %s 已新增", proto)
		case "removed":
			return fmt.Sprintf("协议 %s 已移除", proto)
		case "updated":
			return fmt.Sprintf("协议 %s 链接已更新", proto)
		default:
			return fmt.Sprintf("协议 %s 变更: %s", proto, content)
		}
	}

	if strings.HasPrefix(key, "link_ports[") && strings.HasSuffix(key, "]") {
		proto := key[len("link_ports[") : len(key)-1]
		if strings.HasPrefix(content, "add ") {
			return fmt.Sprintf("协议 %s 端口新增为 %s", proto, strings.TrimSpace(strings.TrimPrefix(content, "add ")))
		}
		if content == "removed" {
			return fmt.Sprintf("协议 %s 端口已移除", proto)
		}
		if strings.Contains(content, " -> ") {
			pair := strings.SplitN(content, " -> ", 2)
			return fmt.Sprintf("协议 %s 端口由 %s 改为 %s", proto, strings.TrimSpace(pair[0]), strings.TrimSpace(pair[1]))
		}
		return fmt.Sprintf("协议 %s 端口变更: %s", proto, content)
	}

	if strings.HasPrefix(key, "link_ip_modes[") && strings.HasSuffix(key, "]") {
		proto := key[len("link_ip_modes[") : len(key)-1]
		if strings.HasPrefix(content, "add ") {
			return fmt.Sprintf("协议 %s IP 模式新增为 %s", proto, ipModeToCN(strings.TrimSpace(strings.TrimPrefix(content, "add "))))
		}
		if content == "removed" {
			return fmt.Sprintf("协议 %s IP 模式已移除", proto)
		}
		if strings.Contains(content, " -> ") {
			pair := strings.SplitN(content, " -> ", 2)
			return fmt.Sprintf("协议 %s IP 模式由 %s 改为 %s", proto, ipModeToCN(strings.TrimSpace(pair[0])), ipModeToCN(strings.TrimSpace(pair[1])))
		}
		return fmt.Sprintf("协议 %s IP 模式变更: %s", proto, content)
	}

	if strings.Contains(content, " -> ") {
		pair := strings.SplitN(content, " -> ", 2)
		oldV := formatValueCN(key, strings.TrimSpace(pair[0]))
		newV := formatValueCN(key, strings.TrimSpace(pair[1]))
		return fmt.Sprintf("%s 由 %s 改为 %s", fieldToCN(key), oldV, newV)
	}

	return fmt.Sprintf("%s 变更为 %s", fieldToCN(key), formatValueCN(key, content))
}

func fieldToCN(key string) string {
	if v, ok := configKeyCNMap[key]; ok {
		return v
	}
	switch key {
	case "name":
		return "节点名称"
	case "routing_type":
		return "节点类型"
	case "is_blocked":
		return "屏蔽状态"
	case "ipv4":
		return "IPv4 地址"
	case "ipv6":
		return "IPv6 地址"
	case "ip_mode":
		return "IP 模式"
	case "reset_day":
		return "重置日"
	case "traffic_limit":
		return "流量限制"
	case "traffic_limit_type":
		return "流量限额类型"
	default:
		return key
	}
}

func ipModeToCN(v string) string {
	v = strings.TrimSpace(v)
	switch v {
	case "0":
		return "跟随系统"
	case "1":
		return "仅 IPv4"
	case "2":
		return "仅 IPv6"
	case "3":
		return "双栈"
	default:
		return v
	}
}

func formatValueCN(key, val string) string {
	val = strings.TrimSpace(val)
	if val == "" || val == "<empty>" {
		return "空"
	}
	if val == "********" {
		return "已隐藏"
	}

	switch key {
	case "traffic_limit_type":
		return TrafficLimitTypeCN(val)
	case "sys_log_level":
		switch strings.ToLower(val) {
		case "debug":
			return "调试"
		case "info":
			return "信息"
		case "warn", "warning":
			return "警告"
		case "error":
			return "错误"
		}
	case "routing_type":
		switch val {
		case "0":
			return "禁用"
		case "1":
			return "中转"
		case "2":
			return "落地"
		case "3":
			return "屏蔽"
		}
	case "ip_mode", "pref_ip_strategy":
		return ipModeToCN(val)
	}

	lower := strings.ToLower(val)
	if lower == "true" {
		return "开启"
	}
	if lower == "false" {
		return "关闭"
	}

	return val
}

func translateLogMessage(msg string) string {
	if msg == "" {
		return ""
	}

	replacer := strings.NewReplacer(
		"service started", "服务已启动",
		"server started", "服务已启动",
		"restarting", "正在重启",
		"restart", "重启",
		"updated", "已更新",
		"update", "更新",
		"failed", "失败",
		"success", "成功",
		"warning", "警告",
		"error", "错误",
		"not found", "未找到",
		"forbidden", "禁止访问",
		"timeout", "超时",
	)

	translated := replacer.Replace(msg)

	if translated != msg {
		return translated
	}

	lower := strings.ToLower(msg)
	if strings.Contains(lower, "fail") {
		return "操作失败: " + msg
	}
	if strings.Contains(lower, "success") {
		return "操作成功: " + msg
	}
	if strings.Contains(lower, "start") {
		return "系统启动相关: " + msg
	}
	if strings.Contains(lower, "login") {
		return "登录相关: " + msg
	}

	return msg
}
