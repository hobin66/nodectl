package service

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"nodectl/internal/database"
	"nodectl/internal/logger"

	"gopkg.in/yaml.v3"
)

// 手动同步、后台同步和删除共用同一把锁，避免删除期间重建订阅节点。
var airportSubscriptionSyncMu sync.Mutex

// DeleteAirportSubscription 串行删除关联节点和订阅本身。
func DeleteAirportSubscription(subID string) error {
	airportSubscriptionSyncMu.Lock()
	defer airportSubscriptionSyncMu.Unlock()

	tx := database.DB.Begin()
	// 1. 删除关联的节点
	if err := tx.Where("sub_id = ?", subID).Delete(&database.AirportNode{}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("删除节点失败: %w", err)
	}
	// 2. 删除订阅本身
	if err := tx.Where("id = ?", subID).Delete(&database.AirportSub{}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("删除订阅失败: %w", err)
	}
	return tx.Commit().Error
}

// SyncAirportSubscription 执行订阅更新核心逻辑
func SyncAirportSubscription(subID string) error {
	airportSubscriptionSyncMu.Lock()
	defer airportSubscriptionSyncMu.Unlock()

	var sub database.AirportSub
	if err := database.DB.First(&sub, "id = ?", subID).Error; err != nil {
		return err
	}

	// 定义一个闭包函数，用于复用流量解析逻辑
	parseTraffic := func(userInfo string) {
		if userInfo == "" {
			return
		}
		parts := strings.Split(userInfo, ";")
		for _, part := range parts {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) == 2 {
				val, _ := strconv.ParseInt(kv[1], 10, 64)
				switch strings.ToLower(kv[0]) {
				case "upload":
					sub.Upload = val
				case "download":
					sub.Download = val
				case "total":
					sub.Total = val
				case "expire":
					sub.Expire = val
				}
			}
		}
	}

	client := &http.Client{Timeout: 15 * time.Second}

	// 1. 第一次请求：伪装成 Clash，专门骗取流量信息 Header
	reqClash, err := http.NewRequest("GET", sub.URL, nil)
	if err == nil {
		reqClash.Header.Set("User-Agent", "Clash/1.18.0")
		if respClash, err := client.Do(reqClash); err == nil {
			parseTraffic(respClash.Header.Get("Subscription-Userinfo"))
			respClash.Body.Close() // 只要 Header，直接丢弃容易解析失败的 YAML Body
		}
	}

	// 2. 第二次请求：伪装成 v2rayN，强制获取最稳定的 Base64 节点数据
	reqV2ray, err := http.NewRequest("GET", sub.URL, nil)
	if err != nil {
		return err
	}
	reqV2ray.Header.Set("User-Agent", "v2rayN/6.31")

	respV2ray, err := client.Do(reqV2ray)
	if err != nil {
		return err
	}
	defer respV2ray.Body.Close()

	// 兜底机制：如果第一步 Clash 意外没拿到流量，尝试用本次 v2rayN 请求的 Header 补救
	if sub.Total == 0 {
		parseTraffic(respV2ray.Header.Get("Subscription-Userinfo"))
	}

	bodyBytes, err := io.ReadAll(respV2ray.Body)
	if err != nil {
		return fmt.Errorf("读取机场订阅响应失败: %w", err)
	}
	content := string(bodyBytes)

	// 2. 解析节点链接 (自动识别 Base64 或 Clash)
	var newLinks []NodeItem
	if isClashYAML(content) {
		newLinks = parseClashYAML(bodyBytes)
	} else {
		newLinks = parseBase64Sub(content)
	}

	if len(newLinks) == 0 {
		logger.Log.Warn("订阅未解析到任何节点", "sub_name", sub.Name)
		return fmt.Errorf("机场订阅 %q 未解析到任何节点", sub.Name)
	}

	// 3. 获取旧节点状态 (Name -> RoutingType 映射)
	var oldNodes []database.AirportNode
	database.DB.Where("sub_id = ?", subID).Find(&oldNodes)
	statusMap := make(map[string]int)
	for _, node := range oldNodes {
		if node.RoutingType != 0 {
			statusMap[node.Name] = node.RoutingType
		}
	}

	// 4. 事务更新：删除旧节点 -> 插入新节点 (保留状态)

	var filterConf database.SysConfig
	filterInvalid := false
	if err := database.DB.Where("key = ?", "airport_filter_invalid").First(&filterConf).Error; err == nil {
		filterInvalid = (filterConf.Value == "true")
	}

	tx := database.DB.Begin()

	if err := tx.Where("sub_id = ?", subID).Delete(&database.AirportNode{}).Error; err != nil {
		tx.Rollback()
		return err
	}

	var nodesToInsert []database.AirportNode
	for i, item := range newLinks {
		// 【核心修复】：严格遵循用户偏好！
		// 只要用户开启了过滤开关，且节点被鉴定为无效（包含结构错误或广告），
		// 直接 continue 丢弃，绝对不加入 nodesToInsert，从而彻底不写入数据库！
		if filterInvalid && isInvalidNode(item.Name, item.Server, item.Port) {
			logger.Log.Debug("根据用户偏好拦截无效节点，不写入数据库", "name", item.Name)
			continue
		}

		routingType := 0
		if oldStatus, ok := statusMap[item.Name]; ok {
			routingType = oldStatus
		}

		nodesToInsert = append(nodesToInsert, database.AirportNode{
			SubID:         subID,
			Name:          item.Name,
			Protocol:      item.Protocol,
			Link:          item.Link,
			OriginalIndex: i,
			RoutingType:   routingType,
		})
	}

	// 4. 批量只插入有效的节点
	if len(nodesToInsert) > 0 {
		if err := tx.Create(&nodesToInsert).Error; err != nil {
			tx.Rollback()
			return err
		}
	}

	// 5. 将更新后的流量信息和时间保存到数据库
	// 显式指定模型和 ID，确保流量数据绝对能被精确更新
	if err := tx.Model(&database.AirportSub{}).Where("id = ?", subID).Updates(map[string]interface{}{
		"upload":     sub.Upload,
		"download":   sub.Download,
		"total":      sub.Total,
		"expire":     sub.Expire,
		"updated_at": time.Now(),
	}).Error; err != nil {
		tx.Rollback()
		return err
	}

	// 6. 提交事务 (这一步非常重要，不提交则上面的节点和流量都不会保存)
	if err := tx.Commit().Error; err != nil {
		return fmt.Errorf("提交机场订阅更新失败: %w", err)
	}
	return nil
}

