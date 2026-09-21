package scenario

import (
	"context"
	"sync"
	"testing"

	harness "github.com/red-copilot/red-harness"
)

// 本文件是 Fake 的**全生命周期契约测试**：六方法的正常路径 + 每条 nil 安全路径
// 各一个用例。测试与实现同包（白盒），因为要断言的是内部账本是否被真的动过
// （started / targets）——那些状态没有公开访问器，而「回收到底做了什么」正是
// v0.4 要钉住的东西。

// newFake 造一个「两道题、每题两个可接受答案」的离线场景，供多数组使用例复用。
func newFake() *Fake {
	return &Fake{
		Challenges: []harness.Challenge{
			{Code: "demo-1", FlagCount: 2, Addrs: []string{"10.0.0.1:8080"}, Category: "web"},
			{Code: "demo-2", FlagCount: 1, Addrs: []string{"10.0.0.2:9999"}},
		},
		Answers: map[string]map[string]bool{
			"demo-1": {"flag{a}": true, "flag{b}": true},
			"demo-2": {"flag{c}": true},
		},
	}
}

func ch(t *testing.T, f *Fake, code string) harness.Challenge {
	t.Helper()
	list, err := f.Discover(context.Background(), harness.RunSpec{})
	if err != nil {
		t.Fatalf("Discover 不应报错: %v", err)
	}
	for _, c := range list {
		if c.Code == code {
			return c
		}
	}
	t.Fatalf("测试夹具里没有题目 %s", code)
	return harness.Challenge{}
}

// ── Discover ──

// Discover 必须返回**深拷贝**：返回切片里的 Challenge.Addrs 与内部是两套底层
// 数组。只做 `append([]Challenge(nil), f.Challenges...)` 是浅拷贝，调用方往
// 返回值的 Addrs 里写一个字节就改掉了内部声明的题目地址——而这条地址正是
// 沙箱出站白名单的唯一来源。
func TestFakeDiscoverReturnsDeepCopy(t *testing.T) {
	f := newFake()
	got, err := f.Discover(context.Background(), harness.RunSpec{})
	if err != nil {
		t.Fatalf("Discover 报错: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Discover 返回 %d 道题，期望 2", len(got))
	}
	// 篡改返回值：改切片元素、改地址切片内容、再 append。
	got[0].Code = "污染"
	got[0].Addrs[0] = "0.0.0.0:1"
	got = append(got, harness.Challenge{Code: "污染-2"})

	again, err := f.Discover(context.Background(), harness.RunSpec{})
	if err != nil {
		t.Fatalf("第二次 Discover 报错: %v", err)
	}
	if len(again) != 2 {
		t.Fatalf("调用方 append 污染了内部题目集合：第二次返回 %d 道题，期望 2", len(again))
	}
	if again[0].Code != "demo-1" || again[0].Addrs[0] != "10.0.0.1:8080" {
		t.Errorf("调用方改写了 Fake 内部状态：第二次拿到 %q / %v", again[0].Code, again[0].Addrs)
	}
}

// ── Prepare ──

// Prepare 返回的地址必须与 Challenge.Addrs 逐字一致，且重复调用稳定：
// 它同时是白名单唯一来源与幂等重试的输入。
func TestFakePrepareReturnsStableAllowlist(t *testing.T) {
	f := newFake()
	c := ch(t, f, "demo-1")
	first, err := f.Prepare(context.Background(), c)
	if err != nil {
		t.Fatalf("Prepare 报错: %v", err)
	}
	if first.Code != "demo-1" || first.Network != "tcp" {
		t.Errorf("Prepare 返回 %+v，期望 code=demo-1 network=tcp", first)
	}
	if !sameAddrs(first.Addrs, c.Addrs) {
		t.Errorf("Prepare 地址 %v 与 Challenge.Addrs %v 不一致——白名单必须来自题目声明", first.Addrs, c.Addrs)
	}
	// 改返回值不能影响内部记下的白名单。
	first.Addrs[0] = "0.0.0.0:1"

	second, err := f.Prepare(context.Background(), c)
	if err != nil {
		t.Fatalf("重复 Prepare 报错: %v", err)
	}
	if !sameAddrs(second.Addrs, c.Addrs) {
		t.Errorf("调用方改写了 Prepare 返回值，第二次拿到 %v", second.Addrs)
	}
	if !f.started["demo-1"] {
		t.Error("Prepare 之后题目应处于「容器已起」状态")
	}
	if _, ok := f.targets["demo-1"]; !ok {
		t.Error("Prepare 应把返回过的 Target 钉住，供重复起题复用")
	}
}

