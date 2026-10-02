# 🎯 NodeCtl：你的个人节点管理神器

> 本仓库为 **illria 独立维护版**，基于上游 `hobin66/nodectl` 持续维护。面板安装、GitHub Release、Docker 镜像与后续版本均由 `illria/nodectl` 独立发布。
>
> 当前准备版本：**`v0.4.76-custom.4`** ｜ Release：<https://github.com/illria/nodectl/releases/latest>

> 一个轻量、高效、功能强大的个人节点与订阅管理面板

📢 **上游项目 TG 频道**：https://t.me/nodectl

---

## 🤔 为什么选择 NodeCtl？

如果你：
- 有自建节点但管理起来很麻烦？
- 只有手机不方便搭建节点？
- 订阅了多个机场但切换配置很繁琐？
- 想要一个简洁直观的面板来统一管理所有代理服务？
- 拥有托管在 Cloudflare 的域名，想充分利用 CF 的能力？
- 没有公网，不会配置tunnel隧道，没有双栈服务器？
- 想用机场节点做中转，又不会配置？

那么 **NodeCtl** 就是为你量身打造的解决方案！

![功能概览](https://nodectl-ipopt.hobin.net/Image/功能概述1.webp)

---

## ✨ 核心亮点

### 体验完整功能需求

- **一台服务器**：只要能安装，无需公网
- **一个域名**：托管于CF的域名

### 🚀 自建节点？一行命令搞定！

NodeCtl 让自建节点管理变得前所未有的简单：

- **一键部署**：VPS 上执行一行命令，自动完成安装并回传链接和IP到面板，覆盖所有常用协议。
- **手动添加**：手动添加协议覆盖主流代理协议
- **无需公网**：内置CF tunnel隧道管理，仅需配置token即可一键部署
- **远程控制**：无需SSH登录，面板端直接重置链接、重装 Sing-box
- **实时监控**：流量使用、在线状态一目了然
- **离线告警**：节点掉线？可选 Telegram 推送离线通知
- **流量告警**：服务器流量达到阈值，自动剔除订阅并重置singbox

![节点管理1](https://nodectl-ipopt.hobin.net/Image/节点管理.webp)
![节点管理2](https://nodectl-ipopt.hobin.net/Image/节点管理2.webp)
![节点管理3](https://nodectl-ipopt.hobin.net/Image/节点管理3.webp)
![节点管理4](https://nodectl-ipopt.hobin.net/Image/节点管理4.webp)

### 🛩️ 机场订阅聚合，告别多端配置

- **多格式支持**：Clash YAML、Base64 等格式一键导入
- **智能过滤**：自动识别并剔除"到期提醒"等无效占位节点
- **批量测速**：内置 Mihomo 核心，真连接并发测速，SSE 实时推送结果
- **测速通知**：机场节点太多，测试太久，可选TG通知，完成即通知将无需等待。
- **灵活调度**：机场节点可分配为直连、落地或禁用，按需组合

![机场订阅](https://nodectl-ipopt.hobin.net/Image/机场管理.webp)

### ☁️ Cloudflare 深度集成

这是 NodeCtl 的一大特色功能：
⚠️Cloudflare CDN 已明文禁止代理方式使用，对于代理套 CDN 的自行承担风险


- **傻瓜式 Tunnel 隧道**：填入 CF Token，一键开启隧道，零配置
- **节点隧道支持**：节点启用 Tunnel？只需找到开关，点一下
- **自动 SSL 证书**：通过 CF API 自动签发续签，HTTP/HTTPS 热重载切换

![Cloudflare集成](https://nodectl-ipopt.hobin.net/Image/Cloudflare集成.webp)

### 🛡️ 强大的订阅分发能力

- **多端兼容**：一键生成 Clash (Meta)、V2Ray/Sing-box 等格式订阅链接
- **可视化分流**：直连/代理规则可视化编辑
- **智能分流**：集成 GeoIP/Geosite 数据，国内外流量自动分流

![订阅管理](https://nodectl-ipopt.hobin.net/Image/订阅管理.webp)

---

### 🎛️ Clash 高级分流配置

NodeCtl 内置了强大的 Clash Meta 分流配置系统：

- **预置分流模块**：内置 AI（ChatGPT/Claude）、Apple、Microsoft、Telegram、YouTube、Netflix、Steam 等常用分流规则
- **自定义分流组**：支持创建自定义代理组，自由配置域名、IP、进程规则
- **可视化规则编辑**：无需手写 YAML，可视化界面轻松添加直连/代理规则
- **GeoIP/Geosite 集成**：自动加载 MetaCubeX 维护的 mrs 规则集，国内外智能分流
- **DNS 策略联动**：分流规则自动同步到 DNS 策略，确保解析与路由一致
- **更新间隔可配置**：订阅、规则集更新频率均可自定义

![Clash分流配置](https://nodectl-ipopt.hobin.net/Image/clash分流1.webp)
![Clash分流配置](https://nodectl-ipopt.hobin.net/Image/clash分流2.webp)

### 🤖 详细的系统设置

NodeCTL 在为小白用户提供合理的默认参数的同时，为进阶用户留下了可自定义的空间

![系统设置](https://nodectl-ipopt.hobin.net/Image/系统设置.webp)

### 🤖 Agent 远程管控

NodeCtl 采用 **Agent + 中心面板** 架构，为你的节点提供强大的远程管控能力：

#### 📡 实时流量监控
- **零分配采集**：Agent 采用常驻 FD + 固定栈缓冲设计，每秒采集流量数据零堆分配
- **WebSocket 实时推送**：毫秒级流量速率上报，面板实时展示上传/下载速度
- **精准计量**：自动处理计数器回绕、机器重启等边缘场景，流量统计精准可靠

#### 🎮 远程命令执行
通过 WebSocket 双向通道，面板可向 Agent 下发多种命令：
- **重置链接**：一键重新生成节点订阅链接
- **重装 Sing-box**：远程重新安装/更新代理内核
- **Tunnel 管理**：远程启动/停止 Cloudflare Tunnel 隧道
- **命令流式输出**：执行结果实时推送，SSE 流式展示进度

---

## 🔥 技术特点

| 特性 | 说明 |
|------|------|
| **零依赖** | 纯 Golang 后端 + 嵌入式 SQLite，无需安装额外依赖 |
| **安全认证** | JWT 令牌鉴权 + 登录 IP 限流策略 |
| **系统监控** | 实时 CPU、内存、Go 协程数及子进程状态看板 |
| **数据库扩展** | 支持 PostgreSQL，可记录任意时长的流量使用记录 |

---

## 🚀 三分钟快速部署

> **重要：本仓库已经独立发布。**
>
> 请使用 `illria/nodectl` 的 Release、GHCR 镜像和安装脚本，不要再使用 `hobin66/nodectl` 的安装地址。
>
> 当前准备版本为 `v0.4.76-custom.4`；发布前请继续使用已发布的 `v0.4.76-custom.3`。

写在前面：Tunnel 隧道原生支持 IPv4 和 IPv6。如果需要安装 Agent，建议优先使用 Tunnel 域名。

### 方式一：Docker Run（推荐）

先拉取当前稳定镜像：

```bash
docker pull ghcr.io/illria/nodectl:v0.4.76-custom.4
```

启动：

```bash
mkdir -p /opt/nodectl/data

docker run -d \
  --name nodectl \
  --restart unless-stopped \
  -p 7878:8080 \
  --log-opt max-size=10m \
  --log-opt max-file=2 \
  -v /opt/nodectl/data:/app/data \
  ghcr.io/illria/nodectl:v0.4.76-custom.4
```

访问：

```text
http://你的服务器IP:7878
```

默认账号：

```text
admin / admin
```

首次登录后请立即修改密码。

如果希望始终跟随本仓库最新构建，可将镜像改为：

```text
ghcr.io/illria/nodectl:latest
```

### 方式二：Docker Compose

```yaml
services:
  nodectl:
    image: ghcr.io/illria/nodectl:v0.4.76-custom.4
    container_name: nodectl
    restart: unless-stopped
    ports:
      - "7878:8080"
    volumes:
      - /opt/nodectl/data:/app/data
    logging:
      options:
        max-size: "10m"
        max-file: "2"
```

启动：

```bash
docker compose up -d
```

### 方式三：Release 二进制一键安装

适合不想使用 Docker 的用户。安装脚本会从 **本仓库 GitHub Releases** 自动识别最新稳定版，并下载对应架构的二进制。

root 用户：

```bash
sh -c "$(curl -fsSL https://raw.githubusercontent.com/illria/nodectl/main/install.sh)"
```

非 root 用户：

```bash
sudo sh -c "$(curl -fsSL https://raw.githubusercontent.com/illria/nodectl/main/install.sh)"
```

支持：

- Debian / Ubuntu
- Alpine Linux
- Linux amd64
- Linux arm64

安装完成后：

- 主程序：`/opt/nodectl/nodectl`
- 数据目录：`/opt/nodectl/data`
- 管理命令：`nt`
- 二进制安装默认 Web 端口：`8080`

常用管理命令：

```bash
nt status
nt start
nt stop
nt restart
```

### Release 包说明

每个正式版本应至少包含以下面板文件：

```text
nodectl-linux-amd64
nodectl-linux-arm64
nodectl-windows-amd64.exe
nodectl-windows-arm64.exe
```

Agent 包：

```text
nodectl-agent-linux-amd64-v0.2.77
nodectl-agent-linux-amd64-v0.2.77.sha256
nodectl-agent-linux-arm64-v0.2.77
nodectl-agent-linux-arm64-v0.2.77.sha256
```

所有正式包统一从这里获取：

<https://github.com/illria/nodectl/releases/latest>

### Docker 升级

数据目录使用 `/opt/nodectl/data` 持久化时，可以直接替换容器，原有数据库和配置不会丢失：

```bash
docker pull ghcr.io/illria/nodectl:v0.4.76-custom.4 && \
docker stop nodectl && \
docker rm nodectl && \
docker run -d \
  --name nodectl \
  --restart unless-stopped \
  -p 7878:8080 \
  --log-opt max-size=10m \
  --log-opt max-file=2 \
  -v /opt/nodectl/data:/app/data \
  ghcr.io/illria/nodectl:v0.4.76-custom.4
```

### 镜像 / Release 对应关系

| 用途 | 地址 |
|---|---|
| 当前准备 Docker | `ghcr.io/illria/nodectl:v0.4.76-custom.4` |
| 跟随 main | `ghcr.io/illria/nodectl:latest` |
| 自定义通道 | `ghcr.io/illria/nodectl:custom` |
| GitHub Release | `https://github.com/illria/nodectl/releases/latest` |
| 安装脚本 | `https://raw.githubusercontent.com/illria/nodectl/main/install.sh` |

---

## 🗺️ 未来规划

NodeCtl 还在持续进化中，以下是已完成和计划中的功能：

### ✅ 已完成功能

**节点管理**
- ✅ 一键部署 Agent，自动回传节点信息
- ✅ 支持主流代理协议（VMess、VLESS、Trojan、Shadowsocks、Hysteria2、Tuic 、AnyTls及变种等）
- ✅ 节点实时状态监控与流量统计
- ✅ 远程重置链接 远程添加删除协议
- ✅ 节点离线 Telegram 告警通知
- ✅ 流量阈值告警与自动剔除
- ✅ 个性化端口协议端口设置

**机场订阅**
- ✅ 多格式机场订阅导入（Clash YAML、Base64）
- ✅ 智能过滤无效占位节点
- ✅ 内置 Mihomo 核心批量测速
- ✅ 测速完成 Telegram 通知
- ✅ 机场节点分配（直连/落地/禁用）

**Cloudflare 集成**
- ✅ CF Tunnel 隧道一键部署
- ✅ 节点 Argo 一键开启
- ✅ CF API 自动签发/续签 SSL 证书
- ✅ HTTP/HTTPS 热重载切换
- ✅ CF 优选 IP 集成

**订阅分发**
- ✅ 多格式订阅链接生成（Clash Meta、V2Ray、Sing-box）
- ✅ 可视化分流规则编辑
- ✅ 预置分流模块（AI、Apple、Microsoft、Telegram、YouTube 等）
- ✅ 自定义代理组与规则

### 🚧 计划中功能

- [ ] CF Worker 节点加入
- [ ] CF Warp 落地
- [ ] 更个性化的订阅节点名称生成
- [ ] 为不同区域节点提供不同区域的优选IP
- [ ] 增加单节点临时测试
- [ ] 提取机场节点链接的快捷复制


### 🚧 考虑中的功能



### 🚧 暂不考虑引入

- [x] 是否引入延迟监控
- [x] 是否引入探针面板


---

## 🌟 写在最后

NodeCtl 的定位很明确——**为个人用户提供一站式节点管理体验**。它不会做多用户、多订阅这些复杂功能，而是专注于让你用最简单的方式管理自己的代理服务。

代码完全开源，欢迎 Star ⭐ 和 PR！

![效果展示](https://nodectl-ipopt.hobin.net/Image/主页1.webp)
![效果展示](https://nodectl-ipopt.hobin.net/Image/主页2.webp)

---

## 🙏 致谢

感谢以下优秀的开源项目：

- [Mihomo 内核](https://github.com/MetaCubeX/mihomo)
- [CloudflareSpeedTest](https://github.com/XIU2/CloudflareSpeedTest)
- [MetaCubeX/meta-rules-dat](https://github.com/MetaCubeX/meta-rules-dat) - GeoIP/Geosite 规则集
- [sing-box 内核](https://github.com/SagerNet/sing-box)

以及其他未一一列举的开源项目，正是开源社区的无私贡献，让 NodeCtl 得以实现更多功能。

---

> ⚠️ 请在遵守所在地法律法规的前提下使用本项目，作者不对任何滥用行为承担责任。