// NodeItem 临时解析结构
type NodeItem struct {
	Name     string
	Protocol string
	Link     string
	Server   string
	Port     int
}

// ------------------- [无效节点过滤引擎] -------------------
// isInvalidNode 判断节点是否为虚假广告/无效信息节点 (名称+结构多维鉴定)
func isInvalidNode(name string, server string, port int) bool {
	// 1. [结构级防伪] 端口非法
	if port <= 0 || port > 65535 {
		return true
	}

	// 2. [结构级防伪] 拦截常见的虚假占位 IP
	dummyServers := map[string]bool{
		"127.0.0.1": true, "0.0.0.0": true, "localhost": true,
		"1.1.1.1": true, "8.8.8.8": true, "1.0.0.1": true,
		"114.114.114.114": true,
	}
	if dummyServers[server] {
		return true
	}

	// 3. [正则防伪] 匹配日期范围特征，拦截如: "2.17 - 2.19"
	datePattern := regexp.MustCompile(`\d{1,4}[-./]\d{1,2}\s*[-~至]\s*\d{1,4}[-./]\d{1,2}`)
	if datePattern.MatchString(name) {
		return true
	}

	// 4. [正则防伪] 拦截名称中包含域名或 IP 地址的占位节点
	ipPattern := regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`)
	if ipPattern.MatchString(name) {
		return true
	}

	domainPattern := regexp.MustCompile(`(?i)[a-zA-Z0-9-]+\.(com|cn|net|org|xyz|io|me|top|pw|cc|info|biz|tv|ws|co|uk)`)
	if domainPattern.MatchString(name) {
		return true
	}

	// 5. [常规防伪] 广告及到期提示关键字
	keywords := []string{
		"到期", "剩余", "流量", "过期", "官网", "套餐", "重置",
		"更新", "群", "广告", "折", "优惠", "福利", "公告",
		"时间", "地址", "频道", "失联", "防联",
	}
	for _, kw := range keywords {
		if strings.Contains(name, kw) {
			return true
		}
	}
	return false
}

// ------------------- [名称获取与清洗引擎] -------------------

// FetchSubscriptionName 尝试从订阅链接响应头获取名称
func FetchSubscriptionName(subURL string) string {
	client := &http.Client{Timeout: 8 * time.Second}
	req, err := http.NewRequest("GET", subURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "Clash/1.18.0")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if title := resp.Header.Get("Profile-Title"); title != "" {
		if decoded, err := url.QueryUnescape(title); err == nil {
			return decoded
		}
		return title
	}

	cd := resp.Header.Get("Content-Disposition")
	if cd != "" {
		if strings.Contains(cd, "filename*=") {
			parts := strings.Split(cd, "filename*=")
			if len(parts) > 1 {
				val := strings.TrimSpace(parts[1])
				if strings.HasPrefix(strings.ToLower(val), "utf-8''") {
					val = val[7:]
				}
				if decoded, err := url.PathUnescape(val); err == nil {
					return strings.TrimSuffix(decoded, ".yaml")
				}
			}
		} else if strings.Contains(cd, "filename=") {
			parts := strings.Split(cd, "filename=")
			if len(parts) > 1 {
				name := strings.Trim(strings.TrimSpace(parts[1]), `"; `)
				return strings.TrimSuffix(name, ".yaml")
			}
		}
	}

	return ""
}

