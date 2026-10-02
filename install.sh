#!/bin/sh
# NodeCTL 一键安装脚本
# 项目地址: https://github.com/illria/nodectl
# 用法: sh -c "$(curl -fsSL https://raw.githubusercontent.com/illria/nodectl/main/install.sh)"

set -e

# ========== 颜色输出 ==========
RED='\033[1;31m'
GREEN='\033[1;32m'
YELLOW='\033[1;33m'
BLUE='\033[1;34m'
CYAN='\033[1;36m'
NC='\033[0m'

info()  { printf "${BLUE}[INFO]${NC} %s\n" "$*"; }
ok()    { printf "${GREEN}[ OK ]${NC} %s\n" "$*"; }
warn()  { printf "${YELLOW}[WARN]${NC} %s\n" "$*"; }
err()   { printf "${RED}[ERR]${NC} %s\n" "$*" >&2; }

# ========== 常量 ==========
GITHUB_REPO="illria/nodectl"
INSTALL_DIR="/opt/nodectl"
DATA_DIR="${INSTALL_DIR}/data"
BIN_PATH="${INSTALL_DIR}/nodectl"
NT_BIN="/usr/local/bin/nt"
DBCONFIG_PATH="${DATA_DIR}/dbconfig.json"
DEFAULT_PORT=8080

# ========== 检查 root 权限 ==========
check_root() {
    if [ "$(id -u)" != "0" ]; then
        err "此脚本需要 root 权限运行"
        err '请使用: sudo sh -c "$(curl -fsSL https://raw.githubusercontent.com/illria/nodectl/main/install.sh)"'
        exit 1
    fi
}

# ========== 检测系统类型 ==========
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
    elif echo "$OS_ID $OS_ID_LIKE" | grep -Ei "debian|ubuntu" >/dev/null 2>&1; then
        OS="debian"
    else
        OS="unknown"
    fi
}

# ========== 检测架构 ==========
detect_arch() {
    local arch
    arch=$(uname -m)
    case "$arch" in
        x86_64|amd64)  ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *) err "不支持的架构: $arch"; exit 1 ;;
    esac
}

# ========== 检测 init 系统 ==========
detect_init() {
    if [ -f /etc/alpine-release ] || command -v rc-service >/dev/null 2>&1; then
        INIT_SYS="openrc"
    else
        INIT_SYS="systemd"
    fi
}

# ========== 安装基础依赖 ==========
install_deps() {
    if command -v curl >/dev/null 2>&1; then
        return
    fi

    info "未检测到 curl，正在自动安装..."
    case "$OS" in
        debian)
            apt-get update -qq >/dev/null 2>&1
            apt-get install -y -qq curl >/dev/null 2>&1
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
        ok "curl 安装成功"
    else
        err "curl 安装失败，请手动安装后重试"
        exit 1
    fi
}

# ========== 获取最新版本号 ==========
get_latest_version() {
    local channel="$1"
    local version=""

    if [ "$channel" = "stable" ]; then
        # 稳定版：跟随 Releases 页面重定向，从最终 /tag/{version} 地址提取版本。
        local release_url
        release_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/${GITHUB_REPO}/releases/latest" 2>/dev/null)
        case "$release_url" in
            */releases/tag/*) version="${release_url##*/}" ;;
        esac
    else
        # Alpha 需要筛选多个预发布版本，保留独立的 releases API 路径。
        local api_url="https://api.github.com/repos/${GITHUB_REPO}/releases"
        version=$(curl -fsSL "${api_url}" 2>/dev/null | grep '"tag_name"' | grep -i 'alpha' | head -1 | sed 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/')
    fi

    if [ -z "$version" ]; then
        err "无法获取 ${channel} 版本号，请检查网络连接"
        exit 1
    fi

    echo "$version"
}

# ========== 用户选择版本 ==========
select_channel() {
    echo ""
    printf "${CYAN}╔══════════════════════════════════════╗${NC}\n"
    printf "${CYAN}║     NodeCTL 一键安装脚本             ║${NC}\n"
    printf "${CYAN}╠══════════════════════════════════════╣${NC}\n"
    printf "${CYAN}║                                      ║${NC}\n"
    printf "${CYAN}║  ${GREEN}1)${CYAN} 稳定版 (Stable)  ${YELLOW}← 推荐${CYAN}          ║${NC}\n"
    printf "${CYAN}║  ${GREEN}2)${CYAN} 开发版 (Alpha)                  ║${NC}\n"
    printf "${CYAN}║                                      ║${NC}\n"
    printf "${CYAN}╚══════════════════════════════════════╝${NC}\n"
    echo ""
    printf "请选择版本 [1/2] (默认 1): "
    read -r choice
    case "$choice" in
        2) CHANNEL="alpha" ;;
        *) CHANNEL="stable" ;;
    esac
}