// 同一道题第二次带着**不同的地址**来起题 ⇒ fail closed。
//
// 为什么是错误而不是「以最新为准」：白名单只能有一个来源，允许漂移等于允许
// 调用方在第二次起题时悄悄扩大授权范围。
func TestFakePrepareRejectsAddrDrift(t *testing.T) {
	f := newFake()
	c := ch(t, f, "demo-1")
	if _, err := f.Prepare(context.Background(), c); err != nil {
		t.Fatalf("首次 Prepare 报错: %v", err)
	}
	drifted := c
	drifted.Addrs = []string{"10.0.0.1:8080", "169.254.169.254:80"}
	if _, err := f.Prepare(context.Background(), drifted); !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("地址漂移应返回 KindConfig，got %v", err)
	}
}

// 题目没声明地址时 Prepare 不得 panic，返回空地址集。
func TestFakePrepareWithoutAddrs(t *testing.T) {
	f := &Fake{Challenges: []harness.Challenge{{Code: "no-addr", FlagCount: 1}}}
	c := ch(t, f, "no-addr")
	tgt, err := f.Prepare(context.Background(), c)
	if err != nil {
		t.Fatalf("Prepare 报错: %v", err)
	}
	if len(tgt.Addrs) != 0 {
		t.Errorf("无地址题目应返回空 Addrs，got %v", tgt.Addrs)
	}
}

// ── Hint ──

// 离线场景没有提示：返回空串且不报错（契约允许「没有提示」）。
func TestFakeHintIsEmptyWithoutError(t *testing.T) {
	f := newFake()
	h, err := f.Hint(context.Background(), ch(t, f, "demo-1"))
	if err != nil || h != "" {
		t.Errorf("Hint = (%q, %v)，期望空串且无错误", h, err)
	}
}

// ── Evaluate ──

// Answers 为 nil 时不得 panic：读 nil map 在 Go 里是安全的（返回零值），
// 所以「整体 nil」与「某题没有条目」走的是同一条路径。
func TestFakeEvaluateNilAnswersIsRejected(t *testing.T) {
	f := &Fake{Challenges: []harness.Challenge{{Code: "demo-1", FlagCount: 1}}}
	ev, err := f.Evaluate(context.Background(), harness.Challenge{Code: "demo-1", FlagCount: 1}, "flag{a}")
	if err != nil {
		t.Fatalf("未声明答案不应报错（那是平台的正常判定）: %v", err)
	}
	if ev.Accepted || ev.Progress || ev.Completed {
		t.Errorf("nil Answers 下任何答案都应判不接受，got %+v", ev)
	}
	// 该题有 map 但里面没有这个答案，同样是「不接受」。
	f2 := &Fake{Answers: map[string]map[string]bool{"demo-1": {"flag{a}": true}}}
	if ev, err := f2.Evaluate(context.Background(), harness.Challenge{Code: "demo-1"}, "flag{x}"); err != nil || ev.Accepted {
		t.Errorf("未声明的答案应被拒且不报错，got (%+v, %v)", ev, err)
	}
}

