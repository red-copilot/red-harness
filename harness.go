package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

const Version = "0.2.0"

// Budget 是一道题的预算。任一维度耗尽即终止。
type Budget struct {
	// MaxRounds 是意图轮次上限（一轮 = 一个意图）。
	MaxRounds int
	// MaxWall 是墙钟上限。
	MaxWall time.Duration
	// MaxTurns 是工具调用总数上限（跨轮累计），护栏语义沿用前身。
	MaxTurns int
	// MaxCostUSD 是累计成本上限（0 表示不限）。
	MaxCostUSD float64
}

// DefaultBudget 是保守默认值，参考前身 `ADAPTER_*` 的量级。
func DefaultBudget() Budget {
	return Budget{MaxRounds: 40, MaxWall: 30 * time.Minute, MaxTurns: 600}
}

// Exhausted 报告预算是否已耗尽，并给出原因。
func (b Budget) Exhausted(used Budget) (bool, string) {
	if b.MaxRounds > 0 && used.MaxRounds >= b.MaxRounds {
		return true, ReasonMaxTurns
	}
	if b.MaxWall > 0 && used.MaxWall >= b.MaxWall {
		return true, ReasonTimeout
	}
	if b.MaxTurns > 0 && used.MaxTurns >= b.MaxTurns {
		return true, ReasonMaxTurns
	}
	if b.MaxCostUSD > 0 && used.MaxCostUSD >= b.MaxCostUSD {
		return true, ReasonTimeout
	}
	return false, ""
}

// Outcome 是一道题的最终结果。
type Outcome struct {
	Code string
	// Reason 见 Reason* 常量。
	Reason string
	// Flags 是被平台确认正确的答案。
	Flags []string
	// Candidates 是全部候选（含被拒的），用于报告的可解释性。
	Candidates []Candidate
	// Submitted 是**去重后的确认数**（不是提交次数——旧契约这里统计错了）。
	Submitted int
	// Duplicates 是平台幂等命中数（等价于已确认）。
	Duplicates int
	// Rejected 是被平台判错的候选数。
	Rejected int
	// Rounds 是实际消耗的意图轮次。
	Rounds int
	// IntentDone 是达成的意图数。
	IntentDone int
	// Negative 是写入的死胡同（已证伪方向）数。
	Negative int
	// HintUsed 是提示次数（提示会按比例扣分，所以要单列）。
	HintUsed int
	// ProgressConfirmed / ProgressTotal 是**平台权威的**作答进度
	// （来自 SubmitResult 的 correct_flag_count / total_flag_count）。
	//
	// 为什么不用 `len(Flags)` 代替：「通关立即终止」必须能在**不知道 FlagCount**
	// 的情况下也生效——`StartResult` 不携带 FlagCount，调用方也可能没填
	// Session.Challenge。而平台每次 Submit 都会回权威进度，这是唯一不依赖调用方
	// 自觉的判据。前身那条「通关即停」是死代码，事故现场就是这么来的。
	ProgressConfirmed int
	ProgressTotal     int
	// Stats 是 pi 会话的权威计数。
	Stats Stats
	// Solve 是**一次性后端**的结果，只在用 Solver 而非 Agent 时非零。
	// 旧契约在错误路径上也给它赋值，那是缺陷。
	Solve SolveResult
	// Report 是报告文件路径（若已生成）。
	Report string
	// Err 非空表示这道题以异常收场。
	Err string
	// StartedAt / EndedAt 用于报告。
	StartedAt time.Time
	EndedAt   time.Time
}

// Duration 返回耗时。
func (o Outcome) Duration() time.Duration {
	if o.StartedAt.IsZero() || o.EndedAt.IsZero() {
		return 0
	}
	return o.EndedAt.Sub(o.StartedAt)
}

// Solved 报告这道题是否拿到了至少一个被确认的 flag。
func (o Outcome) Solved() bool { return len(o.Flags) > 0 }