// cleanNodeName 清理节点名称，去除首尾和开头的无意义符号
func cleanNodeName(name string) string {
	name = strings.TrimSpace(name)
	// 去除开头的无意义符号（横杠、下划线、竖线、星号等）以及它们附带的空格(含全角)
	name = strings.TrimLeft(name, "-_|=+* \t　")
	name = strings.TrimSpace(name)
	if name == "" {
		return "未命名节点"
	}
	return name
}

// ------------------- [解析与转换逻辑] -------------------

func isClashYAML(content string) bool {
	return strings.Contains(content, "proxies:")
}

// parseBase64Sub 解析传统 Base64 订阅
func parseBase64Sub(content string) []NodeItem {
	decoded := safeBase64Decode(content) // 复用 links.go 中的 safeBase64Decode
	lines := strings.Split(strings.ReplaceAll(decoded, "\r\n", "\n"), "\n")
	var result []NodeItem

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		node := ParseLinkToClashNode(line, "")
		if node != nil {
			cleanName := cleanNodeName(node.Name)
			result = append(result, NodeItem{
				Name:     cleanName,
				Protocol: node.Type,
				Link:     line,
				Server:   node.Server, // 提取服务器地址供鉴定
				Port:     node.Port,   // 提取端口供鉴定
			})
		}
	}
	return result
}

