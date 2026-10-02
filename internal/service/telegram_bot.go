package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"nodectl/internal/database"
	"nodectl/internal/logger"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/net/proxy"
)

var (
	tgBotCancel context.CancelFunc

	offlineNotifyMu       sync.Mutex
	offlineNotifyState    = make(map[string]*offlineNotifyRuntime)
	offlineNotifyLoopOnce sync.Once

	nodeNotifyCacheMu sync.RWMutex
	nodeNotifyCache   = make(map[string]*NodeNotifyConfig)

	nodeStatusNotifyMuteMu    sync.RWMutex
	nodeStatusNotifyMuteUntil = make(map[string]time.Time)
)

type NodeNotifyConfig struct {
	UUID                  string
	Name                  string
	OfflineNotifyEnabled  bool
	OfflineNotifyGraceSec int
	OfflineLastNotifyAt   time.Time // 上次离线通知发送时间 (来自 DB)
	PreBootOffline        bool      // 节点是否在系统刚启动并度过 60s 缓冲期时被定性为已离线
}

type offlineNotifyRuntime struct {
	initialized      bool
	online           bool
	disconnectedAt   time.Time // 实际断线时刻
	pendingOfflineAt time.Time // 宽限期到期时间
	lastNotified     string    // offline | online | ""
}

func NormalizeNodeOfflineGraceSec(v int) int {
	if v < 0 {
		return 0
	}
	if v > 86400 {
		return 86400
	}
	return v
}

// SuppressNodeStatusNotifyForDuration 在指定时间窗口内抑制节点上下线 TG 通知。
func SuppressNodeStatusNotifyForDuration(installID string, duration time.Duration) {
	installID = strings.TrimSpace(installID)
	if installID == "" || duration <= 0 {
		return
	}

	until := time.Now().Add(duration)
	nodeStatusNotifyMuteMu.Lock()
	prev := nodeStatusNotifyMuteUntil[installID]
	if prev.IsZero() || until.After(prev) {
		nodeStatusNotifyMuteUntil[installID] = until
	}
	nodeStatusNotifyMuteMu.Unlock()
}

func getNodeStatusNotifyMuteUntil(installID string, now time.Time) (time.Time, bool) {
	installID = strings.TrimSpace(installID)
	if installID == "" {
		return time.Time{}, false
	}

	nodeStatusNotifyMuteMu.RLock()
	until, ok := nodeStatusNotifyMuteUntil[installID]
	nodeStatusNotifyMuteMu.RUnlock()
	if !ok {
		return time.Time{}, false
	}
	if now.Before(until) {
		return until, true
	}

	// 已过期，懒清理。
	nodeStatusNotifyMuteMu.Lock()
	if curr, exists := nodeStatusNotifyMuteUntil[installID]; exists && !now.Before(curr) {
		delete(nodeStatusNotifyMuteUntil, installID)
	}
	nodeStatusNotifyMuteMu.Unlock()
	return time.Time{}, false
}

func clearNodeOfflineRecoveryMarkers(installID string, nodeUUID string) {
	if strings.TrimSpace(nodeUUID) != "" {
		database.DB.Model(&database.NodePool{}).Where("uuid = ?", nodeUUID).Update("offline_last_notify_at", nil)
	}
	nodeNotifyCacheMu.Lock()
	if c, exists := nodeNotifyCache[installID]; exists {
		c.OfflineLastNotifyAt = time.Time{}
		c.PreBootOffline = false
	}
	nodeNotifyCacheMu.Unlock()
}

func StartOfflineNotifyLoop() {
	offlineNotifyLoopOnce.Do(func() {
		go runOfflineNotifyLoop()
	})
}

func runOfflineNotifyLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for now := range ticker.C {
		handleOfflineNotifyTick(now)
	}
}

func handleOfflineNotifyTick(now time.Time) {
	candidates := make([]string, 0)
	type statusTransition struct {
		installID string
		online    bool
	}
	transitions := make([]statusTransition, 0)

	nodeNotifyCacheMu.RLock()
	installIDs := make([]string, 0, len(nodeNotifyCache))
	for installID := range nodeNotifyCache {
		installIDs = append(installIDs, installID)
	}
	nodeNotifyCacheMu.RUnlock()

	offlineNotifyMu.Lock()
	for _, installID := range installIDs {
		desiredOnline := IsNodeOnlineForStatus(installID)
		st := offlineNotifyState[installID]
		if st == nil || !st.initialized {
			// 仅在判定离线时补齐状态机初始化；稳定在线无需进入离线状态机。
			if desiredOnline {
				continue
			}
			transitions = append(transitions, statusTransition{
				installID: installID,
				online:    false,
			})
			continue
		}
		if st.online != desiredOnline {
			transitions = append(transitions, statusTransition{
				installID: installID,
				online:    desiredOnline,
			})
		}
	}

	for installID, st := range offlineNotifyState {
		if st == nil || !st.initialized || st.online {
			continue
		}
		if st.pendingOfflineAt.IsZero() || now.Before(st.pendingOfflineAt) {
			continue
		}
		if st.lastNotified == "offline" {
			st.pendingOfflineAt = time.Time{}
			continue
		}
		candidates = append(candidates, installID)
	}
	offlineNotifyMu.Unlock()

	for _, tr := range transitions {
		OnNodeConnectionStatusChanged(tr.installID, tr.online)
	}

	for _, installID := range candidates {
		notifyOfflineIfDue(installID, now)
	}
}

