// Package store 是运行的**文件存储**：领域事件日志、公开快照、DAG 载荷与
// 私密账本。
//
// ── 它是什么 ──
//
// 一个 `legacy.Store` + `legacy.GraphStore` 的文件实现，零第三方依赖。
// 目录布局见设计文档 §6：
//
//	<StoreDir>/runs/<runID>/
//	├── run.json       0600  快照
//	├── graph.json     0600  DAG（store 只透传 []byte，不理解内容）
//	├── graph.mmd      0600  DAG 的人可读导出（mermaid），与 graph.json 同权限
//	├── events.jsonl   0600  领域事件，每行一个 DomainEvent
//	├── private/       0700
//	│   ├── candidates.jsonl  0600  候选明文账本
//	│   └── evidence/         0700  原始工具输出（含明文）
//	├── report.json    0644
//	└── report.md      0644
//
// ── 三条不可动的性质 ──
//
//  1. **Append 先写事件、再原子写快照。** 反过来会在崩溃时产生「快照指向
//     不存在的事件」——恢复以 `lastAppliedSeq` 为基线重放，指向不存在的事件
//     意味着整次运行不可恢复。
//
//  2. **序号单调**。`Append` 拒绝 `ev.Seq != lastSeq+1`。跳号说明有两个写者
//     或调用方算错了序号，两种情况都必须立刻停下：静默接受会让事件日志出现
//     空洞，而恢复重放会带着错位的状态机继续跑。
//
//  3. **明文只进 private/**。候选明文绝不出现在 run.json / events.jsonl /
//     graph.json / report.* 里（设计文档 §3.3）。
//
// ── 前提：单写者 ──
//
// 一个 run 目录在任一时刻只应由**一个**进程写。引擎的 runLoop 是唯一状态
// 写入者，store 依赖这条约定（`repairTornTail` 会截掉日志末尾的半行——两个
// 进程同时追加时会互相截断；唯一临时文件名只能保证不撞车，不能保证不丢数据）。
// 前提被破坏时不会静默丢数据：序号冲突会被 `Append` 拒绝。
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/legacy"
)

// 目录与文件名常量。集中在这里是为了让「布局」这件事只有一处定义——
// 散落的字符串字面量是文档与代码漂移的开始。
const (
	runsDirName      = "runs"
	snapshotFileName = "run.json"
	graphFileName    = "graph.json"
	// graphExportFileName 是图的人可读导出（mermaid），与 graph.json 并排。
	// 它与图同一个权限：里面有凭证事实与目标地址，只是**形状**更适合人读。
	graphExportFileName = "graph.mmd"
	eventsFileName      = "events.jsonl"
	privateDirName      = "private"
	// candidatesFileName 是候选明文账本。
	candidatesFileName = "candidates.jsonl"
	evidenceDirName    = "evidence"
	// reportJSONName / reportMDName 是公开报告（0644）。store 只负责它们的
	// 路径与权限，内容由 report 包生成。
	reportJSONName = "report.json"
	reportMDName   = "report.md"

	dirPerm     os.FileMode = 0o700
	privatePerm os.FileMode = 0o600
	publicPerm  os.FileMode = 0o644
)

// FileStore 是存储根的句柄：`<StoreDir>`。它只拥有路径，不拥有某个 run。
//
// 为什么构造函数的签名是 `New(dir string) (*FileStore, error)`（只收根目录、
// 不收 RunID）：
//
//   - 根目录是**进程级**配置（`RunSpec.StoreDir`），一个进程只解析一次；
//     RunID 是**每次运行**才知道的东西（Engine.Start 里才生成）。
//   - 根目录要在构造时就校验可写（建不出来就必须立刻失败，而不是等到第一次
//     Append——那时 run 已经跑起来了，失败意味着白跑一轮）。
//   - 一个 Store 实例要能服务多个 run（`Engine.List` 需要扫全部 run 目录），
//     所以 run 是**每次取用**的视图而不是构造参数。
//
// 取某个 run 的视图用 `ForRun(id)`；它返回的 `*FileStore` 满足 `legacy.Store`
// 与 `legacy.GraphStore` 两个契约接口（编译期断言见文件末尾）。
type FileStore struct {
	root string
	// runID 为空时这是「根句柄」，只能用来列 run 或取 run 视图；非空时
	// 它是某个 run 的视图，全部读写都落在这个 run 目录里。
	runID harness.RunID
	dir   string

	// snapshotHook 是**测试专用**的快照写注入点。非 nil 时它在写 run.json
	// 之前被调用，返回错误即模拟「快照写失败」。
	//
	// 为什么留这个口子：`Append` 的顺序（先事件后快照）是本包最重要的一条
	// 不变量，而它只有靠**注入一次快照写失败**才能被真正测到——正常路径下
	// 两种顺序的结果完全一样（都成功）。见 TestAppendOrderEventBeforeSnapshot。
	snapshotHook func() error
}

