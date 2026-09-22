package executor

import (
	"reflect"
	"strings"
	"testing"

	harness "github.com/red-copilot/red-harness"
)

// 本文件是**回收判定的离线用例**（不带 integration tag，不碰 Docker）。
//
// 为什么要单独一组：回收是本包里唯一会**删除别人资源**的代码路径，而它的判据过去
// 只写在 ReclaimStale 的 for 循环里——「哪些情况不该删」既没有名字、也无法单独
// 测试，而漏掉的那几条恰恰是删错对象的成因（两个用不同 --store 的进程各自把对方
// 认成孤儿）。把判定抽成纯函数之后，判据表本身就能被逐条钉住。

// mustOwner 造一个 owner，测试里只关心「是不是同一个」。
func mustOwner(t *testing.T, host string, uid int) harness.OwnerID {
	t.Helper()
	o, err := harness.ResolveOwner(host, uid)
	if err != nil {
		t.Fatalf("ResolveOwner(%q, %d): %v", host, uid, err)
	}
	return o
}

// TestJudgeStaleCriteria 是**判据表**：harness.ReclaimReport 的注释逐条对应一例。
//
// 这条用例的意义在于「不该删」的那三种（owner 不匹配 / 无 owner / 解析不出）也是
// 一等的、被钉住的行为，而不是「没写」。
func TestJudgeStaleCriteria(t *testing.T) {
	self := mustOwner(t, "box", 0)
	other := mustOwner(t, "other-box", 0)

	cases := []struct {
		name string
		objs []scanObject
		live map[harness.RunID]bool
		// selfUnknown 为真时用 harness.OwnerUnknown 当本进程的 owner，
		// 模拟「本机 owner 解析不出来」。
		selfUnknown   bool
		wantReclaimed []harness.RunID
		wantPending   []harness.StaleObject
	}{
		{
			name:          "owner 匹配且不活跃 ⇒ 删除",
			objs:          []scanObject{{Kind: "container", Parsed: true, RunID: "run-1", Owner: self}},
			wantReclaimed: []harness.RunID{"run-1"},
		},
		{
			name: "owner 匹配且活跃 ⇒ 保留，两条都不进",
			objs: []scanObject{{Kind: "container", Parsed: true, RunID: "run-1", Owner: self}},
			live: map[harness.RunID]bool{"run-1": true},
		},
		{
			name: "owner 不匹配 ⇒ 保留 + Pending(owner_mismatch)",
			objs: []scanObject{{Kind: "network", Parsed: true, RunID: "run-1", Owner: other}},
			wantPending: []harness.StaleObject{
				{Kind: "network", RunID: "run-1", Owner: other, Reason: harness.StaleOwnerMismatch},
			},
		},
		{
			// 升级前的旧资源：有 run 标签、没有 owner 标签。「不知道是谁的」
			// 与「确实是别人的」是两件事，理由也必须分开。
			name: "无 owner 标签 ⇒ 保留 + Pending(owner_unknown)",
			objs: []scanObject{{Kind: "container", Parsed: true, RunID: "run-old"}},
			wantPending: []harness.StaleObject{
				{Kind: "container", RunID: "run-old", Owner: harness.OwnerUnknown, Reason: harness.StaleOwnerUnknown},
			},
		},
		{
			// 解析歧义（docker 版本差异、格式变化、异常行）必须退化成「只报告」。
			name:        "解析不出 run 标签 ⇒ 保留 + Pending(unparsable)",
			objs:        []scanObject{{Kind: "container"}}, // Parsed=false
			wantPending: []harness.StaleObject{{Kind: "container", Reason: harness.StaleUnparsable}},
		},
		{
			// 容器与网络都必须被看见，但**同一个 run 只删一次**：Reclaimed 是
			// runID 的集合，重复出现会让「删了几个」的计数说谎。
			name: "同一个 run 的容器与网络只算一次删除",
			objs: []scanObject{
				{Kind: "container", Parsed: true, RunID: "run-1", Owner: self},
				{Kind: "network", Parsed: true, RunID: "run-1", Owner: self},
			},
			wantReclaimed: []harness.RunID{"run-1"},
		},
		{
			// 四种形态混在一起时，各自的去向不得互相污染。
			name: "混合输入",
			objs: []scanObject{
				{Kind: "container", Parsed: true, RunID: "mine-dead", Owner: self},
				{Kind: "container", Parsed: true, RunID: "mine-live", Owner: self},
				{Kind: "network", Parsed: true, RunID: "theirs", Owner: other},
				{Kind: "network", Parsed: true, RunID: "ancient"},
				{Kind: "container"},
			},
			live:          map[harness.RunID]bool{"mine-live": true},
			wantReclaimed: []harness.RunID{"mine-dead"},
			wantPending: []harness.StaleObject{
				{Kind: "network", RunID: "theirs", Owner: other, Reason: harness.StaleOwnerMismatch},
				{Kind: "network", RunID: "ancient", Owner: harness.OwnerUnknown, Reason: harness.StaleOwnerUnknown},
				{Kind: "container", Reason: harness.StaleUnparsable},
			},
		},
		{
			// fail closed：self 不可判定时「谁都不像我的」不能当成「可以删」，
			// 否则这次修复就被原样取消了。
			name:        "self 未知 ⇒ 一律不删",
			selfUnknown: true,
			objs: []scanObject{
				{Kind: "container", Parsed: true, RunID: "run-1", Owner: self},
				{Kind: "container", Parsed: true, RunID: "run-2"},
				{Kind: "network"},
			},
			wantPending: []harness.StaleObject{
				{Kind: "container", RunID: "run-1", Owner: self, Reason: harness.StaleOwnerMismatch},
				{Kind: "container", RunID: "run-2", Owner: harness.OwnerUnknown, Reason: harness.StaleOwnerUnknown},
				{Kind: "network", Reason: harness.StaleUnparsable},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			me := self
			if c.selfUnknown {
				me = harness.OwnerUnknown
			}
			rep := judgeStale(c.objs, me, c.live)

			if !reflect.DeepEqual(rep.Reclaimed, c.wantReclaimed) {
				t.Errorf("Reclaimed = %v, 期望 %v", rep.Reclaimed, c.wantReclaimed)
			}
			if !reflect.DeepEqual(rep.Pending, c.wantPending) {
				t.Errorf("Pending = %v, 期望 %v", rep.Pending, c.wantPending)
			}
			// 计数是公开面的读数（ReclaimReport 的注释：公开面只放计数），
			// 所以它们必须与切片一致——分开断言是为了让「计数漂了」独立可见。
			if rep.ReclaimedTotal() != len(c.wantReclaimed) {
				t.Errorf("ReclaimedTotal = %d, 期望 %d", rep.ReclaimedTotal(), len(c.wantReclaimed))
			}
			if rep.PendingTotal() != len(c.wantPending) {
				t.Errorf("PendingTotal = %d, 期望 %d", rep.PendingTotal(), len(c.wantPending))
			}
		})
	}
}

