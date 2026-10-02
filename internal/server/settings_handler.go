package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"nodectl/internal/database"
	"nodectl/internal/logger"
	"nodectl/internal/middleware"
	"nodectl/internal/service"

	"gorm.io/gorm"
)

// ------------------- [系统设置 API] -------------------

func apiGetSettings(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	if r.Method != http.MethodGet {
		logger.Log.Warn("非法请求方法", "method", r.Method, "ip", clientIP, "path", reqPath)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var configs []database.SysConfig
	if err := database.DB.Where("key IN ?", []string{
		"panel_url", "sub_token", "proxy_port_ss", "proxy_port_hy2", "proxy_port_tuic",
		"proxy_port_reality", "proxy_ss_method",
		"sys_force_http", "login_ip_retry_window_sec", "login_ip_max_retries", "login_ip_block_ttl_sec",
		"proxy_port_socks5", "proxy_socks5_user", "proxy_socks5_pass", "proxy_socks5_random_auth", "pref_use_emoji_flag", "pref_force_protocol_prefix", "sub_custom_name", "pref_ip_strategy", "pref_default_install_protocols",
		"sys_log_level", "airport_filter_invalid", "airport_auto_update_enabled", "airport_auto_update_interval_minutes", "pref_speed_test_file_size", "pref_traffic_stats_retention_days", "pref_traffic_point_persist_interval_sec",
		"auth_cookie_ttl_mode",
		"tg_bot_enabled", "tg_bot_token", "tg_bot_whitelist", "tg_bot_register_commands", "tg_bot_socks5_enabled", "tg_bot_socks5_addr", "tg_bot_socks5_user", "tg_bot_socks5_pass", "tg_login_notify_mode", "tg_speedtest_notify_enabled", "tg_threshold_stop_notify_enabled", "clash_proxies_update_interval", "clash_rules_update_interval", "clash_public_rules_update_interval",
		"geo_auto_update", "mihomo_auto_update", "agent_startup_silent_update_enabled",
		"cf_tunnel_auto_update", "cf_ipopt_auto_update",
		// 新增协议与内核优化配置
		"proxy_port_trojan", "proxy_hy2_sni", "proxy_tuic_sni", "proxy_enable_bbr",
		// VMess 族
		"proxy_port_vmess_tcp", "proxy_port_vmess_ws", "proxy_port_vmess_http", "proxy_port_vmess_quic",
		"proxy_port_vmess_wst", "proxy_port_vmess_hut",
		// VLESS-TLS 族
		"proxy_port_vless_wst", "proxy_port_vless_hut",
		// Trojan-TLS 族
		"proxy_port_trojan_wst", "proxy_port_trojan_hut",
		"proxy_tls_transport_path", "proxy_vmess_tls_sni", "proxy_vless_tls_sni", "proxy_trojan_tls_sni",
		// AnyTLS
		"proxy_port_anytls", "proxy_anytls_sni",
	}).Find(&configs).Error; err != nil {
		logger.Log.Error("读取系统配置失败", "error", err, "ip", clientIP, "path", reqPath)
	}

	data := make(map[string]string)
	for _, c := range configs {
		if (c.Key == "cf_api_key" || c.Key == "tg_bot_token" || c.Key == "tg_bot_socks5_pass") && c.Value != "" {
			data[c.Key] = "********"
		} else {
			data[c.Key] = c.Value
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data":   data,
	})
}

func apiUpdateSettings(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	reqPath := r.URL.Path

	if r.Method != http.MethodPost {
		logger.Log.Warn("非法请求方法", "method", r.Method, "ip", clientIP, "path", reqPath)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req map[string]string
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Log.Warn("解析 JSON 失败", "error", err, "ip", clientIP, "path", reqPath)
		sendJSON(w, "error", "请求格式错误")
		return
	}

	validKeys := map[string]bool{
		"panel_url": true, "sub_token": true, "proxy_port_ss": true, "proxy_port_hy2": true,
		"proxy_port_tuic": true, "proxy_port_reality": true,
		"sys_force_http": true, "login_ip_retry_window_sec": true, "login_ip_max_retries": true, "login_ip_block_ttl_sec": true,
		"proxy_ss_method": true, "proxy_port_socks5": true, "proxy_socks5_user": true, "proxy_socks5_pass": true, "proxy_socks5_random_auth": true, "pref_use_emoji_flag": true, "pref_force_protocol_prefix": true,
		"sub_custom_name": true, "pref_ip_strategy": true, "pref_default_install_protocols": true,
		"sys_log_level":          true,
		"airport_filter_invalid": true, "airport_auto_update_enabled": true, "airport_auto_update_interval_minutes": true,
		"pref_speed_test_file_size": true, "pref_traffic_stats_retention_days": true, "pref_traffic_point_persist_interval_sec": true,
		"auth_cookie_ttl_mode": true,
		"tg_bot_enabled":       true, "tg_bot_token": true, "tg_bot_whitelist": true, "tg_bot_register_commands": true,
		"tg_bot_socks5_enabled": true, "tg_bot_socks5_addr": true, "tg_bot_socks5_user": true, "tg_bot_socks5_pass": true,
		"tg_login_notify_mode": true, "tg_speedtest_notify_enabled": true, "tg_threshold_stop_notify_enabled": true,
		"clash_proxies_update_interval": true, "clash_rules_update_interval": true, "clash_public_rules_update_interval": true,
		"geo_auto_update": true, "mihomo_auto_update": true, "agent_startup_silent_update_enabled": true,
		"cf_tunnel_auto_update": true, "cf_ipopt_auto_update": true,
		// 新增协议与内核优化配置
		"proxy_port_trojan": true,
		"proxy_hy2_sni":     true, "proxy_tuic_sni": true, "proxy_enable_bbr": true,
		// VMess 族
		"proxy_port_vmess_tcp": true, "proxy_port_vmess_ws": true, "proxy_port_vmess_http": true, "proxy_port_vmess_quic": true,
		"proxy_port_vmess_wst": true, "proxy_port_vmess_hut": true,
		// VLESS-TLS 族
		"proxy_port_vless_wst": true, "proxy_port_vless_hut": true,
		// Trojan-TLS 族
		"proxy_port_trojan_wst": true, "proxy_port_trojan_hut": true,
		"proxy_tls_transport_path": true,
		"proxy_vmess_tls_sni":      true, "proxy_vless_tls_sni": true, "proxy_trojan_tls_sni": true,
		// AnyTLS
		"proxy_port_anytls": true, "proxy_anytls_sni": true,
	}

	needRestartTgBot := false
	needReloadLoginRateLimit := false
	needKickGeoAutoUpdate := false
	needKickMihomoAutoUpdate := false
	needKickCFTunnelAutoUpdate := false
	needKickCFIPOptAutoUpdate := false
	changedDetails := make([]string, 0)

	maskValue := func(key, val string) string {
		if key == "cf_api_key" || key == "tg_bot_token" || key == "tg_bot_socks5_pass" {
			if val == "" {
				return "<empty>"
			}
			return "********"
		}
		if val == "" {
			return "<empty>"
		}
		return val
	}

	for k, v := range req {
		if validKeys[k] {
			if (k == "cf_api_key" || k == "tg_bot_token" || k == "tg_bot_socks5_pass") && v == "********" {
				continue
			}

			var oldConfig database.SysConfig
			oldValue := ""
			if err := database.DB.Where("key = ?", k).First(&oldConfig).Error; err == nil {
				oldValue = oldConfig.Value
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				logger.Log.Error("读取旧配置值失败", "key", k, "error", err, "ip", clientIP, "path", reqPath)
			}

			if k == "sys_log_level" {
				v = strings.ToLower(strings.TrimSpace(v))
				if v == "" {
					v = "info"
				}
				if oldValue == v {
					continue
				}
				if !logger.SetLevel(v) {
					logger.Log.Warn("收到非法日志等级配置，已拒绝更新", "value", v, "ip", clientIP, "path", reqPath)
					sendJSON(w, "error", "日志等级无效，仅支持: debug / info / warn / error")
					return
				}
			}

			if k == "pref_traffic_stats_retention_days" {
				v = strings.TrimSpace(v)
				days, err := strconv.Atoi(v)
				if err != nil || days < 1 || days > 3650 {
					sendJSON(w, "error", "流量记录保留天数无效，仅支持 1-3650 天")
					return
				}
				v = strconv.Itoa(days)
			}

			if k == "pref_traffic_point_persist_interval_sec" {
				v = strings.TrimSpace(v)
				seconds, err := strconv.Atoi(v)
				if err != nil || seconds < 0 || (seconds > 0 && seconds < 10) || seconds > 3600 {
					sendJSON(w, "error", "实时流量点数据落库间隔无效，仅支持 0 (关闭) 或 10-3600 秒")
					return
				}
				v = strconv.Itoa(seconds)
			}

			if k == "auth_cookie_ttl_mode" {
				v = normalizeAuthCookieTTLMode(v)
			}

			if k == "login_ip_retry_window_sec" {
				v = strings.TrimSpace(v)
				n, err := strconv.Atoi(v)
				if err != nil || n < 30 || n > 86400 {
					sendJSON(w, "error", "登录失败计数窗口无效，仅支持 30-86400 秒")
					return
				}
				v = strconv.Itoa(n)
			}

			if k == "login_ip_max_retries" {
				v = strings.TrimSpace(v)
				n, err := strconv.Atoi(v)
				if err != nil || n < 1 || n > 100 {
					sendJSON(w, "error", "登录最大重试次数无效，仅支持 1-100 次")
					return
				}
				v = strconv.Itoa(n)
			}

			if k == "login_ip_block_ttl_sec" {
				v = strings.TrimSpace(v)
				n, err := strconv.Atoi(v)
				if err != nil || n < 30 || n > 86400 {
					sendJSON(w, "error", "登录封禁时长无效，仅支持 30-86400 秒")
					return
				}
				v = strconv.Itoa(n)
			}

			if k == "tg_login_notify_mode" {
				v = strings.TrimSpace(v)
				switch v {
				case "off", "success_only", "failure_only", "all":
					// valid
				default:
					sendJSON(w, "error", "登录通知模式无效")
					return
				}
			}

			if k != "sys_log_level" && oldValue == v {
				continue
			}

			if k == "tg_bot_enabled" || k == "tg_bot_token" || k == "tg_bot_whitelist" || k == "tg_bot_register_commands" ||
				k == "tg_bot_socks5_enabled" || k == "tg_bot_socks5_addr" || k == "tg_bot_socks5_user" || k == "tg_bot_socks5_pass" {
				if oldConfig.Value != v {
					needRestartTgBot = true
				}
			}

			if k == "login_ip_retry_window_sec" || k == "login_ip_max_retries" || k == "login_ip_block_ttl_sec" {
				needReloadLoginRateLimit = true
			}

			if k == "geo_auto_update" && oldConfig.Value != v && strings.TrimSpace(strings.ToLower(v)) == "true" {
				needKickGeoAutoUpdate = true
			}

			if k == "mihomo_auto_update" && oldConfig.Value != v && strings.TrimSpace(strings.ToLower(v)) == "true" {
				needKickMihomoAutoUpdate = true
			}

			if k == "cf_tunnel_auto_update" && oldConfig.Value != v && strings.TrimSpace(strings.ToLower(v)) == "true" {
				needKickCFTunnelAutoUpdate = true
			}

			if k == "cf_ipopt_auto_update" && oldConfig.Value != v && strings.TrimSpace(strings.ToLower(v)) == "true" {
				needKickCFIPOptAutoUpdate = true
			}

			// 强制公共规则更新间隔最小为 86400
			if k == "clash_public_rules_update_interval" {
				intVal, err := strconv.Atoi(v)
				if err != nil || intVal < 86400 {
					v = "86400"
				}
			}

			if err := database.DB.Model(&database.SysConfig{}).Where("key = ?", k).Update("value", v).Error; err != nil {
				logger.Log.Error("更新系统配置异常", "key", k, "error", err, "ip", clientIP, "path", reqPath)
				continue
			}

			if k == "pref_default_install_protocols" {
				added, removed, reordered := summarizeListDelta(parseConfigListValue(oldValue), parseConfigListValue(v))
				if len(added) > 0 {
					changedDetails = append(changedDetails, "默认安装协议新增: "+strings.Join(added, ","))
				}
				if len(removed) > 0 {
					changedDetails = append(changedDetails, "默认安装协议删除: "+strings.Join(removed, ","))
				}
				if reordered {
					changedDetails = append(changedDetails, "默认安装协议: 仅顺序变化")
				}
				if len(added) == 0 && len(removed) == 0 && !reordered {
					changedDetails = append(changedDetails, "默认安装协议: 无有效变化")
				}
			} else {
				changedDetails = append(changedDetails, fmt.Sprintf("%s: %s -> %s", k, maskValue(k, oldValue), maskValue(k, v)))
			}
		}
	}

	if needRestartTgBot {
		go service.RestartTelegramBot()
	}

	if needReloadLoginRateLimit {
		if err := middleware.ReloadLoginRateLimitConfigFromDB(); err != nil {
			logger.Log.Error("热更新登录IP限流配置失败", "error", err, "ip", clientIP, "path", reqPath)
		}
	}

	if needKickGeoAutoUpdate {
		service.TriggerGeoAutoUpdateCheckNow()
	}

	if needKickMihomoAutoUpdate {
		service.TriggerMihomoAutoUpdateCheckNow()
	}

	if needKickCFTunnelAutoUpdate {
		service.TriggerCFTunnelAutoUpdateCheckNow()
	}

	if needKickCFIPOptAutoUpdate {
		service.TriggerCFIPOptAutoUpdateCheckNow()
	}

	if len(changedDetails) == 0 {
		logger.Log.Info("系统设置保存完成，但未检测到配置变更", "ip", clientIP, "path", reqPath)
		sendJSON(w, "success", "设置无变化")
		return
	}

	logger.Log.Info("系统全局配置已更新",
		"changed_count", len(changedDetails),
		"changes", strings.Join(changedDetails, " | "),
		"ip", clientIP,
		"path", reqPath,
	)
	sendJSON(w, "success", "设置已保存")
}