// New 建出存储根并返回根句柄。
//
// dir 为空 ⇒ KindConfig 错误（不是 KindPersistence）：这是调用方配置错了，
// 重试无意义。
func New(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, &harness.Error{
			Kind: harness.KindConfig, Op: "store.new",
			Msg: "存储根目录为空",
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, &harness.Error{
			Kind: harness.KindConfig, Op: "store.new",
			Msg: "解析存储根目录失败", Err: err,
		}
	}
	// 建根目录时就**显式 chmod 0700**：它下面就是全部 run（含私密账本），
	// 而 umask 会吃掉 MkdirAll 的 mode 参数。只 chmod 自己创建的那几层——
	// 向上 chmod 会碰到 /tmp 这类共享祖先（把 /tmp 变成 0700 会连累全机）。
	if err := mkdirAllPrivate(abs, dirPerm); err != nil {
		return nil, &harness.Error{
			Kind: harness.KindPersistence, Op: "store.new",
			Msg: "建存储根目录失败", Err: err,
		}
	}
	if err := mkdirAllPrivate(filepath.Join(abs, runsDirName), dirPerm); err != nil {
		return nil, &harness.Error{
			Kind: harness.KindPersistence, Op: "store.new",
			Msg: "建 runs 目录失败", Err: err,
		}
	}
	return &FileStore{root: abs, dir: abs}, nil
}

// Dir 返回存储根目录（根句柄）或本次运行目录（run 视图）。
func (s *FileStore) Dir() string { return s.dir }

// RunDir 返回某个 run 的目录路径（不创建）。CLI 用它把 `--run <id>` 解析成
// 路径，而不必自己拼 `<store>/runs/<id>`（布局只有一处定义）。
func (s *FileStore) RunDir(id harness.RunID) string {
	return filepath.Join(s.root, runsDirName, string(id))
}

// ForRun 返回某个 run 的存储视图，并**建好**该 run 的全部目录与文件骨架。
//
// 建骨架的时机是这里而不是第一次写：`Engine.Start` 拿到 run 目录的那一刻，
// 目录布局就必须已经存在（CLI 的 `list` 要能立刻看见这个 run，看板要能读
// run.json）。id 为空 ⇒ KindConfig 错误——它同时是目录名，空 id 会让所有 run
// 撞进同一个目录。
func (s *FileStore) ForRun(id harness.RunID) (*FileStore, error) {
	if id == "" {
		return nil, &harness.Error{
			Kind: harness.KindConfig, Op: "store.for_run",
			Msg: "RunID 为空",
		}
	}
	if err := validRunID(id); err != nil {
		return nil, err
	}
	dir := s.RunDir(id)
	if err := mkdirAllPrivate(dir, dirPerm); err != nil {
		return nil, &harness.Error{
			Kind: harness.KindPersistence, Op: "store.for_run", RunID: id,
			Msg: "建运行目录失败", Err: err,
		}
	}
	// 私密账本骨架：**首次创建时**建一个空账本，这样「账本缺失」只可能是
	// 被删/被拷漏（恢复路径必须 fail closed），而不是「刚建出来」。
	// 见 private.go 的 ensureLedger 与 Rejected 的注释。
	if err := ensureLedger(dir); err != nil {
		return nil, &harness.Error{
			Kind: harness.KindPersistence, Op: "store.for_run", RunID: id,
			Msg: "初始化私密账本失败", Err: err,
		}
	}
	return &FileStore{root: s.root, runID: id, dir: dir}, nil
}

