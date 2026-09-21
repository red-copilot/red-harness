package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	harness "github.com/red-copilot/red-harness"
)

const resultsDirName = "results"

// 公开结果里的失败分类与原因串。见 sanitizeErrorClass / sanitizeReason。
const (
	// errClassUnclassified 是分类无法表达时的兜底值。
	//
	// 为什么需要兜底而不是直接丢空：空串与「没有失败」同形，调用方会把一次
	// 被脱敏掉的失败读成成功。
	errClassUnclassified = "unclassified"
	// maxErrorClassLen 是分类串的长度上限。Go 的 %T 输出不会接近它，
	// 超过它基本可以断定不是类型名（例如有人把整段响应体塞进了 Err）。
	maxErrorClassLen = 64
	// reasonUnrecognized 是 Reason 不是枚举标识符时的兜底值。
	reasonUnrecognized = "unrecognized"
	// maxReasonLen 是 Reason 的长度上限。Reason* 常量都在 12 字符以内。
	maxReasonLen = 32
)

// ResultFileStore persists only public aggregate metrics. It never marshals
// OutcomeView, Candidate or Flags; those are return/private values.
type ResultFileStore struct{ root string }

var _ harness.ResultStore = (*ResultFileStore)(nil)

func NewResultStore(dir string) (*ResultFileStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, harness.Ef(harness.KindConfig, "resultstore.new", "结果目录为空", nil)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, harness.Ef(harness.KindConfig, "resultstore.new", "解析结果目录失败", err)
	}
	if err := mkdirAllPrivate(filepath.Join(abs, resultsDirName), dirPerm); err != nil {
		return nil, harness.Ef(harness.KindPersistence, "resultstore.new", "创建结果目录失败", err)
	}
	return &ResultFileStore{root: filepath.Join(abs, resultsDirName)}, nil
}

// publicResult 是公开结果的落盘 schema。
//
// 它是**存储层自己的**结构，不是根包契约：加字段只影响新写的文件，读旧文件
// 由 encoding/json 的零值语义兜住（缺字段 = 0 / 空）。
type publicResult struct {
	RunID         string            `json:"runId"`
	Scenario      string            `json:"scenario"`
	ProfileDigest string            `json:"profileDigest,omitempty"`
	Model         string            `json:"model,omitempty"`
	StartedAt     time.Time         `json:"startedAt"`
	EndedAt       time.Time         `json:"endedAt"`
	Completed     bool              `json:"completed"`
	Reason        string            `json:"reason,omitempty"`
	Err           string            `json:"errorClass,omitempty"`
	Challenges    []publicChallenge `json:"challenges,omitempty"`
}

// publicChallenge 是一道题的公开指标。
//
// **RemainingAtStart 与 ProgressTotal 必须落盘**：v0.4 要求「起跑剩余量与
// 本次增量可重算」——只存最终累计进度的话，事后无法把历史进度从本次能力里
// 减掉，通过率结论会系统性偏高（见 Stats 的注释）。Score / CostUSD 同理，
// 不落盘就没法在 stats 里按题聚合。
type publicChallenge struct {
	Code              string    `json:"code"`
	Category          string    `json:"category,omitempty"`
	StartedAt         time.Time `json:"startedAt"`
	EndedAt           time.Time `json:"endedAt"`
	Reason            string    `json:"reason,omitempty"`
	Submitted         int       `json:"submitted"`
	ProgressConfirmed int       `json:"progressConfirmed"`
	ProgressTotal     int       `json:"progressTotal"`
	// RemainingAtStart 是起跑时还差几个 flag；0 表示分母未知（不得据此宣称召回率）。
	RemainingAtStart int `json:"remainingAtStart,omitempty"`
	// Score 是平台给出的累计得分。
	Score int `json:"score,omitempty"`
	// CostUSD 是这道题消耗的模型成本。
	CostUSD         float64 `json:"costUSD,omitempty"`
	Rounds          int     `json:"rounds"`
	HintUsed        int     `json:"hintUsed"`
	DurationSeconds float64 `json:"durationSeconds"`
}

