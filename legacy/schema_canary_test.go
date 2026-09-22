package legacy_test

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/red-copilot/red-harness/legacy"
)

// ── 线上格式的 canary ──
//
// 这些键名是**写进过文件**的：`run.json`（Snapshot）、`events.jsonl`
// （DomainEvent）、旧报告（PublicSummary / RunSummary）。改一个 json tag 或字段名
// 的代价是**静默的**——现存文件读回来字段为零值，而没有任何测试会因此变红
// （结构体照常编译，JSON 照常合法）。
//
// 这正是「把符号搬出根包」这类改动最容易踩的坑：搬的时候顺手统一了命名，
// 或者把 omitempty 加回去，看起来是清理，实际让旧文件读不出来。
//
// 所以这里把键集合**冻结**成显式清单。它同时也是「搬家没有改变线上格式」的证据：
// 这份清单是在搬家之前从根包算出来的。

// jsonKeys 返回 v 序列化后的顶层键集合（排序）。
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestWireFormatKeysAreFrozen(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want []string
	}{
		{"Snapshot（run.json）", legacy.Snapshot{}, []string{
			"budgetUsed", "endedAt", "lastAppliedSeq", "objective", "public",
			"runId", "schemaVersion", "spec", "specDigest", "startedAt", "state"}},
		{"DomainEvent（events.jsonl）", legacy.DomainEvent{}, []string{
			"at", "runId", "seq", "type"}},
		{"PublicSummary", legacy.PublicSummary{}, []string{
			"candidatesSeen", "duplicates", "hintUsed", "intentDone",
			"negative", "rejected", "rounds", "submittedConfirmed"}},
		{"RunSummary", legacy.RunSummary{}, []string{
			"endedAt", "objective", "runId", "scenario", "score", "startedAt", "state"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := jsonKeys(t, c.v)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("%s 的线上格式变了：\n got %q\nwant %q\n"+
					"键名是写进过文件的——改名会让现存文件静默读成零值", c.name, got, c.want)
			}
		})
	}
}

// TestDomainEventTypeValuesAreFrozen：事件类型是**重放的单位**。
//
// 改一个字符串不会破坏编译，但会让旧 events.jsonl 重放时把已知事件读成未知类型
// ——而未知类型的处置是「静默忽略」，状态机停在半路。
func TestDomainEventTypeValuesAreFrozen(t *testing.T) {
	want := map[string]legacy.DomainEventType{
		"run_created": legacy.EvRunCreated, "run_preparing": legacy.EvRunPreparing,
		"target_started": legacy.EvTargetStarted, "round_started": legacy.EvRoundStarted,
		"intent_active": legacy.EvIntentActive, "intent_settled": legacy.EvIntentSettled,
		"candidate_seen": legacy.EvCandidateSeen, "submit_result": legacy.EvSubmitResult,
		"hint_requested": legacy.EvHintRequested, "fact_learned": legacy.EvFactLearned,
		"budget_used": legacy.EvBudgetUsed, "run_paused": legacy.EvRunPaused,
		"run_resumed": legacy.EvRunResumed, "run_ended": legacy.EvRunEnded,
	}
	if len(want) != 14 {
		t.Fatalf("清单有 %d 项，期望 14——漏了一项就说明有人加了事件类型却没同步这里", len(want))
	}
	for literal, got := range want {
		if string(got) != literal {
			t.Errorf("事件类型 %q 变成了 %q——旧 events.jsonl 重放会把它读成未知类型", literal, got)
		}
	}
}

// TestSchemaVersionIsFrozen：版本号是恢复路径的判据。
//
// 它**只增不改**：改小会让新文件被旧代码硬解，改大（在格式没变时）会让所有现存
// 文件要求一次并不存在的迁移。
func TestSchemaVersionIsFrozen(t *testing.T) {
	if legacy.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion = %d，期望 1；改它必须同时提供迁移路径并更新本断言", legacy.SchemaVersion)
	}
}
