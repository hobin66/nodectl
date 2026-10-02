package service

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nodectl/internal/database"

	"github.com/oschwald/geoip2-golang"
)

// GlobalGeoIP 全局实例
var GlobalGeoIP *GeoService

const (
	GeoDownloadURL = "https://github.com/P3TERX/GeoLite.mmdb/releases/latest/download/GeoLite2-Country.mmdb"
	GeoReleaseURL  = "https://github.com/P3TERX/GeoLite.mmdb/releases/latest"
	GeoDBConfigKey = "geo_db_version"
)

type GeoService struct {
	db   *geoip2.Reader
	mu   sync.RWMutex
	path string
}

// InitGeoIP 初始化 GeoIP 服务
func InitGeoIP() {
	svc := &GeoService{
		path: filepath.Join("data", "geo", "GeoLite2-Country.mmdb"),
	}

	if err := os.MkdirAll(filepath.Dir(svc.path), 0755); err != nil {
		slog.Error("创建 GeoIP 目录失败", "err", err)
		return
	}

	GlobalGeoIP = svc

	// 初始尝试加载文件到内存
	if err := svc.Reload(); err != nil {
		slog.Warn("GeoIP 暂无本地数据库，正在后台自动执行首次下载...")
		// 开启后台协程静默下载，不阻塞主程序启动
		go func() {
			if err := svc.ForceUpdate(); err != nil {
				slog.Error("GeoIP 自动下载失败，请稍后在面板手动更新", "err", err)
			}
		}()
	} else {
		slog.Debug("GeoIP 服务加载成功")
	}
}

// Reload 加载/重载数据库文件到内存
func (s *GeoService) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db != nil {
		s.db.Close()
		s.db = nil
	}

	if _, err := os.Stat(s.path); os.IsNotExist(err) {
		return errors.New("数据库文件不存在")
	}

	db, err := geoip2.Open(s.path)
	if err != nil {
		return fmt.Errorf("打开 MMDB 文件失败: %w", err)
	}

	s.db = db
	return nil
}

// GetLocalVersion 读取本地版本号 (双重校验机制)
func (s *GeoService) GetLocalVersion() string {
	// 1. 必须先校验物理文件是否真实存在！
	if _, err := os.Stat(s.path); os.IsNotExist(err) {
		return "" // 如果物理文件丢失，无视数据库记录，直接视为“未下载”
	}

	// 2. 文件存在的情况下，再读取数据库里的版本号
	var config database.SysConfig
	if err := database.DB.Where("key = ?", GeoDBConfigKey).First(&config).Error; err != nil {
		return ""
	}
	return config.Value
}

// GetRemoteVersion 通过 GitHub Releases 普通网页重定向获取最新 Tag。
// 通过普通 GitHub Releases 页面获取版本，避免共享出口 IP 命中未认证 API 速率限制。
func (s *GeoService) GetRemoteVersion() (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodGet, GeoReleaseURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "NodeCTL-Updater")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub Release 页面返回: %s", resp.Status)
	}

	if resp.Request == nil || resp.Request.URL == nil {
		return "", errors.New("无法解析 GitHub Release 最终地址")
	}

	parts := strings.Split(strings.Trim(resp.Request.URL.Path, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] != "tag" {
		return "", fmt.Errorf("无法从 Release 地址解析版本: %s", resp.Request.URL.String())
	}

	version := strings.TrimSpace(parts[len(parts)-1])
	if version == "" || strings.EqualFold(version, "latest") {
		return "", errors.New("未找到 Release 版本号")
	}
	return version, nil
}

// ForceUpdate 强制下载并更新数据库版本号
func (s *GeoService) ForceUpdate() error {
	slog.Info("开始后台更新 GeoIP 数据库...")

	// 1. 获取远程最新版本号
	remoteVersion, versionErr := s.GetRemoteVersion()
	if versionErr != nil {
		// 版本查询失败不应阻断数据库下载。数据库文件本身使用 releases/latest/download，
		// 即使 GitHub API 或版本页暂时不可用，也尽量保证 Geo 功能可恢复。
		slog.Warn("获取 GeoIP 远程版本失败，将继续尝试下载数据库", "err", versionErr)
		remoteVersion = "latest"
	}

	tempPath := s.path + ".update"

	// 2. 下载文件
	if err := downloadFile(tempPath, GeoDownloadURL); err != nil {
		return fmt.Errorf("下载文件失败: %w", err)
	}

	// 3. 加锁替换物理文件
	s.mu.Lock()
	if s.db != nil {
		s.db.Close()
		s.db = nil
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("替换文件失败: %w", err)
	}
	s.mu.Unlock()

	// 4. 重载进内存
	if err := s.Reload(); err != nil {
		return err
	}

	// 5. 写入版本号到 SQLite 数据库
	database.DB.Model(&database.SysConfig{}).Where("key = ?", GeoDBConfigKey).Update("value", remoteVersion)

	slog.Info("GeoIP 数据库更新完成", "version", remoteVersion)
	return nil
}

// GetCountryIsoCode 查询 IP (线程安全)
func (s *GeoService) GetCountryIsoCode(ipStr string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.db == nil || ipStr == "" {
		return ""
	}

	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ""
	}

	record, err := s.db.Country(ip)
	if err != nil {
		return ""
	}

	return record.Country.IsoCode
}

// GetCountryNameZhCN 查询 IP 对应国家中文名（zh-CN）
func (s *GeoService) GetCountryNameZhCN(ipStr string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.db == nil || ipStr == "" {
		return ""
	}

	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return ""
	}

	record, err := s.db.Country(ip)
	if err != nil {
		return ""
	}

	if name := strings.TrimSpace(record.Country.Names["zh-CN"]); name != "" {
		return name
	}
	if name := strings.TrimSpace(record.RegisteredCountry.Names["zh-CN"]); name != "" {
		return name
	}
	if iso := strings.TrimSpace(record.Country.IsoCode); iso != "" {
		return iso
	}
	if iso := strings.TrimSpace(record.RegisteredCountry.IsoCode); iso != "" {
		return iso
	}

	return ""
}

// downloadFile 通用下载
func downloadFile(filepath string, url string) error {
	client := &http.Client{Timeout: 300 * time.Second} // 防止大文件断开
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}

	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}
