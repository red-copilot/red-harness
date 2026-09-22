package local

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/dag"
	"github.com/red-copilot/red-harness/store"
)

// 本文件的测试**不碰 Docker、不碰网络、不碰平台**：装配层的可测部分正是
// 「接线对不对」，而「能不能真的起容器」属于 executor 的集成测试。

// pinDefaultEndpoint 把身份解析的输入钉在默认端点。
//
// 为什么每条装配用例都要调它：`local.New` 会把 DOCKER_HOST / DOCKER_CONTEXT 解析
// 成**锁身份**（那是它的职责），于是本文件的结果会随开发机上的这两个环境变量变化
// ——一条「本地跑绿、CI 变红」的测试比没有测试更糟。要覆盖非默认端点的行为，用
// 本函数与 `t.Setenv("DOCKER_HOST", ...)` 显式构造，别依赖环境。
func pinDefaultEndpoint(t *testing.T) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
}

// TestNewRequiresStoreDir 钉「缺 StoreDir 就装配失败」。
//
// 为什么不能给它一个缺省（比如 cwd）：StoreDir 是 RunSpec 摘要的输入，也是
// 「run 在哪儿建目录」与「list/stats 去哪儿读」共用的那个字符串。凭空给一个
// 缺省等于让同一次运行的落点取决于 cwd，那是最难查的一类漂移。
func TestNewRequiresStoreDir(t *testing.T) {
	pinDefaultEndpoint(t)
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
	pinDefaultEndpoint(t)
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
	pinDefaultEndpoint(t)
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
	//
	// ⚠️ 落点**不在 StoreDir 里**：flock 绑的是 inode，锁与被它保护的数据同住时，
	// 一次 `rm -rf` 旧 store 就会静默解锁一个正在运行的部署。这条断言（以及
	// TestNewLockIsKeyedByEndpoint）就是那个缺陷的出口门。
	wantLock := filepath.Join(DefaultLockDir, "run-13c4025c.lock")
	if r.lockPath != wantLock {
		t.Fatalf("缺省锁落点 = %q，期望 %q", r.lockPath, wantLock)
	}
	if strings.HasPrefix(r.lockPath, r.storeDir) {
		t.Fatalf("缺省锁落在了 StoreDir 里（%q ⊂ %q）：删掉旧 store 会静默解锁正在跑的部署",
			r.lockPath, r.storeDir)
	}
	if _, ok := defaultLock(r.ident.Endpoint).(*FileLock); !ok {
		t.Fatal("缺省锁应当是 *FileLock")
	}
	// 身份两项必须**同源**：owner 进资源标签、endpoint 进锁路径与 DOCKER_HOST。
	// 分头解析的产物是「锁按 A 端点、标签按 B 端点」，而那种错配的表现是
	// 「回收删不掉自己的资源，或删掉了别人的」。
	cfgOwned := r.docker.Config()
	if cfgOwned.Owner != string(r.ident.Owner) || cfgOwned.Owner == "" {
		t.Fatalf("执行器的 owner 与装配身份不一致: %q vs %q", cfgOwned.Owner, r.ident.Owner)
	}
	if cfgOwned.Endpoint.ID() != r.ident.Endpoint.ID() {
		t.Fatalf("执行器的端点与装配身份不一致: %q vs %q", cfgOwned.Endpoint.ID(), r.ident.Endpoint.ID())
	}
	// 审计端口必须真的接上：`Submit=true` 的部署靠它回答「提交了什么、平台怎么判的」。
	// 根包从传给它的 Results 上按类型断言取审计端口，所以这里判的是同一个对象。
	if !auditReady(r.results) {
		t.Fatal("装配出来的结果存储不是候选审计落点：Submit=true 时提交明细将不可复核")
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
	pinDefaultEndpoint(t)
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

// ── 图落盘 ──

// newDagGraphSaver 造一个**走生产接线形态**的 dagGraphSaver。
//
// `saver` 字段必须显式给：store 只接受 wired / disabled（graph.go:389），空串会被
// 拒。也就是说这个字段**没有**「缺省值」可依赖——它只能由构造处给出，这正是
// 「装配身份只有一个定义点」在测试里的表现。
func newDagGraphSaver(root *store.FileStore) *dagGraphSaver {
	return &dagGraphSaver{
		root:  root,
		live:  map[string]*dag.Graph{},
		saver: store.ArtifactSaverWired,
	}
}

// attemptDirFor 拼出某题一次尝试的产物目录（与 store 的 attemptDir 同形）。
//
// 这里**故意不 import store 的内部函数**（也 import 不到）：测试拼路径是**第二条**
// 实现，拼错了就会「测试绿、生产错」。所以下面每条断言都同时用 store 的读接口
// 回读一次——两条独立路径都指到同一处，才算落点正确。
func attemptDirFor(root, runID string, ch harness.Challenge) (string, string) {
	cid, err := harness.ChallengeIDFor(ch.Code)
	if err != nil {
		panic(err)
	}
	return filepath.Join(root, "runs", runID, "challenges", string(cid), "attempts", "1"), string(cid)
}

// TestDagGraphSaverWritesUnderStoreDir 钉落点与权限。
//
// 重点是**写在哪**：图属于**题目**，落在
// `<StoreDir>/runs/<runID>/challenges/<题目 ID>/attempts/1/`；公开指标在结果树
// （`<ResultDir>/results/<id>.json`）。两者缺省同根，一旦调用方把 --results 分开配，
// 写错地方就会让图跑进结果树——而 store 的 PutGraph 是唯一带 0600 与原子写的路径，
// 绕过它自己 os.WriteFile 会把权限也一起丢掉。
//
// ⚠️ N0.3 之前这里钉的是 `<StoreDir>/runs/<runID>/graph.json`（一 run 一图）。那条
// 布局下跑完只剩最后一道题的图，而「索引说两道题都有图」——所以除了新落点，本用例
// 还要断言旧落点**没有**被写：`ReadGraph` 有旧布局回退，旧文件一旦存在，回退就会
// 命中它，一题一图的缺陷原样复活（且是静默的）。
func TestDagGraphSaverWritesUnderStoreDir(t *testing.T) {
	dir := t.TempDir()
	root, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := newDagGraphSaver(root)

	ch := harness.Challenge{Code: "c1", Category: "web",
		Description: "找 flag{...}", FlagFormat: "flag{...}"}
	graph := dag.New(ch)
	if _, err := graph.AddFact(dag.Node{Kind: dag.NodeFact, FactKind: dag.FactService,
		Content: "nginx/1.18.0", Source: "bash: nmap"}); err != nil {
		t.Fatal(err)
	}
	s.register(ch.Code, graph)

	const runID = harness.RunID("run-1")
	state, err := s.SaveGraph(context.Background(), runID, ch)
	if err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}
	if state != harness.GraphSaved {
		t.Fatalf("两份都写成了，状态应当 = %q，得到 %q", harness.GraphSaved, state)
	}

	attDir, cid := attemptDirFor(dir, string(runID), ch)
	path := filepath.Join(attDir, "graph.json")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("图没有落在 <StoreDir>/runs/<runID>/challenges/<cid>/attempts/1/graph.json: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("graph.json 权限 = %o，期望 600（图含凭证事实与目标地址）", perm)
	}
	// 落盘的必须是一张**能载回来**的图，否则「留下来的是个好图」只是假设。
	back, err := dag.Load(path)
	if err != nil {
		t.Fatalf("落盘的图载不回来: %v", err)
	}
	if back.Code != ch.Code {
		t.Errorf("载回的题目编号 = %q，期望 %q", back.Code, ch.Code)
	}
	if len(back.Facts(dag.FactService)) != 1 {
		t.Errorf("载回的事实数不对: %+v", back.Stats())
	}
	// 第二条独立路径：store 的读接口必须指到同一份字节（它同时钉住「索引/布局只有
	// 一处定义」——测试自己拼的路径不能是唯一判据）。
	view, err := root.ForRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	got, src, err := view.ReadGraph(harness.ChallengeID(cid), harness.FirstAttempt)
	if err != nil {
		t.Fatalf("store 按题目读不回刚落的图: %v", err)
	}
	if src != store.GraphSourceChallenge {
		t.Errorf("读到的图来源 = %q，期望 %q（读到旧布局意味着图落错了地方）", src, store.GraphSourceChallenge)
	}
	if !strings.Contains(string(got), "nginx/1.18.0") {
		t.Errorf("store 读回的图里没有刚写的事实:\n%s", got)
	}
	// 登记项必须被清掉：Harness 跨 Run 复用，留着会把上一轮的图认成本轮的。
	s.mu.Lock()
	left := len(s.live)
	s.mu.Unlock()
	if left != 0 {
		t.Errorf("落盘后登记项应清空，还剩 %d 条", left)
	}

	// 人可读导出与图并排，同权限。它是这条链路**唯一**能被人直接看的东西——
	// 只落 graph.json 而没人画得出来，等于图还在但复盘仍然做不了。
	mmd, err := os.ReadFile(filepath.Join(attDir, "graph.mmd"))
	if err != nil {
		t.Fatalf("人可读导出没有落盘: %v", err)
	}
	if !strings.HasPrefix(string(mmd), "flowchart TD") {
		t.Errorf("导出不是一份 mermaid 文档:\n%s", mmd)
	}
	if !strings.Contains(string(mmd), "nginx/1.18.0") {
		t.Errorf("导出里没有图里的事实:\n%s", mmd)
	}
	stMmd, err := os.Stat(filepath.Join(attDir, "graph.mmd"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := stMmd.Mode().Perm(); perm != 0o600 {
		t.Errorf("graph.mmd 权限 = %o，期望 600（它与 graph.json 同级，含凭证事实与目标地址）", perm)
	}

	// 旧布局的 run 根必须**没有**图。
	if _, err := os.Stat(filepath.Join(dir, "runs", string(runID), "graph.json")); err == nil {
		t.Error("图同时写进了旧布局的 run 根：ReadGraph 的回退会命中它，一题一图的缺陷复活")
	}
}

// TestDagGraphSaverWritesPerChallengeAttempts 是 N0.3 的出口门：**一题一处**。
//
// 缺陷形态：图曾经落在 `<runDir>/graph.json`——一个 run 一份。跑两道题时第二题把
// 第一题盖掉，运行结束时盘上只剩最后一道题的图，而复盘的人无从知道第一题留过图。
//
// 所以这里必须同时钉三件事：两题各自的图在**各自的** attempts/1 目录下、两份内容
// **不同**（都写成了同一份就说明还是同一个落点）、以及产物索引里两题都在（索引是
// 「这次运行留下过什么」的唯一凭据，只有文件没有账同样是残缺的）。
func TestDagGraphSaverWritesPerChallengeAttempts(t *testing.T) {
	dir := t.TempDir()
	root, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := newDagGraphSaver(root)

	first := harness.Challenge{Code: "c-first", Description: "找 flag{...}", FlagFormat: "flag{...}"}
	second := harness.Challenge{Code: "c-second", Description: "找 flag{...}", FlagFormat: "flag{...}"}
	// 两题各加一条**不同**的事实：内容一样的话「两处都写了」与「一处写了两遍」
	// 在这些断言下同形。
	for _, tc := range []struct{ ch, fact string }{{
		first.Code, "nginx/1.18.0"}, {second.Code, "redis/6.2.7"}} {
		g := dag.New(harness.Challenge{Code: tc.ch})
		if _, err := g.AddFact(dag.Node{Kind: dag.NodeFact, FactKind: dag.FactService,
			Content: tc.fact, Source: "bash: nmap"}); err != nil {
			t.Fatal(err)
		}
		s.register(tc.ch, g)
	}

	const runID = harness.RunID("run-1")
	for _, ch := range []harness.Challenge{first, second} {
		state, err := s.SaveGraph(context.Background(), runID, ch)
		if err != nil {
			t.Fatalf("SaveGraph(%s): %v", ch.Code, err)
		}
		if state != harness.GraphSaved {
			t.Fatalf("SaveGraph(%s) 状态 = %q，期望 %q", ch.Code, state, harness.GraphSaved)
		}
	}

	firstDir, firstCID := attemptDirFor(dir, string(runID), first)
	secondDir, secondCID := attemptDirFor(dir, string(runID), second)
	if firstDir == secondDir {
		t.Fatal("两道题的产物目录算成了同一个：题目 ID 没有参与落点")
	}
	b1, err := os.ReadFile(filepath.Join(firstDir, "graph.json"))
	if err != nil {
		t.Fatalf("第一题的图没有落盘: %v", err)
	}
	b2, err := os.ReadFile(filepath.Join(secondDir, "graph.json"))
	if err != nil {
		t.Fatalf("第二题的图没有落盘: %v", err)
	}
	if string(b1) == string(b2) {
		t.Fatalf("两题的图字节完全相同：它们落到了同一处，或第二题盖掉了第一题")
	}
	if !strings.Contains(string(b1), "nginx/1.18.0") || strings.Contains(string(b1), "redis/6.2.7") {
		t.Errorf("第一题目录下的图不是第一题的:\n%s", b1)
	}
	if !strings.Contains(string(b2), "redis/6.2.7") || strings.Contains(string(b2), "nginx/1.18.0") {
		t.Errorf("第二题目录下的图不是第二题的:\n%s", b2)
	}
	// 两题各自的 mermaid 导出也要在：导出是按题画的，一题一份。
	for _, d := range []string{firstDir, secondDir} {
		if _, err := os.Stat(filepath.Join(d, "graph.mmd")); err != nil {
			t.Errorf("缺导出 %s: %v", filepath.Join(d, "graph.mmd"), err)
		}
	}

	// 索引里两题都在，且各自指向自己的目录 —— 合并语义（后者不覆盖前者）的出口门。
	view, err := root.ForRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := view.ArtifactIndex()
	if err != nil {
		t.Fatalf("读产物索引失败: %v", err)
	}
	if len(idx.Challenges) != 2 {
		t.Fatalf("索引里有 %d 条，期望 2 条（一题一条）: %+v", len(idx.Challenges), idx.Challenges)
	}
	if idx.GraphSaver != store.ArtifactSaverWired {
		t.Errorf("索引的 graphSaver = %q，期望 %q", idx.GraphSaver, store.ArtifactSaverWired)
	}
	seen := map[string]string{}
	for _, row := range idx.Challenges {
		seen[row.ChallengeID] = row.Code
		if row.Attempt != int(harness.FirstAttempt) {
			t.Errorf("%s 的 attempt = %d，期望 %d", row.Code, row.Attempt, harness.FirstAttempt)
		}
		if row.Graph.State != harness.GraphSaved || row.GraphExport.State != harness.GraphSaved {
			t.Errorf("%s 的产物状态 = %q / %q，期望两份都是 %q",
				row.Code, row.Graph.State, row.GraphExport.State, harness.GraphSaved)
		}
		// saved 必须带得出路径与摘要：没有它们，「账上说存了」就无从核对。
		if row.Graph.Path == "" || row.Graph.SHA256 == "" {
			t.Errorf("%s 的图登记缺路径或摘要: %+v", row.Code, row.Graph)
		}
	}
	if seen[firstCID] != first.Code || seen[secondCID] != second.Code {
		t.Fatalf("索引把两道题对错了号: %+v（期望 %s→%s、%s→%s）",
			seen, firstCID, first.Code, secondCID, second.Code)
	}
}

// TestDagGraphSaverScrubsPlaintextOnWrite：答案明文不得顺着图落盘出去。
//
// 这是端到端版的明文防线：语料走**真实写盘路径**（register → SaveGraph →
// store.PutGraph），而不是直接断言 dag 的序列化。回归背景：`json.Marshal` 曾经
// 漏擦 `Rejected[].Content`，而那里面装的正是命中答案形状的原文——用
// json.Marshal 落图是这件事最自然的实现方式，所以这条必须钉在**装配层**上。
func TestDagGraphSaverScrubsPlaintextOnWrite(t *testing.T) {
	dir := t.TempDir()
	root, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := newDagGraphSaver(root)

	ch := harness.Challenge{Code: "c1", Description: "找 flag{...}", FlagFormat: "flag{...}"}
	graph := dag.New(ch)
	const secret = "flag{leaked_through_graph_json}"
	// 这条会被 ErrAnswerShaped 拒收，而拒收审计里带的就是它的原文。
	if _, err := graph.AddFact(dag.Node{Kind: dag.NodeFact, FactKind: dag.FactArtifact,
		Content: secret, Source: "bash: cat /tmp/f"}); err == nil {
		t.Fatal("前置条件：答案形状的事实必须被拒")
	}
	s.register(ch.Code, graph)

	const runID = harness.RunID("run-1")
	if _, err := s.SaveGraph(context.Background(), runID, ch); err != nil {
		t.Fatal(err)
	}
	attDir, _ := attemptDirFor(dir, string(runID), ch)
	b, err := os.ReadFile(filepath.Join(attDir, "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) || strings.Contains(string(b), "leaked_through") {
		t.Fatalf("图落盘泄漏了答案明文:\n%s", b)
	}
	// 导出是**第二条**序列化路径（dag.Mermaid 也走 document()），必须一起擦。
	mmd, err := os.ReadFile(filepath.Join(attDir, "graph.mmd"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mmd), secret) || strings.Contains(string(mmd), "leaked_through") {
		t.Fatalf("人可读导出泄漏了答案明文:\n%s", mmd)
	}
}

// TestDagGraphSaverSkipsUnregistered：没登记过的题目不是错误。
//
// 图落盘是可选面：自定义 Planner 的调用方（或本题根本没建图）走不到 register，
// 那不是失败，也不该记一笔 GraphSaveFailures——那会把「本来就没有图」读成「图坏了」。
// 切签名之前这里返回 nil，于是「没登记」与「写成功了」在调用方眼里同形；现在它必须
// 明确回答 `absent`，且**没有文件产生**。
func TestDagGraphSaverSkipsUnregistered(t *testing.T) {
	dir := t.TempDir()
	root, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := newDagGraphSaver(root)
	ch := harness.Challenge{Code: "never-registered"}
	state, err := s.SaveGraph(context.Background(), "run-1", ch)
	if err != nil {
		t.Fatalf("未登记的题目不该报错: %v", err)
	}
	if state != harness.GraphAbsent {
		t.Fatalf("未登记的题目状态 = %q，期望 %q（这一题本来就没有图）", state, harness.GraphAbsent)
	}
	// 「absent」必须字面为真：任何产物、任何索引都不该出现。索引同样不能存在——
	// 为一句「没有图」建一份 artifacts.json，会让下游把这一题当成「登记过」。
	if _, err := os.Stat(filepath.Join(dir, "runs", "run-1")); err == nil {
		t.Error("未登记的题目在存储里留下了痕迹：absent 应当意味着一个字节都不写")
	}
}

// TestNewFakeScenarioWiresGraphSaver：装配层必须真的把图落盘接上。
//
// 只断言「Harness 造出来了」不够——可选端口最容易的失败形态就是**没人接**：
// 所有测试全绿，而每次运行都没有图，且没有任何地方说明为什么。
func TestNewFakeScenarioWiresGraphSaver(t *testing.T) {
	pinDefaultEndpoint(t)
	r, err := New(Options{StoreDir: t.TempDir(), Scenario: ScenarioFake})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = r.Close() }()

	var found bool
	for _, c := range r.h.Doctor(context.Background()).Checks {
		if c.Name == "graph_saver" {
			found = true
			if !c.OK {
				t.Errorf("装配层没有接上图落盘: %s", c.Detail)
			}
		}
	}
	if !found {
		t.Fatal("体检里没有 graph_saver 这一项")
	}
}

// TestNewLockIsKeyedByEndpoint 钉「锁的身份是 daemon 端点，不是 StoreDir」。
//
// 这是 N0.2 最上面那条缺陷的**装配级**出口门（单元版在 lock_test.go）。
// 缺陷形态：锁落在 `<StoreDir>/run.lock`。flock 绑的是 **inode** 而不是路径，于是
// `rm -rf` 掉旧 store（或换一个 `--store` 指向同一个部署的数据）之后，同名路径指向
// 一个**新 inode**——正在跑的那次部署被静默解锁，两个进程同时操作同一批容器。
//
// ⚠️ 两次装配**共用同一个 StoreDir**，这不是为了省事：StoreDir 只有在它相同的时候
// 才是「另一个变量被排除掉了」。各给一个临时目录时，旧实现（锁跟随 StoreDir）也能
// 通过「两条路径不同」这条断言——因为两个 StoreDir 本来就不同。共用一个目录之后，
// 剩下的唯一变量就是端点，断言才真的钉在端点上。
//
// 两条断言合起来才够：同一 StoreDir 下两个不同端点拿到两个不同的锁文件（key 是
// 端点），以及两条路径都不在 StoreDir 里（锁不与被它保护的数据同住）。
func TestNewLockIsKeyedByEndpoint(t *testing.T) {
	storeDir := t.TempDir()
	newRunnerAt := func(t *testing.T, host string) *Runner {
		t.Helper()
		pinDefaultEndpoint(t)
		if host != "" {
			t.Setenv("DOCKER_HOST", host)
		}
		r, err := New(Options{StoreDir: storeDir, Scenario: ScenarioFake})
		if err != nil {
			t.Fatalf("装配失败（DOCKER_HOST=%q）: %v", host, err)
		}
		t.Cleanup(func() { _ = r.Close() })
		return r
	}

	def := newRunnerAt(t, "")
	alt := newRunnerAt(t, "unix:///run/docker-alt.sock")

	if def.storeDir != alt.storeDir {
		t.Fatalf("前置条件不成立：两次装配的 StoreDir 不同（%q / %q）", def.storeDir, alt.storeDir)
	}
	if def.lockPath == "" || alt.lockPath == "" {
		t.Fatalf("缺省装配必须自己建锁（否则跨进程互斥没人管）: %q / %q", def.lockPath, alt.lockPath)
	}
	if def.lockPath == alt.lockPath {
		t.Fatalf("同一个 store 下两个不同的 daemon 端点共用了锁文件 %q：互斥范围与资源范围不一致（旧实现锁跟随 StoreDir 时就是这个形态）", def.lockPath)
	}
	if def.ident.Endpoint.ID() == alt.ident.Endpoint.ID() {
		t.Fatalf("前置条件不成立：两个 DOCKER_HOST 解析成了同一个端点 %q", def.ident.Endpoint.ID())
	}
	for _, r := range []*Runner{def, alt} {
		if strings.HasPrefix(r.lockPath, r.storeDir) {
			t.Fatalf("锁落在了 StoreDir 里（%q ⊂ %q）：rm -rf 旧 store 会让它在跑的部署静默解锁",
				r.lockPath, r.storeDir)
		}
		if !strings.HasPrefix(r.lockPath, DefaultLockDir) {
			t.Errorf("锁没有落在 %s 下: %q", DefaultLockDir, r.lockPath)
		}
	}
	// 端点身份必须**同源**：锁路径由它取名，执行器的 DOCKER_HOST 由它给出。
	// 分头解析的错配表现是「锁按 A 端点、容器起在 B 端点」。
	if alt.docker.Config().Endpoint.ID() != alt.ident.Endpoint.ID() {
		t.Fatalf("执行器端点 %q 与锁身份 %q 分家",
			alt.docker.Config().Endpoint.ID(), alt.ident.Endpoint.ID())
	}
	// 显式传 Lock 时**完全以调用方为准**（连落点一起），lockPath 因此留空：
	// 装配层不该假装自己知道调用方那把锁在哪。
	custom := NewFileLock(filepath.Join(t.TempDir(), "custom.lock"))
	pinDefaultEndpoint(t)
	r, err := New(Options{StoreDir: t.TempDir(), Scenario: ScenarioFake, Lock: custom})
	if err != nil {
		t.Fatalf("带显式锁的装配失败: %v", err)
	}
	defer func() { _ = r.Close() }()
	if r.lockPath != "" {
		t.Errorf("显式给了 Lock 时装配层不该报出缺省落点，得到 %q", r.lockPath)
	}
}

// TestNewIdentityFailureLeavesNoTrace 钉「身份不可判定时**什么都没发生就拒绝**」。
//
// 为什么这条单独有测试：身份解析被放在 `New` 的最前面是有意的（见 New 的注释），
// 而「放在最前面」这件事只有在失败路径上才看得出来。挪到建目录 / 起 bridge 子进程
// 之后，一次配置错误就会在磁盘上留下一半现场（空 store、半个结果目录、一个常驻
// python），而错误消息本身完全一样——所以必须有断言钉住「没有痕迹」。
//
// 失败判据用非 unix scheme 的 DOCKER_HOST：它与网络无关、与权限无关，纯配置。
func TestNewIdentityFailureLeavesNoTrace(t *testing.T) {
	pinDefaultEndpoint(t)
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")

	storeDir := filepath.Join(t.TempDir(), "store")
	resultDir := filepath.Join(t.TempDir(), "results")
	_, err := New(Options{StoreDir: storeDir, ResultDir: resultDir, Scenario: ScenarioFake})
	if err == nil {
		t.Fatal("身份不可判定时装配必须失败（fail closed），而不是带着一个编造的身份继续")
	}
	if !harness.IsKind(err, harness.KindConfig) {
		t.Fatalf("应当是配置类错误（用户能改的那种），得到 %v", err)
	}
	for _, d := range []string{storeDir, resultDir} {
		if _, statErr := os.Stat(d); statErr == nil {
			t.Errorf("装配失败却留下了 %s：身份判定必须在任何副作用之前", d)
		}
	}
}

// TestAuditReadyIsATypePredicate 钉「审计就绪的判据是类型断言」。
//
// 这道闸在**今天的默认装配上恒为真**（`store.ResultFileStore` 同时实现 ResultStore
// 与 AuditStore），所以它没有一条能真正走通的失败路径。正因为如此，判据本身必须被
// 直接钉住：否则哪天有人把它改写成 `return true`（或换成某个具体类型判断），没有
// 任何测试会变红，而那道闸就静默消失了——`Submit=true` 的平台写操作照常发生，
// 只是再也没人答得出「提交了什么、平台怎么判的」。
func TestAuditReadyIsATypePredicate(t *testing.T) {
	rs, err := store.NewResultStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !auditReady(rs) {
		t.Error("store.ResultFileStore 必须同时实现 AuditStore：否则 Submit=true 的部署一件都装不起来")
	}
	if auditReady(nil) {
		t.Error("nil 不该被判成审计就绪")
	}
	if auditReady(noAuditStore{}) {
		t.Error("只实现 ResultStore 的类型不该被判成审计就绪：那道闸就是为它准备的")
	}
}

// noAuditStore 是一个**只**实现 ResultStore 的替身（审计闸的假想对手）。
//
// 用嵌入接口而不是手写一堆方法：这里要表达的正是「ResultStore 的其余部分照旧，
// 唯独不实现 AuditStore」。
type noAuditStore struct{ harness.ResultStore }

// TestNewOwnerOverrideIsVerbatimAndSafe 钉「显式 Owner 覆盖：原样采用、非法即拒绝」。
//
// 两件事必须同时成立：覆盖值**不被归一化**（归一化会把两个身份映射成一个，方向是
// 「外来的看起来像我的」——删除闸门唯一不能出的错），以及非法值在**任何副作用之前**
// 被拒（与身份解析失败同一条纪律：配置错误不该在磁盘上留半个现场）。
func TestNewOwnerOverrideIsVerbatimAndSafe(t *testing.T) {
	pinDefaultEndpoint(t)
	dir := t.TempDir()
	r, err := New(Options{StoreDir: filepath.Join(dir, "store"), Scenario: ScenarioFake,
		Owner: "box-9/1000"})
	if err != nil {
		t.Fatalf("合法的 Owner 覆盖不该让装配失败: %v", err)
	}
	defer func() { _ = r.Close() }()
	if got := r.ident.Owner; got != harness.OwnerID("box-9/1000") {
		t.Errorf("装配身份里的 owner = %q，期望原样的 %q", got, "box-9/1000")
	}
	// 覆盖值必须走到**资源标签**那一侧（执行器的 owner 是删除闸门的输入）。
	// 只改 ident 而没传给执行器，表现是「回收删不掉自己的资源」。
	if got := r.docker.Config().Owner; got != "box-9/1000" {
		t.Errorf("执行器的 owner = %q，期望原样的 %q", got, "box-9/1000")
	}

	badDir := filepath.Join(dir, "store-bad")
	// 用 `@` 当非法字符：它在本包的任何消息模板里都不出现，所以「消息里没有 @」
	// 是一条精确的断言——断言「没有那个字符」而不是「没有某个词」，是因为回显
	// 一个单独的字符最容易漏掉（它看起来无害）。
	const badOwner = "probe-bad@host/7"
	_, err = New(Options{StoreDir: badDir, Scenario: ScenarioFake, Owner: badOwner})
	if err == nil {
		t.Fatal("含非法字符的 Owner 覆盖值必须被拒绝（它原样进资源标签与规则注释）")
	}
	if !harness.IsKind(err, harness.KindConfig) {
		t.Fatalf("应当是配置类错误，得到 %v", err)
	}
	if _, statErr := os.Stat(badDir); statErr == nil {
		t.Error("Owner 非法却留下了存储目录：拒绝必须在任何副作用之前")
	}
	// 消息里不得回显原值（也不得回显那个字符）：它是公开面（终端/工单），而
	// owner 的原值来自 hostname。
	if msg := err.Error(); strings.Contains(msg, badOwner) || strings.Contains(msg, "probe-bad") || strings.Contains(msg, "@") {
		t.Errorf("拒绝消息里回显了 owner 原值: %v", err)
	}
}

// TestDoctorReportsIdentityWithoutRawOwner 钉体检里那一行身份。
//
// 为什么这行不能省：N0.2 的缺陷是「锁锁在哪儿」与「资源标签写什么」各算各的，
// 而两者都从身份来。体检是唯一能在**运行之前**核对它们同源的地方。
//
// ⚠️ 两条边界同时钉住：endpoint 的规范化 ID 必须可见（它是「锁到底锁在哪个 daemon
// 上」的唯一判据），owner **原值**必须不可见（原值来自 hostname，而报告会被贴进
// 工单；它的用处已经全部由指纹承担）。
func TestDoctorReportsIdentityWithoutRawOwner(t *testing.T) {
	pinDefaultEndpoint(t)
	// 用一个显式的、一眼能认出来的 owner，好断言它没出现在报告里。
	const raw = "doctor-probe-host/4242"
	r, err := New(Options{StoreDir: t.TempDir(), Scenario: ScenarioFake, Owner: raw})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = r.Close() }()

	var found bool
	for _, c := range r.Doctor(context.Background()).Checks {
		if c.Name != "execution_identity" {
			continue
		}
		found = true
		if c.Fatal {
			t.Error("身份一行不该是 Fatal：走到这里身份已经解析成功，它只是让人核对")
		}
		if !strings.Contains(c.Detail, r.ident.Endpoint.ID()) {
			t.Errorf("详情里没有 endpoint 的规范化 ID（%q）: %s", r.ident.Endpoint.ID(), c.Detail)
		}
		if !strings.Contains(c.Detail, r.ident.Owner.Fingerprint()) {
			t.Errorf("详情里没有 owner 指纹（%q）: %s", r.ident.Owner.Fingerprint(), c.Detail)
		}
		if strings.Contains(c.Detail, raw) || strings.Contains(c.Detail, "doctor-probe-host") {
			t.Errorf("详情里出现了 owner 原值: %s", c.Detail)
		}
	}
	if !found {
		t.Fatal("体检里没有 execution_identity 这一项")
	}
}

// TestChallengeShapeUsesBothSources 钉住「形态判定只有一个来源」。
//
// gate 与 dag 都用 challengeShape：前者决定候选能不能进账本，后者决定答案形状的
// 内容能不能进图。两处此前**各拼一份输入**（gate 只传 Description），于是同一次
// 运行里两个判据可能不是同一个形态——而两边看起来都正常，这种漂移除了对比两个
// 组件的行为之外无从发现。
//
// 这里直接钉住「平台下发的 FlagFormat 会被算进去」：题面什么都没说时，若只看
// 题面就会退回默认信封，而平台明说了格式就该按平台的来。
func TestChallengeShapeUsesBothSources(t *testing.T) {
	// 题面没有线索，平台下发了格式：必须按平台给的形态。
	ch := harness.Challenge{Code: "c1", Description: "找到答案并提交", FlagFormat: "ctf{"}
	got := challengeShape(ch)
	if !got.Contains("ctf{abc}") {
		t.Errorf("平台下发的 FlagFormat 没有生效：%+v", got.Envelopes)
	}

	// 反向：两个来源都空时只认默认信封（v0.5 收紧，见 answer.Infer 的注释）。
	// 这条断言防的是「有人为了兼容把 AllowRaw 兜底加回来」。
	empty := challengeShape(harness.Challenge{Code: "c2"})
	if empty.AllowRaw {
		t.Error("两个来源都空时不得默认允许裸串——那正是 147 次提交的成因")
	}
}