// RunID 返回这个视图对应的 run（根句柄返回空串）。
func (s *FileStore) RunID() harness.RunID { return s.runID }

// ListRuns 列出存储根下已有的 run id（按目录名排序）。目录不存在 ⇒ 空列表。
//
// 为什么由 store 提供而不是让 CLI 自己 glob：run 目录的存在性判据（必须是
// 目录、必须不是临时名、必须通过 validRunID）是布局的一部分，散到调用方
// 就会漂移——而漂移的表现是「`list` 里出现一个 `resume` 打不开的 run」。
func (s *FileStore) ListRuns() ([]harness.RunID, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, runsDirName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, &harness.Error{
			Kind: harness.KindPersistence, Op: "store.list_runs",
			Msg: "读 runs 目录失败", Err: err,
		}
	}
	var out []harness.RunID
	for _, e := range entries {
		if !e.IsDir() || validRunID(harness.RunID(e.Name())) != nil {
			continue
		}
		out = append(out, harness.RunID(e.Name()))
	}
	return out, nil
}

// ReportPath 返回公开报告在**本次运行目录**下的路径。
//
// 只用于展示与诊断（例如在报告里写「报告本身在哪」）。**写报告请用 PutReport**
// ——它同时负责权限（0644）与报告名白名单，而裸路径让调用方自己 os.WriteFile
// 会把 umask 吃掉的权限问题重新引进来。
func (s *FileStore) ReportPath(name string) string { return filepath.Join(s.dir, name) }

// PutReport 原子写一份公开报告并把它置为 0644。
//
// 为什么 store 要管报告的权限而不是让 report 包自己 chmod：0644 与 0600 的
// 区别正是「公开面」与「私密面」的分界，而这条分界是本包的核心不变量之一
// （设计文档 §6/§3.3）。把它放在报告包意味着每个写报告的调用点都要记得做对，
// 漏一次的表现是「报告打不开」（噪音）或者更糟——有人为了让它能打开而把整个
// run 目录放宽到 0755，连带把 private/ 暴露出去。
//
// name 只接受布局里定义的两个名字：报告路径来自调用方（CLI 的 --out 之类），
// 放开它会变成又一个路径逃逸面。
func (s *FileStore) PutReport(name string, data []byte) error {
	if s.runID == "" {
		return &harness.Error{
			Kind: harness.KindConfig, Op: "store.put_report",
			Msg: "根句柄不能写报告：先用 ForRun 取运行视图",
		}
	}
	if name != reportJSONName && name != reportMDName {
		return &harness.Error{
			Kind: harness.KindConfig, Op: "store.put_report", RunID: s.runID,
			Msg: fmt.Sprintf("报告名 %q 非法（只允许 %s / %s）", name, reportJSONName, reportMDName),
		}
	}
	path := filepath.Join(s.dir, name)
	// 报告里只有计数与指纹（明文只进 private/），但**权限必须显式设**：
	// umask 会吃掉创建时的 mode，而 0644 是它作为交付物的前提。
	if err := writeFileAtomic(path, data, publicPerm); err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.put_report", RunID: s.runID,
			Msg: "写报告失败", Err: err,
		}
	}
	if err := SetPublicFileMode(path); err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.put_report", RunID: s.runID,
			Msg: "设置报告权限失败", Err: err,
		}
	}
	return nil
}

// ── legacy.Store ──

