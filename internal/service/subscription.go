package service

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"nodectl/internal/database"
	"nodectl/internal/logger"

	"gopkg.in/yaml.v3"
)

// ClashProvider 是用于生成 0.yaml / 1.yaml 的根结构
type ClashProvider struct {
	Proxies []*ClashNode `yaml:"proxies"`
}

// GenerateRawNodesYAML 动态生成指定路由类型的节点 YAML
// routingType: 1=中转，2=落地；0=禁用不下发。
func GenerateRawNodesYAML(routingType int, useFlag bool) (string, error) {
	var nodes []database.NodePool
	// 按照 SortIndex 排序获取节点
	if err := database.DB.Where("routing_type = ? AND is_blocked = ?", routingType, false).
		Order("sort_index ASC").Find(&nodes).Error; err != nil {
		logger.Log.Error("从数据库获取 Raw 节点列表失败", "error", err, "routing_type", routingType)
		return "", err
	}

	var strategyConfig database.SysConfig
	database.DB.Where("key = ?", "pref_ip_strategy").First(&strategyConfig)
	ipStrategy := strategyConfig.Value
	if ipStrategy == "" {
		ipStrategy = "ipv4_prefer"
	}

	var forcePrefixConfig database.SysConfig
	database.DB.Where("key = ?", "pref_force_protocol_prefix").First(&forcePrefixConfig)
	forcePrefix := forcePrefixConfig.Value == "true"

	var proxyList []*ClashNode

	for i := range nodes {
		node := &nodes[i]
		if shouldExcludeNodeByTrafficThreshold(node) {
			continue
		}
		enabledProtocolCount := 0
		for p, l := range node.Links {
			if strings.TrimSpace(l) == "" {
				continue
			}
			if !contains(node.DisabledLinks, p) && shouldIncludeProtocolInSubscription(*node, p) {
				enabledProtocolCount++
			}
		}

		for proto, link := range node.Links {
			if strings.TrimSpace(link) == "" {
				continue // Agent 尚未上报该协议链接，跳过
			}
			if contains(node.DisabledLinks, proto) {
				continue
			}
			if !shouldIncludeProtocolInSubscription(*node, proto) {
				continue
			}

			// Get protocol-level IP mode (default 3: dual-stack if not mapped)
			protoIPMode := 3
			if m, ok := node.LinkIPModes[proto]; ok && m != 0 {
				protoIPMode = m
			}
			ipOptions := determineIPs(*node, ipStrategy, protoIPMode)

			// 根据 IP 策略可能生成 1 个，也可能生成 2 个(双栈)，也可能跳过(0个)
			for _, ipOpt := range ipOptions {
				var baseName string
				if forcePrefix || enabledProtocolCount > 1 {
					baseName = fmt.Sprintf("%s-%s%s", strings.ToLower(proto), node.Name, ipOpt.Suffix)
				} else {
					baseName = fmt.Sprintf("%s%s", node.Name, ipOpt.Suffix)
				}
				proxyNode := ParseProxyLink(link, baseName, node.Region, useFlag)
				if proxyNode != nil {
					targetHost := resolveNodeAccelerateHost(*node, proto, ipOpt.IP)
					if targetHost != "" {
						proxyNode.Server = targetHost // 覆盖 Clash 解析后的 Server IP / 域名
						if shouldUseTunnelForSubscription(*node, proto) {
							proxyNode.Port = resolveTunnelEdgePortForProto(proto, proxyNode.Port)
							applyTunnelHostToClashNode(proxyNode, targetHost)
							// 全局优选 IP：开启后用 Top1 IP 替换 Server，SNI 保持 Tunnel 域名
							if IsApplyToTunnelNodesEnabled() {
								if top1IP, err := GetTop1IPOptIP(); err == nil && top1IP != "" {
									proxyNode.Server = top1IP
									// SNI 已在 applyTunnelHostToClashNode 中设置为 tunnel 域名，保持不变
								}
							}
						}
					}
					proxyList = append(proxyList, proxyNode)
				}
			}
		}
	}

	// 注入机场订阅节点 (Airport Nodes)
	var airportNodes []database.AirportNode
	// 根据 routingType (1=中转，2=落地) 获取启用的机场节点，并按订阅源和原始顺序排序
	if err := database.DB.Where("routing_type = ?", routingType).
		Order("sub_id, original_index ASC").Find(&airportNodes).Error; err == nil {

		for _, anode := range airportNodes {
			// 解析机场原始链接 (vmess://, ss:// 等) 为 Clash 对象
			// 注意：这里不需要 suffix，直接使用原始链接解析
			proxyNode := ParseLinkToClashNode(anode.Link, "")

			if proxyNode != nil {
				// 强制使用数据库中保存的名称 (用户可能重命名过，且用于去重/管理)
				proxyNode.Name = anode.Name

				// 机场节点通常自带 IP 或域名，直接加入列表
				proxyList = append(proxyList, proxyNode)
			}
		}
	}

	// 注入自定义节点 (Custom Nodes)
	var customNodes []database.CustomNode
	// routingType: 1=中转，2=落地；禁用(0)的不加入订阅
	if err := database.DB.Where("routing_type = ?", routingType).
		Order("created_at ASC").Find(&customNodes).Error; err == nil {

		for _, cnode := range customNodes {
			proxyNode := ParseLinkToClashNode(cnode.Link, "")
			if proxyNode != nil {
				// 使用链接中解析出的原始名称（不做转化）
				if cnode.Name != "" && cnode.Name != "自定义节点" {
					proxyNode.Name = cnode.Name
				}
				proxyList = append(proxyList, proxyNode)
			}
		}
	}

	// 如果遍历完数据库后，proxyList 依然为空（说明用户没配节点，或节点都被禁用了）
	// 强制插入一个 "直连" 类型的占位节点，防止客户端报错，并实现自动直连
	if len(proxyList) == 0 {
		var name string
		if routingType == 1 {
			name = "⚠️ 无中转-自动直连"
		} else {
			name = "⚠️ 无落地-自动直连"
		}

		fallbackNode := &ClashNode{
			Name:              name,
			Type:              "direct", // 使用 direct 类型，Clash 会将其视为直连
			UDP:               true,
			ClientFingerprint: "chrome", // 随便带个指纹让它看起来更像正常节点
		}
		proxyList = append(proxyList, fallbackNode)
	}

	provider := ClashProvider{Proxies: proxyList}

	// 1. 使用 Encoder 设置缩进为 2 空格 (解决默认4空格的问题)
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&provider); err != nil {
		logger.Log.Error("YAML 序列化节点数据失败", "error", err, "routing_type", routingType)
		return "", err
	}
	encoder.Close()

	yamlStr := buf.String()

	// 正则匹配 \U0001F1ED 这种 8 位的 Unicode 逃义符并将其转换回真实的 Emoji
	re := regexp.MustCompile(`\\U([0-9A-Fa-f]{8})`)
	yamlStr = re.ReplaceAllStringFunc(yamlStr, func(s string) string {
		// s 格式为 "\U0001F1ED"，提取后面的 16 进制部分
		code, _ := strconv.ParseInt(s[2:], 16, 32)
		return string(rune(code))
	})

	return yamlStr, nil
}

