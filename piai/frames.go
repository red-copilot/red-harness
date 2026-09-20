package piai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// maxFrameBytes 是单帧上限。M0 实测单帧可达 262 KB（工具 details 原样到达，
// 不截断），旧实现给的 16 MB 够用，这里沿用并写明依据：pi 的 bash 输出本身
// 有 51200*2 字节的截断阈值，加上 details 的任意对象，量级在数百 KB；
// 16 MB 留了两个数量级的余量，同时避免畸形输入把内存吃干。
const maxFrameBytes = 16 << 20

// Command 是出站帧。字段是 RpcCommand 联合类型里我们用到的那些。
//
// 为什么不用 map[string]any：`type` 字段是**必需**的，缺了 pi 会回
// `Unknown command: undefined`（M0 实测，前身踩过）。用结构体 + json tag 让
// 这个字段在编译期就存在，比每处调用点手写 map 更难漏。
type Command struct {
	ID    string `json:"id,omitempty"`
	Type  string `json:"type"`
	Msg   string `json:"message,omitempty"`
	Mode  string `json:"streamingBehavior,omitempty"`
	Level string `json:"level,omitempty"`
}

// CommandType 常量。只列 piai 真正会发的命令，避免「看起来支持但从未测过」。
const (
	CmdPrompt    = "prompt"
	CmdSteer     = "steer"
	CmdFollowUp  = "follow_up"
	CmdAbort     = "abort"
	CmdNewSess   = "new_session"
	CmdGetState  = "get_state"
	CmdGetStats  = "get_session_stats"
	CmdGetCmds   = "get_commands"
	CmdClearQ    = "clear_queue"
	CmdUIResp    = "extension_ui_response"
	CmdThinkLvl  = "set_thinking_level"
	CmdAutoRetry = "set_auto_retry"
)