# ========== 选择 Web 端口 ==========
check_port_available() {
    local port="$1"
    # 方法 1: 使用 ss (大多数 Linux 默认自带)
    if command -v ss >/dev/null 2>&1; then
        if ss -tlnH 2>/dev/null | grep -qE ":${port}\b"; then
            return 1
        fi
        return 0
    fi
    # 方法 2: 使用 netstat
    if command -v netstat >/dev/null 2>&1; then
        if netstat -tln 2>/dev/null | grep -qE ":${port}\b"; then
            return 1
        fi
        return 0
    fi
    # 方法 3: 尝试用 shell 内置功能探测端口 (不引入新依赖)
    if (echo >/dev/tcp/127.0.0.1/"$port") 2>/dev/null; then
        return 1
    fi
    return 0
}

select_port() {
    echo ""
    printf "${CYAN}╔══════════════════════════════════════╗${NC}\n"
    printf "${CYAN}║     设置 Web 服务端口                ║${NC}\n"
    printf "${CYAN}╠══════════════════════════════════════╣${NC}\n"
    printf "${CYAN}║                                      ║${NC}\n"
    printf "${CYAN}║  默认端口: ${GREEN}8080${CYAN}                      ║${NC}\n"
    printf "${CYAN}║  直接回车使用默认端口                ║${NC}\n"
    printf "${CYAN}║                                      ║${NC}\n"
    printf "${CYAN}╚══════════════════════════════════════╝${NC}\n"
    echo ""

    while true; do
        printf "请输入 Web 端口 [1-65535] (默认 ${DEFAULT_PORT}): "
        read -r port_input

        # 空输入使用默认端口
        if [ -z "$port_input" ]; then
            port_input="${DEFAULT_PORT}"
        fi

        # 验证是否为数字
        case "$port_input" in
            ''|*[!0-9]*)
                err "端口必须是数字，请重新输入"
                continue
                ;;
        esac

        # 验证端口范围
        if [ "$port_input" -lt 1 ] || [ "$port_input" -gt 65535 ]; then
            err "端口范围必须在 1-65535 之间，请重新输入"
            continue
        fi

        # 检查端口是否被占用
        if ! check_port_available "$port_input"; then
            err "端口 ${port_input} 已被占用，请更换其他端口"
            continue
        fi

        WEB_PORT="$port_input"
        ok "将使用端口: ${WEB_PORT}"
        break
    done
}

# ========== 写入 Web 端口到 dbconfig.json ==========
write_port_config() {
    local port="$1"
    mkdir -p "${DATA_DIR}"

    if [ -f "${DBCONFIG_PATH}" ]; then
        # 文件已存在，检查是否已有 web_port 字段
        if grep -q '"web_port"' "${DBCONFIG_PATH}" 2>/dev/null; then
            # 已有 web_port，用 sed 替换值（纯 POSIX，不引入 jq 依赖）
            sed -i "s/\"web_port\"[[:space:]]*:[[:space:]]*[0-9]*/\"web_port\": ${port}/" "${DBCONFIG_PATH}"
        else
            # 没有 web_port 字段，在最后一个 } 前插入
            # 先检查文件是否非空且为合法 JSON 对象
            if grep -q '}' "${DBCONFIG_PATH}" 2>/dev/null; then
                sed -i "s/\(.*\)}/\1,\n  \"web_port\": ${port}\n}/" "${DBCONFIG_PATH}"
            else
                # 文件损坏或为空，直接重写
                printf '{\n  "type": "sqlite",\n  "web_port": %d\n}\n' "$port" > "${DBCONFIG_PATH}"
            fi
        fi
    else
        # 文件不存在，创建新的
        printf '{\n  "type": "sqlite",\n  "web_port": %d\n}\n' "$port" > "${DBCONFIG_PATH}"
    fi

    chmod 600 "${DBCONFIG_PATH}"
    ok "端口配置已写入 ${DBCONFIG_PATH}"
}