func toPublic(r harness.RunResult) publicResult {
	p := publicResult{RunID: string(r.RunID), Scenario: r.Scenario, ProfileDigest: r.ProfileDigest,
		Model: r.Model, StartedAt: r.StartedAt, EndedAt: r.EndedAt, Completed: r.Completed,
		Reason: sanitizeReason(r.Reason), Err: sanitizeErrorClass(r.Err)}
	for _, c := range r.Challenges {
		p.Challenges = append(p.Challenges, publicChallenge{Code: c.Challenge.Code,
			Category: c.Challenge.Category, StartedAt: c.StartedAt, EndedAt: c.EndedAt,
			Reason: sanitizeReason(c.Outcome.Reason), Submitted: c.Outcome.Submitted,
			ProgressConfirmed: c.Outcome.ProgressConfirmed, ProgressTotal: c.Outcome.ProgressTotal,
			RemainingAtStart: c.Outcome.RemainingAtStart, Score: c.Outcome.Score,
			CostUSD: c.Outcome.Stats.CostUSD,
			Rounds:  c.Outcome.Rounds, HintUsed: c.Outcome.HintUsed,
			DurationSeconds: c.Outcome.Duration().Seconds()})
	}
	return p
}

// fromPublic 把公开文件读回 RunResult。
//
// **明文一定不在里面**：Flags / Candidates 是返回值，从不落盘（见 toPublic 的
// 白名单式构造）。调用方拿到的是「可比较的指标视图」，不是完整结果。
func fromPublic(p publicResult) harness.RunResult {
	r := harness.RunResult{RunID: harness.RunID(p.RunID), Scenario: p.Scenario,
		ProfileDigest: p.ProfileDigest, Model: p.Model, StartedAt: p.StartedAt,
		EndedAt: p.EndedAt, Completed: p.Completed, Reason: p.Reason, Err: p.Err}
	for _, c := range p.Challenges {
		r.Challenges = append(r.Challenges, harness.ChallengeResult{
			Challenge: harness.Challenge{Code: c.Code, Category: c.Category},
			Outcome: harness.OutcomeView{Code: c.Code, Reason: c.Reason, Submitted: c.Submitted,
				ProgressConfirmed: c.ProgressConfirmed, ProgressTotal: c.ProgressTotal,
				RemainingAtStart: c.RemainingAtStart, Score: c.Score,
				Stats:  harness.Stats{CostUSD: c.CostUSD},
				Rounds: c.Rounds, HintUsed: c.HintUsed, StartedAt: c.StartedAt, EndedAt: c.EndedAt},
			StartedAt: c.StartedAt, EndedAt: c.EndedAt})
	}
	return r
}

// sanitizeErrorClass 把 RunResult.Err 折成一个**可进公开结果**的失败分类。
//
// 为什么不能原样透传：RunResult.Err 是调用方自由填写的字符串——引擎写进去的
// 是 `v04.go` safeError 的 `%T` 结果，但 store 不能假定调用方一定走了那条路。
// 公开结果会被拷进工单、贴进聊天、进 stats 聚合，一次 flag 明文写进去就是
// 泄漏事故（results_test 里有一条固定 canary 的回归钉着这件事）。
//
// 所以只放行「Go 类型名」形态的串：`%T` 的输出必然带包名限定 `.` 或指针前缀
// `*`（`*harness.Error` / `*fmt.wrapError` / `*url.Error`），字符集也限于标识符
// 与限定符。其余一律折叠成 errClassUnclassified。
//
// 为什么不再折成一个固定常量（v0.4 之前是写死的 `"run_error"`）：那样公开结果
// 回答不了「怎么失败的」——provider 故障与执行器故障会变成同一个串，而这两类
// 正是通过率结论必须分开的（roadmap M3「记录失败分类」）。
//
// 已知不足：`%T` 只给出类型，拿不到 `harness.Error.Kind`（provider / executor /
// platform）。要把 Kind 带进公开结果，得由根包的 safeError 输出 Kind——那属于
// 根包改动，不在本文件权限内，已记进实施报告。
func sanitizeErrorClass(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > maxErrorClassLen || !strings.ContainsAny(s, "*.") {
		return errClassUnclassified
	}
	for _, r := range s {
		if !isErrorClassRune(r) {
			return errClassUnclassified
		}
	}
	return s
}