// 辅助函数：检查切片是否包含某个元素
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// GenerateV2RaySubBase64 生成通用 Base64 订阅 (包含中转和落地)
func GenerateV2RaySubBase64(useFlag bool) (string, error) {
	var nodes []database.NodePool
	// 取出中转(1)和落地(2)的节点，排除被禁用的
	if err := database.DB.Where("routing_type IN ? AND is_blocked = ?", []int{1, 2}, false).
		Order("sort_index ASC").Find(&nodes).Error; err != nil {
		logger.Log.Error("从数据库获取全量聚合节点失败", "error", err)
		return "", err
	}

	var strategyConfig database.SysConfig
	database.DB.Where("key = ?", "pref_ip_strategy").First(&strategyConfig)
	ipStrategy := strategyConfig.Value
	if ipStrategy == "" {
		ipStrategy = "ipv4_prefer"
	}

	var forcePrefixConfig database.SysConfig
	database.DB.Where("key = ?", "pref_force_protocol_prefix").First(&forcePrefixConfig)
	forcePrefix := forcePrefixConfig.Value == "true"

	var lines []string

	for i := range nodes {
		node := &nodes[i]
		if shouldExcludeNodeByTrafficThreshold(node) {
			continue
		}
		enabledProtocolCount := 0
		for p, l := range node.Links {
			if strings.TrimSpace(l) == "" {
				continue
			}
			if !contains(node.DisabledLinks, p) && shouldIncludeProtocolInSubscription(*node, p) {
				enabledProtocolCount++
			}
		}

		for proto, link := range node.Links {
			if strings.TrimSpace(link) == "" {
				continue // Agent 尚未上报该协议链接，跳过
			}
			if contains(node.DisabledLinks, proto) {
				continue
			}
			if !shouldIncludeProtocolInSubscription(*node, proto) {
				continue
			}

			// Get protocol-level IP mode (default 3: dual-stack if not mapped)
			protoIPMode := 3
			if m, ok := node.LinkIPModes[proto]; ok && m != 0 {
				protoIPMode = m
			}
			ipOptions := determineIPs(*node, ipStrategy, protoIPMode)

			for _, ipOpt := range ipOptions {
				var baseName string
				if forcePrefix || enabledProtocolCount > 1 {
					baseName = fmt.Sprintf("%s-%s%s", strings.ToLower(proto), node.Name, ipOpt.Suffix)
				} else {
					baseName = fmt.Sprintf("%s%s", node.Name, ipOpt.Suffix)
				}
				finalName := baseName
				if useFlag && node.Region != "" {
					flag := getEmojiFlag(node.Region)
					finalName = fmt.Sprintf("%s %s", flag, strings.ReplaceAll(baseName, flag, ""))
				}
				finalName = strings.TrimSpace(finalName)

				cleanLink := strings.Split(link, "#")[0]

				// 核心：使用刚才写的替换引擎，重构链接
				targetHost := resolveNodeAccelerateHost(*node, proto, ipOpt.IP)
				targetLink := ReplaceLinkIP(cleanLink, targetHost)
				if shouldUseTunnelForSubscription(*node, proto) {
					targetLink = ReplaceLinkPort(targetLink, resolveTunnelEdgePortForProto(proto, 0))
					targetLink = ReplaceLinkSNIAndHost(targetLink, targetHost)
					// 全局优选 IP：开启后用 Top1 IP 替换 Server，SNI 保持 Tunnel 域名
					if IsApplyToTunnelNodesEnabled() {
						if top1IP, err := GetTop1IPOptIP(); err == nil && top1IP != "" {
							targetLink = ReplaceLinkIP(targetLink, top1IP)
						}
					}
				}

				// VMess 命名必须写回 JSON 的 ps 字段，避免部分 V2Ray 客户端对 #fragment 兼容性差
				if strings.HasPrefix(strings.ToLower(targetLink), "vmess://") {
					lines = append(lines, RenameNodeLink(targetLink, finalName))
					continue
				}

				safeName := strings.ReplaceAll(url.QueryEscape(finalName), "+", "%20")
				lines = append(lines, fmt.Sprintf("%s#%s", targetLink, safeName))
			}
		}
	}

	// 注入机场订阅节点 (Airport Nodes)
	var airportNodes []database.AirportNode
	// 获取所有启用的机场节点 (包括中转1 和 落地2)
	if err := database.DB.Where("routing_type IN ?", []int{1, 2}).
		Order("sub_id, original_index ASC").Find(&airportNodes).Error; err == nil {

		for _, anode := range airportNodes {
			// 核心：使用重命名引擎，确保订阅出来的链接名称与数据库中一致
			// RenameNodeLink 函数位于 links.go 中
			finalLink := RenameNodeLink(anode.Link, anode.Name)

			// 追加到订阅列表
			lines = append(lines, finalLink)
		}
	}

	// 注入自定义节点 (Custom Nodes) - V2Ray 订阅中，只要不是屏蔽(0)的都加入
	var customNodes []database.CustomNode
	if err := database.DB.Where("routing_type IN ?", []int{1, 2}).
		Order("created_at ASC").Find(&customNodes).Error; err == nil {

		for _, cnode := range customNodes {
			// 直接使用原始链接，保留链接中的名称
			lines = append(lines, strings.TrimSpace(cnode.Link))
		}
	}

	// 用换行符拼接并进行 Base64 编码
	rawStr := strings.Join(lines, "\n")
	b64Str := base64.StdEncoding.EncodeToString([]byte(rawStr))

	return b64Str, nil
}

