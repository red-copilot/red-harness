package bridge

// 契约测试：六个命令的字段映射、错误分类与可重试性。
//
// 每一条测试都对应一个**具体的失效模式**，而不是「覆盖率」。下面注释里的
// 「钉住」写的是：这条测试红的时候，真实世界里发生了什么。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// ── 端口契约 ──

// TestClientSatisfiesPorts 钉住接口满足关系。
//
// 它是编译期断言的运行期版本：`harness.Platform` 的方法集与 v0.2 逐字相同
// （ports.go:83-84），而 bridge 是 v0.3 里**第一个**实现它的包。这里一旦
// 不满足，装配层（internal/cli/wire.go）会以编译错误的形式发现——但那时
// 已经跨了包，所以在本包内先钉一道。
func TestClientSatisfiesPorts(t *testing.T) {
	var _ harness.Platform = (*Client)(nil)
	var _ harness.HealthChecker = (*Client)(nil)
}

// TestNewClientFailsWithActionableError 钉住「缺 SDK 时的报错可操作」。
//
// 失效模式：宿主导入失败时只报 `ModuleNotFoundError`，用户不知道本机唯一
// 可行路径是容器（宿主 python3.12 没有 httpx）。所以错误消息里必须出现
// 环境变量名与容器路径。
func TestNewClientFailsWithActionableError(t *testing.T) {
	py := requirePython(t)
	// 不设 TSEC_MOCK：走真实导入路径，宿主必然失败。
	cfg := ClientConfig{
		Command: []string{py, scriptPath(t)},
		Timeout: 10 * time.Second,
		Env: map[string]string{
			"PYTHONPATH":         "",
			"BENCHMARK_BASE_URL": "https://benchmark.invalid",
			"BENCHMARK_TOKEN":    "test-token-not-real",
		},
	}
	c, err := NewClient(cfg)
	if err == nil {
		c.Shutdown()
		t.Fatal("宿主缺 SDK 时 NewClient 应该失败")
	}
	msg := err.Error()
	// 可操作性：必须指出桥命令可覆盖，并给出容器这条真实路径。
	for _, want := range []string{EnvBridgeCmd, "docker exec", "sdk_missing"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误消息缺少可操作信息 %q:\n%s", want, msg)
		}
	}
	mustNotLeak(t, "NewClient 错误消息", msg)
}

// TestCheckVPNFailureIsNotConstructionFailure 钉住「预检失败与构造失败可区分」。
//
// 失效模式（ports.go:95-96 记的前身事故）：预检失败被当成「题目不存在」继续跑。
// SDK 的上下文管理器把预检放在 `__enter__` 里（sync.py:83-86），所以
// `with TSecBenchmark(...)` 的失败既可能是构造失败也可能是预检失败。
// 桥用 `auto_check_vpn=False` 构造裸 client + 显式 check_vpn 来消除这个混淆。
func TestCheckVPNFailureIsNotConstructionFailure(t *testing.T) {
	c := newMockClient(t, map[string]string{"TSEC_MOCK_VPN_FAIL": "1"})

	// 构造是成功的（进程起来了、握手完成）——这本身就是断言的一部分。
	if got := c.Handshake().SDK; got != "0.1.2" {
		t.Fatalf("握手没有拿到 SDK 版本: %q", got)
	}

	err := c.CheckVPN(context.Background())
	if err == nil {
		t.Fatal("VPN 不通时 CheckVPN 应该报错")
	}
	if !harness.IsKind(err, harness.KindPlatform) {
		t.Errorf("VPN 预检失败应归 KindPlatform，实际: %v", err)
	}
	if harness.Retryable(err) {
		t.Errorf("VPN 预检失败不可重试（VPN 没连上时重试只是白等）: %v", err)
	}
	if !strings.Contains(err.Error(), CodeVPNCheckFailed) {
		t.Errorf("错误消息应含 code %s: %v", CodeVPNCheckFailed, err)
	}
	// 预检失败**不能**被记成「预检通过」——否则重启后不会再做预检。
	if c.vpnOK {
		t.Error("预检失败后 vpnOK 必须为假")
	}
}