func notifyOfflineIfDue(installID string, now time.Time) {
	if IsNodeOnlineForStatus(installID) {
		offlineNotifyMu.Lock()
		// 节点实际上是在线的，清除离线倒计时和脏数据
		delete(offlineNotifyState, installID)
		offlineNotifyMu.Unlock()
		return
	}

	if muteUntil, muted := getNodeStatusNotifyMuteUntil(installID, now); muted {
		offlineNotifyMu.Lock()
		if st := offlineNotifyState[installID]; st != nil {
			if st.pendingOfflineAt.IsZero() || st.pendingOfflineAt.Before(muteUntil) {
				st.pendingOfflineAt = muteUntil
			}
		}
		offlineNotifyMu.Unlock()
		logger.Log.Debug("节点状态通知处于抑制窗口，跳过离线通知", "install_id", installID, "mute_until", muteUntil.Format(time.RFC3339))
		return
	}

	nodeNotifyCacheMu.RLock()
	node, ok := nodeNotifyCache[installID]
	nodeNotifyCacheMu.RUnlock()

	if !ok || !node.OfflineNotifyEnabled {
		offlineNotifyMu.Lock()
		delete(offlineNotifyState, installID)
		offlineNotifyMu.Unlock()
		return
	}

	// 使用实际断线时间而非当前轮询时间
	offlineNotifyMu.Lock()
	var eventTime time.Time
	if st := offlineNotifyState[installID]; st != nil && !st.disconnectedAt.IsZero() {
		eventTime = st.disconnectedAt
	}
	offlineNotifyMu.Unlock()
	if eventTime.IsZero() {
		eventTime = now
	}

	sent := sendNodeStatusNotification(node.Name, false, eventTime)
	if sent {
		updateNodeOfflineLastNotifyAt(node.UUID, eventTime)
		nodeNotifyCacheMu.Lock()
		if c, exists := nodeNotifyCache[installID]; exists {
			c.OfflineLastNotifyAt = eventTime
		}
		nodeNotifyCacheMu.Unlock()
	}

	offlineNotifyMu.Lock()
	st := offlineNotifyState[installID]
	if st == nil {
		st = &offlineNotifyRuntime{}
		offlineNotifyState[installID] = st
	}
	if sent {
		st.lastNotified = "offline"
		st.pendingOfflineAt = time.Time{}
	} else {
		// 发送失败时不标记为已通知，稍后重试，避免后续误发“上线通知”
		st.pendingOfflineAt = now.Add(30 * time.Second)
	}
	offlineNotifyMu.Unlock()
}

// UpdateNodeNotifyConfigFromDB 供外部修改节点通知配置后立刻同步到内存缓存。
func UpdateNodeNotifyConfigFromDB(installID string, enabled bool, graceSec int, name string) {
	nodeNotifyCacheMu.Lock()
	defer nodeNotifyCacheMu.Unlock()

	if node, exists := nodeNotifyCache[installID]; exists {
		node.OfflineNotifyEnabled = enabled
		node.OfflineNotifyGraceSec = NormalizeNodeOfflineGraceSec(graceSec)
		if strings.TrimSpace(name) != "" {
			node.Name = name
		}
	} else {
		// 如果内存本来没有，可以查一次库把完整数据加载进来并设置
		var dbNode database.NodePool
		if err := database.DB.Where("install_id = ?", installID).First(&dbNode).Error; err == nil {
			nodeNotifyCache[installID] = &NodeNotifyConfig{
				UUID:                  dbNode.UUID,
				Name:                  dbNode.Name,
				OfflineNotifyEnabled:  enabled,
				OfflineNotifyGraceSec: NormalizeNodeOfflineGraceSec(graceSec),
			}
		}
	}
}

// DeleteNodeNotifyConfig 从内存缓存中彻底移除已经被删除的节点配置。
func DeleteNodeNotifyConfig(installID string) {
	nodeNotifyCacheMu.Lock()
	delete(nodeNotifyCache, installID)
	nodeNotifyCacheMu.Unlock()

	offlineNotifyMu.Lock()
	delete(offlineNotifyState, installID)
	offlineNotifyMu.Unlock()
}

// AddNodeToNotifyCache 新增节点后立即将其添加到内存缓存中，确保 Agent WS 连接时可被识别。
func AddNodeToNotifyCache(node *database.NodePool) {
	if node == nil || strings.TrimSpace(node.InstallID) == "" {
		return
	}

	nodeNotifyCacheMu.Lock()
	defer nodeNotifyCacheMu.Unlock()

	nodeNotifyCache[node.InstallID] = &NodeNotifyConfig{
		UUID:                  node.UUID,
		Name:                  node.Name,
		OfflineNotifyEnabled:  node.OfflineNotifyEnabled,
		OfflineNotifyGraceSec: NormalizeNodeOfflineGraceSec(node.OfflineNotifyGraceSec),
	}
}

// InitNodeNotifyConfigCache 全量加载数据库节点的通知配置至内存
func InitNodeNotifyConfigCache() {
	var nodes []database.NodePool
	if err := database.DB.Select("uuid", "install_id", "name", "offline_notify_enabled", "offline_notify_grace_sec", "offline_last_notify_at").Find(&nodes).Error; err != nil {
		logger.Log.Warn("初始化节点通知配置缓存失败", "error", err)
		return
	}

	nodeNotifyCacheMu.Lock()
	// 重置缓存
	nodeNotifyCache = make(map[string]*NodeNotifyConfig)
	for _, n := range nodes {
		if strings.TrimSpace(n.InstallID) == "" {
			continue
		}
		var lastNotify time.Time
		if n.OfflineLastNotifyAt != nil {
			lastNotify = *n.OfflineLastNotifyAt
		}
		nodeNotifyCache[n.InstallID] = &NodeNotifyConfig{
			UUID:                  n.UUID,
			Name:                  n.Name,
			OfflineNotifyEnabled:  n.OfflineNotifyEnabled,
			OfflineNotifyGraceSec: NormalizeNodeOfflineGraceSec(n.OfflineNotifyGraceSec),
			OfflineLastNotifyAt:   lastNotify,
		}
	}
	nodeNotifyCacheMu.Unlock()

	// 启动 1 分钟缓冲协程定性未能在系统启动时及时连上的节点
	go func() {
		time.Sleep(1 * time.Minute)

		nodeNotifyCacheMu.Lock()
		defer nodeNotifyCacheMu.Unlock()

		offlineCount := 0
		for installID, config := range nodeNotifyCache {
			if config.OfflineNotifyEnabled && !IsNodeOnlineForStatus(installID) {
				config.PreBootOffline = true
				offlineCount++
			}
		}
	}()
}

// IsValidInstallID 供 WebSocket 及其他模块快速校验节点有效性（完全基于内存字典）
func IsValidInstallID(installID string) bool {
	nodeNotifyCacheMu.RLock()
	defer nodeNotifyCacheMu.RUnlock()
	_, exists := nodeNotifyCache[installID]
	return exists
}

func updateNodeOfflineLastNotifyAt(nodeUUID string, at time.Time) {
	if err := database.DB.Model(&database.NodePool{}).
		Where("uuid = ?", nodeUUID).
		Update("offline_last_notify_at", at).Error; err != nil {
		logger.Log.Warn("更新节点最后通知时间失败", "uuid", nodeUUID, "error", err)
	}
}

type tgNotifyTarget struct {
	UserID             int64
	AllowNode          bool
	AllowLogin         bool
	AllowSpeed         bool
	AllowThresholdStop bool
}

