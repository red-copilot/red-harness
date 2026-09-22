// Package wire 是 v0.4 的**唯一**装配层：把根包契约与各实现包拼成一台可运行的
// Harness。
//
// 为什么需要它、以及为什么它必须是一个独立的小包：
//
//   - 根包 `harness` 是纯契约（零实现、零内部依赖），实现包之间**不互相导入**
//     （`executor` 不认识 `scenario`，`scenario` 不认识 `dag`）。所以「谁把
//     `scenario.TSecBench` 交给 `harness.Harness`」这件事在架构上必然发生在
//     契约层之外、又必须在某个地方发生——这个地方就是这里。
//   - CLI 不直接导入实现包：它只依赖包内定义的窄接口（`internal/cli.Ports`），
//     这样测试可以注入记账型 fake，而真实装配只有一条路径（`cmd/red-harness`
//     经 `internal/cli` 的 `WireFunc` 调 `wire.New`）。
//
// 依赖方向：`internal/wire` → {harness, answer?, dag, gate, store, executor,
// bridge, piai, scenario}。**没有任何实现包反过来依赖它**，所以它处在依赖图
// 的叶子位置，不会把实现包的编译耦合带进 CLI。
package wire

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/bridge"
	"github.com/red-copilot/red-harness/dag"
	"github.com/red-copilot/red-harness/executor"
	"github.com/red-copilot/red-harness/gate"
	"github.com/red-copilot/red-harness/piai"
	"github.com/red-copilot/red-harness/scenario"
	"github.com/red-copilot/red-harness/store"
)

// 场景名。它们是 CLI 的公开面（`--scenario`），改名字是破坏性变更。
const (
	// ScenarioFake 是离线场景：状态全在内存里，不碰网络、不碰平台。
	ScenarioFake = "fake"
	// ScenarioTSecBench 是真实平台场景（经 Python SDK bridge 常驻子进程）。
	ScenarioTSecBench = "tsecbench"
)

// defaultImageTag 是缺省 runner 镜像，与 `runner/README.md` 记录的产物 tag 一致
// （也是 executor 内部的缺省值）。
const defaultImageTag = "red-harness-runner:v0.3.0"

// 缺省 provider 白名单。
//
// 为什么要在**装配层**补白名单而不是留空：`executor.DockerConfig.ProviderAllowHosts`
// 空表示「全部拒绝」（fail closed，见 executor/spec.go 的注释），于是一个什么都没
// 配置的 run 会在容器里连模型 API 都连不上——而失败形态是 pi 静默超时，不是报错。
// 这里给的是 opencode-go 的 API 域名；换 provider 请用 Options.ProviderHosts 覆盖。
var defaultProviderHosts = []string{"opencode.ai"}

// Options 是装配一台 Harness 需要的全部输入。
//
// ⚠️ **这里没有任何凭据字段，将来也不许加**：RunSpec 会整份写进 run.json，
// 平台 token 只经 bridge 子进程的环境变量传递（`bridge.ClientConfig.Token` 留空
// 即从 BENCHMARK_TOKEN 读），provider key 只经容器主进程环境传递（piai 自己
// 从 .env / 宿主环境解析）。凭据一旦成为本结构的字段，它就会顺着 RunSpec 或
// 日志扩散出去。
type Options struct {
	// StoreDir 是运行目录根（`<StoreDir>/runs/<runID>/`）。必须非空；相对路径
	// 会被绝对化——同一个 store 只能有一种字符串表示（见 internal/cli 的
	// absStoreDir 注释）。
	StoreDir string
	// ResultDir 是公开指标根（`<ResultDir>/results/<runID>.json`）。为空时取
	// StoreDir：指标与运行目录同根是缺省形态，分开是为了让结果能被单独拷走。
	ResultDir string
	// Scenario 是场景名：ScenarioFake / ScenarioTSecBench。为空按 fake 处理。
	Scenario string

	// Lock 是跨进程单运行锁。**为空时装配层自己建一个**（`<StoreDir>/run.lock`
	// 上的 flock）——单运行互斥是生产必需，不该靠调用方记得传。
	Lock harness.RunLocker

	// FakeChallenges 是离线场景的题目夹具 JSON 路径。为空时用内置演示题。
	// 见 loadFakeFixture。
	FakeChallenges string

	// EnvFile 是 pi 凭据 `.env` 的显式路径（空则从工作目录向上查找）。
	// 只用于 doctor 的「凭据是否设置」检查与 piai 的注入。
	EnvFile string
	// ProviderHosts 覆盖 provider 代理白名单。为空用 defaultProviderHosts。
	ProviderHosts []string

	// Agent 是 pi 的启动配置。
	Agent AgentOptions
	// Sandbox 是 Docker sandbox 的配置。
	Sandbox SandboxOptions
	// Run 是每次运行的缺省配置（目标、预算、策略、是否提交）。
	Run RunOptions
	// Profile 是 solver profile。为空时用内置的 defaultProfile()。
	Profile harness.SolverProfile

	// Now 覆盖时钟（测试用）。为空用 time.Now。
	Now func() time.Time
}

