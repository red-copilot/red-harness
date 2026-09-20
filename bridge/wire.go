// Package bridge 是 TSecBench 平台的 Go 侧客户端：它监管一个常驻 Python
// 子进程（bridge.py），用 JSONL/stdio 把六个平台操作送进去、把结构化结果收回来。
//
// 为什么是「Python 子进程」而不是纯 Go HTTP 客户端：官方 SDK 的契约（数据类
// 字段、异常分类、VPN 预检语义、鉴权头拼写）是唯一真源，而 SDK 只有 Python
// 实现。Go 侧重写一遍 HTTP 调用就意味着第二份契约实现——两份实现之间的漂移
// 无人发现（前身 `dag/store.go` 的 `FlagFingerprint` 就是这么分叉出来的）。
//
// 本文件只放**协议结构**：请求/响应/错误的线格式，以及错误分类与可重试性。
// 进程监管在 proc.go，端口适配在 client.go。
package bridge

import (
	"bytes"
	"encoding/json"
	"time"
)

// 六个命令名。**它是协议的一部分**（PLAN.md:43 固定为
// `check_vpn/list/start/hint/submit/close`），与 bridge.py 的分发表逐字对应。
// 改这里必须同时改 bridge.py，否则桥会回 unknown_cmd。
const (
	CmdCheckVPN = "check_vpn"
	CmdList     = "list"
	CmdStart    = "start"
	CmdHint     = "hint"
	CmdSubmit   = "submit"
	CmdClose    = "close"
)

// request 是发往 bridge.py 的一行请求。
//
// Deadline 用 RFC3339 字符串而不是「超时秒数」：桥侧与 Go 侧可能不在同一台
// 机器（本机真跑的唯一路径是 `docker exec`，见 ClientConfig.Command），
// 时钟偏移下「还剩 12 秒」是错的，「不晚于 18:00:00Z」才有意义。
//
// ⚠️ Flag 会经这一行走 stdio。它**不得**出现在任何日志里——proc.go 只把
// stderr 收进 private/，stdout 只走协议。
type request struct {
	ID       string `json:"id"`
	Cmd      string `json:"cmd"`
	Code     string `json:"code,omitempty"`
	Flag     string `json:"flag,omitempty"`
	Deadline string `json:"deadline,omitempty"`
}

// response 是 bridge.py 回来的一行。
type response struct {
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireError      `json:"error,omitempty"`
}

// wireError 是桥的结构化错误。字段名与 bridge.py 的 `_error()` 逐字对应。
type wireError struct {
	Code       string `json:"code"`
	Class      string `json:"class"`
	Retryable  bool   `json:"retryable"`
	Message    string `json:"message"`
	StatusCode int    `json:"statusCode,omitempty"`
}

// ── result 载荷 ──
//
// 字段名**照抄 SDK 数据类**（`models.py` 已逐字核对，v0.1.2），不做重命名：
// 桥的职责是转发，改名会制造第二个命名口径。Go 侧要用的字段由 client.go
// 映射成 harness 的 Challenge/StartResult/…

// challengeWire 对应 `Challenge`（models.py:13）。`SubmitResult` 的对应物
// 不在这里——见 client.go 的说明。
type challengeWire struct {
	UniqueCode       string   `json:"unique_code"`
	Difficulty       string   `json:"difficulty"`
	Level            int      `json:"level"`
	TotalScore       int      `json:"total_score"`
	FlagCount        int      `json:"flag_count"`
	CorrectFlagCount int      `json:"correct_flag_count"`
	IsCompleted      bool     `json:"is_completed"`
	Description      string   `json:"description"`
	ContainerStatus  string   `json:"container_status"`
	ContainerAddr    []string `json:"container_addr"`
}

// startWire 对应 `StartResult`（models.py:45）。
type startWire struct {
	UniqueCode    string   `json:"unique_code"`
	ContainerAddr []string `json:"container_addr"`
}

// hintWire 对应 `HintResult`（models.py:60）。Hint 是 `Optional[str]`，
// 平台没给提示时是 null——所以它必须是 *string，否则 null 与空串不可区分。
type hintWire struct {
	UniqueCode string  `json:"unique_code"`
	Hint       *string `json:"hint"`
}

// submitWire 对应 `SubmitResult`（models.py:75）。**没有 unique_code 字段**，
// 所以桥只能把请求里带的 code 回填给它（见 bridge.py 的 `_cmd_submit`）。
//
// `matched_flag_index` 是 `Optional[int]`：平台判错时为 null，用 *int 表示。
type submitWire struct {
	UniqueCode       string `json:"unique_code"`
	Correct          bool   `json:"correct"`
	Awarded          int    `json:"awarded"`
	CumulativeScore  int    `json:"cumulative_score"`
	CorrectFlagCount int    `json:"correct_flag_count"`
	TotalFlagCount   int    `json:"total_flag_count"`
	MatchedFlagIndex *int   `json:"matched_flag_index"`
	// Duplicate 为真表示平台回的是幂等命中（`DuplicateSubmit`，code=`duplicate`）。
	// **它不是错误**：重复提交同一个 flag 等价于「这个 flag 已被确认」。
	Duplicate bool `json:"duplicate"`
}

