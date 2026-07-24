// 路径: internal/server/install_handler.go
// 安装脚本生成接口（新版极简安装脚本 + Agent 二进制下载 + na 快捷命令）
package server

import (
	"fmt"
	"strings"

	"nodectl/internal/version"
)

// generateMinimalInstallScript 🆕 生成新版极简安装脚本
// 负责：检测系统环境 → 安装依赖 → 下载 agent → 写配置 → 注册系统服务 → 注册 na 快捷命令
// 支持 Debian/Ubuntu、CentOS/RHEL/Fedora、Alpine Linux 三大系统族
// 所有复杂逻辑（sing-box 安装、配置生成、证书管理）内置于 Agent 二进制中
func generateMinimalInstallScript(installID, panelURL string, enableBBR bool) string {
	// 获取当前面板的发布渠道（仅用于下载阶段，确保下载与面板版本匹配的 Agent）
	channel := string(version.GetChannel())

	// 将布尔值转换为 shell 变量
	bbrFlag := "false"
	if enableBBR {
		bbrFlag = "true"
	}

	return fmt.Sprintf(`#!/usr/bin/env bash
# NodeCTL Agent 安装脚本 (新版极简模式)
# 由面板动态生成，用户无需手动修改
# 可选参数：--report-url <URL>  指定备用回调地址（如 Tunnel 隧道地址）

set -euo pipefail

# ========== 面板生成时自动填充 ==========
INSTALL_ID="%s"
PANEL_URL="%s"
CHANNEL="%s"
ENABLE_BBR="%s"
# =========================================

# 彩色输出
info() { echo -e "\033[1;34m[INFO]\033[0m $*"; }
warn() { echo -e "\033[1;33m[WARN]\033[0m $*"; }
err()  { echo -e "\033[1;31m[ERR]\033[0m $*" >&2; }

# -----------------------
# 检测系统类型
detect_os() {
    if [ -f /etc/os-release ]; then
        . /etc/os-release
        OS_ID="${ID:-}"
        OS_ID_LIKE="${ID_LIKE:-}"
    else
        OS_ID=""
        OS_ID_LIKE=""
    fi

    if echo "$OS_ID $OS_ID_LIKE" | grep -qi "alpine"; then
        OS="alpine"
    elif echo "$OS_ID $OS_ID_LIKE" | grep -Ei "debian|ubuntu" >/dev/null; then
        OS="debian"
    elif echo "$OS_ID $OS_ID_LIKE" | grep -Ei "centos|rhel|fedora" >/dev/null; then
        OS="redhat"
    else
        OS="unknown"
    fi
}

# -----------------------
# 安装基础依赖（curl）
install_deps() {
    if command -v curl >/dev/null 2>&1; then
        info "curl 已存在，跳过安装"
        return
    fi

    info "未检测到 curl，正在自动安装..."
    case "$OS" in
        debian)
            apt-get update -qq >/dev/null 2>&1
            apt-get install -y -qq curl >/dev/null 2>&1
            ;;
        redhat)
            if command -v dnf >/dev/null 2>&1; then
                dnf install -y -q curl >/dev/null 2>&1
            else
                yum install -y -q curl >/dev/null 2>&1
            fi
            ;;
        alpine)
            apk update >/dev/null 2>&1
            apk add --no-cache curl >/dev/null 2>&1
            ;;
        *)
            err "未知系统类型，无法自动安装 curl，请手动安装后重试"
            exit 1
            ;;
    esac

    if command -v curl >/dev/null 2>&1; then
        info "curl 安装成功 ✓"
    else
        err "curl 安装失败，请手动安装后重试"
        exit 1
    fi
}

# -----------------------
# 检查 root 权限
check_root() {
    if [ "$(id -u)" != "0" ]; then
        err "此脚本需要 root 权限"
        err "请使用: sudo bash -c \"\$(curl -fsSL ...)\" 或切换到 root 用户"
        exit 1
    fi
}

# 解析命令行参数（兼容 bash -c "script" --report-url URL 调用方式，$0 也可能携带参数）
set -- "$0" "$@"
REPORT_URL=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --report-url)
            REPORT_URL="$2"
            shift 2
            ;;
        *)
            shift
            ;;
    esac
done

# 检测架构
detect_arch() {
    local arch=$(uname -m)
    case "$arch" in
        x86_64|amd64)  echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *) err "不支持的架构: $arch"; exit 1 ;;
    esac
}

# 注册 na 快捷命令（仅卸载功能）
install_na_command() {
    info "注册 na 快捷命令..."

    cat > /usr/local/bin/na <<'NAEOF'
#!/usr/bin/env bash
# NodeCTL Agent 快捷管理命令 (na)
# 用法: na uninstall  或直接运行 na 进入交互式菜单

set -euo pipefail

# 彩色输出
RED='\033[1;31m'
GREEN='\033[1;32m'
YELLOW='\033[1;33m'
BLUE='\033[1;34m'
CYAN='\033[1;36m'
NC='\033[0m'

info()  { echo -e "${BLUE}[INFO]${NC} $*"; }
ok()    { echo -e "${GREEN}[OK]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
err()   { echo -e "${RED}[ERR]${NC} $*" >&2; }

# 检查 root
if [ "$(id -u)" != "0" ]; then
    err "na 命令需要 root 权限，请使用 sudo na"
    exit 1
fi

# 检测 init 系统
detect_init() {
    if [ -f /etc/alpine-release ] || command -v rc-service >/dev/null 2>&1; then
        echo "openrc"
    else
        echo "systemd"
    fi
}

INIT_SYS=$(detect_init)

# ========== 卸载功能 ==========

do_uninstall() {
    echo ""
    warn "⚠️  即将完整卸载 NodeCTL Agent"
    warn "将删除: 服务、二进制、sing-box、配置、证书、日志、na 命令"
    echo ""
    read -r -p "确认卸载？输入 yes 继续: " confirm
    if [ "$confirm" != "yes" ]; then
        info "已取消卸载"
        return
    fi

    info "开始完整卸载..."

    # 1. 停止 sing-box 进程（agent 管理的子进程）
    info "[1/7] 停止 sing-box..."
    killall sing-box 2>/dev/null || true
    # 等待进程退出
    sleep 1
    # 强制清除残余进程
    pkill -9 -f "sing-box" 2>/dev/null || true

    # 2. 停止并禁用 nodectl-agent 服务
    info "[2/7] 停止 nodectl-agent 服务..."
    if [ "$INIT_SYS" = "openrc" ]; then
        rc-service nodectl-agent stop 2>/dev/null || true
        rc-update del nodectl-agent default 2>/dev/null || true
        rm -f /etc/init.d/nodectl-agent
    else
        systemctl stop nodectl-agent 2>/dev/null || true
        systemctl disable nodectl-agent 2>/dev/null || true
        rm -f /etc/systemd/system/nodectl-agent.service
        systemctl daemon-reload 2>/dev/null || true
    fi
    # 确保进程完全退出
    killall nodectl-agent 2>/dev/null || true
    pkill -9 -f "nodectl-agent" 2>/dev/null || true

    # 3. 删除二进制文件
    info "[3/7] 删除二进制文件..."
    rm -f /usr/local/bin/nodectl-agent
    rm -f /var/lib/nodectl-agent/sing-box

    # 4. 删除配置文件
    info "[4/7] 删除配置文件..."
    rm -rf /etc/nodectl-agent
    rm -f /var/lib/nodectl-agent/singbox-config.json
    rm -f /var/lib/nodectl-agent/protocols.json

    # 5. 删除证书文件
    info "[5/7] 删除证书文件..."
    rm -rf /var/lib/nodectl-agent/certs

    # 6. 删除日志文件和数据目录
    info "[6/7] 删除日志和数据目录..."
    rm -f /var/log/nodectl-agent.log
    rm -rf /var/log/nodectl-agent
    rm -f /var/lib/nodectl-agent/singbox.pid
    # 清理数据目录（若已为空则删除）
    rmdir /var/lib/nodectl-agent 2>/dev/null || rm -rf /var/lib/nodectl-agent

    # 7. 删除 na 快捷命令自身
    info "[7/7] 删除 na 快捷命令..."
    rm -f /usr/local/bin/na

    echo ""
    ok "✅ NodeCTL Agent 已完整卸载！"
    ok "  已清除: 二进制、sing-box、配置、证书、日志、服务、na 命令"
    echo ""
}

# ========== 交互式菜单 ==========

show_menu() {
    echo ""
    echo -e "${CYAN}NodeCTL Agent 管理 (na)${NC}"
    echo -e "  ${RED}1)${NC} 完整卸载"
    echo -e "  ${YELLOW}0)${NC} 退出"
    echo ""
    read -r -p "请选择: " choice
    case "$choice" in
        1) do_uninstall ;;
        0) echo "退出"; exit 0 ;;
        *) warn "无效选择，请重试" ;;
    esac
}

# ========== 命令行直接调用 ==========

show_help() {
    echo ""
    echo -e "${CYAN}NodeCTL Agent 快捷命令 (na)${NC}"
    echo ""
    echo "用法: na [命令]"
    echo ""
    echo "可用命令:"
    echo "  uninstall   完整卸载 (清除所有数据)"
    echo "  help        显示此帮助"
    echo ""
    echo "不带参数运行 na 可进入交互式菜单"
    echo ""
}

# ========== 主入口 ==========

if [ $# -eq 0 ]; then
    # 无参数，进入交互式菜单
    while true; do
        show_menu
        echo ""
        read -r -p "按回车继续..." _
    done
else
    # 有参数，直接执行对应命令
    case "$1" in
        uninstall)  do_uninstall ;;
        help|--help|-h) show_help ;;
        *)
            err "未知命令: $1"
            show_help
            exit 1
            ;;
    esac
fi
NAEOF

    chmod +x /usr/local/bin/na
    info "na 快捷命令已注册 ✓"
    info "  用法: na        (交互式菜单)"
    info "  用法: na help   (查看所有命令)"
}

# -----------------------
# BBR 内核加速（从全局设置中的开关控制）
apply_bbr() {
    if [ "$ENABLE_BBR" != "true" ]; then
        info "跳过 BBR 优化（面板全局设置未启用）"
        return 0
    fi

    # 容器环境检测：Docker / LXC 容器继承宿主机内核参数，无需也无法修改
    if [ -f /.dockerenv ] || grep -qsE '(docker|lxc)' /proc/1/cgroup 2>/dev/null; then
        info "检测到容器环境，拥塞算法默认继承宿主机设置，跳过 BBR 优化"
        return 0
    fi

    # 内核参数可写性检测：只读文件系统或权限不足时跳过，防止污染配置文件
    if [ ! -w /proc/sys/net/ipv4/tcp_congestion_control ]; then
        warn "内核参数不可写（只读文件系统或权限不足），跳过 BBR 优化"
        return 0
    fi

    info "尝试启用 BBR 内核加速..."
    local kernel_major kernel_minor
    kernel_major=$(uname -r | cut -d. -f1)
    kernel_minor=$(uname -r | cut -d. -f2)

    if [ "$kernel_major" -ge 5 ] 2>/dev/null || { [ "$kernel_major" -eq 4 ] && [ "$kernel_minor" -ge 9 ]; } 2>/dev/null; then
        sed -i '/net.ipv4.tcp_congestion_control/d' /etc/sysctl.conf 2>/dev/null || true
        sed -i '/net.core.default_qdisc/d' /etc/sysctl.conf 2>/dev/null || true
        {
            echo "net.ipv4.tcp_congestion_control = bbr"
            echo "net.core.default_qdisc = fq"
        } >> /etc/sysctl.conf
        if sysctl -p >/dev/null 2>&1; then
            info "✅ BBR 内核加速已启用"
        else
            warn "❌ BBR 参数写入成功但 sysctl -p 执行失败，请手动检查 /etc/sysctl.conf"
        fi
    else
        warn "内核版本 $(uname -r) 过低（需 4.9+），已跳过 BBR 优化"
    fi
}

# 主安装逻辑
main() {
    # 0. 环境检测与准备
    detect_os
    check_root

    info "NodeCTL Agent 安装脚本"
    info "检测到系统: $OS (${OS_ID:-unknown})"

    local ARCH=$(detect_arch)

    info "检测到架构: $ARCH"

    # 0.5 安装基础依赖（确保 curl 可用）
    install_deps

    # 1. 清理旧 agent（如果存在）
    if [ "$OS" = "alpine" ]; then
        rc-service nodectl-agent stop 2>/dev/null || true
    else
        systemctl stop nodectl-agent 2>/dev/null || true
    fi
    killall nodectl-agent 2>/dev/null || true

    # 2. 下载 agent（channel 参数确保下载与面板版本匹配的 Agent）
    local DOWNLOAD_URL="${PANEL_URL}/api/public/download/agent?arch=${ARCH}&channel=${CHANNEL}"
    local AGENT_BIN="/usr/local/bin/nodectl-agent"
    local CONFIG_DIR="/etc/nodectl-agent"
    local DATA_DIR="/var/lib/nodectl-agent"
    local LOG_DIR="/var/log"

    info "正在下载 nodectl-agent..."
    curl -fsSL "$DOWNLOAD_URL" -o "$AGENT_BIN"
    chmod +x "$AGENT_BIN"

    # 3. 创建必要目录
    mkdir -p "$CONFIG_DIR"
    mkdir -p "$DATA_DIR"

    # 4. 生成最小化配置
    # 如果指定了 --report-url，则使用该地址作为回调地址；否则使用面板地址
    local CALLBACK_URL="${REPORT_URL:-$PANEL_URL}"

    cat > "${CONFIG_DIR}/config.json" <<EOF
{
  "install_id": "$INSTALL_ID",
  "panel_url": "$CALLBACK_URL",
  "interface": "auto",
  "log_level": "info"
}
EOF

    info "配置文件已写入: ${CONFIG_DIR}/config.json"
    if [ -n "$REPORT_URL" ]; then
        info "使用自定义回调地址: $REPORT_URL"
    fi

    # 5. 注册系统服务（根据 init 系统自动选择）
    if [ "$OS" = "alpine" ]; then
        # OpenRC 服务 (Alpine Linux)
        cat > /etc/init.d/nodectl-agent <<'SVCEOF'
#!/sbin/openrc-run
name="nodectl-agent"
description="NodeCTL Agent - 一体化代理管理器"
command="/usr/local/bin/nodectl-agent"
supervisor=supervise-daemon
rc_cgroup_cleanup="NO"
pidfile="/run/nodectl-agent.pid"
respawn_max=0
respawn_period=1800
respawn_delay=5

depend() {
    need net
    after firewall
}
SVCEOF
        chmod +x /etc/init.d/nodectl-agent
        rc-update add nodectl-agent default 2>/dev/null || true
        rc-service nodectl-agent restart 2>/dev/null || \
            rc-service nodectl-agent start 2>/dev/null || true
        info "OpenRC 服务已注册并启动 ✓"
    else
        # systemd 服务 (Debian/Ubuntu/CentOS/RHEL/Fedora 等)
        cat > /etc/systemd/system/nodectl-agent.service <<'SVCEOF'
[Unit]
Description=NodeCTL Agent - 一体化代理管理器
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/nodectl-agent
WorkingDirectory=/var/lib/nodectl-agent
Restart=always
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
SVCEOF
        systemctl daemon-reload
        systemctl enable nodectl-agent
        systemctl restart nodectl-agent
        info "systemd 服务已注册并启动 ✓"
    fi

    # 6. 注册 na 快捷命令
    install_na_command

    # 7. BBR 内核加速优化
    apply_bbr

    # 8. 验证安装
    sleep 2
    if pidof nodectl-agent >/dev/null 2>&1; then
        info "✅ nodectl-agent 安装完成！(PID: $(pidof nodectl-agent | awk '{print $1}'))"
    else
        warn "⚠️ nodectl-agent 进程未检测到，请检查日志"
        warn "  查看日志: tail -20 /var/log/nodectl-agent.log"
    fi

    info ""
    info "快捷管理命令:"
    info "  na            进入交互式管理菜单"
    info "  na uninstall  完整卸载 Agent"
    info "  na help       查看帮助"
}

main
`, installID, panelURL, channel, bbrFlag)
}

// getPanelURLForScript 获取面板 URL 用于安装脚本生成
// 优先使用配置中的 panel_url，否则从请求中推导
func getPanelURLForScript(configPanelURL string, requestHost string, isSecure bool) string {
	if strings.TrimSpace(configPanelURL) != "" {
		return strings.TrimRight(configPanelURL, "/")
	}

	scheme := "http"
	if isSecure {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, requestHost)
}