// isErrorClassRune 报告 r 是否可能出现在 Go 类型名里（含指针与包限定符）。
func isErrorClassRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '*', r == '.', r == '_', r == '/', r == '-', r == '[', r == ']':
		return true
	default:
		return false
	}
}

// sanitizeReason 把 Reason / 题级 Reason 折成一个**可进公开结果**的枚举串。
//
// 为什么要脱敏一个「看起来是枚举」的字段：`RunResult.Reason` 与
// `OutcomeView.Reason` 的类型是 string，契约上只要求它们取 Reason* 常量，
// 但类型系统拦不住调用方塞别的东西进去——而这两个字段会**原样落盘**。
// `ReasonError` 那条路径上游就是 error.Error()，把它原样写进公开结果等于
// 开了一条明文/凭据泄漏通道（results_test 的 canary 回归钉着这条）。
//
// 只放行小写标识符形态（Reason* 常量全部如此，且都在 12 字符以内）：非枚举
// 形态一律折叠成 reasonUnrecognized。**不做白名单**——白名单要在 store 里
// 抄一份 Reason* 常量表，那正是「唯一真源」的反面（新增一个 Reason 却忘了
// 同步这里，会让新原因静默消失）。
func sanitizeReason(s string) string {
	if s == "" {
		return ""
	}
	if len(s) > maxReasonLen {
		return reasonUnrecognized
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return reasonUnrecognized
		}
	}
	return s
}

// Save 原子写公开结果。
//
// 权限用 privatePerm（0600）而不是 publicPerm（0644）：**内容层面**它是公开面
// （只有指标与指纹），但**权限层面**它跟随结果目录的 0700/0600 纪律——同一台
// 机器上的其他用户没有理由读得到谁跑了哪些题、用了哪个模型。0644 在这里没有
// 任何收益（结果目录是 0700，放宽文件权限不会让任何人读到它），只会让「哪些
// 文件是公开面」这条判据从权限上消失。报告（report.json/report.md）才是
// 交付物，由 PutReport 显式置 0644。
//
// writeFileAtomic 保证：先写唯一临时名 → chmod（不受 umask 影响）→ fsync(file)
// → rename → fsync(dir)，所以读者永远看不到半截 JSON，权限也一定是 0600。
func (s *ResultFileStore) Save(ctx context.Context, r harness.RunResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.RunID == "" {
		return harness.Ef(harness.KindConfig, "resultstore.save", "RunID 为空", nil)
	}
	if err := validRunID(r.RunID); err != nil {
		return err
	}
	b, err := json.Marshal(toPublic(r))
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.root, string(r.RunID)+".json"), b, privatePerm)
}

func (s *ResultFileStore) Get(ctx context.Context, id harness.RunID) (harness.RunResult, error) {
	if err := ctx.Err(); err != nil {
		return harness.RunResult{}, err
	}
	if err := validRunID(id); err != nil {
		return harness.RunResult{}, err
	}
	b, err := os.ReadFile(filepath.Join(s.root, string(id)+".json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return harness.RunResult{}, &harness.Error{Kind: harness.KindPersistence,
				Op: "resultstore.get", RunID: id, Msg: "结果不存在", Err: err}
		}
		return harness.RunResult{}, err
	}
	var p publicResult
	if err := json.Unmarshal(b, &p); err != nil {
		return harness.RunResult{}, &harness.Error{Kind: harness.KindPersistence,
			Op: "resultstore.get", RunID: id, Msg: "结果文件损坏", Err: err}
	}
	return fromPublic(p), nil
}