// 未声明的答案是**判定**不是**故障**：引擎据此走「判错」分支（记指纹、永不重提），
// 若这里返回 error，引擎会去 Reconcile 甚至中止整道题。
func TestFakeEvaluateUndeclaredAnswerIsRejectedNotError(t *testing.T) {
	f := newFake()
	ev, err := f.Evaluate(context.Background(), ch(t, f, "demo-1"), "flag{nope}")
	if err != nil {
		t.Fatalf("未声明答案不得报错，got %v", err)
	}
	if ev.Accepted || ev.Progress || ev.Completed || ev.Score != 0 {
		t.Errorf("未声明答案的判定应全为假且零分，got %+v", ev)
	}
	if ev.Message == "" {
		t.Error("判定应带一条可进报告的说明（Message）")
	}
}

// 幂等命中：同一答案提交两次 ⇒ 第二次 Accepted=true、Progress=false。
//
// 这是 v0.4「平台 duplicate 等价于已确认」的契约。两个字段**都**要断言：
// 只测 Accepted 会漏掉「把幂等命中算成进展」这个缺陷——那会让停滞检测永远
// 看不到停滞，卡住的题无限续命。
func TestFakeEvaluateDuplicateIsAcceptedWithoutProgress(t *testing.T) {
	f := newFake()
	c := ch(t, f, "demo-1")
	first, err := f.Evaluate(context.Background(), c, "flag{a}")
	if err != nil {
		t.Fatalf("首次提交报错: %v", err)
	}
	if !first.Accepted || !first.Progress {
		t.Fatalf("首次提交应 Accepted+Progress，got %+v", first)
	}
	second, err := f.Evaluate(context.Background(), c, "flag{a}")
	if err != nil {
		t.Fatalf("重复提交报错: %v", err)
	}
	if !second.Accepted {
		t.Error("幂等命中必须 Accepted=true——平台 duplicate 等价于已确认")
	}
	if second.Progress {
		t.Error("幂等命中不得 Progress=true——它没有让平台侧前进")
	}
	if second.Score != first.Score {
		t.Errorf("重复提交不得改变累计得分：%d → %d", first.Score, second.Score)
	}
}

// Score 是**累计得分**（确认数 × 分值），不是常量 1。
//
// 恒为 1 会让报告里的得分既不可比也不可加：M3 的「得分」指标要按题聚合，
// 而每题 flag 数不同。
func TestFakeEvaluateScoreIsCumulative(t *testing.T) {
	f := newFake()
	f.ScorePerFlag = 10
	c := ch(t, f, "demo-1")
	a, err := f.Evaluate(context.Background(), c, "flag{a}")
	if err != nil {
		t.Fatalf("提交 flag{a} 报错: %v", err)
	}
	if a.Score != 10 {
		t.Errorf("确认 1 个 flag、分值 10 ⇒ 累计 10，got %d", a.Score)
	}
	b, err := f.Evaluate(context.Background(), c, "flag{b}")
	if err != nil {
		t.Fatalf("提交 flag{b} 报错: %v", err)
	}
	if b.Score != 20 {
		t.Errorf("确认 2 个 flag、分值 10 ⇒ 累计 20，got %d", b.Score)
	}
	if !b.Completed {
		t.Errorf("FlagCount=2 且两个 flag 都确认 ⇒ Completed=true，got %+v", b)
	}
	// 分值未设时按 1 计，仍然是累计值。
	f2 := newFake()
	c2 := ch(t, f2, "demo-2")
	if ev, _ := f2.Evaluate(context.Background(), c2, "flag{c}"); ev.Score != 1 {
		t.Errorf("未设 ScorePerFlag 时每个确认按 1 分，got %d", ev.Score)
	}
}

