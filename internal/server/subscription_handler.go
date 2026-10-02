package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"nodectl/internal/database"
	"nodectl/internal/logger"
	"nodectl/internal/service"
)

// ------------------- [订阅与分流规则 API] -------------------

func verifySubToken(r *http.Request) bool {
	token := r.URL.Query().Get("token")
	var config database.SysConfig
	database.DB.Where("key = ?", "sub_token").First(&config)
	return token != "" && token == config.Value
}

func getBaseURL(r *http.Request) string {
	var config database.SysConfig
	database.DB.Where("key = ?", "panel_url").First(&config)
	if config.Value != "" {
		return strings.TrimRight(config.Value, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func apiSubClash(w http.ResponseWriter, r *http.Request) {
	clientIP := getClientIP(r)
	reqPath := r.URL.Path

	if !verifySubToken(r) {
		logger.Log.Warn("Clash 订阅请求: Token 验证失败", "ip", clientIP, "path", reqPath)
		http.Error(w, "Invalid Token", http.StatusForbidden)
		return
	}

	baseURL := getBaseURL(r)
	token := r.URL.Query().Get("token")

	// routing_type=1 是中转节点，routing_type=2 是落地节点。
	relayURL := fmt.Sprintf("%s/sub/raw/1?token=%s", baseURL, token)
	exitURL := fmt.Sprintf("%s/sub/raw/2?token=%s", baseURL, token)

	yamlContent, err := service.RenderClashConfig(relayURL, exitURL, baseURL, token)
	if err != nil {
		logger.Log.Error("生成 Clash 订阅模板失败", "error", err, "ip", clientIP, "path", reqPath)
		http.Error(w, "模板生成失败", http.StatusInternalServerError)
		return
	}

	var nameConfig database.SysConfig
	database.DB.Where("key = ?", "sub_custom_name").First(&nameConfig)
	subName := nameConfig.Value
	if subName == "" {
		subName = "NodeCTL"
	}

	logger.Log.Info("成功下发 Clash 订阅模板", "ip", clientIP, "path", reqPath)
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("profile-title", subName)

	if userinfo := service.GetSubscriptionUserinfo(); userinfo != "" {
		w.Header().Set("Subscription-Userinfo", userinfo)
	}

	encodedName := url.QueryEscape(subName)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename*=utf-8''%s`, encodedName))

	w.Write([]byte(yamlContent))
}

func apiSubV2ray(w http.ResponseWriter, r *http.Request) {
	clientIP := getClientIP(r)
	reqPath := r.URL.Path

	if !verifySubToken(r) {
		logger.Log.Warn("V2Ray 订阅请求: Token 验证失败", "ip", clientIP, "path", reqPath)
		http.Error(w, "Invalid Token", http.StatusForbidden)
		return
	}

	var flagConfig database.SysConfig
	database.DB.Where("key = ?", "pref_use_emoji_flag").First(&flagConfig)
	useFlag := flagConfig.Value != "false"

	b64Content, err := service.GenerateV2RaySubBase64(useFlag)
	if err != nil {
		logger.Log.Error("生成 V2Ray Base64 订阅失败", "error", err, "ip", clientIP, "path", reqPath)
		http.Error(w, "订阅生成失败", http.StatusInternalServerError)
		return
	}

	var nameConfig database.SysConfig
	database.DB.Where("key = ?", "sub_custom_name").First(&nameConfig)
	subName := nameConfig.Value
	if subName == "" {
		subName = "NodeCTL"
	}

	logger.Log.Info("成功下发 V2Ray Base64 订阅", "ip", clientIP, "path", reqPath)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("profile-title", subName)

	if userinfo := service.GetSubscriptionUserinfo(); userinfo != "" {
		w.Header().Set("Subscription-Userinfo", userinfo)
	}

	encodedName := url.QueryEscape(subName)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename*=utf-8''%s`, encodedName))

	w.Write([]byte(b64Content))
}

func apiSubRaw(w http.ResponseWriter, r *http.Request) {
	clientIP := getClientIP(r)
	reqPath := r.URL.Path

	if !verifySubToken(r) {
		logger.Log.Warn("Raw 节点列表请求: Token 验证失败", "ip", clientIP, "path", reqPath)
		http.Error(w, "Invalid Token", http.StatusForbidden)
		return
	}

	pathParts := strings.Split(r.URL.Path, "/")
	typeStr := pathParts[len(pathParts)-1]
	routingType := 1
	if typeStr == "2" {
		routingType = 2
	}

	var flagConfig database.SysConfig
	database.DB.Where("key = ?", "pref_use_emoji_flag").First(&flagConfig)
	useFlag := flagConfig.Value != "false"

	yamlContent, err := service.GenerateRawNodesYAML(routingType, useFlag)
	if err != nil {
		logger.Log.Error("生成 Raw 节点列表失败", "error", err, "routing_type", routingType, "ip", clientIP, "path", reqPath)
		http.Error(w, "节点生成失败", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Write([]byte(yamlContent))
}

// ------------------- [自定义分流规则 API] -------------------

func apiGetCustomRules(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	if r.Method != http.MethodGet {
		logger.Log.Warn("非法请求方法", "method", r.Method, "ip", clientIP, "path", reqPath)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	directRaw := service.GetCustomDirectRules()
	directIcon := service.GetCustomDirectIcon()
	proxyRules := service.GetCustomProxyRules()

	groupNames := make([]string, 0, len(proxyRules))
	for _, rule := range proxyRules {
		name := strings.TrimSpace(rule.Name)
		if name == "" {
			name = strings.TrimSpace(rule.ID)
		}
		if name == "" {
			name = "未命名分组"
		}
		groupNames = append(groupNames, name)
	}
	groupNamesText := "无分流组"
	if len(groupNames) > 0 {
		groupNamesText = strings.Join(groupNames, ",")
	}

	logger.Log.Debug("获取自定义分流规则："+groupNamesText,
		"ip", clientIP,
		"path", reqPath,
		"direct_rules_len", len(strings.TrimSpace(directRaw)),
		"proxy_group_count", len(proxyRules),
	)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"direct":      directRaw,
			"direct_icon": directIcon,
			"proxy":       proxyRules,
		},
	})
}

func apiSaveCustomRules(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	if r.Method != http.MethodPost {
		logger.Log.Warn("非法请求方法", "method", r.Method, "ip", clientIP, "path", reqPath)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	oldDirectRules := service.GetCustomDirectRules()
	oldDirectIcon := service.GetCustomDirectIcon()
	oldProxyRules := service.GetCustomProxyRules()

	var req struct {
		DirectRules string                    `json:"direct"`
		DirectIcon  string                    `json:"direct_icon"`
		ProxyRules  []service.CustomProxyRule `json:"proxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Log.Warn("解析 JSON 失败", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "数据解析失败")
		return
	}

	if err := service.SaveCustomDirectRules(req.DirectRules); err != nil {
		logger.Log.Error("保存自定义直连规则失败", "error", err, "ip", clientIP, "path", reqPath)
	}
	if err := service.SaveCustomDirectIcon(req.DirectIcon); err != nil {
		logger.Log.Error("保存自定义直连图标失败", "error", err, "ip", clientIP, "path", reqPath)
	}
	if err := service.SaveCustomProxyRules(req.ProxyRules); err != nil {
		logger.Log.Error("保存自定义分流规则失败", "error", err, "ip", clientIP, "path", reqPath)
	}

	changedDetails := make([]string, 0)

	oldDirectLines := normalizeCustomRuleLines(oldDirectRules)
	newDirectLines := normalizeCustomRuleLines(req.DirectRules)
	addedDirect, removedDirect := diffCustomRuleLines(oldDirectLines, newDirectLines)
	if strings.TrimSpace(oldDirectIcon) != strings.TrimSpace(req.DirectIcon) {
		changedDetails = append(changedDetails, fmt.Sprintf("全局直连 图标 %s -> %s", strings.TrimSpace(oldDirectIcon), strings.TrimSpace(req.DirectIcon)))
	}
	if len(addedDirect) > 0 {
		changedDetails = append(changedDetails, "全局直连 添加 "+limitJoinedValues(addedDirect, 8))
	}
	if len(removedDirect) > 0 {
		changedDetails = append(changedDetails, "全局直连 删除 "+limitJoinedValues(removedDirect, 8))
	}

	oldByID := make(map[string]service.CustomProxyRule, len(oldProxyRules))
	newByID := make(map[string]service.CustomProxyRule, len(req.ProxyRules))
	for _, rule := range oldProxyRules {
		if id := strings.TrimSpace(rule.ID); id != "" {
			oldByID[id] = rule
		}
	}
	for _, rule := range req.ProxyRules {
		if id := strings.TrimSpace(rule.ID); id != "" {
			newByID[id] = rule
		}
	}

	for id, oldRule := range oldByID {
		if _, ok := newByID[id]; !ok {
			changedDetails = append(changedDetails, fmt.Sprintf("删除策略组 %s", customGroupName(oldRule)))
		}
	}

	for id, newRule := range newByID {
		if _, ok := oldByID[id]; !ok {
			changedDetails = append(changedDetails, fmt.Sprintf("新建策略组 %s", customGroupName(newRule)))
		}
	}

	for _, newRule := range req.ProxyRules {
		oldRule, ok := oldByID[strings.TrimSpace(newRule.ID)]

		if !ok {
			added := normalizeCustomRuleLines(newRule.Content)
			if len(added) > 0 {
				changedDetails = append(changedDetails, fmt.Sprintf("%s 添加 %s", customGroupName(newRule), limitJoinedValues(added, 8)))
			}
			continue
		}

		groupOld := customGroupName(oldRule)
		groupNew := customGroupName(newRule)
		if strings.TrimSpace(oldRule.Name) != strings.TrimSpace(newRule.Name) {
			changedDetails = append(changedDetails, fmt.Sprintf("策略组重命名 %s -> %s", groupOld, groupNew))
		}
		if strings.TrimSpace(oldRule.Icon) != strings.TrimSpace(newRule.Icon) {
			changedDetails = append(changedDetails, fmt.Sprintf("策略组 %s 图标 %s -> %s", groupNew, strings.TrimSpace(oldRule.Icon), strings.TrimSpace(newRule.Icon)))
		}

		added, removed := diffCustomRuleLines(normalizeCustomRuleLines(oldRule.Content), normalizeCustomRuleLines(newRule.Content))
		group := groupNew
		if len(added) > 0 {
			changedDetails = append(changedDetails, fmt.Sprintf("%s 添加 %s", group, limitJoinedValues(added, 8)))
		}
		if len(removed) > 0 {
			changedDetails = append(changedDetails, fmt.Sprintf("%s 删除 %s", group, limitJoinedValues(removed, 8)))
		}
	}

	if len(changedDetails) == 0 {
		logger.Log.Info("自定义分流规则保存成功(无规则变化)", "ip", clientIP, "path", reqPath)
	} else {
		logger.Log.Info("自定义分流规则已更新",
			"changed_count", len(changedDetails),
			"changes", strings.Join(changedDetails, " | "),
			"ip", clientIP,
			"path", reqPath,
		)
	}
	sendJSON(w, "success", "自定义规则保存成功")
}

func apiSubRuleList(w http.ResponseWriter, r *http.Request) {
	clientIP := getClientIP(r)
	reqPath := r.URL.Path

	if !verifySubToken(r) {
		logger.Log.Warn("规则列表订阅请求: Token 验证失败", "ip", clientIP, "path", reqPath)
		http.Error(w, "Invalid Token", http.StatusForbidden)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/sub/rules/")
	var rawContent string
	groupName := "未知规则组"

	if path == "direct" {
		rawContent = service.GetCustomDirectRules()
		groupName = "全局直连"
	} else if strings.HasPrefix(path, "proxy/") {
		id := strings.TrimPrefix(path, "proxy/")
		rules := service.GetCustomProxyRules()
		for _, rule := range rules {
			if rule.ID == id {
				rawContent = rule.Content
				name := strings.TrimSpace(rule.Name)
				if name == "" {
					name = id
				}
				groupName = name
				break
			}
		}
		if strings.TrimSpace(rawContent) == "" {
			groupName = id
		}
	}

	logger.Log.Debug("获取自定义分流规则："+groupName,
		"ip", clientIP,
		"path", reqPath,
		"rules_len", len(strings.TrimSpace(rawContent)),
	)

	formattedContent := service.ParseCustomRules(rawContent)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Write([]byte(formattedContent))
}

// ------------------- [Clash 模板与分流模块 API] -------------------

func apiGetClashSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	config := service.LoadClashModulesConfig()
	customModules := service.GetCustomClashModules()
	activeModules := service.GetActiveClashModules()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"builtin_modules": config.Modules,
			"custom_modules":  customModules,
			"presets":         config.Presets,
			"active_modules":  activeModules,
		},
	})
}

func apiSaveCustomClashModules(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Modules []service.ClashModuleDef `json:"modules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Log.Warn("解析自定义分流模块失败", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "数据格式错误")
		return
	}

	if err := service.SaveCustomClashModules(req.Modules); err != nil {
		logger.Log.Error("保存自定义分流模块失败", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "保存自定义模块失败")
		return
	}
	sendJSON(w, "success", "自定义模块保存成功")
}

func apiSaveClashSettings(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	if r.Method != http.MethodPost {
		logger.Log.Warn("非法请求方法", "method", r.Method, "ip", clientIP, "path", reqPath)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Modules []string `json:"modules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Log.Warn("解析 JSON 失败", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "数据解析失败")
		return
	}

	oldModules := service.GetActiveClashModules()

	if err := service.SaveActiveClashModules(req.Modules); err != nil {
		logger.Log.Error("保存 Clash 模块设置失败", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "保存失败")
		return
	}

	oldSet := make(map[string]struct{}, len(oldModules))
	for _, m := range oldModules {
		oldSet[m] = struct{}{}
	}
	newSet := make(map[string]struct{}, len(req.Modules))
	for _, m := range req.Modules {
		newSet[m] = struct{}{}
	}
	added := make([]string, 0)
	removed := make([]string, 0)
	for m := range newSet {
		if _, ok := oldSet[m]; !ok {
			added = append(added, m)
		}
	}
	for m := range oldSet {
		if _, ok := newSet[m]; !ok {
			removed = append(removed, m)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)

	oldOrder := strings.Join(oldModules, ",")
	newOrder := strings.Join(req.Modules, ",")
	reordered := oldOrder != newOrder && len(added) == 0 && len(removed) == 0

	changeSummary := make([]string, 0)
	if len(added) > 0 {
		changeSummary = append(changeSummary, "新增规则集: "+strings.Join(added, ","))
	}
	if len(removed) > 0 {
		changeSummary = append(changeSummary, "移除规则集: "+strings.Join(removed, ","))
	}
	if reordered {
		changeSummary = append(changeSummary, "仅顺序变化")
	}
	if len(changeSummary) == 0 {
		changeSummary = append(changeSummary, "无变化")
	}

	logger.Log.Info("Clash 模板模块设置已更新",
		"changed", strings.Join(changeSummary, " | "),
		"before_count", len(oldModules),
		"after_count", len(req.Modules),
		"ip", clientIP,
		"path", reqPath,
	)
	sendJSON(w, "success", "Clash 规则组合保存成功")
}
