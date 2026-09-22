package wire

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	harness "github.com/red-copilot/red-harness"
)

// 本文件的测试**不碰 Docker、不碰网络、不碰平台**：装配层的可测部分正是
// 「接线对不对」，而「能不能真的起容器」属于 executor 的集成测试。

// TestNewRequiresStoreDir 钉「缺 StoreDir 就装配失败」。
//
// 为什么不能给它一个缺省（比如 cwd）：StoreDir 是 RunSpec 摘要的输入，也是
// 「run 在哪儿建目录」与「list/stats 去哪儿读」共用的那个字符串。凭空给一个
// 缺省等于让同一次运行的落点取决于 cwd，那是最难查的一类漂移。
func TestNewRequiresStoreDir(t *testing.T) {
	if _, err := New(Options{StoreDir: "  "}); err == nil {
		t.Fatal("StoreDir 为空白时 New 应当失败")
	} else if !harness.IsKind(err, harness.KindConfig) {
		t.Fatalf("应当是配置类错误，得到 %v", err)
	}
}

// TestNewUnknownScenarioNamesAlternatives 钉「场景名写错时点名可用值」。
//
// 只回一句「未知场景」会让用户去翻文档；而可用场景只有两个，直接列出来。
func TestNewUnknownScenarioNamesAlternatives(t *testing.T) {
	_, err := New(Options{StoreDir: t.TempDir(), Scenario: "tsecbench-typo"})
	if err == nil {
		t.Fatal("未知场景应当装配失败")
	}
	msg := err.Error()
	if !strings.Contains(msg, ScenarioFake) || !strings.Contains(msg, ScenarioTSecBench) {
		t.Fatalf("错误里应当列出可用场景，得到: %s", msg)
	}
}

// TestNewFakeScenarioWiresProductionPorts 钉「fake 场景能装配出一台完整 Harness」。
//
// 它同时是「生产必需端口齐备」这条闸的**正向**证明：Planner/Renderer/Gate/
// Results 缺任何一个，New 都会因为 Doctor 报告 OK=false 而失败。所以这个测试
// 只要通过，就说明那条闸真的接上了（而不是「反正没检查，所以也没报错」）。
func TestNewFakeScenarioWiresProductionPorts(t *testing.T) {
	dir := t.TempDir()
	r, err := New(Options{
		StoreDir: dir,
		Scenario: ScenarioFake,
		Agent:    AgentOptions{Provider: "fake-provider", Model: "fake-model"},
	})
	if err != nil {
		t.Fatalf("fake 场景应当装配成功: %v", err)
	}
	defer func() { _ = r.Close() }()

	if r.h == nil {
		t.Fatal("装配结果里没有 Harness")
	}
	// 单运行锁必须是装配层自己建的：生产必需，不该靠调用方记得传。
	// （`harness.Harness.locker` 是根包的非导出字段，这里断言不了「它被传进去了」，
	// 所以改成断言**装配层造出来的那把锁**是什么——见 defaultLock。）
	lk := defaultLock(dir)
	fl, ok := lk.(*FileLock)
	if !ok {
		t.Fatalf("缺省锁应当是 *FileLock，得到 %T", lk)
	}
	if fl.path != filepath.Join(dir, runLockName) {
		t.Fatalf("缺省锁应当落在 <StoreDir>/%s，得到 %q", runLockName, fl.path)
	}
	// ResultDir 为空时取 StoreDir：指标与运行目录同根是缺省形态。
	if r.resultDir != r.storeDir {
		t.Fatalf("ResultDir 缺省应当取 StoreDir，得到 %q vs %q", r.resultDir, r.storeDir)
	}
	// 缺省镜像要落到 RunSpec 上（否则 runChallenge 会回落到 executor 的内置 tag，
	// 而摘要只算一边 ⇒ 同一份配置两种摘要）。
	spec := r.DefaultSpec()
	if spec.Sandbox.Image != defaultImageTag || spec.Executor.Image != defaultImageTag {
		t.Fatalf("缺省镜像没有同时落到 Sandbox 与 Executor: %q / %q",
			spec.Sandbox.Image, spec.Executor.Image)
	}
	// 零值 Budget 在根包里表示「全部不限」——DefaultSpec 必须把它折成默认护栏。
	if spec.Budget != harness.DefaultBudget() {
		t.Fatalf("DefaultSpec 的预算应当是 DefaultBudget，得到 %+v", spec.Budget)
	}
	// store 根必须已经绝对化：相对路径会让装配层按 cwd 建目录，而 list/stats
	// 在另一个 cwd 下就找不到结果目录。v0.4 起它只活在装配层——RunSpec 里不再有
	// StoreDir，所以这条断言钉的是装配层自己持有的那个值。
	if !filepath.IsAbs(r.storeDir) {
		t.Fatalf("装配层的 store 根应当是绝对路径: %q", r.storeDir)
	}
	// provider 白名单为空在 executor 里是「全部拒绝」，装配层必须补上缺省白名单，
	// 否则容器里连模型 API 都连不上，而失败形态是 pi 静默超时。
	if got := r.docker.Config().ProviderAllowHosts; len(got) == 0 {
		t.Fatal("装配层没有补上 provider 白名单（空 = 全部拒绝）")
	}
	// ⚠️ **隔离开关必须是开着的。** 这条断言是回归：装配层曾经手写
	// `DockerConfig{ProviderAllowHosts: hosts}`，其余字段落零值，而零值里所有 bool
	// 都是 false = 关掉隔离——`ManageIptables=false` 让 installNetworkRules 第一行
	// 就静默返回（无白名单、无默认拒绝，容器可直连公网），`ProviderProxy=false`
	// 让模型流量不走宿主侧代理。实测现场：容器内 `1.1.1.1:443` 可达、
	// 非授权内网地址可达、且没有任何 *_PROXY 环境变量。
	//
	// 之所以长期没被发现，是因为集成用例都用 DefaultDockerConfig() 构造执行器，
	// 验的是另一条路径。这条断言钉的正是**生产装配**这一条。
	cfg := r.docker.Config()
	if !cfg.ManageIptables {
		t.Error("装配层把 ManageIptables 关掉了：白名单与默认拒绝都不会生效")
	}
	if !cfg.ProviderProxy {
		t.Error("装配层把 ProviderProxy 关掉了：模型流量不走宿主侧域名白名单代理")
	}
}