// resolveNodeAccelerateHost 返回 Tunnel 加速时使用的域名。
// 域名格式与 buildTunnelHostForProtocol 保持一致，使用扁平化一级子域名：
//
//	abc123-vmess-wst.example.com  ✅ 被 *.example.com 通配证书覆盖
func resolveNodeAccelerateHost(node database.NodePool, proto string, fallbackIP string) string {
	if !node.TunnelEnabled {
		return fallbackIP
	}
	tunnelDomain := strings.TrimSpace(node.TunnelDomain)
	if tunnelDomain == "" {
		return fallbackIP
	}
	if !isTunnelCompatibleProtocolForSub(proto) {
		return fallbackIP
	}
	prefix := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(proto), "_", "-"))
	if prefix == "" {
		return tunnelDomain
	}
	// 将 "abc123.example.com" 拆分为 subdomain="abc123" + parentDomain="example.com"
	dotIndex := strings.Index(tunnelDomain, ".")
	if dotIndex < 0 {
		return prefix + "." + tunnelDomain
	}
	subdomain := tunnelDomain[:dotIndex]
	parentDomain := tunnelDomain[dotIndex+1:]
	// 生成: "abc123-vmess-wst.example.com"
	return subdomain + "-" + prefix + "." + parentDomain
}

// isTunnelCompatibleProtocolForSub 判断该协议是否能走 Tunnel 加速。
// vmess_http（sing-box "http" transport）不兼容 CF Tunnel：cloudflared 作为 HTTP
// 反向代理会解析 HTTP body，VMess 二进制流导致 "unexpected EOF" 错误。
func isTunnelCompatibleProtocolForSub(proto string) bool {
	switch strings.TrimSpace(proto) {
	case "vmess_ws", "vmess_wst", "vmess_hut", "vless_wst", "vless_hut", "trojan_wst", "trojan_hut":
		return true
	default:
		return false
	}
}

