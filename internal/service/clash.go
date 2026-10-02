package service

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"nodectl/internal/database"
	"nodectl/internal/logger"

	"gorm.io/gorm"
)

//go:embed clash_meta.tpl
var ClashTemplateStr string

//go:embed clash_modules.tpl
var ClashModulesJSON []byte

// ClashModuleDef 分流模块终极定义 (完美适配 GitMetaio mrs 架构)
type ClashModuleDef struct {
	Name       string   `json:"name"`
	Icon       string   `json:"icon"`
	URL        string   `json:"url,omitempty"`         // 用于用户自定义的 classical YAML 规则
	DomainURL  string   `json:"domain_url,omitempty"`  // 用于 GitMetaio 的 Domain.mrs
	IPURL      string   `json:"ip_url,omitempty"`      // 用于 GitMetaio 的 IP.mrs
	ExtraRules []string `json:"extra_rules,omitempty"` // 用于挂载 PROCESS-PATH 等附加规则
	Type       string   `json:"type,omitempty"`        // 模块类型，如 "reject" (不再生成单独的策略组，直接路由到 ⛔️ 拒绝连接)
}

type ClashPresetDef struct {
	Name    string   `json:"name"`
	Desc    string   `json:"desc"`
	Modules []string `json:"modules"`
}

type ClashModulesConfig struct {
	Modules []ClashModuleDef `json:"modules"`
	Presets []ClashPresetDef `json:"presets"`
}

// LoadClashModulesConfig 读取内置 JSON 模板
func LoadClashModulesConfig() ClashModulesConfig {
	var config ClashModulesConfig
	if err := json.Unmarshal(ClashModulesJSON, &config); err != nil {
		logger.Log.Error("解析内置 clash_modules.tpl 失败", "error", err)
	}
	return config
}

func GetCustomClashModules() []ClashModuleDef {
	var conf database.SysConfig
	err := database.DB.Where("key = ?", "clash_custom_modules").First(&conf).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		logger.Log.Warn("读取自定义分流模块数据库异常", "error", err)
	}

	var modules []ClashModuleDef
	if conf.Value != "" && conf.Value != "[]" {
		if err := json.Unmarshal([]byte(conf.Value), &modules); err != nil {
			logger.Log.Error("反序列化自定义分流模块失败", "error", err, "raw_data", conf.Value)
		}
	}
	return modules
}

func SaveCustomClashModules(modules []ClashModuleDef) error {
	data, err := json.Marshal(modules)
	if err != nil {
		logger.Log.Error("序列化自定义分流模块失败", "error", err)
		return err
	}

	err = database.DB.Where(database.SysConfig{Key: "clash_custom_modules"}).
		Assign(database.SysConfig{Value: string(data)}).
		FirstOrCreate(&database.SysConfig{}).Error

	if err != nil {
		logger.Log.Error("保存自定义分流模块入库失败", "error", err)
		return err
	}
	return nil
}

// defaultRecommendedModules 是"常用推荐"预设的模块列表，用于首次初始化兜底
var defaultRecommendedModules = []string{
	"Telegram", "Google", "GoogleFCM", "YouTube", "Netflix",
	"Twitter(X)", "GitHub", "OpenAI", "Spotify", "Discord",
	"Microsoft", "TikTok",
}

func GetActiveClashModules() []string {
	var conf database.SysConfig
	database.DB.Where("key = ?", "clash_active_modules").Limit(1).Find(&conf)
	if conf.Value == "" {
		// 兜底：当数据库中值为空时（旧用户从未设置过），自动填充为"常用推荐"预设并持久化
		logger.Log.Info("Clash 分流规则为空，自动应用常用推荐预设")
		_ = SaveActiveClashModules(defaultRecommendedModules)
		return defaultRecommendedModules
	}
	return strings.Split(conf.Value, ",")
}

func SaveActiveClashModules(modules []string) error {
	val := strings.Join(modules, ",")
	return database.DB.Where(database.SysConfig{Key: "clash_active_modules"}).
		Assign(database.SysConfig{Value: val}).
		FirstOrCreate(&database.SysConfig{}).Error
}

