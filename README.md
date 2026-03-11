# NodeCtl

> 轻量、高效的个人节点与订阅管理面板。如果你有托管到cf的域名和一台无论有没有公网的服务器，你都可以体验完整的nodectl。

欢迎加入nodectl 的tg频道 https://t.me/nodectl

## 📖 简介

NodeCtl 提供简洁直观的 Web 界面与稳定的 Golang 后台，采用非内嵌调用二进制形式支持多种开源工具帮助你集中管理**自建节点**和**机场订阅**以及**CF相关功能**。同时兼顾易用性与高度可定制的分流组网能力。

## ✨ 核心功能

### 🚀 自建节点管理
- **一键安装**：在 VPS 执行一行命令即可完成部署并自动回传链接至面板
- **远程控制**：支持面板端远程重置链接、重装 Sing-box、命令执行实时流式输出
- **流量监控**：自动采集上传/下载流量、限额与重置日，实时掌握节点用量
- **在线状态**：节点离线自动推送 Telegram 告警通知
- **协议支持**：内置 Sing-box 运行环境，下发模板高度可自定义

### 🛩️ 机场订阅聚合
- **多格式导入**：支持 Clash YAML、Base64 等格式一键导入并自动解析
- **无效节点过滤**：智能识别并过滤"到期提醒"、"剩余流量"等占位节点
- **批量测速**：内置 Mihomo (Clash Meta) 核心，支持并发真连接测速，SSE 实时推流结果
- **高级路由调度**：可将机场节点分配为"直连"、"落地"或"禁用"加入自建订阅


### ☁️ Cloudflare 集成
- **内置tunnel隧道**：通过CF token，实现傻瓜式一键启用tunnel隧道，不再需要你做任何配置。
- **节点使用tunnel隧道**：节点启用tunnel隧道，无需任何操作，你只需要知道开关在哪。
- **自动签发 SSL**：通过 Cloudflare API 自动签发与续签 SSL 证书，支持热重载无重启切换 HTTP/HTTPS

还未完成，但计划更新 argox warp worker等


### 🛡️ 订阅配置分发
- **多端订阅**：一键生成 Clash (Meta)、V2Ray/Sing-box (Base64) 等格式订阅链接
- **自定义分流**：可视化编辑直连/代理规则组，集成 GeoIP / Geosite 数据，智能国内外分流

### ⚙️ 系统与架构
- **零依赖**：纯 Golang 后端，嵌入式 SQLite (GORM)，无需额外安装任何依赖
- **数据库管理**：对喜欢统计的朋友，增加了psql数据库的支持，可以支持记录任意市场的任意数量节点的流量使用记录。
- **安全认证**：JWT 令牌鉴权 + 登录 IP 限流策略
- **系统看板**：实时监控 CPU、内存、Go 协程数及子进程长期运行状态

---

## 🗺️ 更新计划

- [ ] CF Worker 节点一键部署
- [ ] CF ArgoX 隧道一键
- [ ] 自动优选 IP

## 🚀 快速部署

默认账号admin
默认密码admin

推荐使用 Docker 部署：

### Docker Run

```bash
docker run -d \
  --name nodectl \
  --restart unless-stopped \
  -p 7878:8080 \
  -v /opt/nodectl/data:/app/data \
  ghcr.io/hobin66/nodectl:latest
```

部署成功后访问 `http://你的IP:7878` 即可进入控制台。

### Docker Compose

```yaml
version: '3'
services:
  nodectl:
    image: ghcr.io/hobin66/nodectl:latest
    container_name: nodectl
    restart: unless-stopped
    ports:
      - "7878:8080"
    volumes:
      - ./data:/app/data
```

运行 `docker-compose up -d` 启动。

---

## 💡 高级设置

### 模板与脚本覆盖

内置安装脚本位于源码 `internal/service/singbox.tpl` 等处。如需自定义，在宿主机 `./data/debug/` 目录下放置同名文件，程序启动时会优先加载该目录下的文件覆盖内置模板。

---

## 🤝 声明

1. 代码与构建流程完全开源于 GitHub，欢迎 Review 与 PR。
2. 请在遵守所在地法律法规的前提下使用本项目，作者不对任何滥用行为承担责任。