// Session 是一道题的运行上下文。
//
// 设计约束（每一条都对应一个已确认缺陷）：
//   - Platform 与 Agent 都是接口，便于 fake 端到端测试。
//   - 提交只发生在轮末（harvest），不在事件回调里——旧契约把提交放进 emit
//     路径，于是每次 tool_execution_end 都重遍历全部候选重复提交。
//   - Outcome.Solve 只在成功路径赋值。
type Session struct {
	Platform Platform
	// Agent 是持久 agent（DAG 轮循环用）。与 Solver 二选一。
	Agent Agent
	// Solver 是一次性后端（降级/对照）。与 Agent 二选一。
	Solver Solver
	Gate   Gate
	// Ledger 记录被平台判错的答案，用于「永不重提」与回指 prompt。
	Ledger RejectedLedger
	// Scheduler 决定下一轮做哪个意图。实现见 dag 包。
	Scheduler Scheduler
	// Renderer 把当前状态渲染成本轮 prompt。实现见 dag 包。
	Renderer Renderer
	// Observer 接收归一化事件，供有状态的消费者使用——主要是 DAG：它从
	// tool_execution_end 里抽事实入图。实现见 dag 包。
	Observer Observer
	// Ingest 把**轮内**的事件喂给 DAG 的事实抽取通道（实现是
	// `dag.Scheduler.Ingest`；调用方包一层闭包把轮号绑上）。
	//
	// 为什么必须有这个接线点（核验实测的后果）：不接它，DAG 里除平台种子外
	// **零事实**——图永远不会因为学到东西而解锁新阶段，7 个阶段里只有前 4 个
	// 曾经可达，其余永远 pending。「DAG 驱动」会静默退化成「一条写死的阶段链
	// 跑满预算」，而所有包的自测**全绿**，离线完全看不出来。
	//
	// 用函数类型而不是接口，是为了让根包不依赖 dag（dag 依赖根包拿契约，反向
	// 依赖会成环）；同时避开「接口方法签名必须逐字相同」的匹配陷阱——`Ingest`
	// 有返回值，写成接口断言很容易因为签名差一点而静默不生效。
	Ingest func(ev Event, round int)
	// IntentSink 接收「本轮正在执行哪个意图」，供 gate 回填候选的推导链锚点
	// （实现是 `gate.Gate.SetIntent`）。
	//
	// 为什么必须有：不接它，所有候选的 `IntentID` 为空、`Round` 为 0——
	// gate 的族别判定不依赖它（所以不会报错），但「哪个意图产出了这个答案」
	// 这条审计链断掉，洗白路径的来源追溯无从谈起。
	IntentSink func(intentID string, round int)
	// Saver 在每轮末把 DAG 落盘（实现是 `dag.Graph.Save`）。为 nil 则不落盘。
	//
	// 落盘由**轮循环**驱动、每轮一次，而不是由 Observer 自己在 Observe 里顺手
	// 做——Observe 每个事件都会被调用（一次工具调用几十个事件），在那里落盘等于
	// 每轮写几十次完整图，而且发生在 reader 协程里，会阻塞事件消费、进而把 pi
	// 的 stdout 管道填满。
	Saver Saver
	// GraphPath 是 DAG 的落盘路径。Saver 非 nil 且此路径非空时才落盘。
	GraphPath string
	// Budget 是预算。
	Budget  Budget
	Workdir string
	// Challenge 是这道题的完整信息（FlagCount / Category / FlagFormat）。
	//
	// 为什么必须有这个入口：`StartResult` 只携带地址与题面，**不携带 FlagCount
	// 与 Category**。而轮循环里「通关立即终止」依赖 FlagCount、目标链选择依赖
	// Category——不填的话前者是**死代码**（`ch.FlagCount > 0` 恒假），后者永远
	// 走默认链。这与旧契约里 `Challenge.Description` 是死路径是同一类缺陷。
	//
	// 为 nil 时 Run 会从 Platform.List 里找同 code 的那条；再找不到就只能退化成
	// 「没有 FlagCount」，此时通关检查不生效，但轮循环仍能跑完。
	Challenge *Challenge
	// Submit 为假时只记账不提交（干跑）。
	Submit bool
	// HintPolicy 见 HintPolicy 常量。
	HintPolicy string
	// Prompt 覆盖「本题」段的渲染。为 nil 时用默认渲染。
	Prompt func(Challenge, StartResult) string
	// OnEvent 是**流式**回调（transcript / 态势台 / 日志），每个事件都会到达。
	// **不要在这里做 IO，也不要在这里改图状态**——它可能被调用得很频繁。
	OnEvent func(Event)
	// Now 可注入时钟（测试用）。
	Now func() time.Time
}

const (
	// HintOff 从不请求提示。
	HintOff = "off"
	// HintAuto 连续 dry 轮次达阈值且本访问题未用过提示时请求一次。
	HintAuto = "auto"
	// HintAlways 每道题开始就请求一次提示。
	HintAlways = "always"
)

