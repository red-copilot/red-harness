package scenario

import (
	"context"
	"fmt"
	"sync"

	harness "github.com/red-copilot/red-harness"
)

// Fake 是确定性的离线场景：状态全在内存里，不碰网络、不碰 Docker、不读墙钟。
// 它是 M1 纵向闭环（Fake → Sandbox → stub pi → DAG/Gate → Evaluate → Cleanup）
// 的题目侧替身，也是引擎回归测试的稳定输入源。
//
// 与真实平台的两处**刻意**差异（都写在对应方法上）：
//   - 「容器就绪」是构造性的：Prepare 同步返回，离线场景没有异步起题可等。
//   - 权威进度来自本结构体的账本，而不是远端。
type Fake struct {
	// Challenges 是 Discover 的返回集合。**构造后不要再改**：Prepare 已经把
	// 其中的地址钉成白名单（见 targets），事后修改会让白名单与声明的题目不一致。
	Challenges []harness.Challenge
	// Answers 声明「哪些答案被平台接受」：Answers[code][answer] == true。
	// 未声明的答案一律判「不接受」而**不是**报错——这是 v0.4 的契约：平台拒一个
	// 错误答案是正常判定，不是故障。为 nil 表示这道题没有可接受答案。
	Answers map[string]map[string]bool
	// ScorePerFlag 是每个确认答案的分值；<=0 时按 1 计。
	// 计分策略是 Fake 自己定的（真实分值由平台给），但它必须能表达「累计」：
	// 得分随确认数单调上升，重复提交不涨分。
	ScorePerFlag int

	mu sync.Mutex
	// started 是「容器还开着」的题目集合。Cleanup 必须真的把它清掉，否则
	// 「回收路径可幂等重试」就只是「函数不报错」而已。
	started map[string]bool
	// targets 记录 Prepare 返回过的 Target。
	//
	// v0.4 的硬规矩：目标白名单**只能**来自 Scenario.Prepare 的返回值
	// （executor 的 session 直接读 Target.Addrs 渲染 iptables）。所以第一次
	// Prepare 的结果必须被钉住，重复起题只能拿到同一份地址——否则「谁决定了
	// 授权范围」就变成了调用方每次传参的运气。
	targets map[string]harness.Target
	// confirmed 是**平台侧**已确认答案的账本。它同时是幂等命中判据
	// （Duplicate 等价于已确认）与权威进度的唯一来源。
	confirmed map[string]map[string]bool
}

var _ harness.Scenario = (*Fake)(nil)

// Discover 返回调用方声明的题目集合的**深拷贝**。
//
// 为什么不直接用 `append([]harness.Challenge(nil), f.Challenges...)`：那只是
// 结构体的浅拷贝，Challenge.Addrs 的底层数组仍是同一份。调用方（或渲染层）
// 往返回的切片里写一个字节，Fake 内部声明的题目地址就被改了——而这条地址正是
// 沙箱白名单的来源。浅拷贝很容易被当成「已经复制过了」。
func (f *Fake) Discover(context.Context, harness.RunSpec) ([]harness.Challenge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]harness.Challenge, 0, len(f.Challenges))
	for _, ch := range f.Challenges {
		out = append(out, cloneChallenge(ch))
	}
	return out, nil
}

// Prepare 起题并返回引擎视角的目标。
//
// 「等容器就绪」在 Fake 里是**构造性**的：离线场景没有 pending → available 的
// 异步过程，所以它同步返回，而不是假装轮询一圈。真实平台的轮询语义在
// TSecBench.Prepare 里。
//
// 同一道题重复 Prepare 是幂等的：返回第一次钉住的那份地址；只有当调用方拿
// 一份地址不同的 Challenge 再来时才是错误——那是白名单来源漂移，fail closed。
func (f *Fake) Prepare(_ context.Context, ch harness.Challenge) (harness.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started == nil {
		f.started = map[string]bool{}
	}
	if f.targets == nil {
		f.targets = map[string]harness.Target{}
	}
	if prev, ok := f.targets[ch.Code]; ok {
		if !sameAddrs(prev.Addrs, ch.Addrs) {
			return harness.Target{}, harness.Ef(harness.KindConfig, "scenario.fake.prepare",
				"重复起题时题目地址发生漂移，白名单来源必须稳定", fmt.Errorf("challenge=%s", ch.Code))
		}
		f.started[ch.Code] = true
		return cloneTarget(prev), nil
	}
	t := harness.Target{Code: ch.Code, Addrs: append([]string(nil), ch.Addrs...), Network: "tcp"}
	f.targets[ch.Code] = t
	f.started[ch.Code] = true
	return cloneTarget(t), nil
}

// Hint 返回空串：离线场景没有提示可给。契约允许「没有提示时返回空串且不报错」，
// 所以这不是「未实现」，而是「这道题确实没有提示」。
func (f *Fake) Hint(context.Context, harness.Challenge) (string, error) { return "", nil }

