package database

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"nodectl/internal/logger"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// DB 全局数据库实例
var DB *gorm.DB

// dbMu 保护数据库切换操作的互斥锁
var dbMu sync.RWMutex

// ------------------- [数据库配置] -------------------

// DBConfig 数据库连接配置 (存储在 data/dbconfig.json)
type DBConfig struct {
	Type     string `json:"type"`               // "sqlite" 或 "postgres"
	Host     string `json:"host,omitempty"`     // PostgreSQL 主机地址
	Port     int    `json:"port,omitempty"`     // PostgreSQL 端口
	User     string `json:"user,omitempty"`     // PostgreSQL 用户名
	Password string `json:"password,omitempty"` // PostgreSQL 密码
	DBName   string `json:"dbname,omitempty"`   // PostgreSQL 数据库名
	SSLMode  string `json:"sslmode,omitempty"`  // PostgreSQL SSL 模式 (disable/require/verify-full)
	WebPort  int    `json:"web_port,omitempty"` // Web 服务监听端口 (默认 8080，Docker 环境始终使用 8080)
}

// DBStatus 数据库状态信息
type DBStatus struct {
	Type      string `json:"type"`       // 当前数据库类型
	SizeBytes int64  `json:"size_bytes"` // 数据库大小 (bytes)
	SizeHuman string `json:"size_human"` // 人类可读大小
	Healthy   bool   `json:"healthy"`    // 健康状态
	HealthMsg string `json:"health_msg"` // 健康信息
	// PostgreSQL 连接参数 (密码脱敏)
	Host       string           `json:"host,omitempty"`
	Port       int              `json:"port,omitempty"`
	User       string           `json:"user,omitempty"`
	DBName     string           `json:"dbname,omitempty"`
	SSLMode    string           `json:"sslmode,omitempty"`
	TableCount int              `json:"table_count"` // 表数量
	RecordInfo map[string]int64 `json:"record_info"` // 各表记录数
}

const dbConfigPath = "data/dbconfig.json"

// LoadDBConfig 从文件加载数据库配置
func LoadDBConfig() DBConfig {
	data, err := os.ReadFile(dbConfigPath)
	if err != nil {
		// 文件不存在，返回默认 SQLite 配置
		return DBConfig{Type: "sqlite"}
	}
	var cfg DBConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		logger.Log.Warn("解析数据库配置文件失败，使用默认 SQLite", "err", err)
		return DBConfig{Type: "sqlite"}
	}
	if cfg.Type == "" {
		cfg.Type = "sqlite"
	}
	return cfg
}