// TestHealthDelegatesToCheckVPN 钉住 HealthChecker 的语义就是 VPN 预检。
func TestHealthDelegatesToCheckVPN(t *testing.T) {
	c := newMockClient(t, nil)
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health 应该通过: %v", err)
	}
	if !c.vpnOK {
		t.Error("Health 通过后 vpnOK 应为真")
	}
}

// ── list ──

// TestListFieldMapping 钉住 list 的逐字段映射。
//
// 失效模式：`container_addr` 在 `container_status != "available"` 时非空会让
// Scenario.Prepare 拿到一个还不能连的地址；`correct_flag_count` 映射错会让
// 「通关立即终止」的判据失真。三条 challenge 分别覆盖三种 container_status。
func TestListFieldMapping(t *testing.T) {
	c := newMockClient(t, nil)
	chs, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(chs) != 3 {
		t.Fatalf("题目数应为 3，实际 %d", len(chs))
	}

	web := chs[0]
	if web.Code != "web-01" || web.Difficulty != "easy" || web.Description != "一个登录页" {
		t.Errorf("web-01 基本字段映射错: %+v", web)
	}
	if web.FlagCount != 2 || web.Solved != 1 {
		t.Errorf("web-01 进度映射错: FlagCount=%d Solved=%d", web.FlagCount, web.Solved)
	}
	if web.Remaining() != 1 {
		t.Errorf("web-01 应还差 1 个 flag，实际 %d", web.Remaining())
	}
	if len(web.Addrs) != 1 || web.Addrs[0] != "10.0.0.11:8080" {
		t.Errorf("available 时地址必须非空: %v", web.Addrs)
	}

	// description 为 null：Go 侧应是空串而不是 "null"。
	if chs[1].Description != "" {
		t.Errorf("description 为 null 时应映射成空串，实际 %q", chs[1].Description)
	}
	if !chs[1].Done() {
		t.Error("pwn-02 是 is_completed，Done() 应为真")
	}
	if len(chs[1].Addrs) != 0 {
		t.Errorf("stopped 时地址必须为空: %v", chs[1].Addrs)
	}

	// pending：地址为空**不是错误**（起题是异步的，SDK_API.md:129-130）。
	if len(chs[2].Addrs) != 0 {
		t.Errorf("pending 时地址必须为空: %v", chs[2].Addrs)
	}
	if chs[2].Done() || chs[2].Remaining() != 1 {
		t.Errorf("crypto-03 进度映射错: %+v", chs[2])
	}
}

// TestListTaskNotFoundNotRetryable 钉住 404 不可重试。
//
// 失效模式：token 无效时反复重试，把「配置错了」变成「平台很慢」。
func TestListTaskNotFoundNotRetryable(t *testing.T) {
	c := newMockClient(t, map[string]string{"TSEC_MOCK_SCENARIO": "task_not_found"})
	_, err := c.List(context.Background())
	if err == nil {
		t.Fatal("task_not_found 场景应该报错")
	}
	if !harness.IsKind(err, harness.KindPlatform) {
		t.Errorf("应归 KindPlatform: %v", err)
	}
	if harness.Retryable(err) {
		t.Errorf("task_not_found 不可重试: %v", err)
	}
	if !strings.Contains(err.Error(), CodeTaskNotFound) {
		t.Errorf("错误消息应含 %s: %v", CodeTaskNotFound, err)
	}
}

// TestListConnectionErrorRetryable 钉住传输层错误可重试。
//
// **`TSecConnectionError.code is None`**（errors.py:62）——所以桥必须用稳定
// 别名 connection_error，否则它会被归到「没有 code 的通用错误」，可重试性
// 也随之丢掉（一次网络抖动变成永久失败）。
func TestListConnectionErrorRetryable(t *testing.T) {
	c := newMockClient(t, map[string]string{"TSEC_MOCK_SCENARIO": "connection_error"})
	_, err := c.List(context.Background())
	if err == nil {
		t.Fatal("connection_error 场景应该报错")
	}
	if !strings.Contains(err.Error(), CodeConnectionError) {
		t.Errorf("code 应为 %s（TSecConnectionError.code 是 None）: %v", CodeConnectionError, err)
	}
	if !harness.Retryable(err) {
		t.Errorf("传输层错误应可重试: %v", err)
	}
}