# ========== 停止已有服务 ==========
stop_existing() {
    if [ "$INIT_SYS" = "openrc" ]; then
        rc-service nodectl stop 2>/dev/null || true
    else
        systemctl stop nodectl 2>/dev/null || true
    fi
    # 确保进程退出
    if command -v killall >/dev/null 2>&1; then
        killall nodectl 2>/dev/null || true
    fi
    if command -v pkill >/dev/null 2>&1; then
        pkill -f "${BIN_PATH}" 2>/dev/null || true
    fi
}

# ========== 下载二进制文件 ==========
download_binary() {
    local version="$1"
    local download_url="https://github.com/${GITHUB_REPO}/releases/download/${version}/nodectl-linux-${ARCH}"

    info "正在下载 NodeCTL ${version} (linux/${ARCH})..."
    info "下载地址: ${download_url}"

    mkdir -p "${INSTALL_DIR}"
    mkdir -p "${DATA_DIR}"

    if ! curl -fSL --progress-bar "$download_url" -o "${BIN_PATH}"; then
        err "下载失败，请检查网络连接或版本号是否正确"
        exit 1
    fi

    chmod +x "${BIN_PATH}"
    ok "二进制文件下载完成: ${BIN_PATH}"
}

# ========== 注册系统服务 ==========
install_service() {
    if [ "$INIT_SYS" = "openrc" ]; then
        # OpenRC 服务 (Alpine Linux)
        cat > /etc/init.d/nodectl <<'SVCEOF'
#!/sbin/openrc-run
name="nodectl"
description="NodeCTL - 节点管理面板"
command="/opt/nodectl/nodectl"
command_background=true
directory="/opt/nodectl"
pidfile="/run/nodectl.pid"
output_log="/opt/nodectl/data/nodectl.log"
error_log="/opt/nodectl/data/nodectl.log"

depend() {
    need net
    after firewall
}

start_pre() {
    checkpath --directory --owner root:root /opt/nodectl/data
}
SVCEOF
        chmod +x /etc/init.d/nodectl
        rc-update add nodectl default 2>/dev/null || true
        ok "OpenRC 服务已注册"
    else
        # systemd 服务 (Debian/Ubuntu 等)
        cat > /etc/systemd/system/nodectl.service <<'SVCEOF'
[Unit]
Description=NodeCTL - 节点管理面板
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/nodectl/nodectl
WorkingDirectory=/opt/nodectl
Restart=always
RestartSec=5
LimitNOFILE=1048576

StandardOutput=append:/opt/nodectl/data/nodectl.log
StandardError=append:/opt/nodectl/data/nodectl.log

[Install]
WantedBy=multi-user.target
SVCEOF
        systemctl daemon-reload
        systemctl enable nodectl >/dev/null 2>&1
        ok "systemd 服务已注册"
    fi
}

# ========== 安装 nt 管理工具 ==========
install_nt_command() {
    info "正在注册 nt 管理命令..."

    cat > "${NT_BIN}" <<'NTEOF'
#!/bin/sh
# NodeCTL 管理工具 (nt)
# 项目地址: https://github.com/illria/nodectl
# 用法: nt [命令]  或直接运行 nt 进入交互式菜单
# 兼容: Alpine (OpenRC) / Debian (systemd)

set -e

# ========== 颜色输出 ==========
RED='\033[1;31m'
GREEN='\033[1;32m'
YELLOW='\033[1;33m'
BLUE='\033[1;34m'
CYAN='\033[1;36m'
NC='\033[0m'

info()  { printf "${BLUE}[INFO]${NC} %s\n" "$*"; }
ok()    { printf "${GREEN}[ OK ]${NC} %s\n" "$*"; }
warn()  { printf "${YELLOW}[WARN]${NC} %s\n" "$*"; }
err()   { printf "${RED}[ERR]${NC} %s\n" "$*" >&2; }

# ========== 常量 ==========
GITHUB_REPO="illria/nodectl"
INSTALL_DIR="/opt/nodectl"
DATA_DIR="${INSTALL_DIR}/data"
BIN_PATH="${INSTALL_DIR}/nodectl"
NT_BIN="/usr/local/bin/nt"
INSTALL_INFO="${INSTALL_DIR}/.install_info"

# ========== 检查 root 权限 ==========
check_root() {
    if [ "$(id -u)" != "0" ]; then
        err "nt 命令需要 root 权限，请使用 sudo nt"
        exit 1
    fi
}

# ========== 检测 init 系统 ==========
detect_init() {
    if [ -f /etc/alpine-release ] || command -v rc-service >/dev/null 2>&1; then
        INIT_SYS="openrc"
    else
        INIT_SYS="systemd"
    fi
}

# ========== 检测架构 ==========
detect_arch() {
    local arch
    arch=$(uname -m)
    case "$arch" in
        x86_64|amd64)  ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *) err "不支持的架构: $arch"; exit 1 ;;
    esac
}