// Append 追加一条领域事件，然后原子写快照。
//
// **顺序不可颠倒**（本包的头号不变量）：先写 events.jsonl，再原子写 run.json。
// 先快照后事件会在崩溃时产生「快照的 lastAppliedSeq 指向一条不存在的事件」，
// 恢复时无法重放——用户看到的是「run 损坏」，而根因只是一次崩溃的时机。
// 顺序被写死在代码里，且有 TestAppendOrderEventBeforeSnapshot 注入快照写失败
// 来钉住它（正常路径下两种顺序结果一样，只有注入失败才测得出）。
//
// 快照写失败时**事件已经落盘**，函数返回 KindPersistence 错误：调用方（引擎）
// 必须知道这次进展没被记进快照。这不矛盾——重放时快照落后于日志是安全的
// （恢复以 min(快照, 日志) 为基线重放），反过来才不安全。
func (s *FileStore) Append(ev legacy.DomainEvent) error {
	if s.runID == "" {
		return &harness.Error{
			Kind: harness.KindConfig, Op: "store.append",
			Msg: "根句柄不能 Append：先用 ForRun 取运行视图",
		}
	}
	if err := s.checkSeq(ev); err != nil {
		return err
	}
	// 1) 事件先落盘。整行一次 write + fsync（见 appendEventLine）。
	line, err := json.Marshal(ev)
	if err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.append", RunID: s.runID,
			Msg: "领域事件序列化失败", Err: err,
		}
	}
	if err := s.appendEventLine(append(line, '\n')); err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.append", RunID: s.runID,
			Msg: "写事件日志失败", Err: err,
		}
	}
	// 2) 再原子写快照。这里用 ev 自身更新快照的最小字段——完整的 Snapshot
	// 由引擎通过 PutSnapshot 写入（它知道 Objective/BudgetUsed/State 等）。
	if err := s.writeSnapshotFromEvent(ev); err != nil {
		return err
	}
	return nil
}

// checkSeq 校验序号单调性，并回填 lastSeq 缓存。
func (s *FileStore) checkSeq(ev legacy.DomainEvent) error {
	last, err := s.LastSeq()
	if err != nil {
		return err
	}
	if ev.Seq != last+1 {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.append", RunID: s.runID,
			Msg: fmt.Sprintf("事件序号不单调：收到 seq=%d，期望 %d（跳号说明有两个写者或调用方算错）",
				ev.Seq, last+1),
		}
	}
	return nil
}

// LastSeq 返回「下一条事件应有的序号 - 1」。
//
// 取值来源是两者的**较大值**：
//
//   - 磁盘上事件日志里最后一条**完整**行的序号（撕裂末行不计——它的序号只
//     写了一半，算进去会让下一条跳到空洞上）；
//   - 快照的 `lastAppliedSeq`（事件写完但快照写失败时，日志里其实已经有了
//     那条事件，正常两者一致；快照更大只可能出现在人工修数据之后，那时以
//     快照为准更安全，因为恢复基线就是它）。
//
// 首次调用（两者都没有）⇒ 0，于是第一条事件必须是 Seq=1。
func (s *FileStore) LastSeq() (int64, error) {
	disk, err := lastSeqOnDisk(s.eventsPath())
	if err != nil {
		return 0, &harness.Error{
			Kind: harness.KindPersistence, Op: "store.last_seq", RunID: s.runID,
			Msg: "读事件日志末尾序号失败", Err: err,
		}
	}
	snap, err := s.readSnapshot()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return disk, nil // 首跑：还没有快照
		}
		return 0, err
	}
	if snap.LastAppliedSeq > disk {
		return snap.LastAppliedSeq, nil
	}
	return disk, nil
}

// writeSnapshotFromEvent 用事件里的最小信息更新快照。
//
// 快照的**完整**内容由引擎提供（`PutSnapshot`），这里只做一件在 Append 语义
// 下必须原子完成的事：把 `lastAppliedSeq` 推到刚写入的事件上。若快照还不
// 存在（引擎还没写过），就按事件的 RunID 造一个最小快照——恢复路径要求
// run.json 与 events.jsonl 同时存在，缺一个都无法判定「这是首跑还是损坏」。
func (s *FileStore) writeSnapshotFromEvent(ev legacy.DomainEvent) error {
	if s.snapshotHook != nil {
		if err := s.snapshotHook(); err != nil {
			return &harness.Error{
				Kind: harness.KindPersistence, Op: "store.append", RunID: s.runID,
				Msg: "写快照失败（事件已落盘）", Err: err,
			}
		}
	}
	snap, err := s.readSnapshot()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		snap = legacy.Snapshot{
			SchemaVersion: legacy.SchemaVersion,
			RunID:         ev.RunID,
			State:         harness.RunCreated,
		}
	}
	snap.LastAppliedSeq = ev.Seq
	if snap.RunID == "" {
		snap.RunID = ev.RunID
	}
	return s.PutSnapshot(snap)
}