func parseTGNotifyTargets(raw string) []tgNotifyTarget {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	out := make([]tgNotifyTarget, 0, len(parts))

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}

		entry := strings.SplitN(p, "=", 2)
		idStr := strings.TrimSpace(entry[0])
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}

		target := tgNotifyTarget{UserID: id, AllowNode: true, AllowLogin: true, AllowSpeed: true, AllowThresholdStop: true}

		if len(entry) == 2 {
			rest := strings.TrimSpace(entry[1])
			if rest != "" {
				metaParts := strings.Split(rest, "|")
				for i := 1; i < len(metaParts); i++ {
					meta := strings.TrimSpace(metaParts[i])
					if meta == "" {
						continue
					}
					kv := strings.SplitN(meta, "=", 2)
					if len(kv) != 2 {
						continue
					}
					k := strings.ToLower(strings.TrimSpace(kv[0]))
					v := strings.ToLower(strings.TrimSpace(kv[1]))
					allow := v == "1" || v == "true"
					switch k {
					case "node", "node_status":
						target.AllowNode = allow
					case "login", "login_notify":
						target.AllowLogin = allow
					case "speedtest", "speed_test", "batch_speedtest", "speed_notify":
						target.AllowSpeed = allow
					case "threshold", "threshold_stop", "threshold_notify", "threshold_stop_notify":
						target.AllowThresholdStop = allow
					}
				}
			}
		}

		out = append(out, target)
	}

	return out
}

func parseTGNotifyUsers(raw string) []int64 {
	targets := parseTGNotifyTargets(raw)
	out := make([]int64, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.UserID)
	}
	return out
}

type tgBotSocks5Config struct {
	Enabled bool
	Addr    string
	User    string
	Pass    string
}

func loadTGBotSocks5Config() (tgBotSocks5Config, error) {
	keys := []string{
		"tg_bot_socks5_enabled",
		"tg_bot_socks5_addr",
		"tg_bot_socks5_user",
		"tg_bot_socks5_pass",
	}
	var cfgList []database.SysConfig
	if err := database.DB.Where("key IN ?", keys).Find(&cfgList).Error; err != nil {
		return tgBotSocks5Config{}, err
	}

	cfg := tgBotSocks5Config{}
	for _, item := range cfgList {
		v := strings.TrimSpace(item.Value)
		switch item.Key {
		case "tg_bot_socks5_enabled":
			cfg.Enabled = strings.EqualFold(v, "true")
		case "tg_bot_socks5_addr":
			cfg.Addr = v
		case "tg_bot_socks5_user":
			cfg.User = v
		case "tg_bot_socks5_pass":
			cfg.Pass = v
		}
	}
	return cfg, nil
}

func newTelegramBotAPI(token string) (*tgbotapi.BotAPI, error) {
	proxyCfg, err := loadTGBotSocks5Config()
	if err != nil {
		return nil, fmt.Errorf("读取 TG Bot Socks5 配置失败: %w", err)
	}

	if !proxyCfg.Enabled {
		return tgbotapi.NewBotAPI(token)
	}

	addr := strings.TrimSpace(proxyCfg.Addr)
	if addr == "" {
		return nil, fmt.Errorf("tg_bot_socks5_addr 为空，无法使用代理")
	}

	// 支持填写 host:port 或 socks5://user:pass@host:port
	if strings.Contains(addr, "://") {
		u, parseErr := url.Parse(addr)
		if parseErr != nil {
			return nil, fmt.Errorf("解析 tg_bot_socks5_addr 失败: %w", parseErr)
		}
		if u.Scheme != "socks5" && u.Scheme != "socks5h" {
			return nil, fmt.Errorf("tg_bot_socks5_addr 协议必须是 socks5 或 socks5h")
		}
		if strings.TrimSpace(u.Host) == "" {
			return nil, fmt.Errorf("tg_bot_socks5_addr 缺少 host:port")
		}
		addr = strings.TrimSpace(u.Host)
		if proxyCfg.User == "" && u.User != nil {
			proxyCfg.User = u.User.Username()
			if p, ok := u.User.Password(); ok {
				proxyCfg.Pass = p
			}
		}
	}

	if !strings.Contains(addr, ":") {
		return nil, fmt.Errorf("tg_bot_socks5_addr 必须包含 host:port")
	}

	var auth *proxy.Auth
	if proxyCfg.User != "" || proxyCfg.Pass != "" {
		auth = &proxy.Auth{
			User:     proxyCfg.User,
			Password: proxyCfg.Pass,
		}
	}

	baseDialer := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	socksDialer, err := proxy.SOCKS5("tcp", addr, auth, baseDialer)
	if err != nil {
		return nil, fmt.Errorf("创建 socks5 拨号器失败: %w", err)
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if d, ok := socksDialer.(proxy.ContextDialer); ok {
				return d.DialContext(ctx, network, address)
			}
			return socksDialer.Dial(network, address)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   70 * time.Second,
	}
	return tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, client)
}

func sendNodeStatusNotification(nodeName string, online bool, eventTime time.Time) bool {
	keys := []string{"tg_bot_enabled", "tg_bot_token", "tg_bot_whitelist"}
	var cfgList []database.SysConfig
	if err := database.DB.Where("key IN ?", keys).Find(&cfgList).Error; err != nil {
		logger.Log.Warn("读取 TG 通知配置失败", "error", err)
		return false
	}

	cfg := map[string]string{}
	for _, c := range cfgList {
		cfg[c.Key] = strings.TrimSpace(c.Value)
	}

	if cfg["tg_bot_enabled"] != "true" {
		return false
	}
	token := cfg["tg_bot_token"]
	if token == "" {
		return false
	}
	targets := parseTGNotifyTargets(cfg["tg_bot_whitelist"])
	users := make([]int64, 0, len(targets))
	for _, t := range targets {
		if t.AllowNode {
			users = append(users, t.UserID)
		}
	}
	if len(users) == 0 {
		return false
	}

	bot, err := newTelegramBotAPI(token)
	if err != nil {
		logger.Log.Warn("初始化 TG Bot 失败，无法发送节点状态通知", "error", err)
		return false
	}

	statusText := "离线"
	timeLabel := "离线时间"
	emoji := "🔴"
	if online {
		statusText = "上线"
		timeLabel = "上线时间"
		emoji = "🟢"
	}

	if strings.TrimSpace(nodeName) == "" {
		nodeName = "未命名节点"
	}

	msgText := fmt.Sprintf("%s 节点%s通知\n节点名称：%s\n%s：%s", emoji, statusText, nodeName, timeLabel, eventTime.Format("2006-01-02 15:04:05"))

	sentAny := false
	for _, uid := range users {
		msg := tgbotapi.NewMessage(uid, msgText)
		if _, err := bot.Send(msg); err != nil {
			logger.Log.Warn("发送 TG 节点状态通知失败", "user_id", uid, "node_name", nodeName, "status", statusText, "error", err)
			continue
		}
		sentAny = true
	}

	return sentAny
}

