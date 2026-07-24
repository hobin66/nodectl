// 路径: internal/agent/reporter.go
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"sync"
	"time"

	"nodectl/internal/agent/reporter"

	"nhooyr.io/websocket"
)

// TrafficMessage WebSocket 上报消息结构（动态 JSON）
type TrafficMessage struct {
	InstallID string `json:"install_id"`
	TS        int64  `json:"ts"`
	RXRateBps int64  `json:"rx_rate_bps"`
	TXRateBps int64  `json:"tx_rate_bps"`
	// 新版实时累计字段：每次推送都携带原始计数器与 boot_id
	CounterRXBytes int64  `json:"counter_rx_bytes"`
	CounterTXBytes int64  `json:"counter_tx_bytes"`
	BootID         string `json:"boot_id"`
	AgentVersion   string `json:"agent_version,omitempty"`
}

// ServerCommand 后端下发的命令结构
type ServerCommand struct {
	Type      string          `json:"type"`       // 固定 "command"
	CommandID string          `json:"command_id"` // 幂等键
	Action    string          `json:"action"`     // "reset-links" / "reinstall-singbox" / "tunnel-prepare" / "tunnel-start" / "tunnel-stop" / "check-agent-update"
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// CommandResult Agent 回传命令执行结果
type CommandResult struct {
	Type      string `json:"type"` // "accepted" / "progress" / "result"
	CommandID string `json:"command_id"`
	Status    string `json:"status,omitempty"` // "ok" / "error"
	Message   string `json:"message,omitempty"`
	Stage     string `json:"stage,omitempty"` // 执行阶段描述
}

// CommandHandler 命令处理函数签名
type CommandHandler func(cmd ServerCommand, reply func(CommandResult))

// cmdTask 命令任务（用于 worker 队列）
type cmdTask struct {
	cmd       ServerCommand
	replyFunc func(CommandResult)
}

// msgPeek 轻量消息类型探测结构体（避免 map[string]interface{} 分配）
type msgPeek struct {
	Type string `json:"type"`
}

// bufPool JSON 编码缓冲池（复用 bytes.Buffer，降低 allocs/s）
var bufPool = sync.Pool{
	New: func() interface{} { return new(bytes.Buffer) },
}

// maxPoolBufCap 归还 pool 的 buffer 最大容量，超过则丢弃防止池污染
const maxPoolBufCap = 4096

// Reporter WebSocket 上报器
type Reporter struct {
	mu              sync.Mutex
	cfg             *Config
	conn            *websocket.Conn
	connected       bool
	reconnectCount  int
	maxBackoff      time.Duration
	lastConnectedAt time.Time
	commandHandler  CommandHandler
	readCtxCancel   context.CancelFunc
	// 命令并发限流
	cmdCh chan cmdTask
}

// NewReporter 创建上报器实例
func NewReporter(cfg *Config) *Reporter {
	return &Reporter{
		cfg:        cfg,
		maxBackoff: 60 * time.Second,
		cmdCh:      make(chan cmdTask, 4), // 有界队列，cap=4
	}
}

// SetCommandHandler 注册命令处理器（应在 Connect 前调用）
func (r *Reporter) SetCommandHandler(handler CommandHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commandHandler = handler
}

// Connect 建立 WebSocket 连接（带超时）
func (r *Reporter) Connect(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, r.cfg.WSURL, &websocket.DialOptions{
		Subprotocols: []string{"nodectl-agent"},
	})
	if err != nil {
		r.connected = false
		return fmt.Errorf("WebSocket 连接失败 [%s]: %w", r.cfg.WSURL, err)
	}

	// 关闭旧连接和读协程，防止泄漏
	if r.readCtxCancel != nil {
		r.readCtxCancel()
		r.readCtxCancel = nil
	}
	if r.conn != nil {
		r.conn.Close(websocket.StatusGoingAway, "reconnecting")
		r.conn = nil
	}

	r.conn = conn
	r.connected = true
	r.reconnectCount = 0
	r.lastConnectedAt = time.Now()

	// 启动读协程（接收服务端下发的命令）
	readCtx, readCancel := context.WithCancel(ctx)
	r.readCtxCancel = readCancel

	// 启动命令 worker（2 个固定 worker，防止命令突发 goroutine 爆炸）
	for i := 0; i < 2; i++ {
		go r.cmdWorker(readCtx)
	}

	go r.startReadLoop(readCtx, conn)

	log.Printf("[Agent] WebSocket 已连接: %s", r.cfg.WSURL)
	return nil
}