# ========== 读取安装信息 ==========
load_install_info() {
    if [ -f "$INSTALL_INFO" ]; then
        # 安全读取：逐行解析，避免未加引号的值导致命令执行
        while IFS='=' read -r key value; do
            case "$key" in
                CHANNEL)  CHANNEL="${value#\"}";  CHANNEL="${CHANNEL%\"}" ;;
                VERSION)  VERSION="${value#\"}";  VERSION="${VERSION%\"}" ;;
                INSTALL_TIME) INSTALL_TIME="${value#\"}"; INSTALL_TIME="${INSTALL_TIME%\"}" ;;
                ARCH)     ARCH="${value#\"}";     ARCH="${ARCH%\"}" ;;
                OS)       OS="${value#\"}";       OS="${OS%\"}" ;;
                INIT_SYS) INIT_SYS="${value#\"}"; INIT_SYS="${INIT_SYS%\"}" ;;
                WEB_PORT) WEB_PORT="${value#\"}";  WEB_PORT="${WEB_PORT%\"}" ;;
            esac
        done < "$INSTALL_INFO"
    fi
    # 兼容旧版安装（没有 WEB_PORT 字段），默认 8080
    if [ -z "$WEB_PORT" ]; then
        WEB_PORT="8080"
    fi
}

# ========== 获取当前版本 ==========
get_current_version() {
    if [ -f "$INSTALL_INFO" ]; then
        local ver
        ver=$(grep '^VERSION=' "$INSTALL_INFO" 2>/dev/null | head -1 | cut -d'=' -f2)
        # 移除可能的引号
        ver="${ver#\"}"; ver="${ver%\"}"
        echo "${ver:-未知}"
    else
        echo "未知"
    fi
}

# ========== 获取服务状态 ==========
get_service_status() {
    if [ "$INIT_SYS" = "openrc" ]; then
        if rc-service nodectl status 2>/dev/null | grep -qi "started"; then
            echo "运行中"
        else
            echo "已停止"
        fi
    else
        if systemctl is-active nodectl >/dev/null 2>&1; then
            echo "运行中"
        else
            echo "已停止"
        fi
    fi
}

# ========== 启动服务 ==========
do_start() {
    info "正在启动 NodeCTL..."

    if [ "$INIT_SYS" = "openrc" ]; then
        if rc-service nodectl status 2>/dev/null | grep -qi "started"; then
            warn "NodeCTL 已在运行中"
            return
        fi
        rc-service nodectl start
    else
        if systemctl is-active nodectl >/dev/null 2>&1; then
            warn "NodeCTL 已在运行中"
            return
        fi
        systemctl start nodectl
    fi

    sleep 1
    ok "NodeCTL 已启动"
}

# ========== 停止服务 ==========
do_stop() {
    info "正在停止 NodeCTL..."

    if [ "$INIT_SYS" = "openrc" ]; then
        rc-service nodectl stop 2>/dev/null || true
    else
        systemctl stop nodectl 2>/dev/null || true
    fi

    # 确保进程退出
    if command -v killall >/dev/null 2>&1; then
        killall nodectl 2>/dev/null || true
    fi
    if command -v pkill >/dev/null 2>&1; then
        pkill -f "${BIN_PATH}" 2>/dev/null || true
    fi

    sleep 1
    ok "NodeCTL 已停止"
}