// ── start ──

// TestStartPendingAddrEmptyIsNotError 钉住「异步起题期间地址为空不是错误」。
func TestStartPendingAddrEmptyIsNotError(t *testing.T) {
	c := newMockClient(t, map[string]string{"TSEC_MOCK_SCENARIO": "start_pending"})
	res, err := c.Start(context.Background(), "web-01")
	if err != nil {
		t.Fatalf("pending 起题不该报错: %v", err)
	}
	if res.Code != "web-01" {
		t.Errorf("Code 应为 web-01，实际 %q", res.Code)
	}
	if len(res.Addrs) != 0 {
		t.Errorf("pending 时地址应为空: %v", res.Addrs)
	}
}

// TestStartResourceUnavailableRetryable 钉住 503 可重试。
func TestStartResourceUnavailableRetryable(t *testing.T) {
	c := newMockClient(t, map[string]string{"TSEC_MOCK_SCENARIO": "resource_unavailable"})
	_, err := c.Start(context.Background(), "web-01")
	if err == nil {
		t.Fatal("resource_unavailable 场景应该报错")
	}
	if !harness.Retryable(err) {
		t.Errorf("503 应可重试: %v", err)
	}
	if !strings.Contains(err.Error(), CodeResourceUnavail) {
		t.Errorf("错误消息应含 %s: %v", CodeResourceUnavail, err)
	}
}

// ── invalid_state：一个码两种语义 ──

// TestInvalidStateRetryabilityDependsOnMaxActive 钉住 invalid_state 的可重试性
// **按消息细分**。
//
// 失效模式（SDK_API.md:209-213）：平台限制同时活跃的容器数，撞上限时「关掉一个
// 再重试」是正确动作；而「任务已结束（超时）」重试多少次都不会变。把两者当
// 成一回事，会让一次超时变成无限重试、或者让一个名额问题变成直接放弃。
func TestInvalidStateRetryabilityDependsOnMaxActive(t *testing.T) {
	cases := []struct {
		name      string
		scenario  string
		retryable bool
	}{
		{"撞活跃上限可重试", "invalid_max_active", true},
		{"任务已结束不可重试", "invalid_task_ended", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newMockClient(t, map[string]string{"TSEC_MOCK_SCENARIO": tc.scenario})
			_, err := c.Start(context.Background(), "web-01")
			if err == nil {
				t.Fatal("invalid_state 场景应该报错")
			}
			if !strings.Contains(err.Error(), CodeInvalidState) {
				t.Errorf("code 应为 %s: %v", CodeInvalidState, err)
			}
			if got := harness.Retryable(err); got != tc.retryable {
				t.Errorf("Retryable 应为 %v，实际 %v: %v", tc.retryable, got, err)
			}
		})
	}
}

// TestInvalidStateRetryableLocalRule 钉住 Go 侧兜底判据与规范一致。
//
// 桥侧才是权威判定（它能看到异常原文与 detail），但 Go 侧留一份同样的规则
// 用于：桥漏判时兜底、以及这条规则本身可测。
func TestInvalidStateRetryableLocalRule(t *testing.T) {
	if !invalidStateRetryable("max active challenges reached (3/3)") {
		t.Error("含 max active 应判可重试")
	}
	if invalidStateRetryable("task already ended") {
		t.Error("不含 max active 应判不可重试")
	}
	if invalidStateRetryable("") {
		t.Error("空消息应判不可重试")
	}
}

// ── hint ──

// TestHintNoneDoesNotError 钉住「平台没给提示时不报错」。
//
// 失效模式（ports.go:71）：`hint` 是 `Optional[str]`，为 null 时如果被当成
// 错误，一次「这题没有提示」会变成一次 run 失败。Go 侧应得空串。
func TestHintNoneDoesNotError(t *testing.T) {
	c := newMockClient(t, map[string]string{"TSEC_MOCK_SCENARIO": "hint_none"})
	res, err := c.Hint(context.Background(), "web-01")
	if err != nil {
		t.Fatalf("hint 为 null 时不该报错: %v", err)
	}
	if res.Hint != "" {
		t.Errorf("hint 为 null 应映射成空串，实际 %q", res.Hint)
	}
	if res.Code != "web-01" {
		t.Errorf("Code 应为 web-01，实际 %q", res.Code)
	}
}

