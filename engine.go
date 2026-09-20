package harness

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Engine 是 v0.3 的顶层入口。它取代了 v0.2 的 `Session.Run`。
//
// 与 v0.2 的关键差异：
//   - 状态由引擎独占持有（单写者 runLoop），调用方只能通过 RunHandle 观察与控制。
//   - 运行可以被暂停、恢复、取消；恢复以事件日志重放为基线。
//   - 一次 run 可以跑多道题（`RunSpec.Targets`），而不是一次一道。
type Engine interface {
	Start(ctx context.Context, spec RunSpec) (RunHandle, error)
	Resume(ctx context.Context, id RunID) (RunHandle, error)
	List(ctx context.Context) ([]RunSummary, error)
	Doctor(ctx context.Context) (DoctorReport, error)
}

// Options 是引擎的装配参数。
//
// **必需端口**：Platform、Executor、Agents、Store、Scenarios（至少一项）。
// 其余为 nil 时取默认实现。任何必需端口为 nil ⇒ New 返回 KindConfig 错误。
//
// 为什么必需端口要在构造时校验而不是等到用时：前身事故里「端口没接」的表现
// 是**静默退化**（DAG 零事实、所有包自测全绿）。构造即校验把这类故障提前到
// 启动的第一秒。
type Options struct {
	Platform Platform
	Executor Executor
	Agents   AgentFactory
	Store    Store
	// Scenarios 以名为键。RunSpec.Scenario 必须命中其中一个。
	Scenarios map[string]Scenario

	// Policy 为 nil 时引擎用默认策略：MaxAttemptsPerIntent=3、
	// DryRoundsBeforeHint=3（沿用 dag.DefaultMaxAttempts 与 HintAuto 的阈值）。
	Policy RunPolicy

	// Gate 为 nil 时引擎按每道题的题面形状构造 gate（生产实现见 gate 包）。
	// 可注入是为了让 engine 包的测试能用一个记账型 fake 观察提交行为。
	//
	// ⚠️ 这是**工厂**而不是实例：一道 run 可能跑多道题，而 gate 是每题独立的
	// （v0.2 里 `gate.NewGate(desc)` 就是每题新建）。
	Gate func(ch Challenge) CandidateGate

	// Planner / Renderer 为 nil 时引擎按每道题构造 dag 的实例。
	//
	// ⚠️ 同样是工厂。**默认实现不写在 engine/ 里**——engine 若导入 dag/gate，
	// 就与「每个子包独立并行开发」的编排直接冲突（engine 的开发者会被
	// dag/gate 的编译状态卡住）。默认实现放在装配层 `internal/cli/wire.go`。
	// engine/ 自己只依赖根包契约 + store/，测试用注入的 fake。
	Planner  func(ch Challenge) Planner
	Renderer func(ch Challenge) Renderer

	// OnEvent 是流式事件回调（transcript / 日志）。在 runLoop goroutine 上调用。
	//
	// **不要在这里做 IO，也不要在这里改状态**——它是唯一的消费者路径上的一环，
	// 慢回调会反压整个引擎。
	OnEvent func(Event)

	// Now 可注入时钟（测试用）。nil 时用 time.Now。
	Now func() time.Time
}

// New 校验端口齐备后构造引擎。
//
// **端口校验在根包做**，而不是交给 engine 包：Options 与 MissingPorts 都是
// 根包拥有的契约，校验放在这里意味着任何引擎实现（含测试用的 fake）都不可能
// 绕过它。前身事故里「端口没接」的表现是静默退化（DAG 零事实、所有包自测
// 全绿），所以这条错误必须在启动的第一秒就点名缺哪个端口。
func New(opts Options) (Engine, error) {
	if missing := opts.MissingPorts(); len(missing) > 0 {
		return nil, &Error{
			Kind: KindConfig,
			Op:   "engine.new",
			Msg:  "缺少必需端口: " + strings.Join(missing, ", "),
		}
	}
	return newEngine(opts)
}

// newEngine 由 engine 包通过这个变量注入。
//
// 为什么用注入而不是让根包直接实现：根包必须是**纯契约**（类型 + 接口，零实现）。
// 一旦根包开始实现引擎，它就又变回 v0.2 那个「契约与实现混在一起」的包，
// 而并行化的前提正是拆开它们。
//
// 为什么用变量而不是让根包 import engine：那会成环（engine 依赖根包契约）。
var newEngine = func(opts Options) (Engine, error) {
	return nil, &Error{
		Kind: KindConfig,
		Op:   "engine.new",
		Msg:  "引擎实现未注册（引擎实现由 engine 包在 init 时通过 harness.RegisterEngine 注册）",
	}
}

// RegisterEngine 由 engine 包在 init 中调用，把自己的构造函数交给根包。
//
// 这不是「可选接线点」——CLI 与 example 都会走 harness.New。漏注册的表现是
// 启动第一秒就拿到一个明确的 KindConfig 错误，而不是静默退化。
func RegisterEngine(fn func(Options) (Engine, error)) {
	if fn != nil {
		newEngine = fn
	}
}

// RequiredPorts 列出 Options 里必须非 nil 的字段名，供 New 的错误消息使用。
//
// 导出的目的是让 engine 包的测试可以断言「缺哪个端口」的消息内容，而不必
// 把这份清单抄一遍（抄一遍就会漂移）。
func (o Options) MissingPorts() []string {
	var missing []string
	if o.Platform == nil {
		missing = append(missing, "Platform")
	}
	if o.Executor == nil {
		missing = append(missing, "Executor")
	}
	if o.Agents == nil {
		missing = append(missing, "Agents")
	}
	if o.Store == nil {
		missing = append(missing, "Store")
	}
	if len(o.Scenarios) == 0 {
		missing = append(missing, "Scenarios")
	}
	return missing
}

// ErrNotImplemented 是骨架阶段的占位错误。
//
// 它的存在是为了让「还没实现」与「实现错了」在测试里可区分：CLI 骨架的
// 子命令返回它，T14 把它们逐个换成真实现时，测试会从「断言返回未实现」
// 变成「断言真的跑通了」。
func ErrNotImplemented(what string) error {
	return &Error{Kind: KindConfig, Op: what, Msg: what + " 尚未实现"}
}

// ErrNotFound 报告某个 RunID 不存在。
func ErrNotFound(id RunID) error {
	return &Error{Kind: KindConfig, Op: "engine.resume", RunID: id, Msg: "运行不存在"}
}

// IsTerminal 报告 err 是否表示「对终态 run 调了 Resume」。
func IsTerminal(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == KindConfig && e.Op == "engine.resume" && e.Msg == errTerminalMsg
}

const errTerminalMsg = "运行已处于终态，拒绝恢复"