# ========== 重启服务 ==========
do_restart() {
    info "正在重启 NodeCTL..."

    if [ "$INIT_SYS" = "openrc" ]; then
        rc-service nodectl restart 2>/dev/null || rc-service nodectl start 2>/dev/null || true
    else
        systemctl restart nodectl
    fi

    sleep 2

    # 验证
    local status
    status=$(get_service_status)
    if [ "$status" = "运行中" ]; then
        ok "NodeCTL 重启成功"
    else
        warn "NodeCTL 重启后未检测到进程，请检查日志: cat ${DATA_DIR}/nodectl.log"
    fi
}

# ========== 获取最新版本号 ==========
get_latest_version() {
    local channel="$1"
    local version=""

    if [ "$channel" = "stable" ]; then
        # 稳定版：跟随 Releases 页面重定向，从最终 /tag/{version} 地址提取版本。
        local release_url
        release_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/${GITHUB_REPO}/releases/latest" 2>/dev/null)
        case "$release_url" in
            */releases/tag/*) version="${release_url##*/}" ;;
        esac
    else
        local api_url="https://api.github.com/repos/${GITHUB_REPO}/releases"
        version=$(curl -fsSL "${api_url}" 2>/dev/null | grep '"tag_name"' | grep -i 'alpha' | head -1 | sed 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/')
    fi

    echo "$version"
}

# ========== 更新 ==========
do_update() {
    detect_arch
    load_install_info

    local current_channel="${CHANNEL:-stable}"
    local current_version="${VERSION:-未知}"

    info "当前版本: ${current_version}"
    info "当前渠道: ${current_channel}"
    echo ""

    # 让用户选择更新渠道
    printf "${CYAN}选择更新渠道:${NC}\n"
    printf "  ${GREEN}1)${NC} 稳定版 (Stable)\n"
    printf "  ${GREEN}2)${NC} 开发版 (Alpha)\n"
    printf "  ${GREEN}3)${NC} 保持当前渠道 (${current_channel})\n"
    echo ""
    printf "请选择 [1/2/3] (默认 3): "
    read -r choice

    local update_channel
    case "$choice" in
        1) update_channel="stable" ;;
        2) update_channel="alpha" ;;
        *) update_channel="$current_channel" ;;
    esac

    info "正在检查 ${update_channel} 最新版本..."
    local latest_version
    latest_version=$(get_latest_version "$update_channel")

    if [ -z "$latest_version" ]; then
        err "无法获取最新版本信息，请检查网络连接"
        return 1
    fi

    info "最新版本: ${latest_version}"

    if [ "$latest_version" = "$current_version" ]; then
        ok "当前已是最新版本，无需更新"
        return 0
    fi

    printf "\n确认从 ${YELLOW}${current_version}${NC} 更新到 ${GREEN}${latest_version}${NC}？[y/N] "
    read -r confirm
    case "$confirm" in
        y|Y|yes|YES) ;;
        *) info "已取消更新"; return 0 ;;
    esac

    # 停止服务
    do_stop

    # 下载新版本
    local download_url="https://github.com/${GITHUB_REPO}/releases/download/${latest_version}/nodectl-linux-${ARCH}"
    info "正在下载 ${latest_version}..."

    if ! curl -fSL --progress-bar "$download_url" -o "${BIN_PATH}.new"; then
        err "下载失败"
        # 重新启动旧版本
        do_start
        return 1
    fi

    # 替换二进制
    chmod +x "${BIN_PATH}.new"
    mv -f "${BIN_PATH}.new" "${BIN_PATH}"

    # 更新安装信息
    if [ -f "$INSTALL_INFO" ]; then
        local install_time
        install_time=$(date '+%Y-%m-%d %H:%M:%S')
        local old_os old_init
        old_os="${OS:-unknown}"
        old_init="${INIT_SYS:-unknown}"
        cat > "$INSTALL_INFO" <<EOF
CHANNEL="${update_channel}"
VERSION="${latest_version}"
INSTALL_TIME="${install_time}"
ARCH="${ARCH}"
OS="${old_os}"
INIT_SYS="${old_init}"
EOF
    fi

    # 启动服务
    do_start

    echo ""
    ok "NodeCTL 已更新到 ${latest_version}"
}