// Clash 模板渲染逻辑
type ClashTemplateData struct {
	RelaySubURL             string
	ExitSubURL              string
	BaseURL                 string
	Token                   string
	ActiveModules           []ClashModuleDef
	GlobalDirectIcon        string
	CustomProxies           []CustomProxyRule
	NameserverPolicyRuleSet string // 用于存储动态生成的 DNS 策略字符串，例如 "CN_域,Apple_域"
	ProxiesInterval         string
	RulesInterval           string
	PublicRulesInterval     string
}

func RenderClashConfig(relayURL, exitURL, baseURL, token string) (string, error) {
	activeNames := GetActiveClashModules()
	activeMap := make(map[string]bool)
	for _, n := range activeNames {
		activeMap[n] = true
	}

	builtin := LoadClashModulesConfig().Modules
	custom := GetCustomClashModules()
	allModules := append(builtin, custom...)

	var finalActiveMods []ClashModuleDef

	// 初始化 dnsPolicyList，并包含基础规则 "CN_域"
	dnsPolicyList := []string{"CN_域"}

	for _, m := range allModules {
		if activeMap[m.Name] {
			finalActiveMods = append(finalActiveMods, m)

			// 检查当前启用的模块是否是需要在 DNS 策略中特殊处理的模块
			// 只有当用户勾选了 Apple，才将其加入 DNS 策略
			if m.Name == "Apple" {
				dnsPolicyList = append(dnsPolicyList, "Apple_域")
			}
			// 只有当用户勾选了 Microsoft，才将其加入 DNS 策略
			if m.Name == "Microsoft" {
				dnsPolicyList = append(dnsPolicyList, "Microsoft_域")
			}
		}
	}

	// 将切片用逗号连接成字符串，例如: "CN_域,Microsoft_域" 或 "CN_域"
	dnsPolicyStr := strings.Join(dnsPolicyList, ",")

	// 提前获取所有自定义分流组并且将图标转换为Clash支持的格式
	allCustomProxies := GetCustomProxyRulesForClash()
	var validCustomProxies []CustomProxyRule

	// 遍历检查，只有当解析后确实存在有效规则时，才加入到最终的渲染列表中
	for _, p := range allCustomProxies {
		if strings.TrimSpace(ParseCustomRules(p.Content)) != "" {
			validCustomProxies = append(validCustomProxies, p)
		}
	}

	// 获取 Clash 更新间隔
	var proxiesIntervalConf database.SysConfig
	database.DB.Where("key = ?", "clash_proxies_update_interval").First(&proxiesIntervalConf)
	proxiesInterval := proxiesIntervalConf.Value
	if proxiesInterval == "" {
		proxiesInterval = "3600"
	}

	var rulesIntervalConf database.SysConfig
	database.DB.Where("key = ?", "clash_rules_update_interval").First(&rulesIntervalConf)
	rulesInterval := rulesIntervalConf.Value
	if rulesInterval == "" {
		rulesInterval = "300"
	}

	var publicRulesIntervalConf database.SysConfig
	database.DB.Where("key = ?", "clash_public_rules_update_interval").First(&publicRulesIntervalConf)
	publicRulesInterval := publicRulesIntervalConf.Value
	if publicRulesInterval == "" {
		publicRulesInterval = "86400"
	} else {
		publicIntervalInt, err := strconv.Atoi(publicRulesInterval)
		if err != nil || publicIntervalInt < 86400 {
			publicRulesInterval = "86400"
		}
	}

	data := ClashTemplateData{
		RelaySubURL:             relayURL,
		ExitSubURL:              exitURL,
		ActiveModules:           finalActiveMods,
		BaseURL:                 baseURL,
		Token:                   token,
		GlobalDirectIcon:        getEmojiURL(GetCustomDirectIcon()),
		CustomProxies:           validCustomProxies, // 替换为过滤后的有效分组
		NameserverPolicyRuleSet: dnsPolicyStr,
		ProxiesInterval:         proxiesInterval,
		RulesInterval:           rulesInterval,
		PublicRulesInterval:     publicRulesInterval,
	}

	tmpl, err := template.New("clash").Parse(ClashTemplateStr)
	if err != nil {
		return "", fmt.Errorf("解析 Clash 模板失败: %v", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("渲染 Clash 模板失败: %v", err)
	}

	re1 := regexp.MustCompile(`(?m)^[ \t]+$`)
	step1 := re1.ReplaceAllString(buf.String(), "")
	re2 := regexp.MustCompile(`(\r?\n){3,}`)
	return re2.ReplaceAllString(step1, "\n\n"), nil
}

// 自定义规则处理逻辑
func ParseCustomRules(raw string) string {
	var result []string
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") || strings.Contains(line, ",") {
			if line != "" {
				result = append(result, line)
			}
			continue
		}
		if idx := strings.Index(line, "://"); idx != -1 {
			line = line[idx+3:]
		}
		if isIPOrCIDR(line) {
			if !strings.Contains(line, "/") {
				if strings.Contains(line, ":") {
					line += "/128"
				} else {
					line += "/32"
				}
			}
			if strings.Contains(line, ":") {
				result = append(result, "IP-CIDR6,"+line)
			} else {
				result = append(result, "IP-CIDR,"+line)
			}
		} else {
			if idx := strings.Index(line, "/"); idx != -1 {
				line = line[:idx]
			}
			result = append(result, "DOMAIN-SUFFIX,"+line)
		}
	}
	return strings.Join(result, "\n")
}