// Envelope 是**两段式解码的第一段**：只解出 type（以及 response 的 id/command/
// success/error）。先解信封再按 type 解具体结构体，是因为事件体的形状差异极大
// （262 KB 的 details 与 `{"type":"agent_settled"}` 并存），一次解到具体类型会
// 让未知事件直接解码失败——而 pi 的版本漂移是已知风险（发布节奏 1–2 周/版），
// 未知事件必须被忽略而不是让 reader 死掉。
type Envelope struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Command string `json:"command"`
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// Response 是命令应答。Data 保持原始 JSON，由调用方按 command 解到具体结构体
// （第二段）。
type Response struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`
}

// SessionState 是 get_state 的 data。
type SessionState struct {
	SessionID           string `json:"sessionId"`
	SessionFile         string `json:"sessionFile"`
	IsStreaming         bool   `json:"isStreaming"`
	IsCompacting        bool   `json:"isCompacting"`
	MessageCount        int    `json:"messageCount"`
	PendingMessageCount int    `json:"pendingMessageCount"`
	ThinkingLevel       string `json:"thinkingLevel"`
	SteeringMode        string `json:"steeringMode"`
	FollowUpMode        string `json:"followUpMode"`
	AutoCompaction      bool   `json:"autoCompactionEnabled"`
}

// SessionStats 是 get_session_stats 的 data。字段名与 M0 实测一致。
type SessionStats struct {
	SessionFile       string `json:"sessionFile"`
	SessionID         string `json:"sessionId"`
	UserMessages      int    `json:"userMessages"`
	AssistantMessages int    `json:"assistantMessages"`
	ToolCalls         int    `json:"toolCalls"`
	ToolResults       int    `json:"toolResults"`
	TotalMessages     int    `json:"totalMessages"`
	Tokens            struct {
		Input      int `json:"input"`
		Output     int `json:"output"`
		CacheRead  int `json:"cacheRead"`
		CacheWrite int `json:"cacheWrite"`
		Total      int `json:"total"`
	} `json:"tokens"`
	Cost         float64 `json:"cost"`
	ContextUsage struct {
		Tokens        int     `json:"tokens"`
		ContextWindow int     `json:"contextWindow"`
		Percent       float64 `json:"percent"`
	} `json:"contextUsage"`
}

// SlashCommand 是 get_commands 里的一条。它是**最快的 extension 加载证据**：
// 起进程后立刻查，不用等第一轮跑空（M0 结论 #6）。
type SlashCommand struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Path        string `json:"path"`
	// SourceInfo 是 pi 0.86.0 实际返回的形状（rpc-types.d.ts 里 RpcSlashCommand
	// 带 sourceInfo 而不是 path）。文档 §get_commands 的示例还写着 path，两个都
	// 收：版本漂移时至少有一个能命中。
	SourceInfo struct {
		Path string `json:"path"`
	} `json:"sourceInfo"`
}

// NewSessionData 是 new_session 的 data。cancelled=true 表示某个 extension 的
// session_before_switch 处理器取消了这次切换——不能当成功。
type NewSessionData struct {
	Cancelled bool `json:"cancelled"`
}

// ToolEnd 是 tool_execution_end 的第二段解码结果。
type ToolEnd struct {
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	IsError    bool   `json:"isError"`
	Result     struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		// Details 是 report_fact 的结构化载荷通道（M0 实测 262 KB 不截断）。
		// 必须是 map 而不是 json.RawMessage：dag/gate 要按键取值，而且 JSON 对象
		// 里的键顺序不稳定，重新序列化无意义。
		Details map[string]any `json:"details"`
	} `json:"result"`
}

// Text 把 content 块里的 text 拼起来。pi 的 content 是块数组，工具输出的正文
// 在 text 块里；thinking/图片块会被忽略。
func (t ToolEnd) Text() string {
	var b strings.Builder
	for _, c := range t.Result.Content {
		if c.Type == "text" || c.Type == "" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// UIRequest 是 extension_ui_request 的第二段解码。
//
// 它同时是**唯一会挂死整轮**的事件：dialog 类（select/confirm/input/editor）
// 在 pi 侧阻塞等应答，不答就永久挂住，而 pi 不会自己退出（M0）。所以这个结构
// 体里 Method 的取值集合是安全关键路径。
type UIRequest struct {
	ID      string `json:"id"`
	Method  string `json:"method"`
	Title   string `json:"title"`
	Message string `json:"message"`
	Timeout int    `json:"timeout"`
	Options []any  `json:"options"`
	Prefill string `json:"prefill"`
	// RequestID 是文档标的首选键名（`id` 是 legacy 别名）。pi 0.86.0 实际只发
	// `id`（见 rpc-mode.js 的 output({type,id,...request})），应答时两个都带上，
	// 零成本地对冲将来改成 requestId 的版本。
	RequestID string `json:"requestId"`
}

// IsDialog 报告该请求是否阻塞等待应答。
func (u UIRequest) IsDialog() bool {
	switch u.Method {
	case "select", "confirm", "input", "editor":
		return true
	}
	return false
}

// AgentEnd 是 agent_end 的第二段解码。它携带完整 usage 与末条消息的
// stopReason/errorMessage——provider 失败探测就在这里（M0：401 时
// stopReason=="error" + 空 content + 照样 agent_settled，不识别就会以「跑完了
// 但什么都没发生」的形式烧掉整个题库）。
type AgentEnd struct {
	Messages  []Message `json:"messages"`
	WillRetry bool      `json:"willRetry"`
}

// Message 只解出我们需要的字段。
type Message struct {
	Role         string `json:"role"`
	StopReason   string `json:"stopReason"`
	ErrorMessage string `json:"errorMessage"`
	Usage        struct {
		Input      int `json:"input"`
		Output     int `json:"output"`
		CacheRead  int `json:"cacheRead"`
		CacheWrite int `json:"cacheWrite"`
		Total      int `json:"totalTokens"`
		Cost       struct {
			Total float64 `json:"total"`
		} `json:"cost"`
	} `json:"usage"`
}

// TurnEnd 是 turn_end 的第二段解码。
type TurnEnd struct {
	Message Message `json:"message"`
}

// CompactionEnd 是 compaction_end 的第二段解码。result=null 有两种含义：
// aborted=true（被取消）或 errorMessage 非空（失败，例如配额超限）。
type CompactionEnd struct {
	Reason       string `json:"reason"`
	Aborted      bool   `json:"aborted"`
	WillRetry    bool   `json:"willRetry"`
	ErrorMessage string `json:"errorMessage"`
}

// RetryStart / RetryEnd 是 auto_retry_start/end 的第二段解码。
type RetryStart struct {
	Attempt     int    `json:"attempt"`
	MaxAttempts int    `json:"maxAttempts"`
	DelayMs     int    `json:"delayMs"`
	Error       string `json:"errorMessage"`
}

type RetryEnd struct {
	Success    bool   `json:"success"`
	Attempt    int    `json:"attempt"`
	FinalError string `json:"finalError"`
}

// ExtensionError 是 extension_error 的第二段解码。它必须置 RoundResult.Err：
// extension 抛错意味着事实回流通道断了，静默继续会让整轮结果不可解释。
type ExtensionError struct {
	ExtensionPath string `json:"extensionPath"`
	Event         string `json:"event"`
	Error         string `json:"error"`
}

// ToolProgress 是 tool_execution_update 的第二段解码。partialResult 是**累积**
// 输出（不是增量），客户端可以整体替换显示。
type ToolProgress struct {
	ToolCallID    string `json:"toolCallId"`
	ToolName      string `json:"toolName"`
	PartialResult struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"partialResult"`
}