// FlagCount 未知（0）时，Fake 回落到**自己声明的答案集合**作为分母。
//
// 为什么不能只信 ch.FlagCount：Discover 返回的 Challenge 是调用方填的，而
// FlagCount==0 在根包契约里的含义是「未知」。若 Completed 只认它，调用方没填时
// Completed 恒假——M1 的「通关立即终止」永远不触发，测试会把「已经解完」误读成
// 「没解出来」。Fake 自己知道答案全集，所以它能给出分母；两者都没有时才是真正
// 的未知，那时 Completed 恒假（fail closed）。
func TestFakeEvaluateCompletedFallsBackToDeclaredAnswers(t *testing.T) {
	f := &Fake{
		Challenges: []harness.Challenge{{Code: "demo-x"}}, // FlagCount 留空 = 未知
		Answers:    map[string]map[string]bool{"demo-x": {"flag{a}": true, "flag{b}": true}},
	}
	c := ch(t, f, "demo-x")
	first, err := f.Evaluate(context.Background(), c, "flag{a}")
	if err != nil {
		t.Fatalf("提交报错: %v", err)
	}
	if first.Completed {
		t.Error("只确认了 2 个里的 1 个，不得宣称完成")
	}
	second, err := f.Evaluate(context.Background(), c, "flag{b}")
	if err != nil {
		t.Fatalf("提交报错: %v", err)
	}
	if !second.Completed {
		t.Error("声明了 2 个答案且都已确认 ⇒ Completed=true（FlagCount 未知时回落到声明集合）")
	}
}

// ── Reconcile ──

// confirmed 为 nil 时不得 panic，且不得宣称完成。
func TestFakeReconcileNilLedgerIsNotCompleted(t *testing.T) {
	f := &Fake{}
	obj, err := f.Reconcile(context.Background(), harness.Challenge{Code: "demo-1"})
	if err != nil {
		t.Fatalf("Reconcile 不应报错: %v", err)
	}
	if obj.Completed {
		t.Error("账本为空且分母未知时不得宣称完成")
	}
	if obj.Kind != "flag_count" || obj.Want != 0 || obj.Got != 0 {
		t.Errorf("空账本的 Objective 应为 flag_count 0/0，got %+v", obj)
	}
}

// FlagCount=0（未知）且没有声明答案 ⇒ Completed 恒假。
//
// 分母未知时宣称完成是最坏的一类错误：报告会记成「解出来了」，而实际上可能
// 还差好几个 flag。
func TestFakeReconcileUnknownFlagCountNeverCompletes(t *testing.T) {
	f := &Fake{Challenges: []harness.Challenge{{Code: "demo-1"}}}
	c := ch(t, f, "demo-1")
	obj, err := f.Reconcile(context.Background(), c)
	if err != nil {
		t.Fatalf("Reconcile 报错: %v", err)
	}
	if obj.Completed {
		t.Errorf("FlagCount 未知（0）时 Completed 必须为假，got %+v", obj)
	}
	// 即使账本里有条目（不可能通过 Evaluate 产生，但状态是可达的），
	// 分母未知就不得完成。
	f.confirmed = map[string]map[string]bool{"demo-1": {"flag{a}": true, "flag{b}": true}}
	if obj, _ := f.Reconcile(context.Background(), c); obj.Completed {
		t.Errorf("分母未知时不得因账本非空而完成，got %+v", obj)
	}
}

// Reconcile 以**账本**为准：提交过一个之后 Got 必须变成 1，全部确认后 Completed。
func TestFakeReconcileReportsPlatformProgress(t *testing.T) {
	f := newFake()
	c := ch(t, f, "demo-1")
	if _, err := f.Evaluate(context.Background(), c, "flag{a}"); err != nil {
		t.Fatalf("提交报错: %v", err)
	}
	obj, err := f.Reconcile(context.Background(), c)
	if err != nil {
		t.Fatalf("Reconcile 报错: %v", err)
	}
	if obj.Want != 2 || obj.Got != 1 || obj.Completed {
		t.Errorf("确认 1/2 ⇒ Objective{2,1,false}，got %+v", obj)
	}
	if _, err := f.Evaluate(context.Background(), c, "flag{b}"); err != nil {
		t.Fatalf("提交报错: %v", err)
	}
	if obj, _ := f.Reconcile(context.Background(), c); !obj.Completed || obj.Got != 2 {
		t.Errorf("确认 2/2 ⇒ Completed=true Got=2，got %+v", obj)
	}
}

// ── Cleanup ──