// AgentOptions 是 pi 的启动配置。字段与 harness.AgentSpec 一一对应，外加
// BinPath（容器内的 pi 名字）。
type AgentOptions struct {
	Provider   string
	Model      string
	Thinking   string
	Extensions []string
	Approve    bool
	SessionDir string
	HomeDir    string
	// BinPath 是**容器内**的 pi 名字或路径。为空用 "pi"（容器 PATH 解析）。
	// ⚠️ 填宿主绝对路径是配置错误：那个路径在容器里不存在，pi 起不来。
	BinPath string
}

// SandboxOptions 是 Docker sandbox 的配置。
type SandboxOptions struct {
	// Image 是 runner 镜像。为空用 defaultImageTag。
	Image string
	// ProfileDir 是要只读挂进容器的 solver profile bundle（宿主绝对路径）。
	ProfileDir string
	// CPUs / MemoryMB / PidsLimit 为 0 时用镜像默认。
	CPUs      float64
	MemoryMB  int
	PidsLimit int
}

// RunOptions 是每次运行的缺省配置。
type RunOptions struct {
	Targets    []string
	Budget     harness.Budget
	HintPolicy string
	Submit     bool
	Policy     harness.PolicySpec
}

// Runner 是装配完成的 Harness 及其附属物。
//
// 它是 CLI 眼里的 `Run + Doctor` 两件事的实现；**刻意不暴露 *harness.Harness**：
// CLI 只需要这两个动作，暴露整个 façade 会让「CLI 到底用了什么」变成不可读的
// 事实（这正是 v0.3 的 Ports 注释里那条纪律）。
type Runner struct {
	h      *harness.Harness
	docker *executor.Docker
	// scenario 是本次装配选的场景名（doctor 用它决定要不要做 VPN 预检）。
	scenario string
	// bridgeClient 只在 tsecbench 场景下非空；Close 时负责关掉子进程。
	bridgeClient *bridge.Client
	// opts 是装配时的输入副本（Run 用它拼 RunSpec）。
	opts Options
	// results 是公开指标存储。CLI 的 list/stats 读它，而它**不是**从 h 里
	// 反向取出来的（`harness.Harness.results` 是非导出字段）——所以装配层
	// 自己留一份。
	results harness.ResultStore
	// storeDir / resultDir 是绝对化之后的目录。
	storeDir  string
	resultDir string
}

// 编译期断言：Runner 就是 CLI 要的那两个动作（形状由 internal/cli 的窄接口定义，
// 这里只能断言方法签名与根包契约一致）。
var _ interface {
	Run(context.Context, harness.RunSpec) (harness.RunResult, error)
	Doctor(context.Context) harness.DoctorReport
} = (*Runner)(nil)