// TestJudgeStaleEmpty 钉住「没有对象」是零值报告，而不是 nil 或 panic。
func TestJudgeStaleEmpty(t *testing.T) {
	self := mustOwner(t, "box", 0)
	rep := judgeStale(nil, self, nil)
	if rep.ReclaimedTotal() != 0 || rep.PendingTotal() != 0 {
		t.Errorf("空输入必须得到空报告, got %v / %v", rep.Reclaimed, rep.Pending)
	}
}

// TestParseScan 钉住扫描输出的解析。
//
// 输入形态取自 `docker ps --format '{{json .Labels}}'` 与
// `docker network ls --format '{{json .Labels}}'` 的真实输出（本机 Docker 29.8
// 实测：模板函数 `json` 在两种对象上都可用）。
func TestParseScan(t *testing.T) {
	cases := []struct {
		name string
		kind string
		out  string
		want []scanObject
	}{
		{
			name: "完整标签",
			kind: "container",
			out:  `{"red-harness.attempt":"1","red-harness.challenge":"ab12","red-harness.owner":"box/0","red-harness.role":"runner","red-harness.run":"run-1"}`,
			want: []scanObject{{Kind: "container", Parsed: true, RunID: "run-1", Owner: "box/0"}},
		},
		{
			name: "完整的网络标签",
			kind: "network",
			out:  `{"red-harness.owner":"box/0","red-harness.role":"network","red-harness.run":"run-1"}`,
			want: []scanObject{{Kind: "network", Parsed: true, RunID: "run-1", Owner: "box/0"}},
		},
		{
			// 升级前的旧资源：有 run 标签、没有 owner 标签 ⇒ 仍然 Parsed（它的归属是
			// 「无主」，那是判定阶段的事，不是解析阶段的事）。
			name: "缺少 owner 标签",
			kind: "container",
			out:  `{"red-harness.run":"run-1"}`,
			want: []scanObject{{Kind: "container", Parsed: true, RunID: "run-1"}},
		},
		{
			name: "owner 标签为空串",
			kind: "container",
			out:  `{"red-harness.owner":"","red-harness.run":"run-1"}`,
			want: []scanObject{{Kind: "container", Parsed: true, RunID: "run-1"}},
		},
		{
			// 扫描命令带了 label 存在性过滤，所以读不出 run 只可能是格式变了。
			name: "缺少 run 标签",
			kind: "network",
			out:  `{"red-harness.owner":"box/0"}`,
			want: []scanObject{{Kind: "network"}},
		},
		{
			name: "run 标签为空串",
			kind: "network",
			out:  `{"red-harness.run":""}`,
			want: []scanObject{{Kind: "network"}},
		},
		{
			// 这一条是那个已知的坑：旧版 docker 的 `{{.Label "x"}}` 会打印
			// `key=value`（多个标签时甚至拼成 `k=v,k=v`）。它必须解析失败。
			name: "旧版的 key=value 形态",
			kind: "container",
			out:  "red-harness.run=run-1",
			want: []scanObject{{Kind: "container"}},
		},
		{
			name: "非 JSON 的裸值",
			kind: "container",
			out:  "run-1",
			want: []scanObject{{Kind: "container"}},
		},
		{
			// docker 在没有任何标签时会打印 null（nil map 的 JSON 形态）。
			name: "null",
			kind: "container",
			out:  "null",
			want: []scanObject{{Kind: "container"}},
		},
		{
			name: "空输出",
			kind: "container",
			out:  "",
			want: nil,
		},
		{
			name: "只有空行",
			kind: "container",
			out:  "\n\n",
			want: nil,
		},
		{
			name: "空行与行尾换行",
			kind: "container",
			out:  "\n{\"red-harness.run\":\"run-1\"}\n\n",
			want: []scanObject{{Kind: "container", Parsed: true, RunID: "run-1"}},
		},
		{
			// 一行坏掉不得影响别的行：丢掉它等于「扫不到」，而扫不到的表现是
			// 「ReclaimStale 说无事可做，宿主上却躺着孤儿」。
			name: "坏行夹在好行之间",
			kind: "container",
			out:  "{\"red-harness.run\":\"run-1\"}\nnot-json\n{\"red-harness.run\":\"run-2\"}",
			want: []scanObject{
				{Kind: "container", Parsed: true, RunID: "run-1"},
				{Kind: "container"},
				{Kind: "container", Parsed: true, RunID: "run-2"},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseScan(c.kind, c.out)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("parseScan(%q, ...) = %+v, 期望 %+v", c.kind, got, c.want)
			}
		})
	}
}

