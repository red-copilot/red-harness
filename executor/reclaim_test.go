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

// TestClassifyScanned 钉住「标签表 → 对象列表」这一层的判定。
//
// 输入是**标签表**而不是扫描输出的文本：这一层与扫描方式无关（它是 parseInspect
// 翻译完之后的样子），所以能表驱动地测而不用造 Docker。
//
// ⚠️ 这里刻意**不**测任何输出形态。上一版把形态与判定混在一个用例里，结果是
// 夹具编造了一个 docker 并不打印的形态（见 TestParseInspect 上那段），判定逻辑
// 因此从未被真正执行过——**绿的空转**。
func TestClassifyScanned(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		labels []map[string]string
		want   []scanObject
	}{
		{
			name: "完整标签",
			kind: "container",
			labels: []map[string]string{{
				LabelRun: "run-1", LabelOwner: "box/0",
				LabelChallenge: "ab12", LabelAttempt: "1", LabelRole: "runner",
			}},
			want: []scanObject{{Kind: "container", Parsed: true, RunID: "run-1", Owner: "box/0"}},
		},
		{
			// 升级前的旧资源：有 run 标签、没有 owner 标签 ⇒ 仍然 Parsed（它的归属是
			// 「无主」，那是判定阶段的事，不是解析阶段的事）。
			name:   "缺少 owner 标签",
			kind:   "container",
			labels: []map[string]string{{LabelRun: "run-1"}},
			want:   []scanObject{{Kind: "container", Parsed: true, RunID: "run-1"}},
		},
		{
			name:   "owner 标签为空串",
			kind:   "container",
			labels: []map[string]string{{LabelRun: "run-1", LabelOwner: ""}},
			want:   []scanObject{{Kind: "container", Parsed: true, RunID: "run-1"}},
		},
		{
			// 无标签的容器实测是 `{}`（不是 null，见 TestParseInspect）。
			name:   "空标签表",
			kind:   "container",
			labels: []map[string]string{{}},
			want:   []scanObject{{Kind: "container"}},
		},
		{
			// nil 标签表：inspect 的字段位变了（比如容器改成把标签放顶层）就会走到
			// 这里。它必须与「空标签表」同档，归 unparsable，而不是被丢掉。
			name:   "nil 标签表",
			kind:   "container",
			labels: []map[string]string{nil},
			want:   []scanObject{{Kind: "container"}},
		},
		{
			// 扫描命令带了 label 存在性过滤，所以读不出 run 只可能是标签值真是空串。
			name:   "run 标签为空串",
			kind:   "network",
			labels: []map[string]string{{LabelOwner: "box/0", LabelRun: ""}},
			want:   []scanObject{{Kind: "network"}},
		},
		{
			// run 标签值里的空白**要 trim**（它是 docker 给的字符串，可能带换行）。
			// ⚠️ 但 owner 的值不 trim —— 见 classifyScanned 的注释，规范化 owner
			// 会把两个不同的 owner 合并成一个，方向是「外来的看起来像我的」。
			name:   "run 标签值带空白",
			kind:   "container",
			labels: []map[string]string{{LabelRun: "  run-1\n", LabelOwner: " box/0 "}},
			want:   []scanObject{{Kind: "container", Parsed: true, RunID: "run-1", Owner: " box/0 "}},
		},
		{
			// 一条坏的不影响别的：丢掉它等于「扫不到」，而扫不到的表现是
			// 「ReclaimStale 说无事可做，宿主上却躺着孤儿」。
			name: "坏对象夹在好对象之间",
			kind: "container",
			labels: []map[string]string{
				{LabelRun: "run-1"},
				nil,
				{LabelRun: "run-2"},
			},
			want: []scanObject{
				{Kind: "container", Parsed: true, RunID: "run-1"},
				{Kind: "container"},
				{Kind: "container", Parsed: true, RunID: "run-2"},
			},
		},
		{
			name:   "没有对象",
			kind:   "container",
			labels: nil,
			want:   []scanObject{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyScanned(c.kind, c.labels)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("classifyScanned(%q, ...) = %+v, 期望 %+v", c.kind, got, c.want)
			}
		})
	}
}