// New 装配一台 v0.4 Harness。
//
// **缺生产必需端口时它必须失败**（v0.4 要求），而这里有两道闸：
//
//  1. 本函数自己的校验（场景名、目录、镜像、题目夹具）；
//  2. `harness.NewHarness` 的校验 + 紧接着一次 `Harness.Doctor`——后者是
//     **根包自己**对 Planner/Renderer/Gate/Scenario/Sandbox/Agents 六项的
//     齐备性判定。
//
// 为什么第 2 道闸要写成一次 Doctor 调用而不是「再抄一遍检查」：根包的
// `HarnessOptions` 把 Planner/Renderer/Gate 定义成**可选**（`v04.go` 的
// `NewHarness` 只拒绝 Scenario/Sandbox/Agents），而缺了它们时 `runChallenge`
// 会把每道题记成「本题未执行任何轮次」——一次**静默**的装配错误会被报告读成
// 「模型不行」。让根包自己回答「齐不齐」是最不容易漂移的写法：根包将来往
// Fatal 检查里加一项，这里自动跟着拦住。
func New(opts Options) (*Runner, error) {
	if strings.TrimSpace(opts.StoreDir) == "" {
		return nil, harness.Ef(harness.KindConfig, "wire.new", "StoreDir 不能为空", nil)
	}
	storeDir, err := filepath.Abs(opts.StoreDir)
	if err != nil {
		return nil, harness.Ef(harness.KindConfig, "wire.new", "解析 StoreDir 失败", err)
	}
	resultDir := opts.ResultDir
	if strings.TrimSpace(resultDir) == "" {
		resultDir = storeDir
	} else if resultDir, err = filepath.Abs(resultDir); err != nil {
		return nil, harness.Ef(harness.KindConfig, "wire.new", "解析 ResultDir 失败", err)
	}

	name := strings.TrimSpace(opts.Scenario)
	if name == "" {
		name = ScenarioFake
	}

	// 公开指标存储：它同时是「生产必需端口」之一（缺了它 Run 的结果无处落盘）。
	results, err := store.NewResultStore(resultDir)
	if err != nil {
		return nil, err
	}

	sc, client, err := buildScenario(name, opts, storeDir)
	if err != nil {
		return nil, err
	}
	// 失败路径必须把已经起来的 bridge 子进程关掉：它是一个常驻的 python，
	// 泄漏出去会一直占着 SDK 会话。
	fail := func(err error) (*Runner, error) {
		if client != nil {
			client.Shutdown()
		}
		return nil, err
	}

	image := strings.TrimSpace(opts.Sandbox.Image)
	if image == "" {
		image = defaultImageTag
	}
	hosts := opts.ProviderHosts
	if len(hosts) == 0 {
		hosts = defaultProviderHosts
	}
	docker, err := executor.NewDocker(executor.DockerConfig{ProviderAllowHosts: hosts})
	if err != nil {
		return fail(err)
	}

	locker := opts.Lock
	if locker == nil {
		locker = defaultLock(storeDir)
	}

	profile := opts.Profile
	if profile.Name == "" {
		profile = defaultProfile()
	}

	optsCopy := opts
	optsCopy.StoreDir, optsCopy.ResultDir, optsCopy.Scenario = storeDir, resultDir, name
	optsCopy.Agent.Provider = strings.TrimSpace(opts.Agent.Provider)
	optsCopy.Agent.Model = strings.TrimSpace(opts.Agent.Model)
	optsCopy.Sandbox.Image = image

	h, err := harness.NewHarness(harness.HarnessOptions{
		Scenario: sc,
		Sandbox:  docker,
		Agents:   &piai.Factory{BinPath: opts.Agent.BinPath, EnvFile: opts.EnvFile},
		Results:  results,
		Locker:   locker,
		Gate:     func(ch harness.Challenge) harness.CandidateGate { return gate.NewGate(ch.Description) },
		SolverWithProfile: func(ch harness.Challenge, profile harness.SolverProfile) (harness.Planner, harness.Renderer) {
			graph := dag.New(ch)
			renderer := &dag.Renderer{G: graph}
			renderer.MaxFacts = profileLimit(profile.PromptPolicy, "maxFacts")
			renderer.MaxNegative = profileLimit(profile.PromptPolicy, "maxNegative")
			return dag.NewScheduler(graph), renderer
		},
		Profile: profile,
		Now:     opts.Now,
	})
	if err != nil {
		return fail(err)
	}
	// 第二道闸：根包自己判定「必需端口齐不齐」。见 New 的注释。
	//
	// ⚠️ **已知缺口（属于根包，不在本包权限内）**：`harness.HarnessOptions` 把
	// Planner/Renderer/Gate 定义成可选，`NewHarness` 不拒绝它们为空，而 `Doctor`
	// 的六项 Fatal 检查里**恰好没有 ResultStore**——所以「ResultStore 缺失」这件
	// 事根包既不在启动时拒绝、也不在体检里报告。本包的 New 一定给了 Results
	// （store.NewResultStore 失败即装配失败），所以这条缺口在 wire 路径上不可达；
	// 但直接调 `harness.NewHarness` 的调用方会得到一个「跑完不落盘、也不报错」
	// 的 Harness。建议根包把 Results 加进 Fatal 检查并让 NewHarness 拒绝它为 nil。
	if rep := h.Doctor(context.Background()); !rep.OK {
		return fail(harness.Ef(harness.KindConfig, "wire.new",
			"装配不完整，缺少必需端口: "+missingChecks(rep), nil))
	}
	// ResultStore 齐备性是本包**显式**补上的一条：见上面的缺口说明。
	if optsResultsNil(results) {
		return fail(harness.Ef(harness.KindConfig, "wire.new", "缺少必需端口: Results（公开指标存储）", nil))
	}
	return &Runner{h: h, docker: docker, scenario: name, bridgeClient: client,
		opts: optsCopy, results: results, storeDir: storeDir, resultDir: resultDir}, nil
}