# ========== 卸载 ==========
do_uninstall() {
    echo ""
    warn "⚠️  即将完整卸载 NodeCTL"
    warn "将删除: 二进制文件、data 数据目录、系统服务、nt 管理工具"
    echo ""
    printf "确认卸载？输入 ${RED}yes${NC} 继续: "
    read -r confirm
    if [ "$confirm" != "yes" ]; then
        info "已取消卸载"
        return
    fi

    info "开始卸载..."

    # 1. 停止并禁用服务
    info "[1/4] 停止服务..."
    if [ "$INIT_SYS" = "openrc" ]; then
        rc-service nodectl stop 2>/dev/null || true
        rc-update del nodectl default 2>/dev/null || true
        rm -f /etc/init.d/nodectl
    else
        systemctl stop nodectl 2>/dev/null || true
        systemctl disable nodectl 2>/dev/null || true
        rm -f /etc/systemd/system/nodectl.service
        systemctl daemon-reload 2>/dev/null || true
    fi
    # 确保进程退出
    if command -v killall >/dev/null 2>&1; then
        killall nodectl 2>/dev/null || true
    fi
    if command -v pkill >/dev/null 2>&1; then
        pkill -9 -f "${BIN_PATH}" 2>/dev/null || true
    fi

    # 2. 删除二进制和数据目录
    info "[2/4] 删除程序文件和数据目录..."
    rm -rf "${INSTALL_DIR}"

    # 3. 删除 nt 管理工具
    info "[3/4] 删除 nt 管理工具..."
    rm -f "${NT_BIN}"

    # 4. 清理残余
    info "[4/4] 清理完成"

    echo ""
    ok "✅ NodeCTL 已完整卸载！"
    ok "  已清除: 二进制文件、data 数据、系统服务、nt 管理工具"
    echo ""
}

# ========== 显示状态 ==========
do_status() {
    load_install_info

    local status
    status=$(get_service_status)
    local pid_info=""

    if [ "$status" = "运行中" ]; then
        if command -v pidof >/dev/null 2>&1; then
            pid_info=" (PID: $(pidof nodectl 2>/dev/null | awk '{print $1}'))"
        fi
    fi

    echo ""
    printf "${CYAN}╔══════════════════════════════════════╗${NC}\n"
    printf "${CYAN}║       NodeCTL 状态信息               ║${NC}\n"
    printf "${CYAN}╠══════════════════════════════════════╣${NC}\n"
    printf "${CYAN}║  版本: %-31s║${NC}\n" "${VERSION:-未知}"
    printf "${CYAN}║  渠道: %-31s║${NC}\n" "${CHANNEL:-未知}"
    if [ "$status" = "运行中" ]; then
        printf "${CYAN}║  状态: ${GREEN}%-31s${CYAN}║${NC}\n" "${status}${pid_info}"
    else
        printf "${CYAN}║  状态: ${RED}%-31s${CYAN}║${NC}\n" "${status}"
    fi
    printf "${CYAN}║  端口: %-31s║${NC}\n" "${WEB_PORT:-8080}"
    printf "${CYAN}║  目录: %-31s║${NC}\n" "${INSTALL_DIR}"
    printf "${CYAN}║  架构: %-31s║${NC}\n" "${ARCH:-未知}"
    printf "${CYAN}║  系统: %-31s║${NC}\n" "${INIT_SYS}"
    printf "${CYAN}╚══════════════════════════════════════╝${NC}\n"
    echo ""
}

# ========== 查看日志 ==========
do_log() {
    local log_file="${DATA_DIR}/nodectl.log"
    local lines="${1:-50}"

    if [ ! -f "$log_file" ]; then
        # 尝试旧日志路径
        log_file="${DATA_DIR}/debug/nodectl.log"
    fi

    if [ ! -f "$log_file" ]; then
        warn "未找到日志文件"
        return 1
    fi

    info "显示最近 ${lines} 行日志:"
    echo "---"
    tail -n "$lines" "$log_file"
}