func (s *Session) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// DefaultPrompt 渲染「本题」段。答案格式说明以题面为准——前身
// `_INTRANET_ORCHESTRATION` 规则 2 的原话：题目要求 flag{...} 就写 flag{...}，
// 要求密码/hash/密钥就写原始值，不要自己加外壳。
func DefaultPrompt(c Challenge, s StartResult) string {
	p := fmt.Sprintf("目标：%s", c.Code)
	if c.Description != "" {
		p += "\n题面：" + c.Description
	}
	if len(s.Addrs) > 0 {
		p += fmt.Sprintf("\n地址：%v", s.Addrs)
	}
	if c.Category != "" {
		p += "\n类别：" + c.Category
	}
	if c.FlagCount > 0 {
		p += fmt.Sprintf("\nflag 数：%d（已确认 %d）", c.FlagCount, c.Solved)
	}
	p += "\n答案格式：" + answerFormatHint(c)
	return p
}

// answerFormatHint 生成答案格式说明。绝不硬塞 flag{...}——题目没说的形态
// 就不能替它假设。
func answerFormatHint(c Challenge) string {
	if c.FlagFormat != "" {
		return c.FlagFormat
	}
	return "以题面为准（可能是 flag{...}，也可能是密码 / hash / 密钥的原始值，不要自己加外壳）"
}

// Run 跑一道题。
//
// 这是轮循环的骨架：起题 → 建/载图 → 逐轮选意图 → 渲染 → 跑一轮 → 回填 →
// 轮末提交 → 落盘。终止条件：意图耗尽 / 全部 flag 确认 / 预算耗尽 / ctx 取消。
func (s *Session) Run(ctx context.Context, code string) (Outcome, error) {
	if s.Platform == nil {
		return Outcome{}, errors.New("harness: Session.Platform 未设置")
	}
	if s.Agent == nil && s.Solver == nil {
		return Outcome{}, errors.New("harness: Session.Agent 与 Session.Solver 至少设置一个")
	}

	out := Outcome{Code: code, StartedAt: s.now()}
	defer func() { out.EndedAt = s.now() }()

	// 平台连通性预检（tsecbench 的 VPN 预检就是这个语义）。必须在任何真实
	// 平台调用之前——对齐 SDK 上下文管理器的行为。
	if hc, ok := s.Platform.(HealthChecker); ok {
		if err := hc.Health(ctx); err != nil {
			out.Reason = ReasonError
			out.Err = err.Error()
			return out, fmt.Errorf("harness: 平台预检失败: %w", err)
		}
	}

	start, err := s.Platform.Start(ctx, code)
	if err != nil {
		out.Reason = ReasonError
		out.Err = err.Error()
		return out, fmt.Errorf("harness: 起题 %s 失败: %w", code, err)
	}
	defer func() { _, _ = s.Platform.Close(ctx, code) }()

	ch := s.resolveChallenge(ctx, code, start)

	if s.Solver != nil && s.Agent == nil {
		return s.runOneShot(ctx, ch, start, &out)
	}
	return s.runRounds(ctx, ch, start, &out)
}

// resolveChallenge 组装轮循环需要的完整题目信息。
//
// 优先级：Session.Challenge（显式）> Platform.List 里同 code 的那条 > StartResult
// 能提供的部分。**必须尽力而为**：拿不到 FlagCount 意味着「通关立即终止」这条
// 护栏失效（前身那条是死代码，事故现场），拿不到 Category 意味着目标链退化。
func (s *Session) resolveChallenge(ctx context.Context, code string, start StartResult) Challenge {
	ch := Challenge{Code: code, Addrs: start.Addrs, Description: start.Description}
	if s.Challenge != nil && s.Challenge.Code == code {
		ch = *s.Challenge
		ch.Addrs = start.Addrs
		if start.Description != "" {
			ch.Description = start.Description
		}
		return ch
	}
	if s.Challenge != nil && s.Challenge.Code == "" {
		// 调用方只填了 FlagCount/Category 这类「每题相同」的字段。
		ch.FlagCount = s.Challenge.FlagCount
		ch.Category = s.Challenge.Category
		ch.Difficulty = s.Challenge.Difficulty
		ch.FlagFormat = s.Challenge.FlagFormat
		ch.Solved = s.Challenge.Solved
	}
	if ch.FlagCount == 0 || ch.Category == "" {
		if list, err := s.Platform.List(ctx); err == nil {
			for _, c := range list {
				if c.Code != code {
					continue
				}
				if ch.FlagCount == 0 {
					ch.FlagCount = c.FlagCount
				}
				if ch.Category == "" {
					ch.Category = c.Category
				}
				if ch.FlagFormat == "" {
					ch.FlagFormat = c.FlagFormat
				}
				if ch.Description == "" {
					ch.Description = c.Description
				}
				if ch.Difficulty == "" {
					ch.Difficulty = c.Difficulty
				}
				break
			}
		}
	}
	return ch
}