// TestNewTSecBenchWithoutBridgeFails 钉「tsecbench 场景需要可用的桥」。
//
// 用 REDCOPILOT_BRIDGE_CMD 指向一个立刻退出的命令：桥起不来时装配必须**失败**，
// 而不是带着一个死桥继续跑（那样每一次平台调用都会在运行中途才失败）。
//
// ⚠️ 这里**不设任何凭据环境变量**：本测试与凭据无关，也不该因为环境里有 token
// 而改变行为。
func TestNewTSecBenchWithoutBridgeFails(t *testing.T) {
	t.Setenv("REDCOPILOT_BRIDGE_CMD", "/bin/false")
	_, err := New(Options{StoreDir: t.TempDir(), Scenario: ScenarioTSecBench})
	if err == nil {
		t.Fatal("桥起不来时 tsecbench 场景应当装配失败")
	}
	if !harness.IsKind(err, harness.KindConfig) {
		t.Fatalf("应当是配置类错误（用户能改的那种），得到 %v", err)
	}
}

// TestBuildScenarioFakeIsOffline 钉「fake 场景不产生 bridge 客户端」。
//
// 它是「离线场景真的离线」的可观察判据：bridge 非空意味着起了一个常驻 python
// 子进程，而 fake 场景连网络都不该碰。
func TestBuildScenarioFakeIsOffline(t *testing.T) {
	sc, client, err := buildScenario(ScenarioFake, Options{}, t.TempDir())
	if err != nil {
		t.Fatalf("fake 场景应当构造成功: %v", err)
	}
	if sc == nil {
		t.Fatal("没有场景")
	}
	if client != nil {
		t.Fatal("fake 场景不该起 bridge 子进程")
	}
}

// TestLoadFakeFixtureBuiltin 钉「不给夹具时用内置演示题」。
func TestLoadFakeFixtureBuiltin(t *testing.T) {
	fx, err := loadFakeFixture("")
	if err != nil {
		t.Fatalf("内置演示题应当总是可用: %v", err)
	}
	if len(fx.Challenges) == 0 || len(fx.Answers) == 0 {
		t.Fatal("内置演示题缺少题目或答案")
	}
	ch := fx.Challenges[0]
	if !fx.Answers[ch.Code][demoAnswer] {
		t.Fatalf("内置演示题的答案表里没有它自己的答案: %+v", fx.Answers)
	}
	// 题面必须提到信封形态：gate.NewGate 用题面推断答案形态（answer.Infer），
	// 题面不提 flag{...} 时裸串与信封都可能被收。
	if !strings.Contains(ch.Description, "flag{") {
		t.Fatalf("演示题面里没有信封形态，答案形态推断会不确定: %q", ch.Description)
	}
	// Addrs 是 sandbox 白名单的唯一来源；空白名单会让容器出站全被拒绝。
	if len(ch.Addrs) == 0 {
		t.Fatal("演示题没有 Addrs：容器出站会被全部拒绝")
	}
}

// TestLoadFakeFixtureRejectsUnsolvable 钉「夹具里没有可接受答案时拒绝装配」。
//
// 没有答案表 ⇒ 任何候选都判不接受 ⇒ 每次运行都以 no_progress 收场。那是
// 「看起来跑通了、其实什么都没验证」，比直接报错危险得多。
func TestLoadFakeFixtureRejectsUnsolvable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fx.json")
	body := `{"challenges":[{"code":"c1","description":"找 flag{test-only}","flagCount":1}]}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写夹具失败: %v", err)
	}
	if _, err := loadFakeFixture(path); err == nil {
		t.Fatal("没有答案表的夹具应当被拒绝")
	}
	// 缺文件也要报错（而不是静默退回内置演示题——那会让一次路径打错变成
	// 「用演示题跑了一遍」，用户拿到的是假的验收结论）。
	if _, err := loadFakeFixture(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatal("夹具文件不存在时应当报错，而不是退回内置演示题")
	}
}