// closeWire 对应 `CloseResult`（models.py:98）。
type closeWire struct {
	UniqueCode string `json:"unique_code"`
	Closed     bool   `json:"closed"`
}

// vpnWire 是 `check_vpn` 的结果。SDK 的 `VpnCheckResult`（models.py:117）
// 不带 `ok` 之外的判据，桥额外回一个 `vpn` 布尔以便 doctor 侧无需解析语义。
type vpnWire struct {
	OK       bool   `json:"ok"`
	Status   string `json:"status"`
	ClientIP string `json:"client_ip"`
	Time     string `json:"time"`
}

// ── 错误分类 ──

// 桥侧错误码。与 `errors.py` 的类属性逐字对应（已对源码核对），外加桥自己
// 造的两个：`connection_error` 是 `TSecConnectionError`（它 `code is None`，
// 直接用 None 做 map 键会与「没有 code 的通用错误」撞车）的稳定别名；
// `invalid_response` 覆盖 SDK 的**两个源码级缺陷**——畸形载荷抛的裸
// `KeyError` 与 2xx 非 JSON 抛的 `json.JSONDecodeError`，两者都逃出了 SDK
// 自己的异常网，桥必须给它们一个稳定的码。
const (
	CodeVPNCheckFailed     = "vpn_check_failed"
	CodeTaskNotFound       = "task_not_found"
	CodeChallengeNotFound  = "challenge_not_found"
	CodeInvalidState       = "invalid_state"
	CodeDuplicate          = "duplicate"
	CodeResourceUnavail    = "resource_unavailable"
	CodeInternalError      = "internal_error"
	CodeValidationError    = "validation_error"
	CodeConnectionError    = "connection_error"
	CodeInvalidResponse    = "invalid_response"
	CodeSDKMissing         = "sdk_missing"
	CodeProtocolError      = "protocol_error"
	CodeUnknownCommand     = "unknown_command"
	CodeMissingCredential  = "missing_credential"
	CodeDeadlineExceeded   = "deadline_exceeded"
	CodeSubprocessExit     = "subprocess_exit"
	CodeInvalidRequest     = "invalid_request"
	CodeCanceledByCaller   = "canceled"
	CodeChallengeUnavailab = "container_unavailable"
)

// 错误「类」。**故意粗于 code**：调用方（引擎/doctor）只需要知道该找谁，
// 精确诊断看 code 与 message。与 harness.Kind 的取值刻意同名，方便装配层
// 直接映射而无需一张翻译表。
const (
	ClassPlatform = "platform"
	ClassConfig   = "config"
	ClassInternal = "internal"
	ClassCanceled = "cancelled"
)

// retryableCodes 是**平台错误码 → 可重试性**的静态表。
//
// 可重试 = 「同样的输入稍后重试可能有不同结果」。这里逐条给出理由，因为
// 前身反复踩的坑正是「用消息字符串匹配来判断该不该重试」：
//
//   - task_not_found(404)：token 无效/缺失/未知 ⇒ 重试无意义。
//   - challenge_not_found(404)：code 不在本任务的题目集里 ⇒ 重试无意义。
//   - invalid_state(409)：**两种语义混在一个码里**，所以它不在这张表里，
//     由 client.go 按消息细分（见 invalidStateRetryable）。
//   - duplicate(409)：幂等命中，桥已映射成成功，正常不会走到这里。
//   - resource_unavailable(503)：靶场实例没就绪 / 池子空了 ⇒ 稍后重试有意义。
//   - internal_error(500)：平台内部故障 ⇒ 稍后重试有意义。
//   - validation_error(422)：请求参数不合法 ⇒ 重试无意义（改了参数才可能过）。
var retryableCodes = map[string]bool{
	CodeTaskNotFound:      false,
	CodeChallengeNotFound: false,
	CodeDuplicate:         false,
	CodeResourceUnavail:   true,
	CodeInternalError:     true,
	CodeValidationError:   false,
}

// invalidStateRetryable 区分 `InvalidState` 的两种语义。
//
// SDK_API.md:209-213 给出的判据是消息里的 `max active`：平台限制同时活跃的
// 容器数，撞上限时**释放一个名额再重试**是正确动作；而「任务已结束（超时）」
// 重试多少次都不会变。桥把这个判定放在 **Python 侧**（那里能看到异常对象的
// 原文与 `detail`），Go 侧只消费 `retryable` 字段——两边都判一次就会有第二
// 个口径。
//
// 这里保留一份 Go 侧实现是为了**自测**：测试用它断言 mock SDK 回的 retryable
// 与规范一致，而不是只看桥自己说了什么。
func invalidStateRetryable(message string) bool {
	return bytes.Contains([]byte(message), []byte("max active"))
}

// deadlineFor 把绝对时刻渲染成 RFC3339（秒级）。
//
// 截断到秒是**有意的**：桥侧的 deadline 只是「不要开始一个新的平台调用」的
// 粗略闸门，真正的超时由 Go 侧按纳秒判定（proc.go 的计时器）。秒级截断让
// 两边对同一个字符串的理解完全一致，不受亚秒精度与时钟格式差异影响。
func deadlineFor(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