func shouldIncludeProtocolInSubscription(node database.NodePool, proto string) bool {
	if !node.TunnelEnabled {
		return true
	}
	if strings.TrimSpace(node.TunnelDomain) == "" {
		return true
	}
	return isTunnelCompatibleProtocolForSub(proto)
}

func shouldExcludeNodeByTrafficThreshold(node *database.NodePool) bool {
	if node == nil {
		return false
	}
	used := ComputeTrafficUsedByLimitType(node.TrafficUp, node.TrafficDown, node.TrafficLimitType)
	return CheckAndHandleNodeTrafficThreshold(node, used, "subscription")
}

func shouldUseTunnelForSubscription(node database.NodePool, proto string) bool {
	if !node.TunnelEnabled {
		return false
	}
	if strings.TrimSpace(node.TunnelDomain) == "" {
		return false
	}
	return isTunnelCompatibleProtocolForSub(proto)
}

func resolveTunnelEdgePortForProto(proto string, fallback int) int {
	switch strings.TrimSpace(proto) {
	case "vmess_ws":
		return 80
	default:
		return 443
	}
}

func applyTunnelHostToClashNode(node *ClashNode, host string) {
	if node == nil || strings.TrimSpace(host) == "" {
		return
	}
	node.ServerName = host
	node.SNI = host

	if node.WSOpts != nil {
		headers, ok := node.WSOpts["headers"].(map[string]string)
		if !ok || headers == nil {
			headers = map[string]string{}
		}
		headers["Host"] = host
		node.WSOpts["headers"] = headers
	}

	if node.HTTPOpts != nil {
		h, ok := node.HTTPOpts["headers"].(map[string]interface{})
		if !ok || h == nil {
			h = map[string]interface{}{}
		}
		h["Host"] = []string{host}
		node.HTTPOpts["headers"] = h
	}
}

type IPOption struct {
	IP     string
	Suffix string // 用于双栈分离时给节点名加后缀
}

func DetermineIPsForTest(node database.NodePool, strategy string, protoIPMode int) []IPOption {
	return determineIPs(node, strategy, protoIPMode)
}