# ========== 交互式菜单 ==========
show_menu() {
    load_install_info

    local status
    status=$(get_service_status)
    local ver
    ver=$(get_current_version)
    local current_channel="${CHANNEL:-stable}"

    # 获取最新版本（静默获取，失败不中断）
    local latest=""
    latest=$(get_latest_version "$current_channel" 2>/dev/null) || true
    if [ -z "$latest" ]; then
        latest="获取失败"
    fi

    # 判断是否有更新
    local update_hint=""
    if [ "$latest" != "获取失败" ] && [ "$latest" != "$ver" ] && [ "$ver" != "未知" ]; then
        update_hint="  (有新版本!)"
    elif [ "$latest" = "$ver" ] && [ "$ver" != "未知" ]; then
        update_hint="  (已是最新)"
    fi

    echo ""
    printf "${CYAN}╔══════════════════════════════════════╗${NC}\n"
    printf "${CYAN}║       NodeCTL 管理面板 (nt)          ║${NC}\n"
    printf "${CYAN}╠══════════════════════════════════════╣${NC}\n"
    if [ "$status" = "运行中" ]; then
        printf "${CYAN}║  状态: ${GREEN}运行中${CYAN}    渠道: %-14s║${NC}\n" "${current_channel}"
    else
        printf "${CYAN}║  状态: ${RED}已停止${CYAN}    渠道: %-14s║${NC}\n" "${current_channel}"
    fi
    printf "${CYAN}║  当前: %-31s║${NC}\n" "${ver}"
    printf "${CYAN}║  端口: %-31s║${NC}\n" "${WEB_PORT:-8080}"
    printf "${CYAN}║  最新: %-31s║${NC}\n" "${latest}${update_hint}"
    printf "${CYAN}╠══════════════════════════════════════╣${NC}\n"
    printf "${CYAN}║                                      ║${NC}\n"
    printf "${CYAN}║  ${GREEN}1)${CYAN} 启动服务                        ║${NC}\n"
    printf "${CYAN}║  ${GREEN}2)${CYAN} 停止服务                        ║${NC}\n"
    printf "${CYAN}║  ${GREEN}3)${CYAN} 重启服务                        ║${NC}\n"
    printf "${CYAN}║  ${GREEN}4)${CYAN} 更新版本                        ║${NC}\n"
    printf "${CYAN}║  ${GREEN}5)${CYAN} 查看状态                        ║${NC}\n"
    printf "${CYAN}║  ${GREEN}6)${CYAN} 查看日志                        ║${NC}\n"
    printf "${CYAN}║  ${RED}7)${CYAN} 卸载 NodeCTL                    ║${NC}\n"
    printf "${CYAN}║                                      ║${NC}\n"
    printf "${CYAN}║  ${YELLOW}0)${CYAN} 退出                            ║${NC}\n"
    printf "${CYAN}║                                      ║${NC}\n"
    printf "${CYAN}╚══════════════════════════════════════╝${NC}\n"
    echo ""
    printf "请选择 [0-7]: "
    read -r choice
    case "$choice" in
        1) do_start ;;
        2) do_stop ;;
        3) do_restart ;;
        4) do_update ;;
        5) do_status ;;
        6) do_log ;;
        7) do_uninstall ;;
        0) echo "退出"; exit 0 ;;
        *) warn "无效选择" ;;
    esac
}

# ========== 帮助信息 ==========
show_help() {
    echo ""
    printf "${CYAN}NodeCTL 管理工具 (nt)${NC}\n"
    echo ""
    echo "用法: nt [命令]"
    echo ""
    echo "可用命令:"
    echo "  start       启动 NodeCTL 服务"
    echo "  stop        停止 NodeCTL 服务"
    echo "  restart     重启 NodeCTL 服务"
    echo "  update      更新 NodeCTL 版本"
    echo "  uninstall   完整卸载 NodeCTL (删除程序和数据)"
    echo "  status      查看运行状态"
    echo "  log [N]     查看最近 N 行日志 (默认 50)"
    echo "  help        显示此帮助"
    echo ""
    echo "不带参数运行 nt 可进入交互式菜单"
    echo ""
}

# ========== 主入口 ==========
main() {
    check_root
    detect_init
    detect_arch

    if [ $# -eq 0 ]; then
        # 无参数，进入交互式菜单循环
        while true; do
            show_menu
            echo ""
            printf "按回车继续..."
            read -r _
        done
    else
        # 有参数，直接执行对应命令
        case "$1" in
            start)     do_start ;;
            stop)      do_stop ;;
            restart)   do_restart ;;
            update)    do_update ;;
            uninstall) do_uninstall ;;
            status)    do_status ;;
            log|logs)  do_log "${2:-50}" ;;
            help|--help|-h) show_help ;;
            *)
                err "未知命令: $1"
                show_help
                exit 1
                ;;
        esac
    fi
}

main "$@"
NTEOF

    chmod +x "${NT_BIN}"
    ok "nt 管理命令已注册到 ${NT_BIN}"
}