// SendAdminLoginNotification 发送后台登录通知到 TG 白名单用户
// success=true 表示登录成功；success=false 表示登录失败
func SendAdminLoginNotification(username, loginIP string, loginTime time.Time, success bool, reason string) bool {
	keys := []string{"tg_bot_enabled", "tg_bot_token", "tg_bot_whitelist", "tg_login_notify_mode", "tg_login_notify_enabled"}
	var cfgList []database.SysConfig
	if err := database.DB.Where("key IN ?", keys).Find(&cfgList).Error; err != nil {
		logger.Log.Warn("读取 TG 登录通知配置失败", "error", err)
		return false
	}

	cfg := map[string]string{}
	for _, c := range cfgList {
		cfg[c.Key] = strings.TrimSpace(c.Value)
	}

	if cfg["tg_bot_enabled"] != "true" {
		return false
	}

	mode := strings.TrimSpace(cfg["tg_login_notify_mode"])
	if mode == "" {
		// 兼容旧版本布尔配置
		if strings.TrimSpace(cfg["tg_login_notify_enabled"]) == "true" {
			mode = "all"
		} else {
			mode = "off"
		}
	}

	switch mode {
	case "off":
		return false
	case "success_only":
		if !success {
			return false
		}
	case "failure_only":
		if success {
			return false
		}
	case "all":
		// continue
	default:
		return false
	}

	if !success && strings.TrimSpace(reason) == "" {
		reason = "未知原因"
	}

	if success {
		reason = ""
	}

	token := cfg["tg_bot_token"]
	if token == "" {
		return false
	}

	targets := parseTGNotifyTargets(cfg["tg_bot_whitelist"])
	users := make([]int64, 0, len(targets))
	for _, t := range targets {
		if t.AllowLogin {
			users = append(users, t.UserID)
		}
	}
	if len(users) == 0 {
		return false
	}

	bot, err := newTelegramBotAPI(token)
	if err != nil {
		logger.Log.Warn("初始化 TG Bot 失败，无法发送登录通知", "error", err)
		return false
	}

	if strings.TrimSpace(username) == "" {
		username = "unknown"
	}
	if strings.TrimSpace(loginIP) == "" {
		loginIP = "unknown"
	}

	countryZh := "未知"
	if GlobalGeoIP != nil {
		if name := strings.TrimSpace(GlobalGeoIP.GetCountryNameZhCN(loginIP)); name != "" {
			countryZh = name
		}
	}

	title := "✅ 管理后台登录成功"
	timeLabel := "登录时间"
	if !success {
		title = "⚠️ 管理后台登录失败"
		timeLabel = "失败时间"
	}

	msgText := fmt.Sprintf("%s\n账号：%s\n%s：%s\n登录IP：%s\n归属地：%s", title, username, timeLabel, loginTime.Format("2006-01-02 15:04:05"), loginIP, countryZh)
	if !success {
		msgText += fmt.Sprintf("\n失败原因：%s", reason)
	}

	sentAny := false
	for _, uid := range users {
		msg := tgbotapi.NewMessage(uid, msgText)
		if _, err := bot.Send(msg); err != nil {
			logger.Log.Warn("发送 TG 登录通知失败", "user_id", uid, "username", username, "ip", loginIP, "error", err)
			continue
		}
		sentAny = true
	}

	return sentAny
}

// SendBatchSpeedTestNotification 发送整组测速任务结束通知（完成/中止）
func SendBatchSpeedTestNotification(subName, taskKey, status string, totalCount, resultCount, errorCount int, startedAt time.Time, finishedAt time.Time) bool {
	keys := []string{"tg_bot_enabled", "tg_bot_token", "tg_bot_whitelist", "tg_speedtest_notify_enabled"}
	var cfgList []database.SysConfig
	if err := database.DB.Where("key IN ?", keys).Find(&cfgList).Error; err != nil {
		logger.Log.Warn("读取 TG 测速通知配置失败", "error", err)
		return false
	}

	cfg := map[string]string{}
	for _, c := range cfgList {
		cfg[c.Key] = strings.TrimSpace(c.Value)
	}

	if cfg["tg_bot_enabled"] != "true" || cfg["tg_speedtest_notify_enabled"] != "true" {
		return false
	}

	token := cfg["tg_bot_token"]
	if token == "" {
		return false
	}

	targets := parseTGNotifyTargets(cfg["tg_bot_whitelist"])
	users := make([]int64, 0, len(targets))
	for _, t := range targets {
		if t.AllowSpeed {
			users = append(users, t.UserID)
		}
	}
	if len(users) == 0 {
		return false
	}

	bot, err := newTelegramBotAPI(token)
	if err != nil {
		logger.Log.Warn("初始化 TG Bot 失败，无法发送测速通知", "error", err)
		return false
	}

	if strings.TrimSpace(subName) == "" {
		subName = "未命名订阅"
	}
	if strings.TrimSpace(taskKey) == "" {
		taskKey = startedAt.Format("200601021504")
	}

	title := "✅ 整组测速任务完成"
	if strings.EqualFold(status, "stopped") {
		title = "⏹️ 整组测速任务已中止"
	}

	msgText := fmt.Sprintf("%s\n订阅：%s\n任务：%s\n开始：%s\n结束：%s\n总节点：%d\n已完成：%d\n错误：%d", title, subName, taskKey, startedAt.Format("2006-01-02 15:04:05"), finishedAt.Format("2006-01-02 15:04:05"), totalCount, resultCount, errorCount)

	sentAny := false
	for _, uid := range users {
		msg := tgbotapi.NewMessage(uid, msgText)
		if _, err := bot.Send(msg); err != nil {
			logger.Log.Warn("发送 TG 测速通知失败", "user_id", uid, "task_key", taskKey, "error", err)
			continue
		}
		sentAny = true
	}

	return sentAny
}