// Delta 是 message_update 的第二段解码。
type Delta struct {
	Usage     json.RawMessage `json:"usage"`
	Assistent struct {
		Type         string `json:"type"`
		ContentIndex int    `json:"contentIndex"`
		Delta        string `json:"delta"`
		Content      string `json:"content"`
		ToolCallID   string `json:"id"`
		ToolName     string `json:"toolName"`
	} `json:"assistantMessageEvent"`
}

// FrameReader 按 **严格 LF 分行**读 JSONL。
//
// 为什么不能用 bufio.Scanner 的默认行为之外的任何「行」概念：rpc.md 明确写了
// 严格 JSONL 语义，LF 是唯一分隔符；Node 的 readline 不合规正是因为它在
// U+2028/U+2029 上也切分，而这两个字符在 JSON 字符串里是合法的。用
// bufio.Scanner 的 ScanLines 语义（只切 \n，去掉尾部 \r）恰好就是协议要求的
// 那一条，且它不会做 Unicode 行切分。
//
// 半行（无结尾换行的最后一段）在 EOF 时由 Scanner 作为最后一行返回——但那说明
// 进程死了，reader 的 EOF 处理会覆盖这条路径。
type FrameReader struct {
	sc *bufio.Scanner
}

func NewFrameReader(r io.Reader) *FrameReader {
	sc := bufio.NewScanner(r)
	// 上限放大：单帧可达 262 KB（M0 实测），16 MB 留两个数量级余量。
	sc.Buffer(make([]byte, 0, 64<<10), maxFrameBytes)
	return &FrameReader{sc: sc}
}

// Next 返回下一条原始帧。ok=false 且 err=nil 表示干净的 EOF。
//
// 空行被**跳过**而不是当成一帧返回：它不是协议里的帧，直接交出去只会让上层解出
// 一个 type="" 的怪东西（`Unknown command: undefined` 那条事故的同款噪音）。
func (f *FrameReader) Next() (line []byte, ok bool, err error) {
	for {
		if !f.sc.Scan() {
			if err := f.sc.Err(); err != nil {
				return nil, false, err
			}
			return nil, false, nil
		}
		// Scanner 已经把尾部 \r 去掉了（ScanLines 的行为），但显式再处理一次，
		// 免得将来有人把它换成别的 reader 时协议悄悄失守。
		line = []byte(strings.TrimSuffix(f.sc.Text(), "\r"))
		if len(line) == 0 {
			continue
		}
		if !utf8.Valid(line) {
			// 非法 UTF-8 说明帧边界已经错了（例如上游写了二进制），继续解下去只会
			// 得到一连串无意义的 parse 失败。返回错误让 reader 走进程失效路径。
			return nil, false, fmt.Errorf("piai: 非法 UTF-8 帧（前 64 字节: %q）", truncate(line, 64))
		}
		return line, true, nil
	}
}

func truncate(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}