// Results 返回公开指标存储，供 CLI 的 `list` / `stats` 读取。
//
// 为什么装配层要把它留一份：`harness.Harness.results` 是非导出字段，装配层没有
// 别的办法把它交出去；而 `list`/`stats` 需要的正是这个存储（它们读的是**公开
// 指标**，不是运行中状态——v0.4 没有运行中状态可读）。
func (r *Runner) Results() harness.ResultStore {
	if r == nil {
		return nil
	}
	return r.results
}

// missingChecks 把体检报告里失败的检查项拼成一句人话。
func missingChecks(rep harness.DoctorReport) string {
	var bad []string
	for _, c := range rep.Checks {
		if !c.OK {
			bad = append(bad, c.Name)
		}
	}
	if len(bad) == 0 {
		return "（未报告具体项）"
	}
	return strings.Join(bad, ", ")
}

// optsResultsNil 报告公开指标存储是否缺失。
//
// 单独一个函数是为了让上面那条显式检查可读，也为了让「为什么它必须存在」有
// 一个地方写：缺它时 `Harness.Run` 会**静默跳过**落盘（`v04.go` 里
// `if h.results != nil`），于是每次运行都跑得好好的、却什么指标都没留下——
// 而 stats 读的正是这些指标，表现为「跑了几十次，stats 说零次」。
func optsResultsNil(rs harness.ResultStore) bool { return rs == nil }

func profileLimit(values map[string]any, key string) int {
	v, ok := values[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		if n > 0 && n <= 10000 {
			return n
		}
	case float64:
		i := int(n)
		if n > 0 && n <= 10000 && float64(i) == n {
			return i
		}
	}
	return 0
}

// buildScenario 按名字造场景。返回值里的 *bridge.Client 只在 tsecbench 下非空，
// 调用方负责在失败路径与 Close 时关掉它。
//
// ⚠️ **平台凭据只走子进程环境变量**：ClientConfig.Token 留空 ⇒ bridge 自己从
// `BENCHMARK_TOKEN` 读（`bridge/client.go` 的 EnvToken）。这里**绝不**把 token
// 写进 ClientConfig，更不会写进 argv——argv 在 `ps` 里可见。
func buildScenario(name string, opts Options, storeDir string) (harness.Scenario, *bridge.Client, error) {
	switch name {
	case ScenarioFake:
		fx, err := loadFakeFixture(opts.FakeChallenges)
		if err != nil {
			return nil, nil, err
		}
		return &scenario.Fake{Challenges: fx.Challenges, Answers: fx.Answers, ScorePerFlag: fx.ScorePerFlag}, nil, nil
	case ScenarioTSecBench:
		client, err := bridge.NewClient(bridge.ClientConfig{
			// 桥的 stderr 里可能有 base_url / 请求 URL / token 片段，按明文纪律
			// 只能落进 private/（0700/0600），绝不继承宿主 stderr。
			PrivateDir: filepath.Join(storeDir, "private"),
		})
		if err != nil {
			return nil, nil, harness.Ef(harness.KindConfig, "wire.new",
				"启动 TSecBench bridge 失败（场景 "+ScenarioTSecBench+" 需要可用的 Python SDK 桥）", err)
		}
		return &scenario.TSecBench{Platform: client}, client, nil
	default:
		return nil, nil, harness.Ef(harness.KindConfig, "wire.new",
			fmt.Sprintf("未知场景 %q（可用：%s / %s）", name, ScenarioFake, ScenarioTSecBench), nil)
	}
}

// Close 释放装配层持有的资源（目前只有 bridge 子进程）。
//
// 幂等：重复调用返回 nil。**它不关闭任何 run**——run 的生命周期由 Run 自己
// 负责（ctx 取消 ⇒ 引擎清理容器与网络）。
func (r *Runner) Close() error {
	if r == nil || r.bridgeClient == nil {
		return nil
	}
	c := r.bridgeClient
	r.bridgeClient = nil
	c.Shutdown()
	return nil
}

// Run 执行一次运行。ctx 取消即触发清理（CLI 的 SIGINT 就是这条路径）。
//
// spec 由调用方（CLI 的 flag）给出「这一次跑什么」，装配层负责把**部署级**的
// 字段补齐并归一化：StoreDir / ResultDir / Sandbox.Image / Profile 只有装配层
// 知道，让调用方各自填一遍等于给同一份配置造出多种表示（而 RunSpec 是摘要的
// 输入，两种表示就是两次「配置漂移」）。
//
// **错误原样上抛**：CLI 要按 `harness.IsKind(err, KindCancelled)` 之类做分支，
// 而 `RunResult.Err` 是折叠过的分类串（根包 safeError 的 `%T`），只用于公开
// 结果、不能拿来分支（errors.go 明令禁止解析消息）。
func (r *Runner) Run(ctx context.Context, spec harness.RunSpec) (harness.RunResult, error) {
	if r == nil || r.h == nil {
		return harness.RunResult{}, harness.Ef(harness.KindConfig, "wire.run", "Runner 未装配", nil)
	}
	return r.h.Run(ctx, r.resolve(spec))
}

