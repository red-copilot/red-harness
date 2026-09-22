package harness

// ── 回收报告 ──
//
// `ReclaimStale` 过去只返回 `[]RunID`（删掉了什么），于是「**发现了但不该删**」
// 这件事在公开面上根本不存在——一个没有通道的判定等于没做。下面这些类型把它
// 变成可读的结果。

// StaleReason 说明一个遗留资源**为什么没被删**。它是穷举的枚举，可安全进公开面。
type StaleReason string

const (
	// StaleOwnerMismatch：资源带 owner 标签，但**不是**本进程的。
	StaleOwnerMismatch StaleReason = "owner_mismatch"
	// StaleOwnerUnknown：资源**没有** owner 标签（升级前的旧资源），或标签值为空。
	// 这是「无法证明归属」，与上一条是两件事：前者是「确实是别人的」，
	// 后者是「不知道是谁的」。
	StaleOwnerUnknown StaleReason = "owner_unknown"
	// StaleUnparsable：扫描输出解析不出 run 标签（docker 版本差异、格式变化、
	// 异常行）。**解析歧义必须退化成「只报告」，永不退化成「删除」**。
	StaleUnparsable StaleReason = "unparsable"
)

// StaleObject 是一个被判定为遗留、但**没有被删除**的资源。
type StaleObject struct {
	// RunID 是从标签读到的 run 标识；解析不出时为空串（此时 Reason 必为
	// StaleUnparsable）。
	RunID RunID
	// Kind 是资源种类："container" / "network" / "iptables" / "proxy"。
	Kind string
	// Owner 是资源上实际带的 owner 标签值；缺失时为空（OwnerUnknown）。
	Owner OwnerID
	// Reason 见 StaleReason。
	Reason StaleReason
}

// ReclaimReport 是一次遗留资源回收的完整结果：删了什么、只报告了什么。
//
// 判据（逐条，实现必须照此）：
//
//	owner 匹配 且 runID ∉ live ⇒ 删除，进 Reclaimed
//	owner 匹配 且 runID ∈  live ⇒ 保留，两条都不进（正常活跃，不是遗留）
//	owner 不匹配                ⇒ 保留，进 Pending(owner_mismatch)
//	无 owner 标签               ⇒ 保留，进 Pending(owner_unknown)
//	解析不出 run 标签           ⇒ 保留，进 Pending(unparsable)
//
// 「保留」是**绝不删除**，不是「稍后重试」。
type ReclaimReport struct {
	// Reclaimed 是本次真正删掉的 runID（去重）。
	Reclaimed []RunID
	// Pending 是发现了但没删的资源，**待人工处置**。
	Pending []StaleObject
}

// ReclaimedTotal 是删除计数，供公开指标使用（公开面只放计数）。
func (r ReclaimReport) ReclaimedTotal() int { return len(r.Reclaimed) }

// PendingTotal 是待处置计数。
func (r ReclaimReport) PendingTotal() int { return len(r.Pending) }
