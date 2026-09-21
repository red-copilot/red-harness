package harness

import (
	"errors"
	"fmt"
)

// Kind 是错误分类。调用方据此决定重试、降级还是停机——**不要用字符串匹配
// 错误消息来判断**，那正是前身反复踩的坑。
type Kind string

const (
	// KindConfig：调用方配置错了（缺端口、摘要漂移、schema 不支持）。重试无意义。
	KindConfig Kind = "config"
	// KindScope：超出授权范围（目标不在白名单、地址不可达于授权范围）。
	KindScope Kind = "scope"
	// KindPlatform：平台侧问题（VPN、题目状态、提交被拒）。
	KindPlatform Kind = "platform"
	// KindProvider：模型 provider 故障。前身「280 run / 0 flag / 63 题」事故的类别。
	KindProvider Kind = "provider"
	// KindExecutor：容器/执行器故障（镜像缺失、资源限制命中、容器死了）。
	KindExecutor Kind = "executor"
	// KindBudget：预算耗尽。
	KindBudget Kind = "budget"
	// KindPersistence：落盘/读盘故障。**这类错误绝不能被吞**——吞掉意味着
	// 调用方以为进展保住了。
	KindPersistence Kind = "persistence"
	// KindCancelled：ctx 取消或用户主动取消。
	KindCancelled Kind = "cancelled"
)

// Error 是统一错误。它携带足够的结构化信息让调用方做决定，而不必解析消息。
type Error struct {
	Kind Kind `json:"kind"`
	// Op 是出错的操作（"platform.submit" / "store.append" / "engine.resume"）。
	Op string `json:"op,omitempty"`
	// Retryable 为真表示同样的输入重试可能有不同结果（网络抖动、平台忙）。
	Retryable bool  `json:"retryable"`
	RunID     RunID `json:"runId,omitempty"`
	// Msg 是给人看的消息。**不得包含候选明文或凭据。**
	Msg string `json:"message,omitempty"`
	// Err 是被包裹的底层错误，不参与 JSON 序列化（它可能带敏感上下文）。
	Err error `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b string
	if e.Op != "" {
		b = e.Op + ": "
	}
	b += string(e.Kind)
	if e.Msg != "" {
		b += ": " + e.Msg
	}
	if e.Err != nil {
		b += ": " + e.Err.Error()
	}
	if e.RunID != "" {
		b += fmt.Sprintf(" (run=%s)", e.RunID)
	}
	return b
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// E 构造一个 *Error。err 为 nil 时返回 nil——这样 `return E(...)` 可以出现在
// 任何地方而不必先判空。
//
// **Msg 刻意留空，不从 err 拷贝文本。** Error 会被序列化进 JSON（看板、日志、
// 报告），而底层错误可能带敏感上下文（平台响应体、路径、token）。要给人看的
// 安全消息由调用方显式设置 Msg，或者直接用 &Error{...} 构造。
func E(kind Kind, op string, err error) *Error {
	if err == nil {
		return nil
	}
	return &Error{Kind: kind, Op: op, Err: err}
}

// Ef 构造一个带安全消息的 *Error。msg 会进 JSON，所以**不得包含候选明文或
// 凭据**；敏感细节留给 err（它不参与序列化）。
func Ef(kind Kind, op, msg string, err error) *Error {
	return &Error{Kind: kind, Op: op, Msg: msg, Err: err}
}

// Retryable 报告 err 链上是否有可重试的错误。
func Retryable(err error) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.Retryable
}

// IsKind 报告 err 链上是否有指定分类的错误。
//
// 用 errors.As 而不是类型断言：引擎会在各层包裹错误（例如
// `fmt.Errorf("起题失败: %w", err)`），只看最外层会漏。
func IsKind(err error, k Kind) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.Kind == k
}

// KindOf 返回 err 链上的错误分类；链上没有 *Error 时返回 ok=false。
//
// 为什么需要一个「取分类」而不是「判分类」的函数：**落盘与报告需要分类本身**。
// 公开结果里只能放可比较的枚举值，而 `%T` 折出来的类型名（`*harness.Error`）
// 对通过率结论毫无用处——provider 故障与执行器故障会变成同一个串，而这两类
// 正是必须分开统计的。分类是枚举，它**没有明文**，可以安全进公开结果。
//
// 返回 (Kind, true) 时 Kind 一定非空：空 Kind 与「没有分类」同形，让调用方
// 拿到空串再自己判断，等于把「没分类」这条信息藏进一个合法值里。
func KindOf(err error) (Kind, bool) {
	e, ok := errors.AsType[*Error](err)
	if !ok || e.Kind == "" {
		return "", false
	}
	return e.Kind, true
}