// DefaultSpec 返回装配层眼里的缺省运行配置。
//
// 用途：调用方只需要覆盖自己关心的字段（例如只改 Targets），其余从这份基线出
// 发——**不要把 RunSpec 的零值直接交给 Run**：零值 Budget 在根包里表示「全部
// 不限」（护栏整条消失），零值 Image 表示「用 executor 的内置 tag」，两者都会
// 让「看起来配好了」与「实际没有护栏」长得一样。
func (r *Runner) DefaultSpec() harness.RunSpec { return r.resolve(harness.RunSpec{}) }

// resolve 把调用方给的 spec 与装配层的部署配置合并成最终 RunSpec。
//
// ⚠️ **RunSpec 会整份写进 run.json（公开文件）**，所以这里只能出现非敏感配置：
// 镜像、预算、目标 code、模型名。凭据一律不经此处（见 Options 的注释）。
func (r *Runner) resolve(spec harness.RunSpec) harness.RunSpec {
	a := r.opts.Agent
	if spec.Scenario == "" {
		spec.Scenario = r.opts.Scenario
	}
	if spec.Agent.Provider == "" {
		spec.Agent.Provider = a.Provider
	}
	if spec.Agent.Model == "" {
		spec.Agent.Model = a.Model
	}
	if spec.Agent.Thinking == "" {
		spec.Agent.Thinking = a.Thinking
	}
	// Approve 以装配层为准：不开的话 pi 会**静默**忽略项目本地资源（M0 实测），
	// 而失败形态是「agent 好像什么都没看见」。RunSpec 里的 bool 没有「显式 false」
	// 与「没给」的区别，所以「关掉它」这件事只能由部署配置表达
	// （Options.Agent.Approve）——这样 CLI 的 --approve=false 也仍然有效。
	spec.Agent.Approve = a.Approve
	if len(spec.Agent.Extensions) == 0 {
		spec.Agent.Extensions = append([]string(nil), a.Extensions...)
	}
	if spec.Agent.SessionDir == "" {
		spec.Agent.SessionDir = a.SessionDir
	}
	if spec.Agent.HomeDir == "" {
		spec.Agent.HomeDir = a.HomeDir
	}
	if len(spec.Targets) == 0 {
		spec.Targets = append([]string(nil), r.opts.Run.Targets...)
	}
	if spec.Budget.MaxRounds <= 0 && spec.Budget.MaxWall <= 0 && spec.Budget.MaxTurns <= 0 {
		if b := r.opts.Run.Budget; b.MaxRounds > 0 || b.MaxWall > 0 || b.MaxTurns > 0 {
			spec.Budget = b
		} else {
			spec.Budget = harness.DefaultBudget()
		}
	}
	if spec.HintPolicy == "" {
		spec.HintPolicy = r.opts.Run.HintPolicy
	}
	if spec.HintPolicy == "" {
		spec.HintPolicy = harness.HintAuto
	}
	if spec.Policy == (harness.PolicySpec{}) {
		spec.Policy = r.opts.Run.Policy
	}
	// 部署级字段：以装配层为准，不接受调用方覆盖。
	//
	// 为什么连 Image 也一起：镜像与「这台机器上装了什么 runner」是一件事，
	// 而 RunSpec 是摘要的输入——两个调用方各填一个 tag 会让同一份配置产生两个
	// 摘要，恢复/比对时表现为「配置漂移」。
	sb := r.opts.Sandbox
	spec.Sandbox.Image = sb.Image
	spec.Sandbox.ProfileDir = sb.ProfileDir
	spec.Sandbox.CPUs, spec.Sandbox.MemoryMB, spec.Sandbox.PidsLimit = sb.CPUs, sb.MemoryMB, sb.PidsLimit
	// Executor 是 v0.3 的遗留面（旧快照读它）。v0.4 只从 Sandbox 取配置，但
	// Image 要两边都填：runChallenge 在 Sandbox.Image 为空时会回落到
	// Executor.Image，而摘要（Digest）把两者都算进去——只填一边会让同一份配置
	// 产生两种摘要。ReadOnly 恒真（v0.4 不提供可写的 rootfs）。
	spec.Executor = harness.ExecutorSpec{Image: sb.Image, CPUs: sb.CPUs, MemoryMB: sb.MemoryMB,
		PidsLimit: sb.PidsLimit, ReadOnly: true}
	spec.StoreDir, spec.ResultDir = r.storeDir, r.resultDir
	if spec.Profile.Empty() {
		spec.Profile = r.profile()
	}
	return spec
}