// SaveDBConfig 保存数据库配置到文件
func SaveDBConfig(cfg DBConfig) error {
	if err := os.MkdirAll("data", os.ModePerm); err != nil {
		return fmt.Errorf("创建 data 目录失败: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	return os.WriteFile(dbConfigPath, data, 0600)
}

// GetCurrentDBConfig 获取当前数据库配置 (密码脱敏)
func GetCurrentDBConfig() DBConfig {
	cfg := LoadDBConfig()
	cfg.Password = "" // 脱敏
	return cfg
}

// ------------------- [模型定义区] -------------------

// NodePool 节点池表
type NodePool struct {
	UUID                     string            `gorm:"primaryKey;column:uuid;type:varchar(36)" json:"uuid"`
	InstallID                string            `gorm:"column:install_id;type:varchar(12);uniqueIndex" json:"install_id"`
	Name                     string            `gorm:"column:name" json:"name"`
	OfflineNotifyEnabled     bool              `gorm:"column:offline_notify_enabled;default:false" json:"offline_notify_enabled"`
	OfflineNotifyGraceSec    int               `gorm:"column:offline_notify_grace_sec;default:180" json:"offline_notify_grace_sec"`
	OfflineLastNotifyAt      *time.Time        `gorm:"column:offline_last_notify_at" json:"offline_last_notify_at"`
	RoutingType              int               `gorm:"column:routing_type;default:1" json:"routing_type"` // 0=禁用，1=中转，2=落地
	IsBlocked                bool              `gorm:"column:is_blocked;default:false" json:"is_blocked"` // 是否屏蔽
	Links                    map[string]string `gorm:"column:links;serializer:json" json:"links"`
	LinkIPModes              map[string]int    `gorm:"column:link_ip_modes;serializer:json" json:"link_ip_modes"` //协议级别的IP生成模式
	LinkPorts                map[string]int    `gorm:"column:link_ports;serializer:json" json:"link_ports"`       // 协议级别的个性化端口设置（创建时从SysConfig导入默认值）
	DisabledLinks            []string          `gorm:"column:disabled_links;serializer:json" json:"disabled_links"`
	IPV4                     string            `gorm:"column:ipv4;type:varchar(15)" json:"ipv4"`
	IPV6                     string            `gorm:"column:ipv6;type:varchar(45)" json:"ipv6"`
	Region                   string            `gorm:"column:region" json:"region"`                                                          //存储国家信息
	IPMode                   int               `gorm:"column:ip_mode;default:0" json:"ip_mode"`                                              // 0: 跟随系统, 1: 仅IPv4, 2: 仅IPv6, 3: 双栈
	SortIndex                int               `gorm:"column:sort_index;default:0" json:"sort_index"`                                        //排序
	Remark                   string            `gorm:"column:remark" json:"remark"`                                                          //备注
	TrafficUp                int64             `gorm:"column:traffic_up;default:0" json:"traffic_up"`                                        // 本周期上传流量 (Bytes)
	TrafficDown              int64             `gorm:"column:traffic_down;default:0" json:"traffic_down"`                                    // 本周期下载流量 (Bytes)
	TrafficLimit             int64             `gorm:"column:traffic_limit;default:0" json:"traffic_limit"`                                  // 总流量限额 (Bytes, 0表示不限制)
	TrafficLimitType         string            `gorm:"column:traffic_limit_type;type:varchar(16);default:'total'" json:"traffic_limit_type"` // 限额计算方式: total|max|min|up|down
	TrafficThresholdEnabled  bool              `gorm:"column:traffic_threshold_enabled;default:false" json:"traffic_threshold_enabled"`      // 是否启用阈值停机
	TrafficThresholdPercent  int               `gorm:"column:traffic_threshold_percent;default:0" json:"traffic_threshold_percent"`          // 阈值百分比(0-100, 0表示不限制)
	TrafficThresholdReached  bool              `gorm:"column:traffic_threshold_reached;default:false" json:"traffic_threshold_reached"`      // 是否已触发阈值停机(首次触发后置为 true)
	ResetDay                 int               `gorm:"column:reset_day;default:0" json:"reset_day"`                                          // 每月重置日 (1-31, 0表示不重置)
	TrafficResetMode         string            `gorm:"column:traffic_reset_mode;type:varchar(24);default:'off'" json:"traffic_reset_mode"`   // 重置方式: off|fixed_day|calendar_month|interval_days
	TrafficResetIntervalDays int               `gorm:"column:traffic_reset_interval_days;default:30" json:"traffic_reset_interval_days"`     // 按天数重置周期(天)
	TrafficResetAnchorAt     *time.Time        `gorm:"column:traffic_reset_anchor_at" json:"traffic_reset_anchor_at"`                        // 按天数重置起算时间
	TrafficResetAt           *time.Time        `gorm:"column:traffic_reset_at" json:"traffic_reset_at"`                                      // 上次周期重置时间
	TrafficUpdateAt          *time.Time        `gorm:"column:traffic_update_at" json:"traffic_update_at"`                                    // 流量更新时间
	AgentVersion             string            `gorm:"column:agent_version;type:varchar(32);default:''" json:"agent_version"`                // Agent 版本号
	TrafficHistoryCount      *int64            `gorm:"column:traffic_history_count" json:"traffic_history_count"`                            // 历史流量记录条数（写入计数，NULL表示未初始化需查库）
	TunnelEnabled            bool              `gorm:"column:tunnel_enabled;default:false" json:"tunnel_enabled"`                            // 是否启用 tunnel 加速
	TunnelID                 string            `gorm:"column:tunnel_id;type:varchar(64);default:''" json:"tunnel_id"`                        // 节点绑定的 Tunnel ID
	TunnelToken              string            `gorm:"column:tunnel_token;type:text;default:''" json:"tunnel_token"`                         // 节点专属 Tunnel Token（每节点独立）
	TunnelName               string            `gorm:"column:tunnel_name;type:varchar(128);default:''" json:"tunnel_name"`                   // 节点 Tunnel 名称
	TunnelDomain             string            `gorm:"column:tunnel_domain;type:varchar(255);default:''" json:"tunnel_domain"`               // tunnel 加速域名
	CreatedAt                time.Time         `gorm:"column:created_at" json:"created_at"`
	UpdatedAt                time.Time         `gorm:"column:updated_at" json:"updated_at"`
}

func (NodePool) TableName() string {
	return "node_pool"
}

// NodeTrafficStat 节点流量历史原始记录表（仅存储上报原始值）
type NodeTrafficStat struct {
	ID         uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	NodeUUID   string    `gorm:"column:node_uuid;type:varchar(36);not null;index:idx_nts_node_hour,priority:1;index:idx_nts_node_time,priority:1" json:"node_uuid"`
	ReportedAt time.Time `gorm:"column:reported_at;not null;index:idx_nts_node_time,priority:2" json:"reported_at"`
	HourKey    int       `gorm:"column:hour_key;not null;index:idx_nts_node_hour,priority:2" json:"hour_key"` // 时间字典: YYYYMMDDHH
	TwoHourKey int       `gorm:"column:two_hour_key;not null;index:idx_nts_two_hour_key" json:"two_hour_key"` // 时间字典: YYYYMMDDHH(偶数小时)
	DayKey     int       `gorm:"column:day_key;not null;index:idx_nts_day_key" json:"day_key"`                // 时间字典: YYYYMMDD
	TXBytes    int64     `gorm:"column:tx_bytes;default:0" json:"tx_bytes"`                                   // 节点原始上报上传值
	RXBytes    int64     `gorm:"column:rx_bytes;default:0" json:"rx_bytes"`                                   // 节点原始上报下载值
	CreatedAt  time.Time `gorm:"column:created_at" json:"created_at"`

	Node NodePool `gorm:"foreignKey:NodeUUID;references:UUID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"-"`
}

func (NodeTrafficStat) TableName() string {
	return "node_traffic_stats"
}

func (n *NodePool) BeforeCreate(tx *gorm.DB) (err error) {
	if n.UUID == "" {
		n.UUID = uuid.New().String()
	}
	if n.InstallID == "" {
		n.InstallID = generateSecureRandomID(12)
	}

	// 从 SysConfig 读取所有协议的默认端口，写入 LinkPorts 作为该节点的初始端口配置
	if n.LinkPorts == nil || len(n.LinkPorts) == 0 {
		n.LinkPorts = loadDefaultPortsFromSysConfig(tx)
	}

	return
}

// loadDefaultPortsFromSysConfig 从 SysConfig 中读取所有 proxy_port_* 配置，
// 将协议名映射为端口号返回。用于节点创建时填充默认端口。
func loadDefaultPortsFromSysConfig(tx *gorm.DB) map[string]int {
	// 协议名 -> SysConfig Key 的映射
	protoToKey := map[string]string{
		"ss":         "proxy_port_ss",
		"hy2":        "proxy_port_hy2",
		"tuic":       "proxy_port_tuic",
		"reality":    "proxy_port_reality",
		"socks5":     "proxy_port_socks5",
		"trojan":     "proxy_port_trojan",
		"anytls":     "proxy_port_anytls",
		"vmess_tcp":  "proxy_port_vmess_tcp",
		"vmess_ws":   "proxy_port_vmess_ws",
		"vmess_http": "proxy_port_vmess_http",
		"vmess_quic": "proxy_port_vmess_quic",
		"vmess_wst":  "proxy_port_vmess_wst",
		"vmess_hut":  "proxy_port_vmess_hut",
		"vless_wst":  "proxy_port_vless_wst",
		"vless_hut":  "proxy_port_vless_hut",
		"trojan_wst": "proxy_port_trojan_wst",
		"trojan_hut": "proxy_port_trojan_hut",
	}

	// 收集所有需要查询的 Key
	keys := make([]string, 0, len(protoToKey))
	for _, key := range protoToKey {
		keys = append(keys, key)
	}

	// 使用干净的会话查询 SysConfig，避免 BeforeCreate 事务上下文中
	// 携带的 Model/Where 等子句污染查询，导致结果不全。
	cleanDB := tx.Session(&gorm.Session{NewDB: true})
	var cfgs []SysConfig
	cleanDB.Where("key IN ?", keys).Find(&cfgs)

	// 构建 Key -> Value 映射
	valueByKey := make(map[string]string, len(cfgs))
	for _, c := range cfgs {
		valueByKey[c.Key] = strings.TrimSpace(c.Value)
	}

	// 构建结果 map
	ports := make(map[string]int, len(protoToKey))
	for proto, key := range protoToKey {
		if valStr, ok := valueByKey[key]; ok && valStr != "" {
			if port, err := strconv.Atoi(valStr); err == nil && port > 0 {
				ports[proto] = port
			}
		}
	}

	return ports
}

func generateSecureRandomID(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		panic("failed to generate secure random id: " + err.Error())
	}
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b)
}