// SendThresholdStopNotification 节点达到流量阈值后发送停机通知。
func SendThresholdStopNotification(nodeName string, thresholdPercent int, usedBytes, thresholdBytes, limitBytes int64) bool {
	keys := []string{"tg_bot_enabled", "tg_bot_token", "tg_bot_whitelist", "tg_threshold_stop_notify_enabled"}
	var cfgList []database.SysConfig
	if err := database.DB.Where("key IN ?", keys).Find(&cfgList).Error; err != nil {
		logger.Log.Warn("读取 TG 阈值停机通知配置失败", "error", err)
		return false
	}

	cfg := map[string]string{}
	for _, c := range cfgList {
		cfg[c.Key] = strings.TrimSpace(c.Value)
	}

	if cfg["tg_bot_enabled"] != "true" || cfg["tg_threshold_stop_notify_enabled"] != "true" {
		return false
	}

	token := cfg["tg_bot_token"]
	if token == "" {
		return false
	}

	targets := parseTGNotifyTargets(cfg["tg_bot_whitelist"])
	users := make([]int64, 0, len(targets))
	for _, t := range targets {
		if t.AllowThresholdStop {
			users = append(users, t.UserID)
		}
	}
	if len(users) == 0 {
		return false
	}

	bot, err := newTelegramBotAPI(token)
	if err != nil {
		logger.Log.Warn("初始化 TG Bot 失败，无法发送阈值停机通知", "error", err)
		return false
	}

	if strings.TrimSpace(nodeName) == "" {
		nodeName = "未命名节点"
	}

	msgText := fmt.Sprintf("⛔ 节点阈值停机通知\n节点：%s\n阈值：%d%%\n已用：%s\n阈值线：%s\n限额：%s\n动作：已触发重置链接并从订阅中剔除", nodeName, thresholdPercent, formatBytes(usedBytes), formatBytes(thresholdBytes), formatBytes(limitBytes))

	sentAny := false
	for _, uid := range users {
		msg := tgbotapi.NewMessage(uid, msgText)
		if _, err := bot.Send(msg); err != nil {
			logger.Log.Warn("发送 TG 阈值停机通知失败", "user_id", uid, "node_name", nodeName, "error", err)
			continue
		}
		sentAny = true
	}

	return sentAny
}

// OnNodeConnectionStatusChanged 在节点 WS 连接状态变化时触发。
func OnNodeConnectionStatusChanged(installID string, online bool) {
	installID = strings.TrimSpace(installID)
	if installID == "" {
		return
	}

	// 防御性校验：若当前状态仍在线，则忽略过期连接触发的离线事件。
	if !online && IsNodeOnlineForStatus(installID) {
		logger.Log.Debug("忽略离线状态变更：节点当前仍被判定为在线", "install_id", installID)
		return
	}

	StartOfflineNotifyLoop()
	now := time.Now()

	offlineNotifyMu.Lock()
	st := offlineNotifyState[installID]
	if st == nil {
		st = &offlineNotifyRuntime{}
		offlineNotifyState[installID] = st
	}
	wasInitialized := st.initialized
	prevOnline := st.online
	prevLastNotified := st.lastNotified

	isNewDisconnect := (!wasInitialized || prevOnline) && !online
	offlineNotifyMu.Unlock()

	nodeNotifyCacheMu.RLock()
	node, nodeFound := nodeNotifyCache[installID]
	nodeNotifyCacheMu.RUnlock()

	offlineNotifyMu.Lock()
	// 获取锁更新最新状态
	st.initialized = true
	st.online = online

	if online {
		st.pendingOfflineAt = time.Time{}
		st.disconnectedAt = time.Time{}
	} else if isNewDisconnect {
		graceSec := 180
		if nodeFound {
			graceSec = node.OfflineNotifyGraceSec
		}
		st.disconnectedAt = now
		if graceSec <= 0 {
			st.pendingOfflineAt = time.Time{}
		} else {
			st.pendingOfflineAt = now.Add(time.Duration(graceSec) * time.Second)
		}
	}
	offlineNotifyMu.Unlock()

	if online {
		if nodeFound && node.OfflineNotifyEnabled {
			// 如果该节点是在重启前发过离线通知 (OfflineLastNotifyAt 有值)
			// 或者在刚才 60 秒的启动震荡期结束后，被盖棺定论打上了死胎记 (PreBootOffline == true)
			// 这些情况全都在系统恢复生命周期管理后给您补发通知，以免暗箱掉线的节点漏报
			isBootRecovery := !node.OfflineLastNotifyAt.IsZero() || node.PreBootOffline

			needOnlineNotify := (wasInitialized && !prevOnline && prevLastNotified == "offline") || isBootRecovery
			if needOnlineNotify {
				if muteUntil, muted := getNodeStatusNotifyMuteUntil(installID, now); muted {
					logger.Log.Debug("节点状态通知处于抑制窗口，跳过上线通知", "install_id", installID, "mute_until", muteUntil.Format(time.RFC3339))
					clearNodeOfflineRecoveryMarkers(installID, node.UUID)
				} else if sendNodeStatusNotification(node.Name, true, now) {
					clearNodeOfflineRecoveryMarkers(installID, node.UUID)
				}
			}
		}

		// 节点已恢复健康并闭环，从内存中剔除清空，避免脏数据内存泄漏
		offlineNotifyMu.Lock()
		delete(offlineNotifyState, installID)
		offlineNotifyMu.Unlock()
	}
}

// StartTelegramBot 初始化并启动 Telegram Bot 监听协程
func StartTelegramBot() {
	// 等待数据库初始化完成
	time.Sleep(2 * time.Second)
	InitNodeNotifyConfigCache()
	RestartTelegramBot()
}