// determineIPs 根据策略计算应该生成哪些 IP
func determineIPs(node database.NodePool, strategy string, protoIPMode int) []IPOption {
	hasV4 := node.IPV4 != ""
	hasV6 := node.IPV6 != ""

	if !hasV4 && !hasV6 {
		return []IPOption{{IP: "", Suffix: ""}} // 无IP记录，使用原链接IP
	}

	// 1: IPv4, 2: IPv6, 3: Dual
	effectiveNodeMode := 0

	// Node IPMode: 0: Follow System, 1: IPv4 Only, 2: IPv6 Only, 3: Dual Stack
	switch node.IPMode {
	case 1:
		effectiveNodeMode = 1
	case 2:
		effectiveNodeMode = 2
	case 3:
		effectiveNodeMode = 3
	default: // 0 Follow System
		switch strategy {
		case "ipv4_only":
			effectiveNodeMode = 1
		case "ipv6_only":
			effectiveNodeMode = 2
		case "dual_stack":
			effectiveNodeMode = 3
		case "ipv4_prefer":
			effectiveNodeMode = 4
		case "ipv6_prefer":
			effectiveNodeMode = 5
		default:
			effectiveNodeMode = 4 // Default to ipv4_prefer
		}
	}

	// Now apply the matrix based on effectiveNodeMode and protoIPMode
	genV4 := false
	genV6 := false

	switch effectiveNodeMode {
	case 1: // Node Effect: IPv4 Only
		if protoIPMode == 1 || protoIPMode == 3 {
			genV4 = true
		}
	case 2: // Node Effect: IPv6 Only
		if protoIPMode == 2 || protoIPMode == 3 {
			genV6 = true
		}
	case 3: // Node Effect: Dual Stack
		if protoIPMode == 1 || protoIPMode == 3 {
			genV4 = true
		}
		if protoIPMode == 2 || protoIPMode == 3 {
			genV6 = true
		}
	case 4: // Node Effect: System IPv4 Prefer
		if protoIPMode == 1 || protoIPMode == 3 {
			genV4 = true
		} else if protoIPMode == 2 {
			genV6 = true
		}
	case 5: // Node Effect: System IPv6 Prefer
		if protoIPMode == 1 {
			genV4 = true
		} else if protoIPMode == 2 || protoIPMode == 3 {
			genV6 = true
		}
	}

	var ips []IPOption
	if genV4 && genV6 {
		if hasV4 && hasV6 {
			ips = append(ips, IPOption{IP: node.IPV4})
			ips = append(ips, IPOption{IP: node.IPV6})
		} else if len(ips) == 0 && hasV4 {
			ips = append(ips, IPOption{IP: node.IPV4})
		} else if len(ips) == 0 && hasV6 {
			ips = append(ips, IPOption{IP: node.IPV6})
		}
	} else if genV4 && hasV4 {
		ips = append(ips, IPOption{IP: node.IPV4})
	} else if genV6 && hasV6 {
		ips = append(ips, IPOption{IP: node.IPV6})
	}

	// Falls through to empty if gen requirements are not met and strict matching fails.
	// Do NOT enforce fallback if proto IP generation strictly failed the check.
	// We only fallback if gen rules permitted it but literal IP was missing.
	if len(ips) == 0 && (genV4 || genV6) {
		if effectiveNodeMode == 5 || effectiveNodeMode == 2 { // prefer v6 fallback
			if hasV6 {
				ips = append(ips, IPOption{IP: node.IPV6})
			} else if hasV4 {
				ips = append(ips, IPOption{IP: node.IPV4})
			}
		} else { // prefer v4 fallback
			if hasV4 {
				ips = append(ips, IPOption{IP: node.IPV4})
			} else if hasV6 {
				ips = append(ips, IPOption{IP: node.IPV6})
			}
		}
	}

	// 统一在最后根据 IP 格式决定是否追加后缀
	// 这样无论来源于直接匹配还是 Fallback，只要最后实质上使用了 IPv6，全部加上 -V6 后缀
	for i := range ips {
		if strings.Contains(ips[i].IP, ":") {
			ips[i].Suffix = "-V6"
		} else {
			ips[i].Suffix = ""
		}
	}

	return ips
}

// ParseProxyLink 解析链接并强制覆盖合并后的完美名称
func ParseProxyLink(link string, baseName string, region string, useFlag bool) *ClashNode {
	finalName := baseName
	if useFlag && region != "" {
		flag := getEmojiFlag(region)
		if flag != "" {
			finalName = flag + " " + finalName
		}
	}

	// 调用 links.go 里的核心解析函数
	node := ParseLinkToClashNode(link, "")
	if node != nil {
		node.Name = finalName // 直接强行覆盖为组合好的名字
	}
	return node
}