// List 列出全部公开结果。
//
// 两条布局纪律，与 `FileStore.ListRuns` 一致：
//
//   - 只认 `<合法 RunID>.json`。名字不合法的文件不是 store 写的（临时文件、
//     编辑器备份、手抄的文件），跳过而不是报错——报错会让一个无关的杂散文件
//     把 `list` 与 `stats` 一起锁死。名字合法性的唯一真源是 validRunID，
//     而 Save 也只写合法 id，所以这条跳过规则不会漏掉任何真实结果。
//   - **合法 id 的文件读不动 = 内容损坏 ⇒ fail closed**（整体失败）。这是有意
//     的取舍：静默跳过会让召回率的分母悄悄变小，而「少算了几道题」在通过率
//     结论里完全看不出来（分母变小 = 召回率虚高）。宁可让调用方拿到错误，
//     也不给它一个看起来正常、实际少了样本的数字。错误里带 RunID，调用方
//     可以据此隔离坏文件后重跑。
func (s *ResultFileStore) List(ctx context.Context) ([]harness.RunResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, harness.Ef(harness.KindPersistence, "resultstore.list", "读结果目录失败", err)
	}
	var out []harness.RunResult
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := harness.RunID(strings.TrimSuffix(e.Name(), ".json"))
		if validRunID(id) != nil {
			continue
		}
		r, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Stats 按 v0.4 口径聚合公开结果。
//
// 召回率的分母是**起跑时剩余量**，不是最终总量：
//
//	本次新增确认 = ProgressConfirmed - (ProgressTotal - RemainingAtStart)
//	召回率      = Σ本次新增确认 / ΣRemainingAtStart
//
// 为什么必须这么算：平台上的题目可能起跑前就已被做掉一部分。把最终累计进度
// 当作本次确认量，会让通过率结论系统性偏高；而两批题目组成不同时（有的题
// 一半已解、有的全新），两个数字之间根本不可比。`RemainingAtStart == 0` 表示
// 分母未知（FlagCount 未知），这类挑战**分子分母都不计入**：聚合后的
// `RemainingAtStart == 0` 就是「这批数据不得宣称召回率」的信号（此时
// `ConfirmedFlags` 也必然是 0）。分母为 0 时召回率必须是 0 而不是 NaN——NaN 会
// 顺着 JSON 变成 null，再进聚合就变成静默污染；所以本文件里**除法只发生在
// 分母已确认大于 0 之后**（见 recallDelta 的 ok 返回值）。
//
// 被排除的那些挑战不会因此丢数据：起跑剩余量、平台累计进度与得分都逐题落在
// 公开结果里，调用方可以自己重算、也可以单独统计它们。
//
// ⚠️ **聚合出的召回率本身没有落点**：根包 `harness.StatsReport` 目前只有
// `ConfirmedFlags` 与 `RemainingAtStart` 两个计数器，没有 RecallRate 字段。
// 所以这里只填这两个计数器（口径如上），比率由调用方相除得出。加字段属于根包
// 契约改动，不在本文件权限内（见实施报告）。
//
// 各字段的口径：
//
//   - Runs / Completed / CompletionRate：**run 级**。CompletionRate = Completed/Runs。
//     （注意 RunResult.Completed 在 v0.4 里目前是「运行无错误且有题」，
//     「运行成功」与「解题成功」的区分属于引擎契约，不在本文件。）
//   - 一旦带了 Challenge/Category 过滤，只有**至少命中一道题**的 run 才计入
//     Runs——否则 Runs 与其余按题聚合的数字口径不一致，CompletionRate 会被
//     不含该题的 run 稀释。
//   - ConfirmedFlags / RemainingAtStart：按题聚合，见上。
//   - Score / CostUSD / DurationSeconds：按题聚合（Score 与 CostUSD 来自
//     Outcome.Score 与 Outcome.Stats.CostUSD）。
//   - HintedRuns：**run 级**——至少有 1 道命中的题用过提示的 run 数
//     （v0.4 之前这里累加的是挑战数，与字段名不符）。
//   - ProviderFailures：run 级。判据是 Reason == harness.ReasonProviderFailure
//     （run 级或命中题的题级）。它是契约常量，不是错误文本匹配。**当前恒为 0**：
//     写入这个 Reason 的引擎路径在 M2 才接通，所以读到 0 表示「没有记录到」，
//     不能读成「没有发生过」。
//   - ExecutionFailures：**没有数据来源，恒为 0**，见下。
func (s *ResultFileStore) Stats(ctx context.Context, q harness.StatsQuery) (harness.StatsReport, error) {
	runs, err := s.List(ctx)
	if err != nil {
		return harness.StatsReport{}, err
	}
	// 只有在调用方真的按题过滤时，「命中 0 道题」才是一个排除理由。
	byChallenge := q.Challenge != "" || q.Category != ""
	var out harness.StatsReport
	for _, r := range runs {
		if !statsMatchRun(r, q) {
			continue
		}
		matched := 0
		hinted := false
		providerFailed := r.Reason == harness.ReasonProviderFailure
		for _, c := range r.Challenges {
			if !statsMatchChallenge(c.Challenge, q) {
				continue
			}
			matched++
			if c.Outcome.HintUsed > 0 {
				hinted = true
			}
			if c.Outcome.Reason == harness.ReasonProviderFailure {
				providerFailed = true
			}
			out.DurationSeconds += c.Outcome.Duration().Seconds()
			out.Score += c.Outcome.Score
			out.CostUSD += c.Outcome.Stats.CostUSD
			if delta, remaining, ok := recallDelta(c.Outcome); ok {
				out.ConfirmedFlags += delta
				out.RemainingAtStart += remaining
			}
		}
		if byChallenge && matched == 0 {
			continue
		}
		out.Runs++
		if r.Completed {
			out.Completed++
		}
		if hinted {
			out.HintedRuns++
		}
		if providerFailed {
			out.ProviderFailures++
		}
	}
	if out.Runs > 0 {
		out.CompletionRate = float64(out.Completed) / float64(out.Runs)
	}
	// ExecutionFailures 保持 0：公开结果里**没有任何字段**能区分「执行器故障」
	// 与「别的故障」——RunResult.Err 是 %T 折出来的类型名（见 safeError），
	// harness.Error.Kind 在落盘前就丢了，而按错误文本猜分类正是 errors.go 明令
	// 禁止的做法。这个字段目前是**死字段**，已作为「应从根包 StatsReport 删除」
	// 提出（本文件无权改根包契约），在删除之前它只会读到 0。
	return out, nil
}