// RestartTelegramBot 重启 Telegram Bot 服务，用于热更新配置
func RestartTelegramBot() {
	if tgBotCancel != nil {
		tgBotCancel()
		tgBotCancel = nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	tgBotCancel = cancel

	go runTelegramBot(ctx)
}

func runTelegramBot(ctx context.Context) {
	var enabledConfig database.SysConfig
	var tokenConfig database.SysConfig
	var whitelistConfig database.SysConfig
	var registerConfig database.SysConfig

	// 获取配置
	database.DB.Where("key = ?", "tg_bot_enabled").First(&enabledConfig)
	database.DB.Where("key = ?", "tg_bot_token").First(&tokenConfig)
	database.DB.Where("key = ?", "tg_bot_whitelist").First(&whitelistConfig)
	database.DB.Where("key = ?", "tg_bot_register_commands").First(&registerConfig)

	botEnabled := strings.TrimSpace(enabledConfig.Value) == "true"
	token := strings.TrimSpace(tokenConfig.Value)
	whitelist := sanitizeTgWhitelist(strings.TrimSpace(whitelistConfig.Value))
	registerCommands := strings.TrimSpace(registerConfig.Value) == "true"

	if !botEnabled {
		return
	}

	if token == "" {
		logger.Log.Warn("Telegram Bot Token 未配置，Bot 未启动")
		return
	}
	if whitelist == "" {
		logger.Log.Warn("Telegram Bot 白名单为空或无有效条目，Bot 未启动")
		return
	}

	bot, err := newTelegramBotAPI(token)
	if err != nil {
		logger.Log.Error("Telegram Bot 初始化失败", "error", err)
		return
	}

	// bot.Debug = true // 可选：开启调试模式

	if registerCommands {
		// 删除并重新注册主菜单指令
		deleteConfig := tgbotapi.NewDeleteMyCommands()
		if _, err := bot.Request(deleteConfig); err != nil {
			logger.Log.Warn("清除历史 TG Bot 指令失败", "error", err)
		}

		setCmdConfig := tgbotapi.NewSetMyCommands(
			tgbotapi.BotCommand{
				Command:     "sub",
				Description: "获取我的节点订阅或机场资源",
			},
		)
		if _, err := bot.Request(setCmdConfig); err != nil {
			logger.Log.Error("注册 TG Bot 指令失败", "error", err)
		}
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			logger.Log.Info("检测到配置变化，正在停止旧的 Telegram Bot 服务...")
			bot.StopReceivingUpdates()
			return
		case update, ok := <-updates:
			if !ok {
				return
			}
			// 采用协程并发处理每条消息，避免因为某一条消息卡顿阻塞整体的接收队列，提升多用户并发响应速度
			go func(upd tgbotapi.Update) {
				// 处理消息 (Message)
				if upd.Message != nil {
					handleMessage(bot, upd.Message, whitelist)
				} else if upd.CallbackQuery != nil {
					handleCallbackQuery(bot, upd.CallbackQuery, whitelist)
				}
			}(update)
		}
	}
}

// sanitizeTgWhitelist 清洗白名单字符串，仅保留 ID 为纯数字的有效条目
func sanitizeTgWhitelist(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, ",")
	valid := make([]string, 0, len(parts))
	for _, item := range parts {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		entry := strings.SplitN(item, "=", 2)
		id := strings.TrimSpace(entry[0])
		if id == "" {
			continue
		}
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			logger.Log.Warn("TG 白名单中发现无效条目，已忽略", "entry", item)
			continue
		}
		valid = append(valid, item)
	}
	return strings.Join(valid, ",")
}

// 检查用户是否在白名单中
func isUserAllowed(userID int64, whitelistStr string) bool {
	if whitelistStr == "" {
		return false // 未配置白名单时，默认拒绝所有人
	}
	whitelist := strings.Split(whitelistStr, ",")
	userIDStr := fmt.Sprintf("%d", userID)
	for _, item := range whitelist {
		parts := strings.SplitN(item, "=", 2)
		id := strings.TrimSpace(parts[0])
		if id == userIDStr {
			return true
		}
	}
	return false
}

// 获取基础面板域名和鉴权 Token
func getBaseURLAndToken() (string, string) {
	var panelURLConfig database.SysConfig
	var subTokenConfig database.SysConfig

	database.DB.Where("key = ?", "panel_url").First(&panelURLConfig)
	database.DB.Where("key = ?", "sub_token").First(&subTokenConfig)

	panelURL := strings.TrimRight(strings.TrimSpace(panelURLConfig.Value), "/")
	if panelURL == "" {
		panelURL = "https://yourdomain.com" // 默认占位
	}
	return panelURL, strings.TrimSpace(subTokenConfig.Value)
}

// 处理普通消息
func handleMessage(bot *tgbotapi.BotAPI, message *tgbotapi.Message, whitelistStr string) {
	if !message.IsCommand() {
		return // 目前只响应命令
	}

	// 白名单验证
	if !isUserAllowed(message.From.ID, whitelistStr) {
		logger.Log.Warn("未授权的 TG 用户尝试访问", "user_id", message.From.ID)
		msg := tgbotapi.NewMessage(message.Chat.ID, "⛔ 未经授权的访问。请将您的 TG ID 添加到管理员白名单中。")
		bot.Send(msg)
		return
	}

	switch message.Command() {
	case "start", "sub":
		sendSubMenu(bot, message.Chat.ID)
	default:
		msg := tgbotapi.NewMessage(message.Chat.ID, "未知命令。请发送 /sub 获取订阅菜单。")
		bot.Send(msg)
	}
}

// 发送订阅主菜单 (包含一级按钮)
func sendSubMenu(bot *tgbotapi.BotAPI, chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "🚀 请选择菜单")

	// 创建 Inline Keyboard，包含一级菜单按钮
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🌐 订阅中心", "menu_sub_center"),
			tgbotapi.NewInlineKeyboardButtonData("📡 服务器列表", "menu_nodes:1"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🛡️ TG 代理", "menu_tg_proxy"),
		),
	)

	msg.ReplyMarkup = keyboard
	bot.Send(msg)
}

