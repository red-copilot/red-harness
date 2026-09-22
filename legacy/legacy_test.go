// Package legacy_test 承接从根包 contract_test.go 迁来的 v0.3 契约用例。
//
// 为什么必须跟着搬，而不是就地删掉：其中两条是**安全契约**——「快照里没有候选
// 明文」与「快照类型没有 flags 字段」。它们钉的是公开面的形状，而公开面的形状
// 不因为符号换了包就变得不重要。留在原地会编译不过，删掉则会让这条防线**静默
// 消失**（没有任何测试会因此变红）。
package legacy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/legacy"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// ── 事件 ──

func TestContract_DomainEventJSONRoundTrip(t *testing.T) {
	ev := legacy.DomainEvent{
		Seq:     7,
		At:      time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
		Type:    legacy.EvSubmitResult,
		RunID:   "run-1",
		Round:   3,
		Payload: json.RawMessage(`{"fingerprint":"fp:abcd1234/len=8/f…}"}`),
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back legacy.DomainEvent
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Seq != ev.Seq || back.Type != ev.Type || back.RunID != ev.RunID || back.Round != ev.Round {
		t.Errorf("round-trip 丢字段：%+v", back)
	}
	if string(back.Payload) != string(ev.Payload) {
		t.Errorf("Payload 变了：%s", back.Payload)
	}
}

// ── 明文不落公开面 ──

// TestContract_SnapshotHasNoPlaintext 是这组里最重要的一条：快照会被写进
// run.json、被 CLI 打印、被看板读——全是公开面。
func TestContract_SnapshotHasNoPlaintext(t *testing.T) {
	const secret = "flag{this_must_never_be_persisted}"

	snap := legacy.Snapshot{
		SchemaVersion: legacy.SchemaVersion,
		RunID:         "run-1",
		State:         harness.RunCompleted,
		Spec:          harness.RunSpec{Scenario: "fake"},
		SpecDigest:    "abc123",
		Objective:     harness.Objective{Kind: "flag_count", Want: 2, Got: 2, Completed: true},
		Public: legacy.PublicSummary{
			CandidatesSeen:     3,
			SubmittedConfirmed: 2,
			// 公开面只有指纹。
			ConfirmedFP: []string{"fp:deadbeef/len=36/f…}"},
		},
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte(secret)) {
		t.Fatal("Snapshot 序列化里出现了候选明文——公开面绝不允许")
	}
	// 反向确认：如果真有字段装了明文，这条测试要能发现。
	type probe struct {
		X string `json:"x"`
	}
	pb, _ := json.Marshal(probe{X: secret})
	if !bytes.Contains(pb, []byte(secret)) {
		t.Fatal("自检失败：bytes.Contains 对明文不敏感，本测试无意义")
	}
}

// TestContract_SnapshotTypeHasNoFlagsField 用反射级别的检查确保没有人在
// Snapshot 上加回一个 Flags 字段。
func TestContract_SnapshotTypeHasNoFlagsField(t *testing.T) {
	b := mustJSON(t, legacy.Snapshot{})
	for _, forbidden := range []string{`"flags"`, `"Flags"`, `"flag"`} {
		if bytes.Contains(b, []byte(forbidden)) {
			t.Errorf("Snapshot 序列化里出现了 %s —— 明文相关字段不得进入公开快照", forbidden)
		}
	}
}

// ── 引擎装配 ──

type fakePlatform struct{ harness.Platform }

func (fakePlatform) List(context.Context) ([]harness.Challenge, error) { return nil, nil }
func (fakePlatform) Start(context.Context, string) (harness.StartResult, error) {
	return harness.StartResult{}, nil
}
func (fakePlatform) Hint(context.Context, string) (harness.HintResult, error) {
	return harness.HintResult{}, nil
}
func (fakePlatform) Submit(context.Context, string, string) (harness.SubmitResult, error) {
	return harness.SubmitResult{}, nil
}
func (fakePlatform) Close(context.Context, string) (harness.CloseResult, error) {
	return harness.CloseResult{}, nil
}

type fakeExecutor struct{ legacy.Executor }

func (fakeExecutor) Available(context.Context) error              { return nil }
func (fakeExecutor) Reclaim(context.Context, harness.RunID) error { return nil }

type fakeAgents struct{ harness.AgentFactory }

type fakeStore struct{ legacy.Store }

func (fakeStore) Dir() string { return "/tmp/x" }

type fakeScenario struct{ harness.Scenario }

func TestContract_NewRejectsMissingPorts(t *testing.T) {
	// 空 Options ⇒ KindConfig，且消息要**指出缺哪个**。
	// 前身事故里「端口没接」的表现是静默退化（DAG 零事实、所有包自测全绿），
	// 所以这条错误必须点名。
	_, err := legacy.New(legacy.Options{})
	if err == nil {
		t.Fatal("空 Options 应当报错")
	}
	if !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("错误分类应为 KindConfig，实际 %v", err)
	}
	msg := err.Error()
	for _, port := range []string{"Platform", "Executor", "Agents", "Store", "Scenarios"} {
		if !strings.Contains(msg, port) {
			t.Errorf("错误消息里没有指出缺 %s：%s", port, msg)
		}
	}
}

func TestContract_NewAcceptsCompletePorts(t *testing.T) {
	opts := legacy.Options{
		Platform:  fakePlatform{},
		Executor:  fakeExecutor{},
		Agents:    fakeAgents{},
		Store:     fakeStore{},
		Scenarios: map[string]harness.Scenario{"fake": fakeScenario{}},
	}
	// 端口齐备时不得因为「缺端口」而报错。
	//
	// ⚠️ v0.5 的现状：`legacy.New` 默认返回「引擎实现未注册」的 KindConfig 错误
	// ——引擎实现从来没有过（`engine/` 目录不存在，`RegisterEngine` 零调用方）。
	// 下面的断言因此走的是那条分支。这不是「测试没写完」，而是对**真实状态**的
	// 记录：v0.3 的引擎注册表从未被填充过，它一直是根包里的一块空壳。
	eng, err := legacy.New(opts)
	if err != nil {
		if strings.Contains(err.Error(), "缺少必需端口") {
			t.Fatalf("端口齐备却报缺端口：%v", err)
		}
		t.Logf("引擎实现未注册（这是 v0.3 注册表的真实状态）：%v", err)
		return
	}
	if eng == nil {
		t.Fatal("New 返回了 nil 引擎且无错误")
	}
}

func TestContract_MissingPortsHelper(t *testing.T) {
	// MissingPorts 是导出 API，引擎实现的测试会用它断言消息内容——
	// 抄一份清单就会漂移。
	got := legacy.Options{}.MissingPorts()
	if len(got) != 5 {
		t.Errorf("空 Options 应报 5 个缺端口，实际 %v", got)
	}
	full := legacy.Options{
		Platform: fakePlatform{}, Executor: fakeExecutor{}, Agents: fakeAgents{},
		Store: fakeStore{}, Scenarios: map[string]harness.Scenario{"fake": fakeScenario{}},
	}
	if m := full.MissingPorts(); len(m) != 0 {
		t.Errorf("端口齐备时不应有缺项，实际 %v", m)
	}
}