// TestScanRoundsTripToJudge 把解析与判定串起来：一段**真实的**扫描输出必须得到
// 「自己的回收、别人的与无主的只报告」这个结论。
//
// 单独测两半会漏掉它们之间的接口（比如解析把 owner 放错字段），所以这里再串一次。
func TestScanRoundsTripToJudge(t *testing.T) {
	self := mustOwner(t, "box", 0)
	out := strings.Join([]string{
		`{"red-harness.owner":"box/0","red-harness.run":"mine"}`,
		`{"red-harness.owner":"other-box/0","red-harness.run":"theirs"}`,
		`{"red-harness.run":"ancient"}`,
		"garbage",
	}, "\n")

	rep := judgeStale(parseScan("container", out), self, nil)

	if !reflect.DeepEqual(rep.Reclaimed, []harness.RunID{"mine"}) {
		t.Errorf("Reclaimed = %v, 期望只删 mine", rep.Reclaimed)
	}
	wantReasons := []harness.StaleReason{
		harness.StaleOwnerMismatch, harness.StaleOwnerUnknown, harness.StaleUnparsable,
	}
	if rep.PendingTotal() != len(wantReasons) {
		t.Fatalf("PendingTotal = %d, 期望 %d（%v）", rep.PendingTotal(), len(wantReasons), rep.Pending)
	}
	for i, want := range wantReasons {
		if rep.Pending[i].Reason != want {
			t.Errorf("Pending[%d].Reason = %q, 期望 %q", i, rep.Pending[i].Reason, want)
		}
	}
}