// getEmojiFlag 根据 2 位国家/地区代码智能生成 Emoji 国旗
func getEmojiFlag(region string) string {
	if len(region) != 2 {
		return ""
	}
	region = strings.ToUpper(region)
	flag := ""
	for _, char := range region {
		// A 的 ASCII 码是 65，区域指示符 A 是 127462 (0x1F1E6)
		// 127462 - 65 = 127397 (这就是精确的偏移量)
		flag += string(rune(char) + 127397)
	}
	return flag
}

// ReplaceLinkIP 安全替换标准 URI 链接中的 IP / 域名 (不破坏端口和其他参数)
func ReplaceLinkIP(link string, newIP string) string {
	if newIP == "" {
		return link
	}

	lowerLink := strings.ToLower(link)

	// VMess 是 vmess://base64(json) 结构，不能按 URL Host 直接替换
	if strings.HasPrefix(lowerLink, "vmess://") {
		body := safeBase64Decode(link[8:])
		if body == "" {
			return link
		}

		var vj map[string]interface{}
		if err := json.Unmarshal([]byte(body), &vj); err != nil {
			return link
		}

		oldAdd := ""
		if add, ok := vj["add"].(string); ok {
			oldAdd = add
		}

		// vmess JSON 中地址字段不应携带 IPv6 方括号
		rawIP := strings.TrimPrefix(strings.TrimSuffix(newIP, "]"), "[")
		vj["add"] = rawIP

		if host, ok := vj["host"].(string); ok {
			if host == "" || host == oldAdd {
				vj["host"] = rawIP
			}
		}
		if sni, ok := vj["sni"].(string); ok {
			if sni == "" || sni == oldAdd {
				vj["sni"] = rawIP
			}
		}

		newBody, err := json.Marshal(vj)
		if err != nil {
			return link
		}
		return "vmess://" + base64.StdEncoding.EncodeToString(newBody)
	}

	u, err := url.Parse(link)
	if err != nil || u.Host == "" {
		return link // 解析失败或极其特殊的格式，原样返回防报错
	}

	port := u.Port()
	if port != "" {
		u.Host = net.JoinHostPort(newIP, port)
	} else {
		if strings.Contains(newIP, ":") && !strings.HasPrefix(newIP, "[") {
			u.Host = "[" + newIP + "]"
		} else {
			u.Host = newIP
		}
	}

	return u.String()
}

// ReplaceLinkPort 安全替换标准 URI 链接中的端口。
// 对 vmess://base64(json) 会改写 port 字段；对其他 URI 则改写 Host 的端口。
func ReplaceLinkPort(link string, newPort int) string {
	if newPort <= 0 {
		return link
	}

	lowerLink := strings.ToLower(link)

	if strings.HasPrefix(lowerLink, "vmess://") {
		body := safeBase64Decode(link[8:])
		if body == "" {
			return link
		}

		var vj map[string]interface{}
		if err := json.Unmarshal([]byte(body), &vj); err != nil {
			return link
		}

		vj["port"] = strconv.Itoa(newPort)

		newBody, err := json.Marshal(vj)
		if err != nil {
			return link
		}
		return "vmess://" + base64.StdEncoding.EncodeToString(newBody)
	}

	u, err := url.Parse(link)
	if err != nil || u.Host == "" {
		return link
	}

	hostname := u.Hostname()
	if hostname == "" {
		return link
	}
	u.Host = net.JoinHostPort(hostname, strconv.Itoa(newPort))
	return u.String()
}

