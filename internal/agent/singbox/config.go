// 路径: internal/agent/singbox/config.go
// sing-box 配置生成器：根据 ProtocolConfig 生成 sing-box JSON 配置文件
package singbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// 默认路径常量
const (
	DefaultWorkDir       = "/var/lib/nodectl-agent"
	DefaultConfigPath    = "/var/lib/nodectl-agent/singbox-config.json"
	DefaultProtocolsPath = "/var/lib/nodectl-agent/protocols.json"
	DefaultCertDir       = "/var/lib/nodectl-agent/certs"
	DefaultCertPath      = "/var/lib/nodectl-agent/certs/fullchain.pem"
	DefaultKeyPath       = "/var/lib/nodectl-agent/certs/privkey.pem"
)

// ConfigManager sing-box 配置管理器
type ConfigManager struct {
	configPath    string          // sing-box 配置文件输出路径
	protocolsPath string          // 协议配置缓存路径
	certDir       string          // 证书目录
	Protocols     *ProtocolConfig // 当前协议配置
}

// NewConfigManager 创建配置管理器
func NewConfigManager() *ConfigManager {
	return &ConfigManager{
		configPath:    DefaultConfigPath,
		protocolsPath: DefaultProtocolsPath,
		certDir:       DefaultCertDir,
		Protocols:     DefaultProtocolConfig(),
	}
}

// NewConfigManagerWithPaths 使用自定义路径创建配置管理器
func NewConfigManagerWithPaths(configPath, protocolsPath, certDir string) *ConfigManager {
	cm := &ConfigManager{
		configPath:    configPath,
		protocolsPath: protocolsPath,
		certDir:       certDir,
		Protocols:     DefaultProtocolConfig(),
	}
	if cm.configPath == "" {
		cm.configPath = DefaultConfigPath
	}
	if cm.protocolsPath == "" {
		cm.protocolsPath = DefaultProtocolsPath
	}
	if cm.certDir == "" {
		cm.certDir = DefaultCertDir
	}
	return cm
}

// GetConfigPath 返回 sing-box 配置文件路径
func (cm *ConfigManager) GetConfigPath() string {
	return cm.configPath
}

// GetCertPath 返回证书路径
func (cm *ConfigManager) GetCertPath() string {
	return filepath.Join(cm.certDir, "fullchain.pem")
}

// GetKeyPath 返回私钥路径
func (cm *ConfigManager) GetKeyPath() string {
	return filepath.Join(cm.certDir, "privkey.pem")
}

// --- sing-box JSON 配置结构体（用于序列化） ---

// sbConfig sing-box 顶层配置
type sbConfig struct {
	Log       sbLog        `json:"log"`
	Inbounds  []any        `json:"inbounds"`
	Outbounds []sbOutbound `json:"outbounds"`
}

type sbLog struct {
	Level     string `json:"level"`
	Timestamp bool   `json:"timestamp"`
}

type sbOutbound struct {
	Type string `json:"type"`
	Tag  string `json:"tag"`
}

// --- 各协议 inbound 结构体 ---

type sbSSInbound struct {
	Type       string `json:"type"`
	Listen     string `json:"listen"`
	ListenPort int    `json:"listen_port"`
	Method     string `json:"method"`
	Password   string `json:"password"`
	Tag        string `json:"tag"`
}

type sbHY2Inbound struct {
	Type       string       `json:"type"`
	Tag        string       `json:"tag"`
	Listen     string       `json:"listen"`
	ListenPort int          `json:"listen_port"`
	Users      []sbHY2User  `json:"users"`
	TLS        *sbTLSConfig `json:"tls"`
}

type sbHY2User struct {
	Password string `json:"password"`
}

type sbTUICInbound struct {
	Type              string       `json:"type"`
	Tag               string       `json:"tag"`
	Listen            string       `json:"listen"`
	ListenPort        int          `json:"listen_port"`
	Users             []sbTUICUser `json:"users"`
	CongestionControl string       `json:"congestion_control"`
	TLS               *sbTLSConfig `json:"tls"`
}

type sbTUICUser struct {
	UUID     string `json:"uuid"`
	Password string `json:"password"`
}