// profile 返回生效的 solver profile（与 New 时交给 Harness 的那份必须逐字相同，
// 否则摘要对不上：Harness 用装配时那份算 ProfileDigest，RunSpec 里这份是给
// 结果分组用的）。
func (r *Runner) profile() harness.SolverProfile {
	if r.opts.Profile.Name == "" {
		return defaultProfile()
	}
	return r.opts.Profile
}

// defaultProfile 是内置的缺省 profile。
//
// 它的作用目前只有两个：**结果分组**（ProfileDigest 进公开结果，stats 按它
// 过滤）与运行身份。刻意**不**填 SystemPrompt：v0.4 的根包还没有把 profile 的
// prompt 送进 AgentStart 的路径（`Harness.runChallenge` 只转发 `spec.Agent.*`），
// 填了就是一个「看起来在、实际没生效」的字段——那正是本仓库反复强调要避免的
// 形态。等根包接上注入点再填。
func defaultProfile() harness.SolverProfile {
	return harness.SolverProfile{Name: "default"}
}

// ── 离线夹具 ──

// fakeFixture 是离线场景的题目与答案。
type fakeFixture struct {
	Challenges []harness.Challenge `json:"challenges"`
	// Answers 声明「哪些答案被平台接受」：Answers[code][answer] == true。
	Answers      map[string]map[string]bool `json:"answers"`
	ScorePerFlag int                        `json:"scorePerFlag,omitempty"`
}

// demoAnswer / demoChallengeCode 是内置演示题的答案与题号。
//
// ⚠️ 这个串是**明显的假值**：它存在的意义是让离线纵向验收（Fake → Docker →
// stub pi → DAG/Gate → Evaluate → Cleanup）有一条能真的走到「平台确认」的
// 路径，而不是永远停在「跑完了但什么都没解出来」。它不是任何真实平台的 flag。
const (
	demoChallengeCode = "demo-1"
	demoAnswer        = "flag{demo-offline-acceptance}"
)

// loadFakeFixture 载入离线夹具。
//
// 为什么 fake 场景需要调用方给夹具：`scenario.Fake` 的 Challenges 与 Answers
// 是**声明式**的（它就是「平台」本身），它不知道有哪些题、也不知道哪些答案会被
// 接受。所以 CLI 的 `--scenario fake` 必须有地方拿这两样东西，本实现给两条路：
//
//   - `Options.FakeChallenges`：一份 JSON（见 fakeFixture 的字段），供真实的
//     离线验收使用；CLI 侧经 `RED_HARNESS_FAKE_CHALLENGES` 传进来；
//   - 不给就是内置的那一道演示题（demoChallengeCode）。
//
// 两条路都**只服务于离线纵向验收**：fake 场景不碰网络、不碰平台，它的答案明文
// 写在夹具里，所以它永远不能用来证明「模型能解题」——它证明的是编排接线是通的。
func loadFakeFixture(path string) (fakeFixture, error) {
	if strings.TrimSpace(path) == "" {
		return fakeFixture{
			Challenges: []harness.Challenge{{
				Code: demoChallengeCode,
				// Description 里必须出现 flag{...}：gate.NewGate 用题面推断答案
				// 形态（answer.Infer），题面不提信封形态时裸串与信封都可能被收，
				// 演示题不该有这种不确定性。
				Description: "离线演示题（fake 场景）：容器里出现 " + demoAnswer + " 即算解出。本场景不接任何真实平台。",
				Category:    "misc",
				FlagCount:   1,
				// Addrs 是 sandbox 白名单的唯一来源（executor 直接读它渲染
				// iptables）。演示题不需要网络，给一个本地回环地址：它让
				// 白名单非空（空白名单会让容器出站全被拒绝），又不会真的
				// 打到任何东西上。
				Addrs: []string{"127.0.0.1:1"},
			}},
			Answers:      map[string]map[string]bool{demoChallengeCode: {demoAnswer: true}},
			ScorePerFlag: 1,
		}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fakeFixture{}, harness.Ef(harness.KindConfig, "wire.fake", "读取离线夹具失败: "+path, err)
	}
	var fx fakeFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		return fakeFixture{}, harness.Ef(harness.KindConfig, "wire.fake", "解析离线夹具失败: "+path, err)
	}
	if len(fx.Challenges) == 0 {
		return fakeFixture{}, harness.Ef(harness.KindConfig, "wire.fake",
			"离线夹具里没有任何题目: "+path, nil)
	}
	if len(fx.Answers) == 0 {
		// 没有可接受答案 ⇒ 任何候选都判不接受 ⇒ 每次运行都以 no_progress 收场。
		// 那是「看起来跑通了、其实什么都没验证」，明确拒绝。
		return fakeFixture{}, harness.Ef(harness.KindConfig, "wire.fake",
			"离线夹具里没有任何可接受答案：这道题永远无法被解出，运行只会以 no_progress 收场: "+path, nil)
	}
	return fx, nil
}