// parseClashYAML 解析 Clash YAML 并转换回分享链接
func parseClashYAML(content []byte) []NodeItem {
	var raw struct {
		Proxies []map[string]interface{} `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(content, &raw); err != nil {
		logger.Log.Error("解析 Clash 订阅 YAML 失败", "error", err)
		return nil
	}

	var result []NodeItem
	for _, p := range raw.Proxies {
		name := getString(p["name"])
		server := getString(p["server"])
		if name == "" || server == "" {
			continue
		}

		cleanName := cleanNodeName(name)
		link := convertClashProxyToLink(p, cleanName)

		if link != "" {
			result = append(result, NodeItem{
				Name:     cleanName,
				Protocol: getString(p["type"]),
				Link:     link,
				Server:   server,            // 已在上方提取，直接传入
				Port:     getInt(p["port"]), // 直接提取端口
			})
		}
	}
	return result
}

// convertClashProxyToLink 核心转换器：Clash Map -> 标准 URI
func convertClashProxyToLink(p map[string]interface{}, cleanName string) string {
	protoType := getString(p["type"])
	server := getString(p["server"])
	port := getInt(p["port"])
	if port == 0 {
		return ""
	}

	switch protoType {
	case "ss":
		cipher := getString(p["cipher"])
		password := getString(p["password"])
		auth := base64.StdEncoding.EncodeToString([]byte(cipher + ":" + password))
		return fmt.Sprintf("ss://%s@%s:%d#%s", auth, server, port, url.PathEscape(cleanName))

	case "trojan":
		password := getString(p["password"])
		sni := getString(p["sni"])
		network := getString(p["network"])

		link := fmt.Sprintf("trojan://%s@%s:%d?sni=%s", password, server, port, sni)
		if network != "" {
			link += "&type=" + network
			if wsOpts, ok := p["ws-opts"].(map[string]interface{}); ok {
				if path := getString(wsOpts["path"]); path != "" {
					link += "&path=" + url.QueryEscape(path)
				}
				if headers, ok := wsOpts["headers"].(map[string]interface{}); ok {
					if host := getString(headers["Host"]); host != "" {
						link += "&host=" + url.QueryEscape(host)
					}
				}
			}
		}
		return link + "#" + url.PathEscape(cleanName)

	case "vmess":
		uuid := getString(p["uuid"])
		cipher := getString(p["cipher"])
		if cipher == "" {
			cipher = "auto"
		}
		network := getString(p["network"])
		if network == "" {
			network = "tcp"
		}

		vj := map[string]interface{}{
			"v":    "2",
			"ps":   cleanName,
			"add":  server,
			"port": port,
			"id":   uuid,
			"aid":  getInt(p["alterId"]),
			"scy":  cipher,
			"net":  network,
			"type": "none",
			"host": "",
			"path": "",
			"tls":  "",
			"sni":  "",
		}

		if getBool(p["tls"]) {
			vj["tls"] = "tls"
			if sni := getString(p["servername"]); sni != "" {
				vj["sni"] = sni
			}
		}

		if network == "ws" {
			if wsOpts, ok := p["ws-opts"].(map[string]interface{}); ok {
				if path := getString(wsOpts["path"]); path != "" {
					vj["path"] = path
				}
				if headers, ok := wsOpts["headers"].(map[string]interface{}); ok {
					if host := getString(headers["Host"]); host != "" {
						vj["host"] = host
					}
				}
			}
		} else if network == "grpc" {
			if grpcOpts, ok := p["grpc-opts"].(map[string]interface{}); ok {
				if svc := getString(grpcOpts["grpc-service-name"]); svc != "" {
					vj["path"] = svc
				}
			}
		}

		jsonBytes, _ := json.Marshal(vj)
		return "vmess://" + base64.StdEncoding.EncodeToString(jsonBytes)

	case "vless":
		uuid := getString(p["uuid"])
		network := getString(p["network"])

		link := fmt.Sprintf("vless://%s@%s:%d?encryption=none", uuid, server, port)

		if network != "" {
			link += "&type=" + network
		}

		if getBool(p["tls"]) {
			link += "&security=tls"
			if sni := getString(p["servername"]); sni != "" {
				link += "&sni=" + sni
			}
		}

		if ropts, ok := p["reality-opts"].(map[string]interface{}); ok {
			link += "&security=reality"
			if pbk := getString(ropts["public-key"]); pbk != "" {
				link += "&pbk=" + pbk
			}
			if sid := getString(ropts["short-id"]); sid != "" {
				link += "&sid=" + sid
			}
			if fp := getString(p["client-fingerprint"]); fp != "" {
				link += "&fp=" + fp
			}
		}

		return link + "#" + url.PathEscape(cleanName)

	default:
		// 暂不支持或不需要的协议
		return ""
	}
}

// ------------------- [安全类型断言助手] -------------------

func getInt(v interface{}) int {
	switch val := v.(type) {
	case int:
		return val
	case float64:
		return int(val)
	case int64:
		return int(val)
	default:
		return 0
	}
}

func getString(v interface{}) string {
	if val, ok := v.(string); ok {
		return val
	}
	return ""
}

func getBool(v interface{}) bool {
	if val, ok := v.(bool); ok {
		return val
	}
	return false
}
