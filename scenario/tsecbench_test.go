package scenario

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// 本文件用**注入的假 Platform** 覆盖 TSecBench 的六方法映射。
//
// 为什么不用 mock 框架：仓库零第三方依赖（go.mod 无 require 块），而这里要
// 断言的东西（调用次数、每次返回什么、第几次返回错误）用一个结构体 + 函数字段
// 就够。假 Platform 的每个方法都能注入错误，用来钉住「错误分类不被吞」。

// fakePlatform 是可注入的 harness.Platform 替身。
//
// 所有函数字段都为 nil 时行为是「空平台」：List 返回 nil、其余方法返回零值。
// 每个字段都可以单独替换，测试只关心自己要钉的那一条路径。
type fakePlatform struct {
	mu sync.Mutex
	// list 是 List 的返回；listErrs 非空时按调用序依次返回错误（用完之后
	// 继续返回最后一个），用来构造「第一次成功、第二次失败」这类序列。
	list     []harness.Challenge
	listErrs []error
	// 下面几个按调用序返回错误序列；空序列表示永远成功。
	startErrs  []error
	hintErrs   []error
	submitErrs []error
	closeErrs  []error

	listN, startN, hintN, submitN, closeN int
	// lastSubmit 记录最后一次提交的 flag，用于断言「提交的是候选原文」。
	lastSubmit string
	// startResult / hintResult / submitResult / closeResult 是成功时的返回值。
	startResult  harness.StartResult
	hintResult   harness.HintResult
	submitResult harness.SubmitResult
	closeResult  harness.CloseResult
	// onList 在每次 List 时被调用（持锁），可用来在轮询中途改状态或取消 ctx。
	onList func(n int)
}

func (p *fakePlatform) next(list []error, n int) error {
	if len(list) == 0 {
		return nil
	}
	if n >= len(list) {
		return list[len(list)-1]
	}
	return list[n]
}

func (p *fakePlatform) List(context.Context) ([]harness.Challenge, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listN++
	if p.onList != nil {
		p.onList(p.listN)
	}
	if err := p.next(p.listErrs, p.listN-1); err != nil {
		return nil, err
	}
	// 刻意**不拷贝**就交出去：真实的 bridge 客户端可能缓存并复用同一份切片，
	// 所以 Discover 不得就地改写平台返回值（`items[:0]` 就会）。测试夹具必须
	// 保留这个危险，否则「就地过滤」这类缺陷在测试里看不出来。
	return p.list, nil
}

func (p *fakePlatform) Start(context.Context, string) (harness.StartResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.startN++
	if err := p.next(p.startErrs, p.startN-1); err != nil {
		return harness.StartResult{}, err
	}
	return p.startResult, nil
}

func (p *fakePlatform) Hint(context.Context, string) (harness.HintResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hintN++
	if err := p.next(p.hintErrs, p.hintN-1); err != nil {
		return harness.HintResult{}, err
	}
	return p.hintResult, nil
}

func (p *fakePlatform) Submit(_ context.Context, _, flag string) (harness.SubmitResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.submitN++
	p.lastSubmit = flag
	if err := p.next(p.submitErrs, p.submitN-1); err != nil {
		return harness.SubmitResult{}, err
	}
	return p.submitResult, nil
}

func (p *fakePlatform) Close(context.Context, string) (harness.CloseResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeN++
	if err := p.next(p.closeErrs, p.closeN-1); err != nil {
		return harness.CloseResult{}, err
	}
	return p.closeResult, nil
}

func (p *fakePlatform) counts() (list, start, hint, submit, close int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listN, p.startN, p.hintN, p.submitN, p.closeN
}

// newTSec 造一个轮询间隔极短的适配器：PollEvery 默认 1ms，否则测试会被默认的
// 500ms 间隔拖慢几十倍。
func newTSec(p harness.Platform) *TSecBench {
	return &TSecBench{Platform: p, PollEvery: time.Millisecond, PollLimit: 2 * time.Second}
}

// ── Discover ──