// 处理点击 Inline Keyboard 回调事件
func handleCallbackQuery(bot *tgbotapi.BotAPI, callbackQuery *tgbotapi.CallbackQuery, whitelistStr string) {
	// 同样需要进行白名单验证 (防止之前的消息被转发或未授权用户点击)
	if !isUserAllowed(callbackQuery.From.ID, whitelistStr) {
		bot.Request(tgbotapi.NewCallback(callbackQuery.ID, "⛔ 未经授权"))
		return
	}

	// 响应 Telegram 提示收到回调，消除按钮顶部的 loading 状态（提速体感）
	bot.Request(tgbotapi.NewCallback(callbackQuery.ID, ""))

	data := callbackQuery.Data
	chatID := callbackQuery.Message.Chat.ID
	messageID := callbackQuery.Message.MessageID

	// 采用异步处理以提升点击按钮时的响应速度
	go func() {
		switch data {
		case "menu_sub_center":
			// 点击“订阅中心”，修改原消息，显示二级菜单
			editMsg := tgbotapi.NewEditMessageText(chatID, messageID, "👇 请选择具体的订阅格式：")
			keyboard := tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("🐱 Clash 订阅", "get_sub_clash"),
					tgbotapi.NewInlineKeyboardButtonData("✌️ V2ray 订阅", "get_sub_v2ray"),
				),
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("🔄 重置订阅", "reset_sub_token"),
					tgbotapi.NewInlineKeyboardButtonData("🔙 返回上级", "menu_main"),
				),
			)
			editMsg.ReplyMarkup = &keyboard
			bot.Send(editMsg)

		case "menu_main":
			// 返回主菜单
			editMsg := tgbotapi.NewEditMessageText(chatID, messageID, "🚀 请选择要获取的订阅类型：")
			keyboard := tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("🌐 订阅中心", "menu_sub_center"),
					tgbotapi.NewInlineKeyboardButtonData("📡 服务器列表", "menu_nodes:1"),
				),
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("🛡️ TG 代理", "menu_tg_proxy"),
				),
			)
			editMsg.ReplyMarkup = &keyboard
			bot.Send(editMsg)

		case "get_sub_clash":
			panelURL, token := getBaseURLAndToken()
			subURL := fmt.Sprintf("%s/sub/clash?token=%s", panelURL, token)
			replyText := fmt.Sprintf("✅ **您的 Clash (Mihomo) 订阅链接：**\n\n`%s`", subURL)

			msg := tgbotapi.NewMessage(chatID, replyText)
			msg.ParseMode = "Markdown"
			sentMsg, err := bot.Send(msg)
			if err == nil {
				go func(chatID int64, messageID int) {
					time.Sleep(10 * time.Second)
					deleteMsg := tgbotapi.NewDeleteMessage(chatID, messageID)
					bot.Request(deleteMsg)
				}(chatID, sentMsg.MessageID)
			}

		case "get_sub_v2ray":
			panelURL, token := getBaseURLAndToken()
			subURL := fmt.Sprintf("%s/sub/v2ray?token=%s", panelURL, token)
			replyText := fmt.Sprintf("✅ **您的 V2ray 订阅链接：**\n\n`%s`", subURL)

			msg := tgbotapi.NewMessage(chatID, replyText)
			msg.ParseMode = "Markdown"
			sentMsg, err := bot.Send(msg)
			if err == nil {
				go func(chatID int64, messageID int) {
					time.Sleep(10 * time.Second)
					deleteMsg := tgbotapi.NewDeleteMessage(chatID, messageID)
					bot.Request(deleteMsg)
				}(chatID, sentMsg.MessageID)
			}

		case "menu_tg_proxy":
			var nodes []database.NodePool
			database.DB.Where("is_blocked = ? AND ipv6 != ''", false).Find(&nodes)

			var tgProxies []string
			for _, node := range nodes {
				if link, ok := node.Links["socks5"]; ok && link != "" {
					parsedNode := ParseLinkToClashNode(link, "")
					if parsedNode != nil && parsedNode.Type == "socks5" {
						params := url.Values{}
						params.Add("server", node.IPV6)
						params.Add("port", strconv.Itoa(parsedNode.Port))
						if parsedNode.Username != "" {
							params.Add("user", parsedNode.Username)
						}
						if parsedNode.Password != "" {
							params.Add("pass", parsedNode.Password)
						}

						proxyLink := "https://t.me/socks?" + params.Encode()
						escapedName := escapeMarkdown(node.Name)
						tgProxies = append(tgProxies, fmt.Sprintf("▪️ **%s** | [👉 代理直链](%s)", escapedName, proxyLink))
					}
				}
			}

			var replyText string
			if len(tgProxies) == 0 {
				replyText = "📭 目前没有任何支持 IPv6 的 Socks5 节点。"
			} else {
				replyText = "🛡️ **可用的 TG 代理：**\n\n" + strings.Join(tgProxies, "\n")
			}

			msg := tgbotapi.NewMessage(chatID, replyText)
			msg.ParseMode = "Markdown"
			sentMsg, err := bot.Send(msg)
			if err == nil && len(tgProxies) > 0 {
				go func(cID int64, mID int) {
					time.Sleep(30 * time.Second)
					bot.Request(tgbotapi.NewDeleteMessage(cID, mID))
				}(chatID, sentMsg.MessageID)
			}

		case "reset_sub_token":
			secureBytes := make([]byte, 16)
			rand.Read(secureBytes)
			newToken := hex.EncodeToString(secureBytes)

			err := database.DB.Model(&database.SysConfig{}).Where("key = ?", "sub_token").Update("value", newToken).Error
			if err != nil {
				logger.Log.Error("TG Bot 重置 Sub Token 失败", "error", err)
				msg := tgbotapi.NewMessage(chatID, "❌ 重置订阅 Token 失败，请检查系统日志。")
				bot.Send(msg)
				return
			}

			logger.Log.Info("用户通过 TG Bot 重置了订阅 Token", "user_id", callbackQuery.From.ID)
			replyText := "✅ **订阅重置成功！**"

			msg := tgbotapi.NewMessage(chatID, replyText)
			msg.ParseMode = "Markdown"
			bot.Send(msg)

		default:
			if strings.HasPrefix(data, "menu_nodes:") {
				handleMenuNodes(bot, chatID, messageID, data)
			} else if strings.HasPrefix(data, "node_info:") {
				handleNodeInfo(bot, chatID, messageID, data)
			}
		}
	}()
}

func escapeMarkdown(text string) string {
	text = strings.ReplaceAll(text, "_", "\\_")
	text = strings.ReplaceAll(text, "*", "\\*")
	text = strings.ReplaceAll(text, "`", "\\`")
	text = strings.ReplaceAll(text, "[", "\\[")
	return text
}

func formatBytes(b int64) string {
	if b == 0 {
		return "0G"
	}
	gb := float64(b) / (1024 * 1024 * 1024)
	if gb >= 1024 {
		return fmt.Sprintf("%.2fT", gb/1024)
	}
	return fmt.Sprintf("%.2fG", gb)
}