// recallDelta 返回一道题在 v0.4 口径下的「本次新增确认」与「起跑剩余」。
//
// ok 为 false 表示起跑剩余量未知（RemainingAtStart <= 0），这道题既不进召回率
// 的分子也不进分母——它进不了分母是硬约束（不能拿最终总量当分母），既然分母
// 都不算，分子也必须一起排除，否则分子里会混进一道没有对应分母的题，把比率
// 抬高。返回 ok=false 时另外两个返回值都是 0。
//
// 为什么不把 delta 算成负数：平台进度可能回退（题目被重置、并发操作）。负的
// 「新增确认」会把别的题的贡献抵掉，而它表达的其实是「这次没确认到」。
func recallDelta(o harness.OutcomeView) (delta, remaining int, ok bool) {
	remaining = o.RemainingAtStart
	if remaining <= 0 {
		return 0, 0, false
	}
	delta = o.ProgressConfirmed - (o.ProgressTotal - remaining)
	if delta < 0 {
		delta = 0
	}
	return delta, remaining, true
}

// statsMatchRun 是 run 级的过滤维度。
func statsMatchRun(r harness.RunResult, q harness.StatsQuery) bool {
	if q.ProfileDigest != "" && r.ProfileDigest != q.ProfileDigest ||
		q.Model != "" && r.Model != q.Model ||
		q.Scenario != "" && r.Scenario != q.Scenario {
		return false
	}
	if !q.Since.IsZero() && r.StartedAt.Before(q.Since) ||
		!q.Until.IsZero() && r.StartedAt.After(q.Until) {
		return false
	}
	return true
}

// statsMatchChallenge 是题级的过滤维度（Challenge / Category）。
func statsMatchChallenge(c harness.Challenge, q harness.StatsQuery) bool {
	if q.Challenge != "" && c.Code != q.Challenge {
		return false
	}
	if q.Category != "" && c.Category != q.Category {
		return false
	}
	return true
}
