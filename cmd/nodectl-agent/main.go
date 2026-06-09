// 路径: cmd/nodectl-agent/main.go
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"

	"nodectl/internal/agent"
)

func main() {
	configPath := flag.String("config", agent.DefaultConfigPath, "配置文件路径")
	showVersion := flag.Bool("version", false, "显示版本号")
	flag.Parse()

	if *showVersion {
		fmt.Printf("nodectl-agent %s (commit=%s, built=%s)\n",
			agent.AgentVersion, agent.GitCommit, agent.BuildTime)
		os.Exit(0)
	}

	log.SetFlags(log.Ldate | log.Ltime | log.Lmsgprefix)
	log.SetPrefix("")

	// 统一日志：同时写入 /var/log/nodectl-agent.log 和 stdout
	// 所有系统（Alpine/Debian/CentOS 等）均可通过 tail -f /var/log/nodectl-agent.log 查看
	agentLogPath := "/var/log/nodectl-agent.log"
	if dir := filepath.Dir(agentLogPath); dir != "" {
		os.MkdirAll(dir, 0755)
	}
	if lf, err := os.OpenFile(agentLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		multiWriter := io.MultiWriter(os.Stdout, lf)
		log.SetOutput(multiWriter)
	} else {
		log.Printf("[Agent] 无法打开日志文件 %s: %v (仅输出到 stdout)", agentLogPath, err)
	}

	// 设置 GOMEMLIMIT（如果环境变量未覆盖，则使用 5 MiB 软上限）
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(5 << 20) // 5 MiB
	}

	// 设置 GOGC（如果环境变量未覆盖，则使用更激进的 10）
	// 说明：更小的 GOGC 会更频繁 GC，以换取更低内存占用。
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(10)
	}

	// 初始化自动更新器 + 崩溃循环检测
	updater, err := agent.NewUpdater()
	if err != nil {
		log.Printf("[Agent] 初始化更新器失败 (将禁用自动更新): %v", err)
	}
	if updater != nil {
		if needRestart := updater.RecordStartup(); needRestart {
			// 崩溃次数已达上限，RecordStartup 已将旧版本还原到 selfPath。
			// 优先通过 ReexecSelf() 就地加载旧二进制（同 PID，无需 systemd 重启）；
			// 若 execve 失败则 ReexecSelf 内部会调用 os.Exit(1) 由 systemd 兜底。
			log.Printf("[Agent] 连续崩溃已达阈值，已回滚到旧版本，尝试就地加载旧二进制...")
			updater.ReexecSelf() // 不会返回
		}
	}

	// 1. 加载配置
	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("[Agent] 加载配置失败: %v", err)
	}

	// 2. 创建并启动运行时
	rt := agent.NewRuntime(cfg, updater)
	if err := rt.Run(); err != nil {
		log.Fatalf("[Agent] 运行时异常退出: %v", err)
	}
}