// spec.Targets 为空 ⇒ 只返回未完成的题。已通关的题必须被过滤掉，否则每次
// 全量跑都会重新起一遍已经做掉的容器。
func TestTSecBenchDiscoverFiltersCompletedWhenNoTargets(t *testing.T) {
	p := &fakePlatform{list: []harness.Challenge{
		{Code: "done", FlagCount: 2, Solved: 2},
		{Code: "half", FlagCount: 3, Solved: 1},
		{Code: "fresh", FlagCount: 1},
		{Code: "unknown", FlagCount: 0, Solved: 0}, // 分母未知 ⇒ 不算完成
	}}
	got, err := newTSec(p).Discover(context.Background(), harness.RunSpec{})
	if err != nil {
		t.Fatalf("Discover 报错: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应过滤掉已通关的题，got %d 道: %+v", len(got), got)
	}
	for _, c := range got {
		if c.Code == "done" {
			t.Error("已通关的题必须被过滤")
		}
	}
}

// spec.Targets 非空 ⇒ 按 code 精确匹配，**顺序与调用方给出的一致**。
// 顺序敏感是根包 RunSpec 的既定语义（Digest 把顺序算进去）。
func TestTSecBenchDiscoverMatchesTargetsByCode(t *testing.T) {
	p := &fakePlatform{list: []harness.Challenge{
		{Code: "a", FlagCount: 1},
		{Code: "b", FlagCount: 1},
		{Code: "c", FlagCount: 1},
	}}
	got, err := newTSec(p).Discover(context.Background(), harness.RunSpec{Targets: []string{"c", "a"}})
	if err != nil {
		t.Fatalf("Discover 报错: %v", err)
	}
	if len(got) != 2 || got[0].Code != "c" || got[1].Code != "a" {
		t.Fatalf("按 code 精确匹配且保持调用方顺序，got %+v", got)
	}
}