// runOneShot 走一次性后端：一轮到底。用于降级与对照。
func (s *Session) runOneShot(ctx context.Context, ch Challenge, _ StartResult, out *Outcome) (Outcome, error) {
	sr := StartResult{Code: ch.Code, Addrs: ch.Addrs, Description: ch.Description}
	prompt := DefaultPrompt(ch, sr)
	if s.Prompt != nil {
		prompt = s.Prompt(ch, sr)
	}
	req := SolveRequest{Prompt: prompt, Workdir: s.Workdir, FlagFormat: answerFormatHint(ch)}

	res, err := s.Solver.Solve(ctx, req, s.emit)
	// 缺陷修正：只在成功路径把 Solve 结果写进 Outcome。
	if err == nil {
		out.Solve = res
	} else {
		out.Reason = ReasonError
		out.Err = err.Error()
	}
	s.harvest(ctx, out)
	out.EndedAt = s.now()
	if err != nil {
		return *out, fmt.Errorf("harness: 引擎执行失败: %w", err)
	}
	return *out, nil
}

// runRounds 是 DAG 驱动的轮循环。
func (s *Session) runRounds(ctx context.Context, ch Challenge, start StartResult, out *Outcome) (Outcome, error) {
	if s.Scheduler == nil || s.Renderer == nil {
		return *out, errors.New("harness: 轮循环需要 Scheduler 与 Renderer")
	}
	if err := s.Agent.Start(ctx, AgentStart{
		Workdir: s.Workdir,
	}); err != nil {
		out.Reason = ReasonError
		out.Err = err.Error()
		return *out, fmt.Errorf("harness: 启动 agent 失败: %w", err)
	}
	defer func() { _ = s.Agent.Close(ctx) }()

	started := s.now()
	var turns int
	dry := 0

	for {
		// 每轮开头检查「通关立即终止」——前身这条是死代码（stop_check 从未被
		// 调用），驱动承诺的「通关即停」形同虚设。
		//
		// 两个判据取「或」，因为它们的可用性不同：
		//   - `ch.FlagCount` 来自调用方/平台 list，可能缺失；
		//   - 平台进度来自每次 Submit 的返回值，**总是权威且总是有**。
		// 只依赖前者会重演「死代码」，只依赖后者则在一题都没提交成功时判不出来。
		if (ch.FlagCount > 0 && len(out.Flags) >= ch.FlagCount) ||
			(out.ProgressTotal > 0 && out.ProgressConfirmed >= out.ProgressTotal) {
			out.Reason = ReasonSolved
			break
		}
		used := Budget{MaxRounds: out.Rounds, MaxWall: s.now().Sub(started), MaxTurns: turns,
			MaxCostUSD: out.Stats.CostUSD}
		if done, reason := s.Budget.Exhausted(used); done {
			out.Reason = reason
			break
		}

		it := s.Scheduler.Next(ctx, ch, out)
		if it == nil {
			out.Reason = ReasonNoIntent
			break
		}
		s.Scheduler.Activate(it)

		// 告诉 gate「现在在跑哪个意图」。不调这一步，候选的 IntentID 会全空、
		// Round 会全是 0——族别判定不受影响（所以不会报错），但推导链锚点丢失，
		// 「这个答案是哪条路试出来的」再也追溯不回来。
		if s.IntentSink != nil {
			s.IntentSink(it.ID, out.Rounds+1)
		}

		prompt := s.Renderer.Render(ctx, ch, it, out)
		before := len(out.Candidates)

		// 把 Ingest 绑上本轮号，让轮内每个事件都能喂给 DAG。
		//
		// 轮号必须在这里绑定（而不是让实现自己去猜）：`dag.Scheduler.Settle`
		// 靠「Activate 时的水位线 + 轮次新鲜度」重建产出判定，而
		// `harness.Scheduler.Settle` 的签名不给 produced 列表。轮号一旦不同步，
		// 后果是**静默的**——所有意图都会判 failed，阶段链永远推不动。
		emit := s.emit
		if s.Ingest != nil {
			round := out.Rounds + 1
			ing := s.Ingest
			emit = func(ev Event) {
				s.emit(ev)
				ing(ev, round)
			}
		}

		res, err := s.Agent.Round(ctx, prompt, emit)
		out.Rounds++
		turns += res.Turns
		if res.CancelledUI > 0 {
			s.observe(Event{Kind: EventUIRequest,
				Err: fmt.Sprintf("本轮被自动取消的对话框: %d", res.CancelledUI)})
		}

		// ctx 取消/超时**必须先判**。否则下面那条 provider 护栏会先命中：
		// 一次零回合的墙钟超时同样满足 `turns == 0 && err != ""`（err 是
		// context.DeadlineExceeded），于是被记成 provider 故障——一次正常超时
		// 变成了「模型服务挂了」的错误结论。
		if ctx.Err() != nil {
			out.Reason = ReasonStopped
			out.Err = ctx.Err().Error()
			// Agent 的 Err 更具体（例如 pi_process_died），优先用它。
			if res.Err != "" {
				out.Err = res.Err
			}
			break
		}

		// 前身护栏：0 回合 + 有错误 ⇒ provider 故障。pi 会把 provider 错误呈现
		// 为一次静默的空会话（M0 实测：401 时 stopReason=error、content 为空、
		// 照样 agent_settled），不显式识别就会以「跑完了但什么都没发生」的形式
		// 烧掉整个题库。
		//
		// 注意这条护栏只覆盖**第一轮就炸**的情形。第 2 轮之后的 provider 故障
		// 会表现为「一轮没有新事实」，被 dry 计数慢慢吃掉预算而不报错——所以
		// 还要看本轮独立的 ProviderError（见下）。
		if res.Turns == 0 && res.Err != "" {
			out.Reason = ReasonProviderFailure
			out.Err = res.Err
			break
		}
		if err != nil {
			out.Reason = ReasonError
			out.Err = err.Error()
			break
		}
		// 本轮独立的 provider 错误：不看 Turns，任何一轮都判。
		// 这里必须用**本轮**的标志而不是轮级累积的 Err——累积值会让第 1 轮的
		// 一次故障污染后面每一轮，第 3 轮已经恢复并拿到 flag 了却被判失败。
		if res.ProviderError != "" {
			out.Reason = ReasonProviderFailure
			out.Err = res.ProviderError
			break
		}
		if res.Err != "" {
			out.Reason = ReasonError
			out.Err = res.Err
			break
		}

		s.Scheduler.Settle(it, res)

		// 每轮落盘：断点续跑的粒度就是轮。落盘失败**不静默**——它意味着这一轮
		// 的进展没保住，必须让调用方看见。
		if s.Saver != nil && s.GraphPath != "" {
			if err := s.Saver.Save(s.GraphPath); err != nil {
				s.observe(Event{Kind: EventError, Err: "落盘失败: " + err.Error()})
			}
		}

		// dry 计数：本轮没有产出新候选也没产出新事实 ⇒ 卡住了。
		if len(out.Candidates) == before {
			dry++
		} else {
			dry = 0
		}
		s.maybeHint(ctx, ch, out, dry)

		s.harvest(ctx, out)
	}

	s.harvest(ctx, out)
	out.EndedAt = s.now()
	if st, err := s.Agent.Stats(ctx); err == nil {
		out.Stats = st
	}
	return *out, nil
}