func handleMenuNodes(bot *tgbotapi.BotAPI, chatID int64, messageID int, data string) {
	pageStr := strings.TrimPrefix(data, "menu_nodes:")
	page, err := strconv.Atoi(pageStr)
	if err != nil || page < 1 {
		page = 1
	}

	var totalNodes int64
	database.DB.Model(&database.NodePool{}).Where("is_blocked = ?", false).Count(&totalNodes)

	if totalNodes == 0 {
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, "📭 当前没有任何节点。")
		btn := tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("🔙 返回上级", "menu_main"))
		keyboard := tgbotapi.NewInlineKeyboardMarkup(btn)
		editMsg.ReplyMarkup = &keyboard
		bot.Send(editMsg)
		return
	}

	perPage := 12
	totalPages := int(math.Ceil(float64(totalNodes) / float64(perPage)))
	if page > totalPages {
		page = totalPages
	}

	offset := (page - 1) * perPage

	var nodes []database.NodePool
	// 按照流量消耗排行，一页显示12个
	database.DB.Where("is_blocked = ?", false).
		Order("(traffic_up + traffic_down) DESC").
		Limit(perPage).Offset(offset).Find(&nodes)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📊 **服务器流量排行榜 (第 %d/%d 页)**\n\n", page, totalPages))

	getStringWidth := func(s string) int {
		width := 0
		for _, r := range s {
			if r > 127 {
				width += 2
			} else {
				width += 1
			}
		}
		return width
	}

	var maxNameWidth int
	var maxUsedWidth int

	type listNodeInfo struct {
		id        string
		name      string
		used      string
		limit     string
		limitType string
	}
	var infos []listNodeInfo

	for _, node := range nodes {
		nameWidth := getStringWidth(node.Name)
		if nameWidth > maxNameWidth {
			maxNameWidth = nameWidth
		}

		usedBytes := ComputeTrafficUsedByLimitType(node.TrafficUp, node.TrafficDown, node.TrafficLimitType)
		usedStr := formatBytes(usedBytes)
		usedWidth := getStringWidth(usedStr)
		if usedWidth > maxUsedWidth {
			maxUsedWidth = usedWidth
		}

		limitStr := "不限量"
		if node.TrafficLimit > 0 {
			limitStr = formatBytes(node.TrafficLimit)
		}
		infos = append(infos, listNodeInfo{node.UUID, node.Name, usedStr, limitStr, TrafficLimitTypeCN(node.TrafficLimitType)})
	}

	for _, info := range infos {
		namePad := maxNameWidth - getStringWidth(info.name)
		if namePad < 0 {
			namePad = 0
		}
		paddedName := info.name + strings.Repeat(" ", namePad)

		usedPad := maxUsedWidth - getStringWidth(info.used)
		if usedPad < 0 {
			usedPad = 0
		}
		paddedUsed := strings.Repeat(" ", usedPad) + info.used

		sb.WriteString(fmt.Sprintf("`%s | %s | %s(%s)`\n", paddedName, paddedUsed, info.limit, info.limitType))
	}

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, sb.String())
	editMsg.ParseMode = "Markdown"

	// 构建键盘
	var keyboard [][]tgbotapi.InlineKeyboardButton

	var currentRow []tgbotapi.InlineKeyboardButton
	for i, n := range nodes {
		currentRow = append(currentRow, tgbotapi.NewInlineKeyboardButtonData(n.Name, "node_info:"+n.UUID))
		if len(currentRow) == 3 || i == len(nodes)-1 {
			keyboard = append(keyboard, currentRow)
			currentRow = nil
		}
	}

	// 翻页行
	var pageRow []tgbotapi.InlineKeyboardButton
	if page > 1 {
		pageRow = append(pageRow, tgbotapi.NewInlineKeyboardButtonData("◀️ 上一页", fmt.Sprintf("menu_nodes:%d", page-1)))
	}
	if page < totalPages {
		pageRow = append(pageRow, tgbotapi.NewInlineKeyboardButtonData("▶️ 下一页", fmt.Sprintf("menu_nodes:%d", page+1)))
	}
	if len(pageRow) > 0 {
		keyboard = append(keyboard, pageRow)
	}

	// 返回行
	keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔙 返回主菜单", "menu_main"),
	))

	kbd := tgbotapi.NewInlineKeyboardMarkup(keyboard...)
	editMsg.ReplyMarkup = &kbd
	bot.Send(editMsg)
}

func handleNodeInfo(bot *tgbotapi.BotAPI, chatID int64, messageID int, data string) {
	uuid := strings.TrimPrefix(data, "node_info:")
	var node database.NodePool
	if err := database.DB.Where("uuid = ?", uuid).First(&node).Error; err != nil {
		bot.Send(tgbotapi.NewEditMessageText(chatID, messageID, "❌ 找不到该节点信息。"))
		return
	}

	routingType := "禁用节点"
	switch node.RoutingType {
	case 1:
		routingType = "中转节点"
	case 2:
		routingType = "落地节点"
	}

	upStr := formatBytes(node.TrafficUp)
	downStr := formatBytes(node.TrafficDown)
	usedStr := formatBytes(ComputeTrafficUsedByLimitType(node.TrafficUp, node.TrafficDown, node.TrafficLimitType))
	limitStr := "不限量"
	if node.TrafficLimit > 0 {
		limitStr = formatBytes(node.TrafficLimit)
	}
	limitTypeStr := TrafficLimitTypeCN(node.TrafficLimitType)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("💻 **[%s] 的专属信息**\n\n", node.Name))
	sb.WriteString(fmt.Sprintf("▪️ **网络组：** %s\n", routingType))
	sb.WriteString(fmt.Sprintf("▪️ **已用上传：** %s\n", upStr))
	sb.WriteString(fmt.Sprintf("▪️ **已用下载：** %s\n", downStr))
	sb.WriteString(fmt.Sprintf("▪️ **限额类型：** %s\n", limitTypeStr))
	sb.WriteString(fmt.Sprintf("▪️ **计费消耗：** %s / %s\n", usedStr, limitStr))
	if node.Remark != "" {
		sb.WriteString(fmt.Sprintf("▪️ **备注：** %s\n", node.Remark))
	}

	sb.WriteString("\n🔗 **协议链接：**\n")
	hasLink := false
	for protoType, link := range node.Links {
		if link != "" {
			sb.WriteString(fmt.Sprintf("**%s**: `%s`\n", strings.ToUpper(protoType), link))
			hasLink = true
		}
	}

	if !hasLink {
		sb.WriteString("⚠️ 该节点目前没有任何协议链接。\n")
	}

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, sb.String())
	editMsg.ParseMode = "Markdown"

	keyboard := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 返回服务器列表", "menu_nodes:1"),
		),
	}

	kbd := tgbotapi.NewInlineKeyboardMarkup(keyboard...)
	editMsg.ReplyMarkup = &kbd
	bot.Send(editMsg)
}