type sbVLESSInbound struct {
	Type       string        `json:"type"`
	Tag        string        `json:"tag"`
	Listen     string        `json:"listen"`
	ListenPort int           `json:"listen_port"`
	Users      []sbVLESSUser `json:"users"`
	TLS        *sbTLSConfig  `json:"tls"`
	Transport  *sbTransport  `json:"transport,omitempty"`
}

type sbVLESSUser struct {
	UUID string `json:"uuid"`
	Flow string `json:"flow,omitempty"`
}

type sbSocksInbound struct {
	Type       string        `json:"type"`
	Tag        string        `json:"tag"`
	Listen     string        `json:"listen"`
	ListenPort int           `json:"listen_port"`
	Users      []sbSocksUser `json:"users"`
}

type sbSocksUser struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type sbTrojanInbound struct {
	Type       string         `json:"type"`
	Tag        string         `json:"tag"`
	Listen     string         `json:"listen"`
	ListenPort int            `json:"listen_port"`
	Users      []sbTrojanUser `json:"users"`
	TLS        *sbTLSConfig   `json:"tls"`
	Transport  *sbTransport   `json:"transport,omitempty"`
}

type sbTrojanUser struct {
	Password string `json:"password"`
}

type sbAnyTLSInbound struct {
	Type       string         `json:"type"`
	Tag        string         `json:"tag"`
	Listen     string         `json:"listen"`
	ListenPort int            `json:"listen_port"`
	Users      []sbAnyTLSUser `json:"users"`
	Padding    []interface{}  `json:"padding_scheme"`
	TLS        *sbTLSConfig   `json:"tls"`
}

type sbAnyTLSUser struct {
	Password string `json:"password"`
}

type sbVMessInbound struct {
	Type       string        `json:"type"`
	Tag        string        `json:"tag"`
	Listen     string        `json:"listen"`
	ListenPort int           `json:"listen_port"`
	Users      []sbVMessUser `json:"users"`
	TLS        *sbTLSConfig  `json:"tls,omitempty"`
	Transport  *sbTransport  `json:"transport,omitempty"`
}

type sbVMessUser struct {
	UUID    string `json:"uuid"`
	AlterID int    `json:"alterId"`
}

// sbTLSConfig sing-box TLS 配置
type sbTLSConfig struct {
	Enabled         bool          `json:"enabled"`
	ALPN            []string      `json:"alpn,omitempty"`
	ServerName      string        `json:"server_name,omitempty"`
	CertificatePath string        `json:"certificate_path,omitempty"`
	KeyPath         string        `json:"key_path,omitempty"`
	Reality         *sbRealityTLS `json:"reality,omitempty"`
}

type sbRealityTLS struct {
	Enabled    bool        `json:"enabled"`
	Handshake  sbRealityHS `json:"handshake"`
	PrivateKey string      `json:"private_key"`
	ShortID    []string    `json:"short_id"`
}

type sbRealityHS struct {
	Server     string `json:"server"`
	ServerPort int    `json:"server_port"`
}

// sbTransport sing-box 传输层配置
type sbTransport struct {
	Type                string `json:"type"`
	Path                string `json:"path,omitempty"`
	EarlyDataHeaderName string `json:"early_data_header_name,omitempty"`
}

// --- 配置生成逻辑 ---