// Cleanup 幂等：正常结束、取消、失败三条回收路径都会调它，重复调用不得报错。
// 一次重复调用报错就会把真正的「为什么停」盖掉。
func TestFakeCleanupIsIdempotent(t *testing.T) {
	f := newFake()
	c := ch(t, f, "demo-1")
	if _, err := f.Prepare(context.Background(), c); err != nil {
		t.Fatalf("Prepare 报错: %v", err)
	}
	if err := f.Cleanup(context.Background(), c); err != nil {
		t.Fatalf("首次 Cleanup 报错: %v", err)
	}
	if f.started["demo-1"] {
		t.Error("Cleanup 必须真的回收容器状态，而不是只返回 nil")
	}
	if err := f.Cleanup(context.Background(), c); err != nil {
		t.Errorf("重复 Cleanup 必须幂等，got %v", err)
	}
	// 从未起过的题也能清理（delete 不存在的键是 no-op）。
	if err := f.Cleanup(context.Background(), harness.Challenge{Code: "从未起过"}); err != nil {
		t.Errorf("清理未知题目必须幂等，got %v", err)
	}
	// 回收后重新起题仍然拿到同一份白名单。
	again, err := f.Prepare(context.Background(), c)
	if err != nil {
		t.Fatalf("回收后重新 Prepare 报错: %v", err)
	}
	if !sameAddrs(again.Addrs, c.Addrs) {
		t.Errorf("回收后重新起题地址漂移：%v", again.Addrs)
	}
}

// 关题不清账本：平台已确认的答案不会因为关题而失效。清掉会让「同一答案永不
// 重提」与幂等命中判定在同一进程的后续题目上失效。
func TestFakeCleanupKeepsConfirmedLedger(t *testing.T) {
	f := newFake()
	c := ch(t, f, "demo-1")
	if _, err := f.Evaluate(context.Background(), c, "flag{a}"); err != nil {
		t.Fatalf("提交报错: %v", err)
	}
	if err := f.Cleanup(context.Background(), c); err != nil {
		t.Fatalf("Cleanup 报错: %v", err)
	}
	obj, err := f.Reconcile(context.Background(), c)
	if err != nil {
		t.Fatalf("Reconcile 报错: %v", err)
	}
	if obj.Got != 1 {
		t.Errorf("关题不应清空已确认账本，Got=%d 期望 1", obj.Got)
	}
}

// ── 并发 ──

// 同一答案被并发提交时，只能有一次 Progress=true，且累计得分只加一次。
// 这条用 -race 跑：Fake 被 Harness 串行调用，但账本读写的原子性不能靠调用方。
func TestFakeConcurrentEvaluateIsAtomic(t *testing.T) {
	f := newFake()
	f.ScorePerFlag = 7
	c := ch(t, f, "demo-1")
	const n = 16
	var wg sync.WaitGroup
	progress := make([]bool, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ev, err := f.Evaluate(context.Background(), c, "flag{a}")
			progress[i], errs[i] = ev.Progress, err
		}(i)
	}
	wg.Wait()
	seen := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("并发提交报错: %v", errs[i])
		}
		if progress[i] {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("并发提交同一答案应恰好 1 次 Progress=true，got %d", seen)
	}
	obj, _ := f.Reconcile(context.Background(), c)
	if obj.Got != 1 {
		t.Errorf("并发提交后账本应有 1 个确认，got %d", obj.Got)
	}
}

// ── 契约断言 ──

// Fake 必须是 harness.Scenario：编译期断言写在实现文件里，这里再钉一条运行期
// 的，防止有人把断言删掉后「编译绿但契约未对齐」。
func TestFakeImplementsScenario(t *testing.T) {
	var s harness.Scenario = newFake()
	if s == nil {
		t.Fatal("Fake 必须实现 harness.Scenario")
	}
	if _, err := s.Discover(context.Background(), harness.RunSpec{}); err != nil {
		t.Errorf("Discover 经接口调用报错: %v", err)
	}
	if err := s.Cleanup(context.Background(), harness.Challenge{Code: "x"}); err != nil {
		t.Errorf("Cleanup 经接口调用报错: %v", err)
	}
}