// TestParseInspect 钉住 `docker inspect` 输出的解析。
//
// ⚠️ **这些夹具是实测抓下来的，不是手写的。** 上一版这个用例的夹具是手写的
// `{"red-harness.run":"run-1"}`，而它的注释声称「取自 `{{json .Labels}}` 的真实
// 输出」——两句话都是假的：真实的 `{{json .Labels}}` 打印的是一个 **JSON 字符串**
// （逗号拼接的 `k=v`），照对象去解析**每一行都失败**。判定逻辑因此在生产路径上
// 从未被执行过，而用例是绿的。**夹具的出处要是谎话，测试就是谎话的放大器。**
//
// 出处：本机 Docker 29.8，`docker inspect <id> | jq` 截取**代码实际读取的那两个
// 字段位**（容器 `.Config.Labels`、网络顶层 `.Labels`）。只截子树不截整份是因为
// 整份有几十个字段、上千行；而 inspectObject 只声明需要的字段，多余字段被忽略
// 这件事本身由下面的「多余字段」一例钉住。
func TestParseInspect(t *testing.T) {
	// 实测：容器把用户标签放在 .Config.Labels，且**没有**顶层 Labels（顶层的
	// MountLabel / ProcessLabel 是 SELinux 的东西，与用户标签无关，Go 的 json
	// 也不会把它们匹配到 `Labels` 上）。
	const containerReal = `[{"Config":{"Labels":{"red-harness.owner":"probe-owner","red-harness.run":"probe-run"}}}]`

	// 实测：网络把用户标签放在**顶层** .Labels（没有 Config 这一层）。
	const networkReal = `[{"Labels":{"red-harness.owner":"box/0","red-harness.role":"network","red-harness.run":"it-stale"}}]`

	cases := []struct {
		name    string
		kind    string
		out     string
		want    []scanObject
		wantErr bool
	}{
		{
			name: "容器（实测形态）",
			kind: "container",
			out:  containerReal,
			want: []scanObject{{Kind: "container", Parsed: true, RunID: "probe-run", Owner: "probe-owner"}},
		},
		{
			name: "网络（实测形态）",
			kind: "network",
			out:  networkReal,
			want: []scanObject{{Kind: "network", Parsed: true, RunID: "it-stale", Owner: "box/0"}},
		},
		{
			// 竞态：ls 与 inspect 之间对象消失了。实测 docker 的行为是
			// **exit 1 但 stdout 仍含有效对象**，且数组里**不再有**消失的那个。
			// 所以「已消失」自然等价于「无需回收」——scanLabeled 据此让
			// 解析先于判错。
			name: "竞态后只剩一个（实测形态）",
			kind: "container",
			out:  containerReal,
			want: []scanObject{{Kind: "container", Parsed: true, RunID: "probe-run", Owner: "probe-owner"}},
		},
		{
			// 实测：全部 ID 都不存在时 docker 打印 `[]` 并 exit 1。
			name: "空数组",
			kind: "container",
			out:  "[]",
			want: []scanObject{},
		},
		{
			// 实测：`docker inspect` 无参数时 exit 1、stdout 为空 —— 所以
			// scanLabeled 必须在没有 ID 时提前返回，不能把空串喂进来。
			name:    "空输出（inspect 无参数）",
			kind:    "container",
			out:     "",
			wantErr: true,
		},
		{
			name:    "被截断的 JSON",
			kind:    "container",
			out:     `[{"Config":{"Labels":{"red-harness.run":"run-1"}}`,
			wantErr: true,
		},
		{
			// 形态变了（比如某天 docker 让 inspect 也能出 NDJSON）必须**报错**，
			// 而不是降级成一堆 unparsable 对象——那等于给形态变化编一份假报告。
			name:    "NDJSON（形态变了）",
			kind:    "container",
			out:     "{\"Config\":{\"Labels\":{\"red-harness.run\":\"run-1\"}}}\n{\"Config\":{}}",
			wantErr: true,
		},
		{
			// inspectObject 只声明需要的字段，多余字段被忽略。整份 inspect 输出有
			// 几十个字段，这条钉住「只截子树当夹具」是成立的。
			name: "多余字段被忽略",
			kind: "container",
			out: `[{"Id":"e3e15080f6c6","Created":"2026-09-22T00:19:15Z",` +
				`"MountLabel":"","ProcessLabel":"",` +
				`"State":{"Status":"running","Running":true},` +
				`"Config":{"Hostname":"rh-x","Labels":{"red-harness.run":"run-9"}}}]`,
			want: []scanObject{{Kind: "container", Parsed: true, RunID: "run-9"}},
		},
		{
			// 实测：没有标签的容器是 `{}`（不是 null）。它必须归 unparsable。
			name: "容器无标签（实测空对象）",
			kind: "container",
			out:  `[{"Config":{"Labels":{}}}]`,
			want: []scanObject{{Kind: "container"}},
		},
		{
			// 把网络当容器解析（kind 传错）会读到 nil 标签表 ⇒ unparsable ⇒
			// **不删**。这是刻意的退化方向：kind 错配绝不能退化成「删除」。
			name: "kind 错配只退化成不删",
			kind: "container",
			out:  networkReal,
			want: []scanObject{{Kind: "container"}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseInspect(c.kind, c.out)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseInspect 应当报错, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseInspect 不该报错: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("parseInspect(%q, ...) = %+v, 期望 %+v", c.kind, got, c.want)
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
	// 一整段**实测形态**的 inspect 输出（数组，元素的 .Config.Labels 是标签表）：
	// 自己的、别人的、升级前的无主资源、以及一条读不出 run 的。
	out := `[` +
		`{"Config":{"Labels":{"red-harness.owner":"box/0","red-harness.run":"mine"}}},` +
		`{"Config":{"Labels":{"red-harness.owner":"other-box/0","red-harness.run":"theirs"}}},` +
		`{"Config":{"Labels":{"red-harness.run":"ancient"}}},` +
		`{"Config":{"Labels":{}}}` +
		`]`

	objs, err := parseInspect("container", out)
	if err != nil {
		t.Fatalf("parseInspect: %v", err)
	}
	rep := judgeStale(objs, self, nil)

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