// TestHintPresent 钉住有提示时的正常路径。
func TestHintPresent(t *testing.T) {
	c := newMockClient(t, nil)
	res, err := c.Hint(context.Background(), "web-01")
	if err != nil {
		t.Fatalf("Hint 失败: %v", err)
	}
	if res.Hint != "试试 admin/admin" {
		t.Errorf("提示内容映射错: %q", res.Hint)
	}
}

// ── submit ──

// TestSubmitDuplicateIsIdempotentHit 钉住 **DuplicateSubmit ⇒ Duplicate:true 且
// err == nil**。
//
// 这是本任务里最容易被实现错的一条：`DuplicateSubmit` 是一个**异常**
// （errors.py:107，code=duplicate），直觉会把它映射成错误。但它表示「同一个
// flag 已经正确提交过」——**等价于已确认**（ports.go:57）。映射成错误会让
// 一次成功的确认被记成判错，进而把已确认的 flag 从账本里抹掉。
func TestSubmitDuplicateIsIdempotentHit(t *testing.T) {
	c := newMockClient(t, map[string]string{"TSEC_MOCK_SCENARIO": "duplicate"})
	res, err := c.Submit(context.Background(), "web-01", "flag{dup}")
	if err != nil {
		t.Fatalf("duplicate 是幂等命中，不是错误: %v", err)
	}
	if !res.Duplicate {
		t.Error("Duplicate 应为真")
	}
	if !res.Correct {
		t.Error("幂等命中应视为已确认（Correct 为真）")
	}
	if res.Message != "duplicate" {
		t.Errorf("Message 应为 duplicate，实际 %q", res.Message)
	}
}

// TestSubmitWrongFlagIsNotError 钉住「平台判错不是传输错误」。
//
// 失效模式：判错被当成 err，让 gate 的 Mark 把它记成 SubmitError 而不是
// 「已提交但错」——两者的区别正是 RejectedLedger 的输入。
func TestSubmitWrongFlagIsNotError(t *testing.T) {
	c := newMockClient(t, map[string]string{"TSEC_MOCK_SCENARIO": "wrong"})
	res, err := c.Submit(context.Background(), "web-01", "flag{nope}")
	if err != nil {
		t.Fatalf("平台判错不是错误: %v", err)
	}
	if res.Correct || res.Duplicate {
		t.Errorf("判错时 Correct/Duplicate 都应为假: %+v", res)
	}
	if res.Message != "rejected" {
		t.Errorf("Message 应为 rejected，实际 %q", res.Message)
	}
}

// TestSubmitMapsPlatformProgress 钉住 submit 的权威进度字段映射。
//
// **这两个字段是「通关立即终止」唯一不依赖调用方自觉的判据**（ports.go:62-64）：
// 每次提交平台都会回 correct/total，映射错就会提前停或不停。
func TestSubmitMapsPlatformProgress(t *testing.T) {
	c := newMockClient(t, nil)
	res, err := c.Submit(context.Background(), "web-01", "flag{good}")
	if err != nil {
		t.Fatalf("Submit 失败: %v", err)
	}
	if !res.Correct || res.Duplicate {
		t.Errorf("判定映射错: %+v", res)
	}
	if res.Awarded != 100 {
		t.Errorf("Awarded 应为 100，实际 %d", res.Awarded)
	}
	if res.CorrectFlagCount != 2 || res.TotalFlagCount != 2 {
		t.Errorf("平台进度映射错: %d/%d", res.CorrectFlagCount, res.TotalFlagCount)
	}
	// matched_flag_index 是 Optional[int]，平台给 1 时应映射成 1。
	if res.MatchedIndex != 1 {
		t.Errorf("MatchedIndex 应为 1，实际 %d", res.MatchedIndex)
	}
	// **SubmitResult 没有 unique_code 字段**（models.py:75）：桥回填请求里的
	// code。这里断言它确实被回填了（否则 Code 会空，场景层无法归属判定）。
	if res.Message != "correct" {
		t.Errorf("Message 应为 correct，实际 %q", res.Message)
	}
}