// GenerateConfig 根据当前协议配置生成 sing-box JSON 配置
func (cm *ConfigManager) GenerateConfig() ([]byte, error) {
	pc := cm.Protocols

	cfg := sbConfig{
		Log: sbLog{
			Level:     "info",
			Timestamp: true,
		},
		Outbounds: []sbOutbound{
			{Type: "direct", Tag: "direct-out"},
		},
	}

	certPath := cm.GetCertPath()
	keyPath := cm.GetKeyPath()

	// 构建 inbounds
	if pc.IsEnabled(ProtoSS) {
		cfg.Inbounds = append(cfg.Inbounds, sbSSInbound{
			Type: "shadowsocks", Listen: "::", ListenPort: pc.SS.Port,
			Method: pc.SS.Method, Password: pc.SS.Password, Tag: "ss-in",
		})
	}

	if pc.IsEnabled(ProtoHY2) {
		cfg.Inbounds = append(cfg.Inbounds, sbHY2Inbound{
			Type: "hysteria2", Tag: "hy2-in", Listen: "::", ListenPort: pc.HY2.Port,
			Users: []sbHY2User{{Password: pc.HY2.Password}},
			TLS: &sbTLSConfig{
				Enabled: true, ALPN: []string{"h3"},
				CertificatePath: certPath, KeyPath: keyPath,
			},
		})
	}

	if pc.IsEnabled(ProtoTUIC) {
		cfg.Inbounds = append(cfg.Inbounds, sbTUICInbound{
			Type: "tuic", Tag: "tuic-in", Listen: "::", ListenPort: pc.TUIC.Port,
			Users:             []sbTUICUser{{UUID: pc.TUIC.UUID, Password: pc.TUIC.Password}},
			CongestionControl: "bbr",
			TLS: &sbTLSConfig{
				Enabled: true, ALPN: []string{"h3"},
				CertificatePath: certPath, KeyPath: keyPath,
			},
		})
	}

	if pc.IsEnabled(ProtoReality) {
		cfg.Inbounds = append(cfg.Inbounds, sbVLESSInbound{
			Type: "vless", Tag: "vless-in", Listen: "::", ListenPort: pc.Reality.Port,
			Users: []sbVLESSUser{{UUID: pc.Reality.UUID, Flow: "xtls-rprx-vision"}},
			TLS: &sbTLSConfig{
				Enabled:    true,
				ServerName: pc.Reality.SNI,
				Reality: &sbRealityTLS{
					Enabled: true,
					Handshake: sbRealityHS{
						Server: pc.Reality.SNI, ServerPort: 443,
					},
					PrivateKey: pc.Reality.PrivateKey,
					ShortID:    []string{pc.Reality.ShortID},
				},
			},
		})
	}

	if pc.IsEnabled(ProtoSocks5) {
		cfg.Inbounds = append(cfg.Inbounds, sbSocksInbound{
			Type: "socks", Tag: "socks-in", Listen: "::", ListenPort: pc.Socks5.Port,
			Users: []sbSocksUser{{Username: pc.Socks5.Username, Password: pc.Socks5.Password}},
		})
	}

	if pc.IsEnabled(ProtoAnyTLS) {
		cfg.Inbounds = append(cfg.Inbounds, sbAnyTLSInbound{
			Type: "anytls", Tag: "anytls-in", Listen: "::", ListenPort: pc.AnyTLS.Port,
			Users:   []sbAnyTLSUser{{Password: pc.AnyTLS.Password}},
			Padding: []interface{}{},
			TLS: &sbTLSConfig{
				Enabled:         true,
				CertificatePath: certPath, KeyPath: keyPath,
			},
		})
	}

	if pc.IsEnabled(ProtoTrojan) {
		cfg.Inbounds = append(cfg.Inbounds, sbTrojanInbound{
			Type: "trojan", Tag: "trojan-in", Listen: "::", ListenPort: pc.Trojan.Port,
			Users: []sbTrojanUser{{Password: pc.Trojan.Password}},
			TLS: &sbTLSConfig{
				Enabled:         true,
				CertificatePath: certPath, KeyPath: keyPath,
			},
		})
	}

	// --- VMess 族 ---
	vmessUUID := pc.VMess.UUID
	tp := pc.GetTransportPath()

	if pc.IsEnabled(ProtoVmessTCP) {
		cfg.Inbounds = append(cfg.Inbounds, sbVMessInbound{
			Type: "vmess", Tag: "vmess-tcp-in", Listen: "::", ListenPort: pc.VMess.TCPPort,
			Users: []sbVMessUser{{UUID: vmessUUID, AlterID: 0}},
		})
	}

	if pc.IsEnabled(ProtoVmessWS) {
		cfg.Inbounds = append(cfg.Inbounds, sbVMessInbound{
			Type: "vmess", Tag: "vmess-ws-in", Listen: "::", ListenPort: pc.VMess.WSPort,
			Users:     []sbVMessUser{{UUID: vmessUUID, AlterID: 0}},
			Transport: &sbTransport{Type: "ws", Path: tp, EarlyDataHeaderName: "Sec-WebSocket-Protocol"},
		})
	}

	if pc.IsEnabled(ProtoVmessHTTP) {
		cfg.Inbounds = append(cfg.Inbounds, sbVMessInbound{
			Type: "vmess", Tag: "vmess-http-in", Listen: "::", ListenPort: pc.VMess.HTTPPort,
			Users:     []sbVMessUser{{UUID: vmessUUID, AlterID: 0}},
			Transport: &sbTransport{Type: "http", Path: tp},
		})
	}

	if pc.IsEnabled(ProtoVmessQUIC) {
		cfg.Inbounds = append(cfg.Inbounds, sbVMessInbound{
			Type: "vmess", Tag: "vmess-quic-in", Listen: "::", ListenPort: pc.VMess.QUICPort,
			Users: []sbVMessUser{{UUID: vmessUUID, AlterID: 0}},
			TLS: &sbTLSConfig{
				Enabled: true, ALPN: []string{"h3"},
				CertificatePath: certPath, KeyPath: keyPath,
			},
			Transport: &sbTransport{Type: "quic"},
		})
	}

	if pc.IsEnabled(ProtoVmessWST) {
		cfg.Inbounds = append(cfg.Inbounds, sbVMessInbound{
			Type: "vmess", Tag: "vmess-wst-in", Listen: "::", ListenPort: pc.VMess.WSTPort,
			Users: []sbVMessUser{{UUID: vmessUUID, AlterID: 0}},
			TLS: &sbTLSConfig{
				Enabled:         true,
				CertificatePath: certPath, KeyPath: keyPath,
			},
			Transport: &sbTransport{Type: "ws", Path: tp, EarlyDataHeaderName: "Sec-WebSocket-Protocol"},
		})
	}

	if pc.IsEnabled(ProtoVmessHUT) {
		cfg.Inbounds = append(cfg.Inbounds, sbVMessInbound{
			Type: "vmess", Tag: "vmess-hut-in", Listen: "::", ListenPort: pc.VMess.HUTPort,
			Users: []sbVMessUser{{UUID: vmessUUID, AlterID: 0}},
			TLS: &sbTLSConfig{
				Enabled: true, ALPN: []string{"http/1.1"},
				CertificatePath: certPath, KeyPath: keyPath,
			},
			Transport: &sbTransport{Type: "httpupgrade", Path: tp},
		})
	}

	// --- VLESS-TLS 族 ---
	vlessTLSUUID := pc.VlessTLS.UUID

	if pc.IsEnabled(ProtoVlessWST) {
		cfg.Inbounds = append(cfg.Inbounds, sbVLESSInbound{
			Type: "vless", Tag: "vless-wst-in", Listen: "::", ListenPort: pc.VlessTLS.WSTPort,
			Users: []sbVLESSUser{{UUID: vlessTLSUUID}},
			TLS: &sbTLSConfig{
				Enabled:         true,
				CertificatePath: certPath, KeyPath: keyPath,
			},
			Transport: &sbTransport{Type: "ws", Path: tp, EarlyDataHeaderName: "Sec-WebSocket-Protocol"},
		})
	}

	if pc.IsEnabled(ProtoVlessHUT) {
		cfg.Inbounds = append(cfg.Inbounds, sbVLESSInbound{
			Type: "vless", Tag: "vless-hut-in", Listen: "::", ListenPort: pc.VlessTLS.HUTPort,
			Users: []sbVLESSUser{{UUID: vlessTLSUUID}},
			TLS: &sbTLSConfig{
				Enabled: true, ALPN: []string{"http/1.1"},
				CertificatePath: certPath, KeyPath: keyPath,
			},
			Transport: &sbTransport{Type: "httpupgrade", Path: tp},
		})
	}

	// --- Trojan-TLS 族 ---
	trojanTLSPwd := pc.TrojanTLS.Password

	if pc.IsEnabled(ProtoTrojanWST) {
		cfg.Inbounds = append(cfg.Inbounds, sbTrojanInbound{
			Type: "trojan", Tag: "trojan-wst-in", Listen: "::", ListenPort: pc.TrojanTLS.WSTPort,
			Users: []sbTrojanUser{{Password: trojanTLSPwd}},
			TLS: &sbTLSConfig{
				Enabled:         true,
				CertificatePath: certPath, KeyPath: keyPath,
			},
			Transport: &sbTransport{Type: "ws", Path: tp, EarlyDataHeaderName: "Sec-WebSocket-Protocol"},
		})
	}

	if pc.IsEnabled(ProtoTrojanHUT) {
		cfg.Inbounds = append(cfg.Inbounds, sbTrojanInbound{
			Type: "trojan", Tag: "trojan-hut-in", Listen: "::", ListenPort: pc.TrojanTLS.HUTPort,
			Users: []sbTrojanUser{{Password: trojanTLSPwd}},
			TLS: &sbTLSConfig{
				Enabled: true, ALPN: []string{"http/1.1"},
				CertificatePath: certPath, KeyPath: keyPath,
			},
			Transport: &sbTransport{Type: "httpupgrade", Path: tp},
		})
	}

	return json.MarshalIndent(cfg, "", "  ")
}