// cmdWorker 命令执行 worker，从有界 channel 读取任务
func (r *Reporter) cmdWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case task, ok := <-r.cmdCh:
			if !ok {
				return
			}
			r.mu.Lock()
			handler := r.commandHandler
			r.mu.Unlock()
			if handler != nil {
				handler(task.cmd, task.replyFunc)
			}
		}
	}
}

// startReadLoop 读取后端下发的消息（命令）
// 优化：使用轻量 msgPeek 结构体探测 type 字段，避免 map[string]interface{} 堆分配
func (r *Reporter) startReadLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return // 上下文取消，正常退出
			}
			log.Printf("[Agent] WS 读取异常: %v", err)
			r.mu.Lock()
			r.connected = false
			r.mu.Unlock()
			return
		}

		// 轻量探测消息类型（仅解一个字段，零 map 分配）
		var peek msgPeek
		if err := json.Unmarshal(data, &peek); err != nil {
			continue
		}

		if peek.Type != "command" {
			continue
		}

		var cmd ServerCommand
		if err := json.Unmarshal(data, &cmd); err != nil {
			log.Printf("[Agent] 命令解析失败: %v", err)
			continue
		}

		log.Printf("[Agent] 收到命令: action=%s, command_id=%s", cmd.Action, cmd.CommandID)

		r.mu.Lock()
		handler := r.commandHandler
		r.mu.Unlock()

		if handler == nil {
			log.Printf("[Agent] 未注册命令处理器，忽略命令 %s", cmd.CommandID)
			continue
		}

		// 创建回复函数
		replyFunc := func(result CommandResult) {
			result.CommandID = cmd.CommandID
			r.sendResult(ctx, result)
		}

		// 通过有界 channel 分发到 worker，队列满时拒绝
		select {
		case r.cmdCh <- cmdTask{cmd: cmd, replyFunc: replyFunc}:
		default:
			replyFunc(CommandResult{
				Type:    "result",
				Status:  "error",
				Message: "命令队列已满，请稍后重试",
			})
		}
	}
}

// sendResult 发送命令结果回服务端
func (r *Reporter) sendResult(ctx context.Context, result CommandResult) {
	r.mu.Lock()
	conn := r.conn
	connected := r.connected
	r.mu.Unlock()

	if !connected || conn == nil {
		log.Printf("[Agent] 无法回传命令结果，连接已断开: %s", result.CommandID)
		return
	}

	data, err := json.Marshal(result)
	if err != nil {
		log.Printf("[Agent] 命令结果序列化失败: %v", err)
		return
	}

	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := conn.Write(writeCtx, websocket.MessageText, data); err != nil {
		log.Printf("[Agent] 命令结果发送失败: %v", err)
	}
}

// SendLiveMessage 发送实时流量消息（速率 + 原始计数器）
func (r *Reporter) SendLiveMessage(ctx context.Context, rxRate, txRate, counterRX, counterTX int64, bootID string) error {
	msg := TrafficMessage{
		InstallID:      r.cfg.InstallID,
		TS:             time.Now().Unix(),
		RXRateBps:      rxRate,
		TXRateBps:      txRate,
		CounterRXBytes: counterRX,
		CounterTXBytes: counterTX,
		BootID:         bootID,
		AgentVersion:   AgentVersion,
	}
	return r.sendTrafficJSON(ctx, msg)
}

// SendMessage 🆕 发送通用消息（用于新增的消息类型：node_online / links_update 等）
// 通过统一的 WebSocket 连接复用传输
func (r *Reporter) SendMessage(ctx context.Context, msg *reporter.Message) error {
	r.mu.Lock()
	conn := r.conn
	connected := r.connected
	r.mu.Unlock()

	if !connected || conn == nil {
		return fmt.Errorf("WebSocket 未连接")
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("序列化消息失败: %w", err)
	}

	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := conn.Write(writeCtx, websocket.MessageText, data); err != nil {
		r.mu.Lock()
		r.connected = false
		r.mu.Unlock()
		return fmt.Errorf("WebSocket 写入失败: %w", err)
	}

	return nil
}