// PutSnapshot 原子写快照（run.json）。
//
// 它在 `legacy.Store` 之外——引擎每轮末需要写完整的快照（State/Objective/
// BudgetUsed/Public），而 `Append` 只带得动 lastAppliedSeq。恢复路径只依赖
// `Snapshot()`，所以这个额外方法不扩大契约的语义面。
//
// **不校验 lastAppliedSeq 与日志的关系**：快照落后于日志是合法的（崩溃后
// 就是这样），恢复以快照为基线重放即可。反过来（快照超过日志末尾）由
// `LastSeq` 取较大值来兜底，不需要在这里拒绝——拒绝会让「崩溃后无法续跑」
// 变成「崩溃后连修都不能修」。
func (s *FileStore) PutSnapshot(snap legacy.Snapshot) error {
	if s.runID == "" {
		return &harness.Error{
			Kind: harness.KindConfig, Op: "store.put_snapshot",
			Msg: "根句柄不能写快照：先用 ForRun 取运行视图",
		}
	}
	if snap.SchemaVersion == 0 {
		// 零值 schema 会让恢复路径无法判断兼容性（它按 SchemaVersion 判），
		// 静默补上当前版本号是安全的：调用方显式设过的话就不会是 0。
		snap.SchemaVersion = legacy.SchemaVersion
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.put_snapshot", RunID: s.runID,
			Msg: "快照序列化失败", Err: err,
		}
	}
	if err := writeFileAtomic(s.snapshotPath(), b, privatePerm); err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.put_snapshot", RunID: s.runID,
			Msg: "写快照失败", Err: err,
		}
	}
	return nil
}

// Snapshot 读回快照。文件不存在 ⇒ 包装了 os.ErrNotExist 的错误。
//
// 为什么「不存在」与「损坏」必须是两个可区分的错误：调用方靠
// `errors.Is(err, os.ErrNotExist)` 判定「这是首跑，可以新建」，而把一次损坏
// 伪装成「不存在」会让调用方把整个 run 从头再跑一遍（重复向平台提交）。
func (s *FileStore) Snapshot(_ context.Context) (legacy.Snapshot, error) {
	if s.runID == "" {
		return legacy.Snapshot{}, &harness.Error{
			Kind: harness.KindConfig, Op: "store.snapshot",
			Msg: "根句柄不能读快照：先用 ForRun 取运行视图",
		}
	}
	return s.readSnapshot()
}

// readSnapshot 读并解析 run.json（不做 runID 校验，Append 内部也要用）。
func (s *FileStore) readSnapshot() (legacy.Snapshot, error) {
	b, err := os.ReadFile(s.snapshotPath())
	if err != nil {
		return legacy.Snapshot{}, fmt.Errorf("读快照 %s 失败: %w", s.snapshotPath(), err)
	}
	var snap legacy.Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return legacy.Snapshot{}, &harness.Error{
			Kind: harness.KindPersistence, Op: "store.snapshot", RunID: s.runID,
			Msg: "快照解析失败（run.json 被截断或被人工改坏）", Err: err,
		}
	}
	return snap, nil
}

// Private 返回私密账本。明文只在这里（设计文档 §3.3）。
func (s *FileStore) Private() legacy.EvidenceStore {
	return &evidenceStore{runDir: s.dir}
}

// ── legacy.GraphStore ──

// PutGraph 原子写 DAG 载荷（graph.json）。
//
// store **不理解** blob 的内容：`dag/store.go` 的 schema 1 + migrate 是前向
// 兼容的唯一实现，store 再写一份 DAG 序列化就会有第二份实现（v0.2 的
// `dag.FlagFingerprint` 就是这样被分叉出来的）。而且 store 一旦导入 dag，
// 两个包就不能并行开发了——它们本来就是不同波次的独立子系统。
func (s *FileStore) PutGraph(blob legacy.GraphBlob) error {
	if s.runID == "" {
		return &harness.Error{
			Kind: harness.KindConfig, Op: "store.put_graph",
			Msg: "根句柄不能写图：先用 ForRun 取运行视图",
		}
	}
	if err := writeFileAtomic(s.graphPath(), blob, privatePerm); err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.put_graph", RunID: s.runID,
			Msg: "写图失败", Err: err,
		}
	}
	return nil
}