// GenerateAndSave 生成并保存 sing-box 配置文件
func (cm *ConfigManager) GenerateAndSave() error {
	data, err := cm.GenerateConfig()
	if err != nil {
		return fmt.Errorf("生成 sing-box 配置失败: %w", err)
	}

	if dir := filepath.Dir(cm.configPath); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建配置目录失败: %w", err)
		}
	}

	if err := os.WriteFile(cm.configPath, data, 0644); err != nil {
		return fmt.Errorf("写入 sing-box 配置文件失败: %w", err)
	}

	return nil
}

// LoadFromCache 从协议缓存文件加载配置
func (cm *ConfigManager) LoadFromCache() error {
	data, err := os.ReadFile(cm.protocolsPath)
	if err != nil {
		return fmt.Errorf("读取协议缓存失败 [%s]: %w", cm.protocolsPath, err)
	}

	var pc ProtocolConfig
	if err := json.Unmarshal(data, &pc); err != nil {
		return fmt.Errorf("解析协议缓存失败: %w", err)
	}

	cm.Protocols = &pc
	return nil
}

// SaveToCache 保存协议配置到缓存文件
func (cm *ConfigManager) SaveToCache() error {
	if dir := filepath.Dir(cm.protocolsPath); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建缓存目录失败: %w", err)
		}
	}

	data, err := json.MarshalIndent(cm.Protocols, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化协议配置失败: %w", err)
	}

	if err := os.WriteFile(cm.protocolsPath, data, 0644); err != nil {
		return fmt.Errorf("写入协议缓存失败: %w", err)
	}

	return nil
}