func isIPOrCIDR(s string) bool {
	if _, _, err := net.ParseCIDR(s); err == nil {
		return true
	}
	if ip := net.ParseIP(s); ip != nil {
		return true
	}
	return false
}

type CustomProxyRule struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Icon    string `json:"icon"`
	Content string `json:"content"`
}

func getEmojiURL(icon string) string {
	// Only convert if it's one of our known emojis, otherwise return as-is
	// Twitter Emoji (Twemoji) SVG CDN is historically reliable
	emojiMap := map[string]string{
		"🎯":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f3af.svg",
		"🤖":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f916.svg",
		"🍎":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f34e.svg",
		"📺":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f4fa.svg",
		"🎮":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f3ae.svg",
		"✈️": "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/2708.svg",
		"🌍":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f30d.svg",
		"🏠":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f3e0.svg",
		"💬":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f4ac.svg",
		"🎬":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f3ac.svg",
		"📚":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f4da.svg",
		"💼":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f4bc.svg",
		"🛒":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f6d2.svg",
		"💳":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f4b3.svg",
		"🔒":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f512.svg",
		"☁️": "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/2601.svg",
		"⚡":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/26a1.svg",
		"🔥":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f525.svg",
		"🚀":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f680.svg",
		"🐱":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f431.svg",
		"🐶":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f436.svg",
		"🐼":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f43c.svg",
		"🌐":  "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/1f310.svg",
	}

	if url, ok := emojiMap[icon]; ok {
		return url
	}
	return icon
}

func GetCustomProxyRules() []CustomProxyRule {
	var conf database.SysConfig
	database.DB.Where("key = ?", "clash_custom_proxy_rules").Limit(1).Find(&conf)
	var rules []CustomProxyRule
	if conf.Value != "" {
		json.Unmarshal([]byte(conf.Value), &rules)
	}
	return rules
}

func GetCustomProxyRulesForClash() []CustomProxyRule {
	rules := GetCustomProxyRules()
	for i := range rules {
		if rules[i].Icon != "" {
			rules[i].Icon = getEmojiURL(rules[i].Icon)
		}
	}
	return rules
}

func SaveCustomProxyRules(rules []CustomProxyRule) error {
	data, _ := json.Marshal(rules)
	return database.DB.Where(database.SysConfig{Key: "clash_custom_proxy_rules"}).
		Assign(database.SysConfig{Value: string(data)}).
		FirstOrCreate(&database.SysConfig{}).Error
}

func GetCustomDirectRules() string {
	var conf database.SysConfig
	database.DB.Where("key = ?", "clash_custom_direct_raw").Limit(1).Find(&conf)
	return conf.Value
}

func SaveCustomDirectRules(content string) error {
	// 使用 .Update("value", content) 强制指定更新字段，绕过空字符串忽略机制
	return database.DB.Model(&database.SysConfig{}).
		Where("key = ?", "clash_custom_direct_raw").
		Update("value", content).Error
}

func GetCustomDirectIcon() string {
	var conf database.SysConfig
	database.DB.Where("key = ?", "clash_custom_direct_icon").Limit(1).Find(&conf)
	if conf.Value == "" {
		return "🌐"
	}
	return conf.Value
}

func SaveCustomDirectIcon(icon string) error {
	return database.DB.Where(database.SysConfig{Key: "clash_custom_direct_icon"}).
		Assign(database.SysConfig{Value: icon}).
		FirstOrCreate(&database.SysConfig{}).Error
}