// GetGraph 读回 DAG 载荷。文件不存在 ⇒ 包装了 os.ErrNotExist 的错误，调用方
// 据此区分「首跑」（用题目新建一张图）与「损坏」（必须让用户知道，不能悄悄
// 新建空图——那等于把已积累的事实全部丢掉）。
func (s *FileStore) GetGraph() (legacy.GraphBlob, error) {
	if s.runID == "" {
		return nil, &harness.Error{
			Kind: harness.KindConfig, Op: "store.get_graph",
			Msg: "根句柄不能读图：先用 ForRun 取运行视图",
		}
	}
	b, err := os.ReadFile(s.graphPath())
	if err != nil {
		return nil, fmt.Errorf("读图 %s 失败: %w", s.graphPath(), err)
	}
	return b, nil
}

// SetSnapshotHook 装一个测试用的快照写注入点（见 snapshotHook 字段）。
func (s *FileStore) SetSnapshotHook(fn func() error) { s.snapshotHook = fn }

func (s *FileStore) snapshotPath() string { return filepath.Join(s.dir, snapshotFileName) }
func (s *FileStore) graphPath() string    { return filepath.Join(s.dir, graphFileName) }
func (s *FileStore) graphExportPath() string {
	return filepath.Join(s.dir, graphExportFileName)
}

// PutGraphExport 写图的人可读导出（mermaid），与 graph.json 并排、同权限。
//
// **为什么由 store 写而不是让装配层自己 os.WriteFile**：权限与原子性只有这一处
// 定义。绕过它自己写，文件权限会落到进程的 umask（通常是 0644），而这份导出里有
// 凭证事实与目标地址——`dag.Graph.Save` 的注释专门为这个理由把权限定在 0600。
//
// 载荷对 store 依然是不透明的：它不理解 mermaid，只负责路径、权限与原子性
// （与 PutGraph 同一条纪律）。
func (s *FileStore) PutGraphExport(blob []byte) error {
	if s.runID == "" {
		return &harness.Error{
			Kind: harness.KindConfig, Op: "store.put_graph_export",
			Msg: "根句柄不能写图导出：先用 ForRun 取运行视图",
		}
	}
	if err := writeFileAtomic(s.graphExportPath(), blob, privatePerm); err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.put_graph_export", RunID: s.runID,
			Msg: "写图导出失败", Err: err,
		}
	}
	return nil
}

// validRunID 校验 RunID 能安全地当**单个目录名**用。
//
// 为什么必须挡：RunID 来自调用方（CLI 的 `--run`、API 的调用点），一个含
// `../` 的 id 会让 run 目录落到存储根之外——而存储根之外是**公开面**，私密
// 账本会写在那儿。用 filepath.IsLocal 作为第一道判据（它挡住绝对路径与
// `..`），再显式排除分隔符：`a/b` 是 local 的，但它会在存储根下造出嵌套
// 目录，于是 `ListRuns` 看到的「run」与 `ForRun` 收到的 id 不再是同一个东西。
func validRunID(id harness.RunID) error {
	s := string(id)
	bad := &harness.Error{
		Kind: harness.KindConfig, Op: "store.run_id",
		Msg: fmt.Sprintf("RunID %q 非法：必须是单个目录名（不得含路径分隔符或 ..）", s),
	}
	if s == "" || !filepath.IsLocal(s) || s == "." || s == ".." {
		return bad
	}
	if strings.ContainsAny(s, `/\`) || s[0] == '.' {
		// 以点开头会与隐藏文件、临时文件、`.tmp.*` 混淆。
		return bad
	}
	return nil
}

// 编译期断言：run 视图必须同时满足两个契约接口。
var (
	_ legacy.Store      = (*FileStore)(nil)
	_ legacy.GraphStore = (*FileStore)(nil)
)