# ========== 启动服务 ==========
start_service() {
    info "正在启动 NodeCTL..."
    if [ "$INIT_SYS" = "openrc" ]; then
        rc-service nodectl restart 2>/dev/null || rc-service nodectl start 2>/dev/null || true
    else
        systemctl restart nodectl
    fi

    # 等待启动
    sleep 2

    # 验证
    if command -v pidof >/dev/null 2>&1; then
        if pidof nodectl >/dev/null 2>&1; then
            ok "NodeCTL 启动成功 (PID: $(pidof nodectl | awk '{print $1}'))"
            return 0
        fi
    fi

    # pidof 不可用时尝试其他方式检测
    if [ "$INIT_SYS" = "openrc" ]; then
        if rc-service nodectl status 2>/dev/null | grep -qi "started"; then
            ok "NodeCTL 启动成功"
            return 0
        fi
    else
        if systemctl is-active nodectl >/dev/null 2>&1; then
            ok "NodeCTL 启动成功"
            return 0
        fi
    fi

    warn "NodeCTL 进程未检测到，请检查日志: cat ${DATA_DIR}/nodectl.log"
}

# ========== 保存安装信息 ==========
save_install_info() {
    local version="$1"
    local channel="$2"
    local port="$3"
    local install_time
    install_time=$(date '+%Y-%m-%d %H:%M:%S')
    cat > "${INSTALL_DIR}/.install_info" <<EOF
CHANNEL="${channel}"
VERSION="${version}"
INSTALL_TIME="${install_time}"
ARCH="${ARCH}"
OS="${OS}"
INIT_SYS="${INIT_SYS}"
WEB_PORT="${port}"
EOF
}

# ========== 主安装逻辑 ==========
main() {
    check_root
    detect_os
    detect_arch
    detect_init

    info "检测到系统: ${OS} (${OS_ID:-unknown})"
    info "检测到架构: ${ARCH}"
    info "Init 系统: ${INIT_SYS}"

    # 安装基础依赖
    install_deps

    # 用户选择版本
    select_channel

    # 用户选择 Web 端口
    select_port

    # 获取最新版本号
    info "正在获取 ${CHANNEL} 最新版本..."
    VERSION=$(get_latest_version "$CHANNEL")
    ok "最新版本: ${VERSION}"

    # 停止已有服务
    stop_existing

    # 下载二进制
    download_binary "$VERSION"

    # 写入端口配置到 dbconfig.json
    write_port_config "$WEB_PORT"

    # 注册系统服务
    install_service

    # 安装 nt 管理工具
    install_nt_command

    # 保存安装信息
    save_install_info "$VERSION" "$CHANNEL" "$WEB_PORT"

    # 启动服务
    start_service

    # 完成提示
    local access_url="http://你的IP:${WEB_PORT}"
    echo ""
    printf "${GREEN}╔══════════════════════════════════════╗${NC}\n"
    printf "${GREEN}║     NodeCTL 安装完成！               ║${NC}\n"
    printf "${GREEN}╠══════════════════════════════════════╣${NC}\n"
    printf "${GREEN}║                                      ║${NC}\n"
    printf "${GREEN}║  版本: %-31s║${NC}\n" "${VERSION}"
    printf "${GREEN}║  渠道: %-31s║${NC}\n" "${CHANNEL}"
    printf "${GREEN}║  目录: %-31s║${NC}\n" "${INSTALL_DIR}"
    printf "${GREEN}║  端口: %-31s║${NC}\n" "${WEB_PORT}"
    printf "${GREEN}║                                      ║${NC}\n"
    printf "${GREEN}║  访问: %-31s║${NC}\n" "${access_url}"
    printf "${GREEN}║  账号: admin / admin                 ║${NC}\n"
    printf "${GREEN}║                                      ║${NC}\n"
    printf "${GREEN}║  管理命令: nt                         ║${NC}\n"
    printf "${GREEN}║                                      ║${NC}\n"
    printf "${GREEN}╚══════════════════════════════════════╝${NC}\n"
    echo ""
    info "快捷管理命令:"
    info "  nt           进入交互式管理菜单"
    info "  nt start     启动服务"
    info "  nt stop      停止服务"
    info "  nt restart   重启服务"
    info "  nt update    更新版本"
    info "  nt uninstall 卸载 NodeCTL"
    info "  nt help      查看帮助"
    echo ""
}

main