// SysConfig 系统全局配置表 (Key-Value 设计)
type SysConfig struct {
	Key         string    `gorm:"primaryKey;column:key;type:varchar(64)" json:"key"`
	Value       string    `gorm:"column:value;type:text" json:"value"`
	Description string    `gorm:"column:description;type:varchar(255)" json:"description"`
	UpdatedAt   time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (SysConfig) TableName() string {
	return "sys_config"
}

// ------------------- 机场订阅相关模型] -------------------

// AirportSub 机场订阅源表
type AirportSub struct {
	ID        string    `gorm:"primaryKey;type:varchar(36)" json:"id"`
	Name      string    `gorm:"type:varchar(64)" json:"name"`                             // 机场名称
	URL       string    `gorm:"type:text" json:"url"`                                     // 订阅链接
	Upload    int64     `gorm:"default:0" json:"upload"`                                  // 已用上行 (Bytes)
	Download  int64     `gorm:"default:0" json:"download"`                                // 已用下行 (Bytes)
	Total     int64     `gorm:"default:0" json:"total"`                                   // 总流量 (Bytes)
	Expire    int64     `gorm:"default:0" json:"expire"`                                  // 到期时间戳
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime:false" json:"updated_at"` // 仅成功同步时显式更新
}

func (AirportSub) TableName() string {
	return "airport_subs"
}

func (s *AirportSub) BeforeCreate(tx *gorm.DB) (err error) {
	if s.ID == "" {
		s.ID = uuid.New().String()
	}
	return
}

// AirportNode 机场节点表 (关联 AirportSub)
type AirportNode struct {
	ID            string `gorm:"primaryKey;type:varchar(36)" json:"id"`
	SubID         string `gorm:"index;type:varchar(36)" json:"sub_id"` // 外键关联 AirportSub
	Name          string `gorm:"index" json:"name"`                    // 节点名称
	Protocol      string `gorm:"type:varchar(32)" json:"protocol"`     // 协议类型 (新增)
	Link          string `gorm:"type:text" json:"link"`                // 原始链接 (vmess://, ss:// 等)
	RoutingType   int    `gorm:"default:0" json:"routing_type"`        // 0=禁用，1=中转，2=落地
	OriginalIndex int    `gorm:"default:0" json:"original_index"`      // 原始排序索引
}

func (AirportNode) TableName() string {
	return "airport_nodes"
}

func (n *AirportNode) BeforeCreate(tx *gorm.DB) (err error) {
	if n.ID == "" {
		n.ID = uuid.New().String()
	}
	return
}

// AirportSpeedTestHistory 机场测速历史任务（仅记录每次整组测试的概要）
type AirportSpeedTestHistory struct {
	ID          string     `gorm:"primaryKey;type:varchar(36)" json:"id"`
	SubID       string     `gorm:"index;type:varchar(36);not null" json:"sub_id"`
	SubName     string     `gorm:"type:varchar(128)" json:"sub_name"`
	TaskKey     string     `gorm:"index;type:varchar(64);not null" json:"task_key"`
	Status      string     `gorm:"index;type:varchar(32)" json:"status"` // running | completed | stopped | failed
	TotalCount  int        `gorm:"default:0" json:"total_count"`
	ResultCount int        `gorm:"default:0" json:"result_count"`
	ErrorCount  int        `gorm:"default:0" json:"error_count"`
	StartedAt   time.Time  `gorm:"column:started_at" json:"started_at"`
	FinishedAt  *time.Time `gorm:"column:finished_at" json:"finished_at,omitempty"`
	CreatedAt   time.Time  `gorm:"column:created_at" json:"created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at" json:"updated_at"`

	Sub     AirportSub               `gorm:"foreignKey:SubID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"-"`
	Results []AirportSpeedTestResult `gorm:"foreignKey:HistoryID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"-"`
}

func (AirportSpeedTestHistory) TableName() string {
	return "airport_speed_test_histories"
}

func (h *AirportSpeedTestHistory) BeforeCreate(tx *gorm.DB) (err error) {
	if h.ID == "" {
		h.ID = uuid.New().String()
	}
	if strings.TrimSpace(h.TaskKey) == "" {
		h.TaskKey = time.Now().Format("200601021504")
	}
	return
}

// AirportSpeedTestResult 机场测速详细结果（每个节点一条记录，按历史任务外键关联）
type AirportSpeedTestResult struct {
	ID         string    `gorm:"primaryKey;type:varchar(36)" json:"id"`
	HistoryID  string    `gorm:"index;type:varchar(36);not null" json:"history_id"`
	TaskKey    string    `gorm:"index;type:varchar(64);not null" json:"task_key"`
	SubID      string    `gorm:"index;type:varchar(36);not null" json:"sub_id"`
	NodeID     string    `gorm:"index;type:varchar(36);not null" json:"node_id"`
	NodeName   string    `gorm:"type:text" json:"node_name"`
	ResultType string    `gorm:"column:result_type;index;type:varchar(32)" json:"result_type"` // ping | tcp | speed | error
	ResultText string    `gorm:"column:result_text;type:text" json:"result_text"`
	CreatedAt  time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at" json:"updated_at"`

	History AirportSpeedTestHistory `gorm:"foreignKey:HistoryID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;" json:"-"`
}

func (AirportSpeedTestResult) TableName() string {
	return "airport_speed_test_results"
}

func (r *AirportSpeedTestResult) BeforeCreate(tx *gorm.DB) (err error) {
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	return
}

// ------------------- 自定义节点 -------------------

// CustomNode 自定义协议链接节点表
type CustomNode struct {
	ID          string    `gorm:"primaryKey;type:varchar(36)" json:"id"`
	Link        string    `gorm:"type:text" json:"link"`            // 原始协议链接 (vmess://, vless://, ss:// 等)
	Name        string    `gorm:"type:varchar(255)" json:"name"`    // 从链接中解析出的节点名称
	Protocol    string    `gorm:"type:varchar(32)" json:"protocol"` // 协议类型
	RoutingType int       `gorm:"default:1" json:"routing_type"`    // 0=禁用，1=中转，2=落地
	CreatedAt   time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (CustomNode) TableName() string {
	return "custom_nodes"
}

func (n *CustomNode) BeforeCreate(tx *gorm.DB) (err error) {
	if n.ID == "" {
		n.ID = uuid.New().String()
	}
	return
}

// ------------------- 数据库初始化 -------------------

// openSQLite 打开 SQLite 数据库连接
func openSQLite() (*gorm.DB, error) {
	dbPath := filepath.Join("data", "nodectl.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("连接 SQLite 失败: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("获取底层 sql.DB 失败: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(time.Hour)

	// 启用 SQLite 外键约束
	db.Exec("PRAGMA foreign_keys = ON")
	return db, nil
}

// buildPostgresDSN 构建 PostgreSQL DSN 连接字符串
func buildPostgresDSN(cfg DBConfig) string {
	if cfg.Port == 0 {
		cfg.Port = 5432
	}
	if cfg.SSLMode == "" {
		cfg.SSLMode = "disable"
	}
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s TimeZone=Asia/Shanghai",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.DBName, cfg.SSLMode)
}

// openPostgres 打开 PostgreSQL 数据库连接
func openPostgres(cfg DBConfig) (*gorm.DB, error) {
	dsn := buildPostgresDSN(cfg)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("获取底层 sql.DB 失败: %w", err)
	}
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(time.Hour)

	return db, nil
}

// autoMigrateAll 自动迁移所有表结构
func autoMigrateAll(db *gorm.DB) error {
	return db.AutoMigrate(
		&NodePool{},
		&NodeTrafficStat{},
		&SysConfig{},
		&AirportSub{},
		&AirportNode{},
		&AirportSpeedTestHistory{},
		&AirportSpeedTestResult{},
		&CustomNode{},
	)
}

// InitDB 初始化数据库连接并同步表结构
func InitDB() {
	if err := os.MkdirAll("data", os.ModePerm); err != nil {
		logger.Log.Error("创建 data 目录失败", "err", err.Error())
		panic("NodeCTL工作目录初始化失败")
	}

	cfg := LoadDBConfig()

	var db *gorm.DB
	var err error

	switch cfg.Type {
	case "postgres":
		db, err = openPostgres(cfg)
		if err != nil {
			logger.Log.Error("连接 PostgreSQL 失败，回退到 SQLite", "err", err.Error())
			// 回退到 SQLite
			db, err = openSQLite()
			if err != nil {
				logger.Log.Error("SQLite 回退也失败", "err", err.Error())
				panic("数据库连接失败")
			}
			// 更新配置为 SQLite
			cfg.Type = "sqlite"
			_ = SaveDBConfig(cfg)
			logger.Log.Warn("已自动回退到 SQLite 数据库")
		} else {
			logger.ConsoleAndLog.Info("数据库引擎已启动", "type", "PostgreSQL", "host", cfg.Host, "dbname", cfg.DBName)
		}
	default:
		db, err = openSQLite()
		if err != nil {
			logger.Log.Error("连接 SQLite 失败", "err", err.Error())
			panic("数据库连接失败")
		}
		logger.ConsoleAndLog.Info("数据库引擎已启动", "type", "SQLite")
	}

	// 自动迁移所有的表
	if err := autoMigrateAll(db); err != nil {
		logger.Log.Error("自动同步表结构失败", "err", err.Error())
		panic("数据库表结构迁移失败")
	}

	// 赋值给全局变量
	DB = db

	// 调用外部模块初始化默认系统设置
	initDefaultConfigs()

	// PostgreSQL: 保障序列值与历史数据对齐（避免 duplicate key on node_traffic_stats_pkey）
	if strings.EqualFold(cfg.Type, "postgres") {
		if err := SyncNodeTrafficStatSequence(); err != nil {
			logger.Log.Warn("同步 node_traffic_stats 序列失败", "err", err.Error())
		}
	}
}

// ------------------- 数据库管理功能 -------------------

// GetDBStatus 获取当前数据库的状态信息
func GetDBStatus() DBStatus {
	cfg := LoadDBConfig()
	status := DBStatus{
		Type:       cfg.Type,
		Host:       cfg.Host,
		Port:       cfg.Port,
		User:       cfg.User,
		DBName:     cfg.DBName,
		SSLMode:    cfg.SSLMode,
		RecordInfo: make(map[string]int64),
	}

	// 健康检查
	sqlDB, err := DB.DB()
	if err != nil {
		status.Healthy = false
		status.HealthMsg = "无法获取数据库连接: " + err.Error()
		return status
	}
	if err := sqlDB.Ping(); err != nil {
		status.Healthy = false
		status.HealthMsg = "数据库连接异常: " + err.Error()
		return status
	}
	status.Healthy = true
	status.HealthMsg = "运行正常"

	// 获取数据库大小
	if cfg.Type == "postgres" {
		var size int64
		row := sqlDB.QueryRow("SELECT pg_database_size(current_database())")
		if err := row.Scan(&size); err == nil {
			status.SizeBytes = size
			status.SizeHuman = formatBytesHuman(size)
		}
	} else {
		dbPath := filepath.Join("data", "nodectl.db")
		if info, err := os.Stat(dbPath); err == nil {
			status.SizeBytes = info.Size()
			status.SizeHuman = formatBytesHuman(info.Size())
		}
	}

	// 获取各表记录数
	var count int64
	DB.Model(&NodePool{}).Count(&count)
	status.RecordInfo["node_pool"] = count
	DB.Model(&NodeTrafficStat{}).Count(&count)
	status.RecordInfo["node_traffic_stats"] = count
	DB.Model(&SysConfig{}).Count(&count)
	status.RecordInfo["sys_config"] = count
	DB.Model(&AirportSub{}).Count(&count)
	status.RecordInfo["airport_subs"] = count
	DB.Model(&AirportNode{}).Count(&count)
	status.RecordInfo["airport_nodes"] = count
	DB.Model(&CustomNode{}).Count(&count)
	status.RecordInfo["custom_nodes"] = count

	status.TableCount = 6
	return status
}

// TestPostgresConnection 测试 PostgreSQL 连接是否可用
func TestPostgresConnection(cfg DBConfig) (string, error) {
	dsn := buildPostgresDSN(cfg)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		return "", fmt.Errorf("连接失败: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return "", fmt.Errorf("获取连接失败: %w", err)
	}
	defer sqlDB.Close()

	if err := sqlDB.Ping(); err != nil {
		return "", fmt.Errorf("Ping 失败: %w", err)
	}

	// 获取版本信息
	var version string
	row := sqlDB.QueryRow("SELECT version()")
	if err := row.Scan(&version); err != nil {
		return "连接成功 (无法获取版本)", nil
	}
	return version, nil
}

// SwitchDatabase 切换数据库引擎
func SwitchDatabase(cfg DBConfig) error {
	dbMu.Lock()
	defer dbMu.Unlock()

	var newDB *gorm.DB
	var err error

	switch cfg.Type {
	case "postgres":
		newDB, err = openPostgres(cfg)
	default:
		cfg.Type = "sqlite"
		newDB, err = openSQLite()
	}

	if err != nil {
		return fmt.Errorf("打开新数据库失败: %w", err)
	}

	// 自动迁移表结构
	if err := autoMigrateAll(newDB); err != nil {
		return fmt.Errorf("新数据库表结构迁移失败: %w", err)
	}

	// 初始化默认配置 (使用新的DB)
	oldDB := DB
	DB = newDB

	// 初始化默认系统设置到新数据库
	initDefaultConfigs()

	// PostgreSQL: 切换后主动对齐序列
	if strings.EqualFold(cfg.Type, "postgres") {
		if err := SyncNodeTrafficStatSequence(); err != nil {
			logger.Log.Warn("切换后同步 node_traffic_stats 序列失败", "err", err.Error())
		}
	}

	// 关闭旧连接
	if oldDB != nil {
		if sqlDB, err := oldDB.DB(); err == nil {
			sqlDB.Close()
		}
	}

	// 保存配置
	if err := SaveDBConfig(cfg); err != nil {
		return fmt.Errorf("保存配置文件失败: %w", err)
	}

	logger.Log.Info("数据库引擎已切换", "type", cfg.Type)
	return nil
}

// MigrateToPostgres 从 SQLite 迁移所有数据到 PostgreSQL
func MigrateToPostgres(pgCfg DBConfig) error {
	// 1. 打开 SQLite 源数据库
	srcDB, err := openSQLite()
	if err != nil {
		return fmt.Errorf("打开 SQLite 源数据库失败: %w", err)
	}
	srcSqlDB, _ := srcDB.DB()
	defer srcSqlDB.Close()

	// 2. 打开 PostgreSQL 目标数据库
	dstDB, err := openPostgres(pgCfg)
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 目标数据库失败: %w", err)
	}

	// 3. 在目标数据库上创建表结构
	if err := autoMigrateAll(dstDB); err != nil {
		return fmt.Errorf("目标数据库表结构迁移失败: %w", err)
	}

	// 4. 逐表迁移数据
	// 4.1 迁移 SysConfig
	if err := migrateTable[SysConfig](srcDB, dstDB, "sys_config"); err != nil {
		return fmt.Errorf("迁移 sys_config 失败: %w", err)
	}

	// 4.2 迁移 NodePool
	if err := migrateTable[NodePool](srcDB, dstDB, "node_pool"); err != nil {
		return fmt.Errorf("迁移 node_pool 失败: %w", err)
	}

	// 4.3 迁移 NodeTrafficStat (可能数据量大，分批处理)
	if err := migrateTableBatched[NodeTrafficStat](srcDB, dstDB, "node_traffic_stats", 500); err != nil {
		return fmt.Errorf("迁移 node_traffic_stats 失败: %w", err)
	}

	// 4.4 迁移 AirportSub
	if err := migrateTable[AirportSub](srcDB, dstDB, "airport_subs"); err != nil {
		return fmt.Errorf("迁移 airport_subs 失败: %w", err)
	}

	// 4.5 迁移 AirportNode
	if err := migrateTable[AirportNode](srcDB, dstDB, "airport_nodes"); err != nil {
		return fmt.Errorf("迁移 airport_nodes 失败: %w", err)
	}

	// 4.6 迁移 AirportSpeedTestHistory
	if err := migrateTable[AirportSpeedTestHistory](srcDB, dstDB, "airport_speed_test_histories"); err != nil {
		return fmt.Errorf("迁移 airport_speed_test_histories 失败: %w", err)
	}

	// 4.7 迁移 AirportSpeedTestResult
	if err := migrateTable[AirportSpeedTestResult](srcDB, dstDB, "airport_speed_test_results"); err != nil {
		return fmt.Errorf("迁移 airport_speed_test_results 失败: %w", err)
	}

	// 4.8 迁移 CustomNode
	if err := migrateTable[CustomNode](srcDB, dstDB, "custom_nodes"); err != nil {
		return fmt.Errorf("迁移 custom_nodes 失败: %w", err)
	}

	// 5. 修正 PostgreSQL 自增序列，避免后续写入撞主键
	if err := syncNodeTrafficStatSequenceForDB(dstDB); err != nil {
		return fmt.Errorf("同步 node_traffic_stats 序列失败: %w", err)
	}

	logger.Log.Info("数据迁移完成: SQLite → PostgreSQL")
	return nil
}

// SyncNodeTrafficStatSequence 同步 node_traffic_stats.id 的 PostgreSQL 序列到当前最大 ID。
// 典型场景：从 SQLite 迁移后已写入显式 ID，但序列仍停留在较小值，导致后续插入报 23505。
func SyncNodeTrafficStatSequence() error {
	if DB == nil {
		return nil
	}
	return syncNodeTrafficStatSequenceForDB(DB)
}

func syncNodeTrafficStatSequenceForDB(db *gorm.DB) error {
	if db == nil {
		return nil
	}

	// nextval 将返回 max(id)+1（当表为空时返回 1）
	return db.Exec(`
		SELECT setval(
			pg_get_serial_sequence('node_traffic_stats', 'id'),
			COALESCE((SELECT MAX(id) FROM node_traffic_stats), 0) + 1,
			false
		)
	`).Error
}

// migrateTable 通用表迁移器 (全量读取后写入)
func migrateTable[T any](src, dst *gorm.DB, tableName string) error {
	var records []T
	if err := src.Find(&records).Error; err != nil {
		return fmt.Errorf("读取源表 %s 失败: %w", tableName, err)
	}
	if len(records) == 0 {
		logger.Log.Info("迁移跳过空表", "table", tableName)
		return nil
	}

	// 先清空目标表
	dst.Exec("DELETE FROM " + tableName)

	// 批量插入 (禁用钩子以避免 UUID 重新生成)
	if err := dst.Session(&gorm.Session{SkipHooks: true}).CreateInBatches(records, 100).Error; err != nil {
		return fmt.Errorf("写入目标表 %s 失败: %w", tableName, err)
	}

	logger.Log.Info("表迁移成功", "table", tableName, "records", len(records))
	return nil
}

// migrateTableBatched 分批迁移大表
func migrateTableBatched[T any](src, dst *gorm.DB, tableName string, batchSize int) error {
	// 先清空目标表
	dst.Exec("DELETE FROM " + tableName)

	var total int64
	src.Model(new(T)).Count(&total)
	if total == 0 {
		logger.Log.Info("迁移跳过空表", "table", tableName)
		return nil
	}

	offset := 0
	migrated := int64(0)
	for {
		var batch []T
		if err := src.Offset(offset).Limit(batchSize).Find(&batch).Error; err != nil {
			return fmt.Errorf("读取源表 %s (offset=%d) 失败: %w", tableName, offset, err)
		}
		if len(batch) == 0 {
			break
		}
		if err := dst.Session(&gorm.Session{SkipHooks: true}).CreateInBatches(batch, batchSize).Error; err != nil {
			return fmt.Errorf("写入目标表 %s (offset=%d) 失败: %w", tableName, offset, err)
		}
		migrated += int64(len(batch))
		offset += batchSize
		logger.Log.Debug("批量迁移进度", "table", tableName, "migrated", migrated, "total", total)
	}

	logger.Log.Info("大表迁移成功", "table", tableName, "records", migrated)
	return nil
}

// ------------------- 节点流量数据批量删除 -------------------

// DeleteNodeTrafficStatsBatched 分批删除指定节点的流量统计数据。
// 每批删除 batchSize 条记录，每批使用独立事务，大幅降低长事务锁持有时间，
// 避免在流量数据量巨大时一次性 DELETE 导致数据库锁表、WAL 膨胀或连接池耗尽。
// 返回总删除条数和首次遇到的错误（如果有）。
func DeleteNodeTrafficStatsBatched(nodeUUID string, batchSize int) (int64, error) {
	if batchSize <= 0 {
		batchSize = 1000
	}

	var totalDeleted int64
	for {
		result := DB.Where("node_uuid = ?", nodeUUID).Limit(batchSize).Delete(&NodeTrafficStat{})
		if result.Error != nil {
			return totalDeleted, result.Error
		}
		totalDeleted += result.RowsAffected
		if result.RowsAffected < int64(batchSize) {
			// 本批删除的行数少于 batchSize，说明已全部删完
			break
		}
	}
	return totalDeleted, nil
}

// ------------------- PostgreSQL VACUUM -------------------

// VacuumDatabase 对 PostgreSQL 执行 VACUUM（回收已删除行占用的磁盘空间）。
// PostgreSQL 使用 MVCC 机制，DELETE 只是标记行为"死元组"，磁盘空间不会立即释放，
// 必须通过 VACUUM 回收。普通 VACUUM 回收空间供表复用，VACUUM FULL 会彻底压缩表文件
// 但需要排他锁。此函数执行普通 VACUUM（非阻塞），适合在线运行。
func VacuumDatabase() error {
	cfg := LoadDBConfig()
	if cfg.Type != "postgres" {
		return fmt.Errorf("VACUUM 仅适用于 PostgreSQL 数据库")
	}
	sqlDB, err := DB.DB()
	if err != nil {
		return fmt.Errorf("获取底层连接失败: %w", err)
	}
	// VACUUM 不能在事务中执行，直接使用底层 sql.DB
	_, err = sqlDB.Exec("VACUUM")
	if err != nil {
		return fmt.Errorf("执行 VACUUM 失败: %w", err)
	}
	return nil
}

// VacuumTable 对 PostgreSQL 指定表执行 VACUUM。
func VacuumTable(tableName string) error {
	cfg := LoadDBConfig()
	if cfg.Type != "postgres" {
		return fmt.Errorf("VACUUM 仅适用于 PostgreSQL 数据库")
	}
	// 表名白名单，防止 SQL 注入
	allowed := map[string]bool{
		"node_pool":                    true,
		"node_traffic_stats":           true,
		"sys_config":                   true,
		"airport_subs":                 true,
		"airport_nodes":                true,
		"airport_speed_test_histories": true,
		"airport_speed_test_results":   true,
		"custom_nodes":                 true,
	}
	if !allowed[tableName] {
		return fmt.Errorf("不允许对表 %s 执行 VACUUM", tableName)
	}
	sqlDB, err := DB.DB()
	if err != nil {
		return fmt.Errorf("获取底层连接失败: %w", err)
	}
	_, err = sqlDB.Exec("VACUUM " + tableName)
	if err != nil {
		return fmt.Errorf("执行 VACUUM %s 失败: %w", tableName, err)
	}
	return nil
}

// ------------------- 辅助函数 -------------------

// formatBytesHuman 格式化字节数为人类可读形式
func formatBytesHuman(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// GetUnderlyingDB 获取底层 sql.DB 实例 (用于外部模块的 Ping、Stats 等)
func GetUnderlyingDB() (*sql.DB, error) {
	return DB.DB()
}

// ------------------- Web 端口管理 -------------------

// DefaultWebPort Web 服务默认监听端口
const DefaultWebPort = 8080

// isRunningInDocker 检测当前是否运行在 Docker 容器内
// 通过检查 /.dockerenv 文件或 /proc/1/cgroup 中是否包含 docker/containerd 关键字判断，
// 不需要任何额外的环境依赖。
func isRunningInDocker() bool {
	// 方法 1: 检查 /.dockerenv 文件（Docker 运行时自动创建）
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	// 方法 2: 检查 /proc/1/cgroup 中是否包含 docker 或 containerd 关键字
	data, err := os.ReadFile("/proc/1/cgroup")
	if err == nil {
		content := strings.ToLower(string(data))
		if strings.Contains(content, "docker") || strings.Contains(content, "containerd") {
			return true
		}
	}
	return false
}

// GetWebPort 获取 Web 服务监听端口。
// 规则：
//  1. Docker 环境始终返回 8080（避免用户在容器内误改端口导致服务不可达）
//  2. 非 Docker 环境：优先读取 data/dbconfig.json 中的 web_port 字段
//  3. 若未配置或值无效（<1 或 >65535），返回默认端口 8080
func GetWebPort() int {
	if isRunningInDocker() {
		return DefaultWebPort
	}
	cfg := LoadDBConfig()
	if cfg.WebPort >= 1 && cfg.WebPort <= 65535 {
		return cfg.WebPort
	}
	return DefaultWebPort
}

// GetWebPortStr 返回 Web 端口的字符串形式，格式如 ":8080"，可直接用于 http.Server.Addr
func GetWebPortStr() string {
	return fmt.Sprintf(":%d", GetWebPort())
}