// ReplaceLinkSNIAndHost 将链接中的 host/sni/peer 参数改为指定域名。
// 适用于 Tunnel 加速场景，确保请求正确命中 Tunnel hostname 路由。
func ReplaceLinkSNIAndHost(link string, host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return link
	}

	lowerLink := strings.ToLower(link)

	if strings.HasPrefix(lowerLink, "vmess://") {
		body := safeBase64Decode(link[8:])
		if body == "" {
			return link
		}

		var vj map[string]interface{}
		if err := json.Unmarshal([]byte(body), &vj); err != nil {
			return link
		}

		vj["host"] = host
		vj["sni"] = host

		newBody, err := json.Marshal(vj)
		if err != nil {
			return link
		}
		return "vmess://" + base64.StdEncoding.EncodeToString(newBody)
	}

	u, err := url.Parse(link)
	if err != nil {
		return link
	}
	q := u.Query()
	q.Set("host", host)
	if q.Has("sni") {
		q.Set("sni", host)
	}
	if q.Has("peer") {
		q.Set("peer", host)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// 节点重命名引擎

// RenameNodeLink 修改节点分享链接的名字后缀 (#name)
// 智能重命名引擎：已支持解析各种 Base64 嵌套进行安全重命名
func RenameNodeLink(link string, newName string) string {
	lowerLink := strings.ToLower(link)

	// 处理 VMess (JSON 修改并重新 Base64 编码)
	if strings.HasPrefix(lowerLink, "vmess://") {
		body := safeBase64Decode(link[8:])
		if body == "" {
			return link
		}
		var vj map[string]interface{}
		if err := json.Unmarshal([]byte(body), &vj); err == nil {
			vj["ps"] = newName
			if newBody, err := json.Marshal(vj); err == nil {
				return "vmess://" + base64.StdEncoding.EncodeToString(newBody)
			}
		}
		return link
	}

	// 处理 SSR (复杂查询参数和 Base64 编码)
	if strings.HasPrefix(lowerLink, "ssr://") {
		decoded := safeBase64Decode(link[6:])
		if decoded == "" {
			return link
		}
		if strings.Contains(decoded, "remarks=") {
			parts := strings.Split(decoded, "remarks=")
			prefix := parts[0]
			suffix := parts[1]
			// 去掉旧的 remarks 值 (直到下一个 & 或结尾)
			if idx := strings.Index(suffix, "&"); idx != -1 {
				suffix = suffix[idx:]
			} else {
				suffix = ""
			}
			newDecoded := prefix + "remarks=" + base64.RawURLEncoding.EncodeToString([]byte(newName)) + suffix
			return "ssr://" + base64.RawURLEncoding.EncodeToString([]byte(newDecoded))
		}
		// 如果没有 remarks 参数，追加上去
		sep := "/?"
		if strings.Contains(decoded, "/?") {
			sep = "&"
		}
		newDecoded := decoded + sep + "remarks=" + base64.RawURLEncoding.EncodeToString([]byte(newName))
		return "ssr://" + base64.RawURLEncoding.EncodeToString([]byte(newDecoded))
	}

	// 处理 SS 纯 Base64 格式的特殊情况
	if strings.HasPrefix(lowerLink, "ss://") {
		u, err := url.Parse(link)
		if err != nil || u.Host == "" {
			encoded := strings.TrimPrefix(link, "ss://")
			hashIdx := strings.LastIndex(encoded, "#")
			if hashIdx != -1 {
				encoded = encoded[:hashIdx]
			}
			return "ss://" + encoded + "#" + url.PathEscape(newName)
		}
		u.Fragment = newName
		return u.String()
	}

	// 处理其他标准 URI 格式协议 (包含新增的 anytls)
	if strings.HasPrefix(lowerLink, "vless://") ||
		strings.HasPrefix(lowerLink, "trojan://") ||
		strings.HasPrefix(lowerLink, "hy2://") ||
		strings.HasPrefix(lowerLink, "hysteria2://") ||
		strings.HasPrefix(lowerLink, "hy://") ||
		strings.HasPrefix(lowerLink, "hysteria://") ||
		strings.HasPrefix(lowerLink, "tuic://") ||
		strings.HasPrefix(lowerLink, "socks5://") ||
		strings.HasPrefix(lowerLink, "anytls://") ||
		strings.HasPrefix(lowerLink, "http://") ||
		strings.HasPrefix(lowerLink, "https://") {
		u, err := url.Parse(link)
		if err == nil {
			u.Fragment = newName
			return u.String()
		}
	}

	return link
}

// GetSubscriptionUserinfo 聚合所有自建节点的流量数据，
// 返回标准 Subscription-Userinfo 格式字符串（单位：字节）
// 格式: upload=X; download=Y; total=Z
// 如果 total 为 0（没有配置任何限额），返回空字符串
func GetSubscriptionUserinfo() string {
	var result struct {
		SumUp    int64
		SumDown  int64
		SumLimit int64
	}
	database.DB.Model(&database.NodePool{}).
		Select("COALESCE(SUM(traffic_up),0) as sum_up, COALESCE(SUM(traffic_down),0) as sum_down, COALESCE(SUM(traffic_limit),0) as sum_limit").
		Scan(&result)

	if result.SumLimit == 0 {
		return ""
	}

	return fmt.Sprintf("upload=%d; download=%d; total=%d", result.SumUp, result.SumDown, result.SumLimit)
}