// maybeHint 按 HintPolicy 决定是否请求提示并注入。
func (s *Session) maybeHint(ctx context.Context, ch Challenge, out *Outcome, dry int) {
	if s.HintPolicy == HintOff || s.HintPolicy == "" {
		return
	}
	if s.HintPolicy == HintAuto && dry < 3 {
		return
	}
	if out.HintUsed > 0 && s.HintPolicy == HintAuto {
		return // auto 模式每题只请求一次
	}
	h, err := s.Platform.Hint(ctx, ch.Code)
	if err != nil || h.Hint == "" {
		return
	}
	out.HintUsed++
	// 提示是方向线索，必须回到目标命令验证——把它作为 steering 注入，
	// 而不是替换当前意图。
	_ = s.Agent.Steer(ctx, "【平台提示（仅方向线索，必须回到目标命令验证）】\n"+h.Hint)
}

// emit 是给 Agent/Solver 的逐事件回调。**这里只做记账，绝不做 IO**。
func (s *Session) emit(ev Event) {
	s.observe(ev)
	if s.Observer != nil {
		s.Observer.Observe(ev)
	}
	if s.Gate != nil {
		s.Gate.Observe(ev)
	}
}

// harvest 是轮末的提交循环。只有未提交过的候选会被提交，且被判错的答案
// 永不重提。
func (s *Session) harvest(ctx context.Context, out *Outcome) {
	if s.Gate == nil {
		return
	}
	out.Candidates = s.Gate.Candidates()
	for _, c := range s.Gate.New() {
		if !s.Submit {
			s.Gate.Mark(c.Flag, SubmitResult{}, nil)
			continue
		}
		if s.Ledger != nil && s.Ledger.Has(c.Flag) {
			continue
		}
		res, err := s.Platform.Submit(ctx, out.Code, c.Flag)
		s.Gate.Mark(c.Flag, res, err)
		switch {
		case err != nil:
			// 提交出错不计确认，也不记入判错账本（可能是网络问题，值得重试）
		case res.Duplicate:
			out.Duplicates++
			out.Flags = appendUnique(out.Flags, c.Flag)
		case res.Correct:
			out.Flags = appendUnique(out.Flags, c.Flag)
		default:
			out.Rejected++
			if s.Ledger != nil {
				s.Ledger.Record(c.Flag, "platform_rejected")
			}
		}
		// 平台每次 Submit 都回权威进度，记下来供「通关立即终止」判据使用。
		if res.TotalFlagCount > 0 {
			out.ProgressTotal = res.TotalFlagCount
		}
		if res.CorrectFlagCount > 0 {
			out.ProgressConfirmed = res.CorrectFlagCount
		}
	}
	// 缺陷修正：Submitted 是**去重后的确认数**，不是提交次数。
	out.Submitted = len(out.Flags)
	out.Candidates = s.Gate.Candidates()
}