// Evaluate 提交一个候选。
//
// 语义与平台对齐的三点：
//   - 未声明的答案 → 判「不接受」（Accepted=false）且**不报错**。
//   - 幂等命中（同一答案第二次提交）→ Accepted=true 但 **Progress=false**：
//     平台回 duplicate 等价于「这个答案已经确认过」，把它算成进展会让停滞
//     检测永远看不到停滞（v0.4：平台 duplicate 等价于已确认）。
//   - Score 是**累计得分**（已确认答案数 × 分值），不是本次增量。返回常量 1
//     会让报告里的「得分」既不可比也不可加；累计值在重复提交时不变，符合
//     「只升不降」。
//
// nil 安全：f.Answers 为 nil、或该题没有条目时，`f.Answers[code][answer]`
// 对 nil map 的读取返回零值 false（Go 规范：读 nil map 不 panic），所以这里
// 不需要额外判空——判空只是把同一条路径写两遍。
func (f *Fake) Evaluate(_ context.Context, ch harness.Challenge, answer string) (harness.Evaluation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Answers[ch.Code][answer] {
		return harness.Evaluation{Message: "rejected"}, nil
	}
	if f.confirmed == nil {
		f.confirmed = map[string]map[string]bool{}
	}
	if f.confirmed[ch.Code] == nil {
		f.confirmed[ch.Code] = map[string]bool{}
	}
	dup := f.confirmed[ch.Code][answer]
	f.confirmed[ch.Code][answer] = true
	want := f.want(ch.Code, ch.FlagCount)
	got := len(f.confirmed[ch.Code])
	return harness.Evaluation{
		Accepted: true,
		Progress: !dup,
		// 分母未知（want==0）时 Completed 恒假：宁可不说完成，也不能在不知道
		// 还差几个 flag 的情况下宣称通关。
		Completed: want > 0 && got >= want,
		Score:     got * f.scorePerFlag(),
		Message:   "accepted",
	}, nil
}

// Reconcile 返回**平台权威**的进度。
//
// Fake 里没有「容器状态」这一维：Prepare 之后地址就固定了（且关题不改变平台
// 给出的地址），所以对账只剩进度一维。真实平台要同时核对容器状态，那是
// TSecBench 的职责。
//
// nil 安全：f.confirmed 为 nil、或该题没有条目时，len(nil map) == 0。
func (f *Fake) Reconcile(_ context.Context, ch harness.Challenge) (harness.Objective, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := f.want(ch.Code, ch.FlagCount)
	got := len(f.confirmed[ch.Code])
	return harness.Objective{
		Kind: "flag_count", Want: want, Got: got,
		Completed: want > 0 && got >= want,
	}, nil
}

// Cleanup 关题（回收容器）。**幂等**：delete 一个不存在的键是 no-op，nil map 上
// 的 delete 也是 no-op（Go 规范），所以不需要额外判空，重复调用同样返回 nil。
//
// 为什么幂等是硬要求：正常结束、ctx 取消、失败三条回收路径都会调它，而
// Cleanup 的错误**不得覆盖主要终止原因**——一次重复调用报错就会把真正的
// 「为什么停」盖掉。
//
// 刻意不清 confirmed：关题不改变平台已经确认过的答案，清掉会让「同一答案
// 永不重提」与幂等命中判定在同一进程内的后续题目上失效。
func (f *Fake) Cleanup(_ context.Context, ch harness.Challenge) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.started, ch.Code)
	return nil
}

// want 返回这道题的**权威 flag 数**（分母）。调用方必须持锁。
//
// 为什么不直接信 ch.FlagCount：Discover 返回的 Challenge 是**调用方填的**，
// 而 FlagCount == 0 在根包契约里的含义是「未知」（StartResult 不携带它）。
// 若 Completed 只认 ch.FlagCount，调用方没填时 Completed 恒假——测试会把
// 「已经全部解出」误读成「没解出来」，M1 的「通关立即终止」也永远不触发。
// Fake 自己就知道答案全集（Answers[code] 的键就是被平台接受的答案），所以：
// 调用方填了就信调用方，没填就回落到声明答案数；两者都没有才算分母未知，
// 此时 Completed 恒假——这是**有意**的 fail closed。
func (f *Fake) want(code string, declared int) int {
	if declared > 0 {
		return declared
	}
	return len(f.Answers[code])
}

// scorePerFlag 返回每个确认答案的分值。调用方必须持锁。
func (f *Fake) scorePerFlag() int {
	if f.ScorePerFlag <= 0 {
		return 1
	}
	return f.ScorePerFlag
}

// cloneChallenge 深拷贝一道题（当前只有 Addrs 是引用类型，但按字段全量写，
// 将来 Challenge 加切片字段时不会静默漏拷）。
func cloneChallenge(ch harness.Challenge) harness.Challenge {
	ch.Addrs = append([]string(nil), ch.Addrs...)
	return ch
}

// cloneTarget 深拷贝一个 Target：Prepare 的返回值是白名单的唯一来源，调用方
// 拿着返回值改一个字节都不该影响到 Fake 记下的那份。
func cloneTarget(t harness.Target) harness.Target {
	t.Addrs = append([]string(nil), t.Addrs...)
	return t
}

// sameAddrs 逐元素比较地址列表（顺序敏感：白名单是按顺序渲染的）。
// 不复用根包的 contains——根包是纯契约层，工具函数不外借。
func sameAddrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