// ── 体检 ──

// Doctor 合成一份体检报告：根包的端口齐备性 + Docker/runner 镜像 +
// （tsecbench 场景下的）bridge 与 VPN 预检。
//
// **它不做任何平台写操作**：CheckVPN 是一次只读预检，而它在 v0.4 里是「能不能
// 上平台」的判据（前身事故：预检失败被当成「题目不存在」继续跑，白烧预算）。
//
// ⚠️ **凭据只报「是否设置」**：值一旦打印就会进终端 scrollback、工单与 CI 日志。
func (r *Runner) Doctor(ctx context.Context) harness.DoctorReport {
	if r == nil || r.h == nil {
		return harness.DoctorReport{OK: false, Checks: []harness.DoctorCheck{
			{Name: "harness", OK: false, Detail: "Runner 未装配", Fatal: true}}}
	}
	rep := r.h.Doctor(ctx)
	rep.Checks = append(rep.Checks,
		harness.DoctorCheck{Name: "scenario_name", OK: true, Detail: r.scenario},
		r.dockerCheck(ctx),
		r.imageCheck(ctx),
		credentialCheck(r.opts.Agent.Provider, r.opts.EnvFile),
	)
	if r.scenario == ScenarioTSecBench {
		rep.Checks = append(rep.Checks, r.bridgeChecks(ctx)...)
	}
	for _, c := range rep.Checks {
		if c.Fatal && !c.OK {
			rep.OK = false
		}
	}
	return rep
}

// dockerCheck 检查 Docker daemon 是否可用。
func (r *Runner) dockerCheck(ctx context.Context) harness.DoctorCheck {
	if r.docker == nil {
		return harness.DoctorCheck{Name: "docker", OK: false, Detail: "执行器未装配", Fatal: true}
	}
	if err := r.docker.Available(ctx); err != nil {
		// 只报「不可用」这一件事：底层错误可能带宿主路径与 docker 输出，
		// 而体检报告会被贴进工单。
		return harness.DoctorCheck{Name: "docker", OK: false, Detail: "docker daemon 不可用", Fatal: true}
	}
	return harness.DoctorCheck{Name: "docker", OK: true, Fatal: true}
}

// imageCheck 检查 runner 镜像在不在本机。
//
// 为什么要单独一项：镜像不存在时第一次 Prepare 会失败，而那条失败发生在
// 「已经起过一次 run」之后——体检的意义就是把它提前。
func (r *Runner) imageCheck(ctx context.Context) harness.DoctorCheck {
	img := r.opts.Sandbox.Image
	if img == "" {
		img = defaultImageTag
	}
	cmd := dockerCommand(ctx, "image", "inspect", "--format", "{{.Id}}", img)
	if cmd.err != nil {
		return harness.DoctorCheck{Name: "runner_image", OK: false,
			Detail: "无法执行 docker image inspect（" + cmd.errClass + "）", Fatal: true}
	}
	if strings.TrimSpace(cmd.out) == "" {
		return harness.DoctorCheck{Name: "runner_image", OK: false,
			Detail: "镜像不存在: " + img + "（构建：docker build -t " + img + " runner/）", Fatal: true}
	}
	// 镜像 ID 是公开信息（不含凭据），带上它便于排查「跑的是哪个镜像」。
	return harness.DoctorCheck{Name: "runner_image", OK: true,
		Detail: img + " " + shortDigest(cmd.out), Fatal: true}
}

// bridgeChecks 是 tsecbench 场景特有的两项：桥进程 + VPN 预检。
func (r *Runner) bridgeChecks(ctx context.Context) []harness.DoctorCheck {
	if r.bridgeClient == nil {
		return []harness.DoctorCheck{{Name: "bridge", OK: false,
			Detail: "bridge 未启动（场景 tsecbench 必须有可用的 Python SDK 桥）", Fatal: true}}
	}
	out := []harness.DoctorCheck{{Name: "bridge", OK: true,
		Detail: fmt.Sprintf("已启动（重启次数 %d）", r.bridgeClient.Restarts()), Fatal: true}}
	out = append(out, harness.DoctorCheck{Name: "benchmark_token",
		OK: strings.TrimSpace(os.Getenv(bridge.EnvToken)) != "",
		// **只报是否设置，绝不打印值**。
		Detail: tokenDetail(), Fatal: false})
	if err := r.bridgeClient.CheckVPN(ctx); err != nil {
		// VPN 预检失败是 Fatal：带着它去跑题只会白烧预算，而且失败形态是
		// 「题目看起来不存在」（前身事故）。
		out = append(out, harness.DoctorCheck{Name: "vpn", OK: false,
			Detail: "VPN 预检未通过（平台不可达）", Fatal: true})
	} else {
		out = append(out, harness.DoctorCheck{Name: "vpn", OK: true, Fatal: true})
	}
	return out
}