func (s *Session) observe(ev Event) {
	if s.OnEvent == nil {
		return
	}
	// 回调 panic 不能杀死 reader 协程——reader 一死整个 pi 会话就废了。
	defer func() { _ = recover() }()
	s.OnEvent(ev)
}

func appendUnique(xs []string, v string) []string {
	if slices.Contains(xs, v) {
		return xs
	}
	return append(xs, v)
}

// IntentRef 是 Scheduler 眼里一个意图的最小视图。定义在 harness 而不是 dag，
// 是为了让根包不依赖 dag（避免循环依赖：dag 需要引用 harness 的 Challenge/Outcome）。
type IntentRef struct {
	ID    string
	Kind  string
	Goal  string
	Round int
}

// Scheduler 决定下一轮做哪个意图。
type Scheduler interface {
	// Next 返回下一个可执行的意图；返回 nil 表示前沿耗尽。
	Next(ctx context.Context, ch Challenge, out *Outcome) *IntentRef
	// Activate 标记意图进入执行中。
	Activate(it *IntentRef)
	// Settle 回填一轮的结果。
	Settle(it *IntentRef, res RoundResult)
}

// Renderer 把当前状态渲染成本轮 prompt。
type Renderer interface {
	Render(ctx context.Context, ch Challenge, it *IntentRef, out *Outcome) string
}

// Saver 把 DAG 落盘。实现是 `dag.Graph.Save`。
//
// 单独一个接口（而不是塞进 Observer）是因为存盘与观察是两件事：轮循环需要在
// **每轮结束时**落盘，而「只观察不落盘」的实现（例如态势台）不该被迫实现一个
// 空方法。
type Saver interface {
	Save(path string) error
}

// Observer 接收归一化事件，供**有状态**的消费者使用——主要是 DAG：它从
// tool_execution_end 里抽事实入图。实现见 dag 包。
//
// 与 OnEvent 的分工：两者都会收到每个事件，但语义不同。
//   - Observer 是**有状态**的（要维护图/账本），只应当有一个。
//   - OnEvent 是**流式**的（transcript、态势台、日志），不应当有状态。
//
// 分开是为了让「谁在图里写东西」只有一个答案——否则两处都在改状态，
// 排查起来会很痛苦。
type Observer interface {
	Observe(ev Event)
}