// sendTrafficJSON 内部方法：序列化并发送流量 JSON 消息（保持原有 flat 结构兼容）
// 优化：使用 sync.Pool 复用 bytes.Buffer，降低 allocs/s
func (r *Reporter) sendTrafficJSON(ctx context.Context, msg TrafficMessage) error {
	r.mu.Lock()
	conn := r.conn
	connected := r.connected
	r.mu.Unlock()

	if !connected || conn == nil {
		return fmt.Errorf("WebSocket 未连接")
	}

	// 从池中获取 buffer
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()

	err := json.NewEncoder(buf).Encode(msg)
	if err != nil {
		r.putBuf(buf)
		return fmt.Errorf("序列化消息失败: %w", err)
	}

	// Encode 会追加 '\n'，去掉末尾换行
	payload := buf.Bytes()
	if len(payload) > 0 && payload[len(payload)-1] == '\n' {
		payload = payload[:len(payload)-1]
	}

	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := conn.Write(writeCtx, websocket.MessageText, payload); err != nil {
		r.putBuf(buf)
		r.mu.Lock()
		r.connected = false
		r.mu.Unlock()
		return fmt.Errorf("WebSocket 写入失败: %w", err)
	}

	r.putBuf(buf)
	return nil
}

// putBuf 归还 buffer 到池，超过容量上限则丢弃防止池污染
func (r *Reporter) putBuf(buf *bytes.Buffer) {
	if buf.Cap() <= maxPoolBufCap {
		bufPool.Put(buf)
	}
	// 超过上限直接丢弃，让 GC 回收
}

// Close 关闭 WebSocket 连接
func (r *Reporter) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.readCtxCancel != nil {
		r.readCtxCancel()
		r.readCtxCancel = nil
	}
	if r.conn != nil {
		r.conn.Close(websocket.StatusNormalClosure, "agent shutting down")
		r.conn = nil
		r.connected = false
	}
}

// IsConnected 检查连接状态
func (r *Reporter) IsConnected() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.connected
}

// ReconnectWithBackoff 指数退避重连
// 退避策略：1s → 2s → 4s → ... 上限 60s + 随机抖动
// 优化：使用 time.NewTimer 替代 time.After，上下文取消时立即释放 timer
func (r *Reporter) ReconnectWithBackoff(ctx context.Context) error {
	r.mu.Lock()
	r.reconnectCount++
	count := r.reconnectCount
	r.mu.Unlock()

	backoff := reconnectBackoff(count, r.maxBackoff)
	// 添加随机抖动 (0-25% 的退避时间)。退避时间很小时不调用 rand.N，
	// 避免把 0 作为上界传入而触发 panic。
	if jitterLimit := backoff / 4; jitterLimit > 0 {
		backoff += rand.N(jitterLimit)
	}

	log.Printf("[Agent] 第 %d 次重连，等待 %v ...", count, backoff)

	// 使用 NewTimer 替代 After，确保上下文取消时 timer 被回收
	timer := time.NewTimer(backoff)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}

	return r.Connect(ctx)
}

// reconnectBackoff 计算指数退避时间。
//
// 不能先计算 1<<count 再转换为 time.Duration：重连次数较大时，
// 纳秒值会在乘以 time.Second 前溢出为负数，最终导致 rand.N 触发
// "invalid argument to N" 并使整个 agent panic。
func reconnectBackoff(count int, maxBackoff time.Duration) time.Duration {
	if maxBackoff <= 0 {
		return 0
	}
	if count < 1 {
		count = 1
	}

	backoff := time.Second
	for i := 1; i < count; i++ {
		// 在翻倍可能超过上限前直接封顶，避免任何 duration 溢出。
		if backoff >= maxBackoff || backoff > maxBackoff/2 {
			return maxBackoff
		}
		backoff *= 2
	}
	if backoff > maxBackoff {
		return maxBackoff
	}
	return backoff
}