// tokenDetail 只描述「设没设」。
func tokenDetail() string {
	if strings.TrimSpace(os.Getenv(bridge.EnvToken)) != "" {
		return "已设置（值不打印）"
	}
	return "未设置（" + bridge.EnvToken + " 为空；桥会在首次平台调用时报 missing_credential）"
}

// credentialCheck 检查 provider 凭据**是否设置**（不检查值）。
//
// 为什么值得单独一项：pi 把 401 呈现成一次**静默的空会话**（M0 实测
// stopReason=error + 空 content + 照样 agent_settled），整个题库会以「跑完了但
// 什么都没发生」的形式被烧掉。这里提前说一句，好过事后从结果里反推。
//
// 名字列表与 `piai/agent.go` 的 providerCredentialNames 是同一份**知识**的两次
// 表达——真源在那里（它决定真正注入什么），这里是**告警**（非 Fatal）。之所以
// 不去 import 它：那个函数没有导出，而为了体检去改 piai 的公开面不值得。
func credentialCheck(provider, envFile string) harness.DoctorCheck {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return harness.DoctorCheck{Name: "provider_credentials", OK: false,
			Detail: "未指定 provider（--provider）：agent 起不来", Fatal: true}
	}
	names := []string{strings.ToUpper(strings.ReplaceAll(provider, "-", "_")) + "_API_KEY"}
	switch provider {
	case "opencode-go", "opencode":
		names = append(names, "OPENCODE_API_KEY", "OPENCODE_GO_API_KEY")
	case "anthropic":
		names = append(names, "ANTHROPIC_API_KEY")
	}
	if envHasAny(envFile, names) {
		return harness.DoctorCheck{Name: "provider_credentials", OK: true,
			Detail: provider + "：已设置（值不打印）", Fatal: true}
	}
	return harness.DoctorCheck{Name: "provider_credentials", OK: false,
		Detail: provider + "：未设置（" + strings.Join(names, " / ") + "）；pi 会以静默空会话失败",
		Fatal:  true}
}

// envHasAny 报告这些名字里有没有任何一个被设置了（宿主环境或 .env）。
//
// **只回答「有/没有」，绝不返回值**。.env 的解析复用 piai 的 LoadEnv（向上逐级
// 查找），因为「pi 从哪儿读凭据」这件事的真源就是它——在这里另写一套查找规则
// 只会让体检与真实启动路径不一致。
func envHasAny(envFile string, names []string) bool {
	for _, n := range names {
		if strings.TrimSpace(os.Getenv(n)) != "" {
			return true
		}
	}
	start := "."
	if strings.TrimSpace(envFile) != "" {
		start = filepath.Dir(envFile)
	}
	env, _, err := piai.LoadEnv(start)
	if err != nil {
		return false
	}
	for _, n := range names {
		if strings.TrimSpace(env[n]) != "" {
			return true
		}
	}
	return false
}

// shortDigest 把 `sha256:...` 截成前 12 位十六进制。
func shortDigest(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// ── docker 只读查询 ──

// dockerResult 是一次 docker 查询的结果。
type dockerResult struct {
	out string
	// errClass 是失败分类（只放类型名，不放输出）：docker 的输出可能带宿主路径。
	errClass string
	err      error
}

// dockerCommand 跑一条**只读**的 docker 命令。
//
// 为什么不用 executor 包：executor 的 docker 调用全是围绕容器生命周期的写操作
// （它的 run 方法不可导出，也不接受任意的只读查询）。体检需要的是
// `docker image inspect` 这种纯查询，在本包内用 exec 直连 docker CLI 是最小实现
// ——仓库的零第三方依赖纪律决定了「引 Docker SDK」不是一个选项。
func dockerCommand(ctx context.Context, args ...string) dockerResult {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	// stderr 丢弃：docker 的错误输出可能带宿主路径与 daemon 地址，而体检报告
	// 会被贴进工单。失败与否由 Run 的返回值回答，够用了。
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return dockerResult{errClass: fmt.Sprintf("%T", err), err: err}
	}
	return dockerResult{out: out.String()}
}