// 指名要求一道**已完成**的题：跳过，不报错。
// 它是幂等重跑的正常输入，报错会让「补跑一道已经做掉的题」变成硬失败。
func TestTSecBenchDiscoverSkipsNamedCompletedTarget(t *testing.T) {
	p := &fakePlatform{list: []harness.Challenge{{Code: "a", FlagCount: 1, Solved: 1}}}
	got, err := newTSec(p).Discover(context.Background(), harness.RunSpec{Targets: []string{"a"}})
	if err != nil {
		t.Fatalf("指名已完成的题不应报错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("已完成的指名目标应被跳过，got %+v", got)
	}
}

// 目标不在平台集合里 ⇒ KindScope（越权，重试无意义），且**不是** KindPlatform。
func TestTSecBenchDiscoverUnknownTargetIsScopeError(t *testing.T) {
	p := &fakePlatform{list: []harness.Challenge{{Code: "a", FlagCount: 1}}}
	_, err := newTSec(p).Discover(context.Background(), harness.RunSpec{Targets: []string{"a", "不在集合里"}})
	if !harness.IsKind(err, harness.KindScope) {
		t.Fatalf("不在平台集合的目标应返回 KindScope，got %v", err)
	}
}

// List 的错误原样上抛（不重分类、不吞）。
func TestTSecBenchDiscoverPropagatesListError(t *testing.T) {
	sentinel := errors.New("bridge: VPN 未连通")
	p := &fakePlatform{listErrs: []error{sentinel}}
	_, err := newTSec(p).Discover(context.Background(), harness.RunSpec{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("List 错误必须原样上抛，got %v", err)
	}
}

// 平台未设置 ⇒ KindConfig，且不发任何调用。
func TestTSecBenchDiscoverWithoutPlatformIsConfigError(t *testing.T) {
	for _, s := range []*TSecBench{nil, {}} {
		if _, err := s.Discover(context.Background(), harness.RunSpec{}); !harness.IsKind(err, harness.KindConfig) {
			t.Errorf("未设置 platform 应返回 KindConfig，got %v", err)
		}
	}
}

// 平台返回的切片**不得被就地改写**：实现可能缓存并复用那份底层数组，用
// `items[:0]` 过滤会把它的缓冲区写坏，让下一次 List 凭空少几道题。
func TestTSecBenchDiscoverDoesNotMutatePlatformSlice(t *testing.T) {
	shared := []harness.Challenge{
		{Code: "done", FlagCount: 1, Solved: 1},
		{Code: "a", FlagCount: 1},
		{Code: "b", FlagCount: 1},
	}
	p := &fakePlatform{}
	p.list = shared
	if _, err := newTSec(p).Discover(context.Background(), harness.RunSpec{}); err != nil {
		t.Fatalf("Discover 报错: %v", err)
	}
	if shared[0].Code != "done" || shared[1].Code != "a" || shared[2].Code != "b" {
		t.Errorf("平台返回的切片被就地改写：%+v", shared)
	}
}

// ── Prepare ──

// Prepare 必须**轮询到容器可用**才返回，并把平台给出的地址当白名单。
// 第 1、2 次 List 还是 pending（无地址），第 3 次才 available。
func TestTSecBenchPreparePollsUntilAvailable(t *testing.T) {
	p := &fakePlatform{list: []harness.Challenge{{Code: "a", FlagCount: 1}}}
	p.onList = func(n int) {
		if n >= 3 {
			p.list = []harness.Challenge{{Code: "a", FlagCount: 1, Addrs: []string{"10.0.0.9:80"}}}
		}
	}
	s := newTSec(p)
	tgt, err := s.Prepare(context.Background(), harness.Challenge{Code: "a"})
	if err != nil {
		t.Fatalf("Prepare 报错: %v", err)
	}
	if len(tgt.Addrs) != 1 || tgt.Addrs[0] != "10.0.0.9:80" {
		t.Fatalf("Prepare 应返回平台给出的地址，got %+v", tgt)
	}
	if tgt.Code != "a" || tgt.Network != "tcp" {
		t.Errorf("Target 字段不对：%+v", tgt)
	}
	if l, _, _, _, _ := p.counts(); l < 3 {
		t.Errorf("应在容器就绪前持续轮询，List 只调了 %d 次", l)
	}
}

// ctx 取消 ⇒ KindCancelled（而不是被记成平台故障）。这是「为什么停」的分界线：
// 用户按 Ctrl-C 与平台挂了必须可区分。
func TestTSecBenchPrepareCancelledIsCancelledKind(t *testing.T) {
	p := &fakePlatform{list: []harness.Challenge{{Code: "a", FlagCount: 1}}}
	ctx, cancel := context.WithCancel(context.Background())
	p.onList = func(int) { cancel() } // 容器永远不就绪，第 1 次轮询后就取消
	_, err := newTSec(p).Prepare(ctx, harness.Challenge{Code: "a"})
	if !harness.IsKind(err, harness.KindCancelled) {
		t.Fatalf("ctx 取消应返回 KindCancelled，got %v", err)
	}
}

// 期限到了但 ctx 还活着 ⇒ KindPlatform（平台侧没能在时限内让容器可用）。
// 与取消分开：前者可重试，后者不该被当成平台故障。
func TestTSecBenchPrepareTimeoutIsPlatformKind(t *testing.T) {
	p := &fakePlatform{list: []harness.Challenge{{Code: "a", FlagCount: 1}}}
	s := newTSec(p)
	s.PollEvery = time.Millisecond
	s.PollLimit = 10 * time.Millisecond
	_, err := s.Prepare(context.Background(), harness.Challenge{Code: "a"})
	if !harness.IsKind(err, harness.KindPlatform) {
		t.Fatalf("等待超时应返回 KindPlatform，got %v", err)
	}
	if harness.IsKind(err, harness.KindCancelled) {
		t.Error("超时不是取消：两者必须可区分")
	}
}

// Start 失败：错误原样上抛（分类由 bridge 给出，比这里猜的更准）。
func TestTSecBenchPreparePropagatesStartError(t *testing.T) {
	sentinel := errors.New("bridge: 起题失败")
	p := &fakePlatform{startErrs: []error{sentinel}}
	_, err := newTSec(p).Prepare(context.Background(), harness.Challenge{Code: "a"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Start 错误必须原样上抛，got %v", err)
	}
}

// 轮询期间 List 失败：原样上抛，不静默继续等（静默会等到超时，把平台故障
// 记成「容器没起来」）。
func TestTSecBenchPreparePropagatesListError(t *testing.T) {
	sentinel := errors.New("bridge: 查询失败")
	p := &fakePlatform{listErrs: []error{nil, sentinel}}
	_, err := newTSec(p).Prepare(context.Background(), harness.Challenge{Code: "a"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("轮询期间 List 错误必须上抛，got %v", err)
	}
}

// ctx 取消后平台返回的传输错误应改判为 KindCancelled：底层错误是 "context
// canceled"，直接上抛会被引擎记成平台故障。
func TestTSecBenchPrepareClassifiesTransportErrorAfterCancel(t *testing.T) {
	p := &fakePlatform{listErrs: []error{nil, context.Canceled}}
	ctx, cancel := context.WithCancel(context.Background())
	p.onList = func(n int) {
		if n >= 2 {
			cancel()
		}
	}
	_, err := newTSec(p).Prepare(ctx, harness.Challenge{Code: "a"})
	if !harness.IsKind(err, harness.KindCancelled) {
		t.Fatalf("取消后平台返回传输错误应改判 KindCancelled，got %v", err)
	}
}

// ── Hint ──

// 平台没有提示（Hint 为空）时返回空串且不报错。
func TestTSecBenchHintEmptyIsNotAnError(t *testing.T) {
	p := &fakePlatform{hintResult: harness.HintResult{Code: "a"}}
	h, err := newTSec(p).Hint(context.Background(), harness.Challenge{Code: "a"})
	if err != nil || h != "" {
		t.Fatalf("空提示应是 (%q, nil)，got (%q, %v)", "", h, err)
	}
	// 有提示时原样返回。
	p2 := &fakePlatform{hintResult: harness.HintResult{Code: "a", Hint: "看看 /admin"}}
	if h, err := newTSec(p2).Hint(context.Background(), harness.Challenge{Code: "a"}); err != nil || h != "看看 /admin" {
		t.Fatalf("提示应原样返回，got (%q, %v)", h, err)
	}
	// 平台报错时上抛，不吞成空串——吞掉会让「提示失败」看起来像「没有提示」。
	sentinel := errors.New("bridge: hint 失败")
	p3 := &fakePlatform{hintErrs: []error{sentinel}}
	if _, err := newTSec(p3).Hint(context.Background(), harness.Challenge{Code: "a"}); !errors.Is(err, sentinel) {
		t.Fatalf("Hint 错误必须上抛，got %v", err)
	}
}

// ── Evaluate ──

// 平台的 Duplicate ⇒ Accepted=true、Progress=false（幂等命中等价于已确认，
// 但没有让平台侧前进）。
func TestTSecBenchEvaluateDuplicateIsAcceptedWithoutProgress(t *testing.T) {
	p := &fakePlatform{submitResult: harness.SubmitResult{
		Correct: false, Duplicate: true, Message: "duplicate",
		CorrectFlagCount: 1, TotalFlagCount: 2, Awarded: 100,
	}}
	ev, err := newTSec(p).Evaluate(context.Background(), harness.Challenge{Code: "a"}, "flag{x}")
	if err != nil {
		t.Fatalf("Evaluate 报错: %v", err)
	}
	if !ev.Accepted {
		t.Error("Duplicate 必须映射成 Accepted=true")
	}
	if ev.Progress {
		t.Error("Duplicate 不得映射成 Progress=true——它没有让平台侧前进")
	}
	if ev.Score != 100 || ev.Message != "duplicate" {
		t.Errorf("得分与消息应原样透传，got %+v", ev)
	}
	if p.lastSubmit != "flag{x}" {
		t.Errorf("提交的应是候选原文，got %q", p.lastSubmit)
	}
}

// 平台的 Correct ⇒ Accepted=true、Progress=true。
func TestTSecBenchEvaluateCorrectIsProgress(t *testing.T) {
	p := &fakePlatform{submitResult: harness.SubmitResult{
		Correct: true, Awarded: 50, CorrectFlagCount: 2, TotalFlagCount: 2,
	}}
	ev, err := newTSec(p).Evaluate(context.Background(), harness.Challenge{Code: "a"}, "flag{y}")
	if err != nil {
		t.Fatalf("Evaluate 报错: %v", err)
	}
	if !ev.Accepted || !ev.Progress || !ev.Completed {
		t.Errorf("Correct 且进度已满 ⇒ Accepted/Progress/Completed 全真，got %+v", ev)
	}
}

// 平台判错（Correct=false 且非 Duplicate）⇒ 全假，且**不报错**。
// 判错是平台的正常判定，报错会让引擎去 Reconcile 甚至中止整道题。
func TestTSecBenchEvaluateWrongAnswerIsNotAnError(t *testing.T) {
	p := &fakePlatform{submitResult: harness.SubmitResult{Message: "wrong"}}
	ev, err := newTSec(p).Evaluate(context.Background(), harness.Challenge{Code: "a"}, "flag{z}")
	if err != nil {
		t.Fatalf("平台判错不应报错: %v", err)
	}
	if ev.Accepted || ev.Progress || ev.Completed {
		t.Errorf("判错应全假，got %+v", ev)
	}
}

// TotalFlagCount 未知（0）时 Completed 恒假：分母未知不得宣称通关。
func TestTSecBenchEvaluateCompletedNeedsKnownTotal(t *testing.T) {
	p := &fakePlatform{submitResult: harness.SubmitResult{Correct: true, CorrectFlagCount: 3, TotalFlagCount: 0}}
	ev, err := newTSec(p).Evaluate(context.Background(), harness.Challenge{Code: "a"}, "flag{y}")
	if err != nil {
		t.Fatalf("Evaluate 报错: %v", err)
	}
	if ev.Completed {
		t.Errorf("分母未知（total=0）时不得 Completed，got %+v", ev)
	}
}

// 提交错误原样上抛：引擎要靠它区分「写操作不确定」（先 Reconcile 再决定重发）
// 与「平台判错」。
func TestTSecBenchEvaluatePropagatesSubmitError(t *testing.T) {
	sentinel := errors.New("bridge: 提交超时")
	p := &fakePlatform{submitErrs: []error{sentinel}}
	_, err := newTSec(p).Evaluate(context.Background(), harness.Challenge{Code: "a"}, "flag{x}")
	if !errors.Is(err, sentinel) {
		t.Fatalf("Submit 错误必须上抛，got %v", err)
	}
	if !harness.IsKind(err, harness.KindPlatform) && harness.IsKind(err, harness.KindCancelled) {
		t.Error("未分类的传输错误不应被误标成取消")
	}
}

// ── Reconcile ──

// Reconcile 用**平台权威进度**（Challenge.Solved / FlagCount），不靠本地推测：
// 提交的写操作可能超时但实际已生效，本地账本恰好是最不可靠的来源。
func TestTSecBenchReconcileUsesPlatformProgress(t *testing.T) {
	p := &fakePlatform{list: []harness.Challenge{{Code: "a", FlagCount: 4, Solved: 2}}}
	obj, err := newTSec(p).Reconcile(context.Background(), harness.Challenge{Code: "a"})
	if err != nil {
		t.Fatalf("Reconcile 报错: %v", err)
	}
	if obj.Kind != "flag_count" || obj.Want != 4 || obj.Got != 2 || obj.Completed {
		t.Fatalf("应以平台进度为准 ⇒ {flag_count,4,2,false}，got %+v", obj)
	}
	// 平台侧已满 ⇒ Completed。
	p.list = []harness.Challenge{{Code: "a", FlagCount: 4, Solved: 4}}
	if obj, err := newTSec(p).Reconcile(context.Background(), harness.Challenge{Code: "a"}); err != nil || !obj.Completed {
		t.Fatalf("平台侧已通关 ⇒ Completed=true，got (%+v, %v)", obj, err)
	}
}

// 分母未知（FlagCount=0）时不得宣称完成，即使平台说 Solved>0。
func TestTSecBenchReconcileUnknownTotalNeverCompletes(t *testing.T) {
	p := &fakePlatform{list: []harness.Challenge{{Code: "a", FlagCount: 0, Solved: 3}}}
	obj, err := newTSec(p).Reconcile(context.Background(), harness.Challenge{Code: "a"})
	if err != nil {
		t.Fatalf("Reconcile 报错: %v", err)
	}
	if obj.Completed {
		t.Errorf("FlagCount 未知时不得 Completed，got %+v", obj)
	}
}

// 题目不在平台当前进度中 ⇒ KindPlatform（「题目消失了」不是调用方能修的错）。
func TestTSecBenchReconcileMissingChallengeIsPlatformError(t *testing.T) {
	p := &fakePlatform{list: []harness.Challenge{{Code: "别的题", FlagCount: 1}}}
	_, err := newTSec(p).Reconcile(context.Background(), harness.Challenge{Code: "a"})
	if !harness.IsKind(err, harness.KindPlatform) {
		t.Fatalf("题目不在平台进度中应返回 KindPlatform，got %v", err)
	}
}

// List 失败时 Reconcile 上抛，不得返回一个「零进度但没错误」的假象——
// 引擎正是靠这个错误决定「能不能确认写操作已生效」。
func TestTSecBenchReconcilePropagatesListError(t *testing.T) {
	sentinel := errors.New("bridge: 查询失败")
	p := &fakePlatform{listErrs: []error{sentinel}}
	if _, err := newTSec(p).Reconcile(context.Background(), harness.Challenge{Code: "a"}); !errors.Is(err, sentinel) {
		t.Fatalf("List 错误必须上抛，got %v", err)
	}
}

// ── Cleanup ──

// Cleanup 把平台错误**原样上抛**：关题失败意味着容器可能还在跑、还在计费，
// 吞掉它等于把一条真实的资源泄漏记成「已回收」。
func TestTSecBenchCleanupPropagatesError(t *testing.T) {
	sentinel := errors.New("bridge: close 失败")
	p := &fakePlatform{closeErrs: []error{sentinel}}
	if err := newTSec(p).Cleanup(context.Background(), harness.Challenge{Code: "a"}); !errors.Is(err, sentinel) {
		t.Fatalf("Cleanup 必须原样上抛平台错误，got %v", err)
	}
	// 正常关题返回 nil。
	p2 := &fakePlatform{closeResult: harness.CloseResult{Code: "a", Closed: true}}
	if err := newTSec(p2).Cleanup(context.Background(), harness.Challenge{Code: "a"}); err != nil {
		t.Fatalf("正常关题不应报错，got %v", err)
	}
	if _, _, _, _, c := p2.counts(); c != 1 {
		t.Errorf("Cleanup 应恰好调用一次 Close，got %d", c)
	}
}

// ── 契约断言 ──

func TestTSecBenchImplementsScenario(t *testing.T) {
	var s harness.Scenario = newTSec(&fakePlatform{})
	if s == nil {
		t.Fatal("TSecBench 必须实现 harness.Scenario")
	}
}

// 六个方法在平台未设置时都必须 fail closed：不得静默返回零值继续跑。
// 尤其 Prepare —— 它决定白名单，静默返回空地址会让出站全被拒绝而看不出原因。
func TestTSecBenchNilPlatformFailsClosed(t *testing.T) {
	s := &TSecBench{}
	ctx := context.Background()
	c := harness.Challenge{Code: "a"}
	if _, err := s.Prepare(ctx, c); !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("Prepare 缺平台应返回 KindConfig，got %v", err)
	}
	if _, err := s.Discover(ctx, harness.RunSpec{}); !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("Discover 缺平台应返回 KindConfig，got %v", err)
	}
}

// 轮询间隔与时限的默认值：不设时不能变成「忙等」或「瞬间超时」。
func TestTSecBenchPrepareDefaultPollingIsBounded(t *testing.T) {
	// 默认 PollEvery=500ms、PollLimit=2m。这里只断言默认值让调用有界：
	// 用一个已经取消的 ctx，Prepare 必须立刻返回取消错误而不是转起来。
	p := &fakePlatform{list: []harness.Challenge{{Code: "a", Addrs: []string{"10.0.0.1:1"}}}}
	s := &TSecBench{Platform: p}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { _, err := s.Prepare(ctx, harness.Challenge{Code: "a"}); done <- err }()
	select {
	case err := <-done:
		if err != nil && !harness.IsKind(err, harness.KindCancelled) && !harness.IsKind(err, harness.KindPlatform) {
			t.Errorf("取消后应返回 KindCancelled/KindPlatform，got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("已取消的 ctx 下 Prepare 不得挂住")
	}
}