// UpdateProtocol 更新单个协议配置（用于部分重置）
func (cm *ConfigManager) UpdateProtocol(protocol string, config any) error {
	if !ValidateProtocolName(protocol) {
		return fmt.Errorf("未知协议: %s", protocol)
	}

	pc := cm.Protocols
	switch protocol {
	case ProtoSS:
		if c, ok := config.(SSConfig); ok {
			pc.SS = c
		}
	case ProtoHY2:
		if c, ok := config.(HY2Config); ok {
			pc.HY2 = c
		}
	case ProtoTUIC:
		if c, ok := config.(TUICConfig); ok {
			pc.TUIC = c
		}
	case ProtoReality:
		if c, ok := config.(RealityConfig); ok {
			pc.Reality = c
		}
	case ProtoSocks5:
		if c, ok := config.(Socks5Config); ok {
			pc.Socks5 = c
		}
	case ProtoTrojan:
		if c, ok := config.(TrojanConfig); ok {
			pc.Trojan = c
		}
	case ProtoAnyTLS:
		if c, ok := config.(AnyTLSConfig); ok {
			pc.AnyTLS = c
		}
	default:
		return fmt.Errorf("协议 %s 不支持单独更新，请更新对应族配置", protocol)
	}

	return nil
}

// DisableProtocol 禁用单个协议
func (cm *ConfigManager) DisableProtocol(protocol string) error {
	if !ValidateProtocolName(protocol) {
		return fmt.Errorf("未知协议: %s", protocol)
	}
	cm.Protocols.SetEnabled(protocol, false)
	return nil
}

// EnsureCerts 确保自签证书存在（如有协议需要）
func (cm *ConfigManager) EnsureCerts(commonName string) error {
	if !cm.Protocols.NeedSelfSignedCert() {
		return nil
	}
	return EnsureCert(cm.GetCertPath(), cm.GetKeyPath(), commonName)
}