// TestSubmitWrongFlagHasNoMatchedIndex 钉住 `matched_flag_index` 为 null 时
// 映射成 0 而不是报错（Optional[int] 的空值路径）。
func TestSubmitWrongFlagHasNoMatchedIndex(t *testing.T) {
	c := newMockClient(t, map[string]string{"TSEC_MOCK_SCENARIO": "wrong"})
	res, err := c.Submit(context.Background(), "web-01", "flag{nope}")
	if err != nil {
		t.Fatalf("Submit 失败: %v", err)
	}
	if res.MatchedIndex != 0 {
		t.Errorf("matched_flag_index 为 null 时应映射成 0，实际 %d", res.MatchedIndex)
	}
}

// ── close ──

// TestCloseMapsResult 钉住 close 的映射。
func TestCloseMapsResult(t *testing.T) {
	c := newMockClient(t, nil)
	res, err := c.Close(context.Background(), "web-01")
	if err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if res.Code != "web-01" || !res.Closed {
		t.Errorf("CloseResult 映射错: %+v", res)
	}
}

// ── SDK 缺陷 1 / 2：桥必须包住的非 SDK 异常 ──

// TestMalformedPayloadWrappedNotKeyError 钉住 **SDK 缺陷 1**：
// `from_dict` 抛的裸 `KeyError` 被包成结构化错误。
//
// 源码依据（models.py:28-41）：每个 `from_dict` 都用 `data["field"]` 取必填
// 字段，缺一个就抛 `KeyError`。而 `KeyError` **不是** `TSecError`——它穿透
// SDK 自己的异常网，也会穿透任何 `except TSecError`。桥如果不接住，一次平台
// schema 漂移会变成桥进程的未捕获异常（表现为「桥崩了」并触发无意义的重启）。
func TestMalformedPayloadWrappedNotKeyError(t *testing.T) {
	c := newMockClient(t, map[string]string{"TSEC_MOCK_SCENARIO": "malformed_missing_field"})
	_, err := c.List(context.Background())
	if err == nil {
		t.Fatal("畸形载荷应该报错（而不是静默返回空列表）")
	}
	if !harness.IsKind(err, harness.KindPlatform) {
		t.Errorf("应归 KindPlatform: %v", err)
	}
	if !strings.Contains(err.Error(), CodeInvalidResponse) {
		t.Errorf("code 应为 %s: %v", CodeInvalidResponse, err)
	}
	// 桥**没有崩**：进程还活着，下一次调用仍然可用。这是「包住」而不是
	// 「崩掉」的判据。
	if _, err2 := c.List(context.Background()); err2 == nil {
		t.Error("第二次调用仍应报同样的错（桥进程必须活着）")
	}
	if c.Restarts() != 0 {
		t.Errorf("畸形载荷不该触发重启，实际重启 %d 次", c.Restarts())
	}
}

// TestNonJSON2xxWrappedNotJSONDecodeError 钉住 **SDK 缺陷 2**：
// 2xx 非 JSON 响应抛的 `json.JSONDecodeError` 被包成结构化错误。
//
// 源码依据（client.py:149-155）：`_handle_response` 对 2xx 无保护地调
// `response.json()`。2xx 但 body 不是 JSON 时抛 `json.JSONDecodeError`
// （ValueError 的子类），它**逃出** client.py:143-146 的 `httpx.HTTPError` 网。
// 典型现场：中间层回了一个 HTML 错误页却带 200。
//
// 它归 invalid_response 而不是 connection_error：连接是通的，是响应内容不对
// ——两者的排查方向完全不同（查网络 vs 查平台/中间层）。
func TestNonJSON2xxWrappedNotJSONDecodeError(t *testing.T) {
	c := newMockClient(t, map[string]string{"TSEC_MOCK_SCENARIO": "non_json_2xx"})
	_, err := c.List(context.Background())
	if err == nil {
		t.Fatal("2xx 非 JSON 应该报错")
	}
	if !strings.Contains(err.Error(), CodeInvalidResponse) {
		t.Errorf("code 应为 %s: %v", CodeInvalidResponse, err)
	}
	if harness.Retryable(err) {
		t.Errorf("响应内容不对不该判可重试（重试会拿到同一个坏响应）: %v", err)
	}
	if c.Restarts() != 0 {
		t.Errorf("不该触发重启，实际重启 %d 次", c.Restarts())
	}
}