// TestReclaimArgvCarriesOwner 钉住删除路径的**第二道闸**：定位参数里必须有 owner。
//
// 只按 run 定位的失效模式是一次已核实的真实事故——两个用不同 --store 的进程各自
// 拿到锁、各自把对方认成孤儿，然后删掉对方正在用的容器、网络与规则。
func TestReclaimArgvCarriesOwner(t *testing.T) {
	cfg := DockerConfig{Owner: "box/0"}
	for _, argv := range [][]string{
		reclaimContainerArgv(cfg, "run-1"),
		reclaimNetworkArgv(cfg, "run-1"),
	} {
		joined := argvString(argv)
		if !strings.Contains(joined, "--filter label="+LabelRun+"=run-1") {
			t.Errorf("回收必须按 run 定位: %v", argv)
		}
		if !strings.Contains(joined, "--filter label="+LabelOwner+"=box/0") {
			t.Errorf("回收必须同时按 owner 定位（只按 run 会删掉别的部署的同名 run）: %v", argv)
		}
	}
	// owner 为空时这一条**仍然渲染**：`label=<key>=` 匹配不到任何对象（fail closed），
	// 而省略它会让查询退化成「谁的都删」。
	if joined := argvString(reclaimContainerArgv(DockerConfig{}, "run-1")); !strings.Contains(joined, LabelOwner+"=") {
		t.Errorf("owner 为空时也不得省略 owner 过滤: %s", joined)
	}
}

// TestSelfOwnerFailsClosed 钉住「owner 不可判定 ⇒ 拒绝回收」。
//
// 一个直接构造出来的 Docker（没走 NewDocker / normalize）的 Owner 是空的；
// 此时正确的行为是**不删并报错**，而不是删得少一点——后者在现场看起来与
// 「今天没有孤儿」完全一样。
func TestSelfOwnerFailsClosed(t *testing.T) {
	d := &Docker{cfg: DockerConfig{}}
	if _, err := d.selfOwner(); err == nil {
		t.Fatal("Owner 未解析时必须报错")
	} else if !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("应是 KindConfig, got %v", err)
	}
	ok := &Docker{cfg: DockerConfig{Owner: "box/0"}}
	got, err := ok.selfOwner()
	if err != nil {
		t.Fatalf("selfOwner: %v", err)
	}
	if got != harness.OwnerID("box/0") {
		t.Errorf("selfOwner = %q, 期望 box/0", got)
	}
	if err := (&Docker{cfg: DockerConfig{}}).Reclaim(t.Context(), "run-1"); err == nil {
		t.Error("Owner 未解析时 Reclaim 必须拒绝（不得在副作用之后才失败）")
	}
	// 拒绝时返回的报告是**零值**，这不是随手写的：扫描**根本没有发生**，所以
	// 「回收了 0 个、待定 0 个」是唯一如实的答案。若这里返回一个「已回收 N 个」
	// 的非空报告，调用方会把一次「我拒绝执行」读成「我干完了」——那正是
	// owner 未解析这条路径最危险的读法。
	rep, err := (&Docker{cfg: DockerConfig{}}).ReclaimStale(t.Context(), nil)
	if err == nil {
		t.Error("Owner 未解析时 ReclaimStale 必须拒绝")
	}
	if rep.ReclaimedTotal() != 0 || rep.PendingTotal() != 0 {
		t.Errorf("拒绝时报告必须是零值（扫描尚未发生），得到 回收=%d 待定=%d",
			rep.ReclaimedTotal(), rep.PendingTotal())
	}
}