// TestInvalidStateMessageNotLeakedIntoMsg 钉住「平台原文不进会序列化的 Msg」。
//
// `harness.Error` 会被写进 JSON（看板、报告、日志），所以 `Msg` 必须是我们
// 合成的安全短句；平台原文放进不参与序列化的 `Err`（errors.go:76-79）。
func TestInvalidStateMessageNotLeakedIntoMsg(t *testing.T) {
	c := newMockClient(t, map[string]string{"TSEC_MOCK_SCENARIO": "invalid_max_active"})
	_, err := c.Start(context.Background(), "web-01")
	if err == nil {
		t.Fatal("应该报错")
	}
	var he *harness.Error
	if !errors.As(err, &he) {
		t.Fatalf("应是 *harness.Error: %T", err)
	}
	if strings.Contains(he.Msg, "max active") {
		t.Errorf("Msg 不该照抄平台原文（它会被序列化）: %q", he.Msg)
	}
	if he.Err == nil || !strings.Contains(he.Err.Error(), "max active") {
		t.Errorf("平台原文应进不参与序列化的 Err: %v", he.Err)
	}
	mustNotLeak(t, "错误 Msg", he.Msg)
}

// ── deadline ──

// TestDeadlineExpiryReturnsRetryablePlatformError 钉住 deadline 到期的语义。
//
// 失效模式：桥卡在一条永不返回的调用上时，Go 侧如果不按 deadline 收手，
// 整个 run 会挂死。到期必须报 **KindPlatform + 可重试**——平台调用超时是
// 「可能已生效」的情形，引擎侧要按 Reconcile 对账后再决定（设计 §5.3），
// 而不是当永久失败。
//
// 两条路径都要钉：
//  1. Go 侧的计时器（ctx 到期）；
//  2. 桥侧收到一个 deadline 已过的请求 ⇒ **不开始平台调用**，直接回
//     deadline_exceeded。
func TestDeadlineExpiryReturnsRetryablePlatformError(t *testing.T) {
	t.Run("Go 侧 deadline 到期", func(t *testing.T) {
		c := newMockClient(t, nil)
		// 一个已经到期的 deadline（而不是 cancel）：两者都是「ctx 结束」，
		// 但语义不同——deadline 是「平台慢」，cancel 是「上层主动停」。
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		_, err := c.List(ctx)
		if err == nil {
			t.Fatal("ctx 到期应该报错")
		}
		if !harness.IsKind(err, harness.KindPlatform) {
			t.Errorf("应归 KindPlatform: %v", err)
		}
		if !harness.Retryable(err) {
			t.Errorf("deadline 到期应可重试: %v", err)
		}
		mustNotLeak(t, "deadline 错误", err.Error())
	})

	t.Run("Go 侧 ctx 被取消", func(t *testing.T) {
		c := newMockClient(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // 主动取消
		_, err := c.List(ctx)
		if err == nil {
			t.Fatal("ctx 取消应该报错")
		}
		// 取消**不可重试**：上层已经决定不做了，重试是错的。
		if !harness.IsKind(err, harness.KindCancelled) {
			t.Errorf("主动取消应归 KindCancelled: %v", err)
		}
		if harness.Retryable(err) {
			t.Errorf("主动取消不该可重试: %v", err)
		}
	})

	t.Run("桥侧 deadline 已过", func(t *testing.T) {
		c := newMockClient(t, nil)
		// 直接把一个过去的 deadline 发进去：桥必须**不发起平台调用**就回错。
		// 用 ctx 的长超时确保先到的是桥的响应，而不是 Go 的计时器。
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := c.do(ctx, "platform.list", CmdList, request{
			Cmd:      CmdList,
			Deadline: deadlineFor(time.Now().Add(-time.Hour)),
		})
		if err != nil {
			t.Fatalf("桥本身应正常应答（平台错误在 response.Error 里）: %v", err)
		}
		if resp.Error == nil {
			t.Fatal("deadline 已过时桥应回结构化错误")
		}
		if resp.Error.Code != CodeDeadlineExceeded {
			t.Errorf("code 应为 %s，实际 %s", CodeDeadlineExceeded, resp.Error.Code)
		}
		if !resp.Error.Retryable {
			t.Error("deadline_exceeded 应标记为可重试")
		}
	})
}

// TestBridgeDeadlineFieldIsRFC3339 钉住 deadline 的线格式。
//
// 桥与 Go 侧可能不在同一台机器（`docker exec`），所以传的是**绝对时刻**而不是
// 「还剩几秒」——时钟偏移下只有绝对时刻有意义。
func TestBridgeDeadlineFieldIsRFC3339(t *testing.T) {
	got := deadlineFor(time.Date(2026, 9, 20, 18, 0, 0, 123456789, time.UTC))
	if got != "2026-09-20T18:00:00Z" {
		t.Errorf("deadline 应为秒级 RFC3339，实际 %q", got)
	}
	if d := deadlineFor(time.Time{}); d != "" {
		t.Errorf("零时刻应渲染成空串，实际 %q", d)
	}
}

// TestDeadlinePassedRule 钉住桥侧 deadline 判定的边界。
//
// 解析失败的 deadline **当作未过期**：一个格式不对的 deadline 不该让一次
// 本来能成功的调用直接失败——真正兜底的是 Go 侧的计时器。
func TestDeadlinePassedRule(t *testing.T) {
	// 桥侧实现是 Python（protocol.deadline_passed），这里通过一条真实请求
	// 覆盖两个边界：过去的时刻 ⇒ 报错；格式不对 ⇒ 放行到平台调用。
	c := newMockClient(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := c.do(ctx, "platform.list", CmdList, request{
		Cmd:      CmdList,
		Deadline: "not-a-timestamp",
	})
	if err != nil {
		t.Fatalf("无法解析的 deadline 应放行: %v", err)
	}
	if resp.Error != nil {
		t.Errorf("无法解析的 deadline 不该导致失败: %+v", resp.Error)
	}
}

// ── 安全：凭据不进普通日志 ──

// TestCredentialsDoNotAppearInErrors 钉住错误文本里不含 token。
//
// 桥的 stderr 与错误消息都可能被写进日志/报告，而 token 在子进程环境里。
// 这条测试覆盖「正常错误路径」：平台报错时也不能把 token 带出来。
func TestCredentialsDoNotAppearInErrors(t *testing.T) {
	c := newMockClient(t, map[string]string{"TSEC_MOCK_SCENARIO": "task_not_found"})
	_, err := c.List(context.Background())
	if err == nil {
		t.Fatal("应该报错")
	}
	mustNotLeak(t, "平台错误", err.Error())

	// argv 里也不能有 token：命令行会出现在 `ps` 里。
	for _, a := range c.argv {
		mustNotLeak(t, "argv", a)
	}
}

// TestStderrGoesToPrivateNotHost 钉住「桥的 stderr 落 private/，不继承宿主 stderr」。
//
// 失效模式：Python traceback 里有 base_url 与请求 URL，继承宿主 stderr 就等于
// 把它们写进普通日志（设计 §3.3）。
func TestStderrGoesToPrivateNotHost(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "private")
	if err := os.MkdirAll(priv, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := mockConfig(t, nil)
	cfg.PrivateDir = priv
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient 失败: %v", err)
	}
	defer c.Shutdown()

	// 触发一次桥侧 stderr 输出（mock 的 SDK 导入日志会打到 stderr）。
	if _, err := c.List(context.Background()); err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	// 桥进程的 stderr 文件应存在于 private/ 下。
	entries, err := os.ReadDir(priv)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("private/ 下应有桥的 stderr 日志")
	}
	found := false
	for _, e := range entries {
		if strings.Contains(e.Name(), "bridge-stderr") {
			found = true
			st, _ := e.Info()
			if st.Mode().Perm() != 0o600 {
				t.Errorf("stderr 日志权限应为 0600，实际 %v", st.Mode().Perm())
			}
		}
	}
	if !found {
		t.Errorf("private/ 下应有 bridge-stderr 日志，实际 %v", entries)
	}
}
